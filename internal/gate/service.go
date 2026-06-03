package gate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
	"github.com/micronwave/orca/internal/ui"
)

type lineResult struct {
	line  string
	err   error
	epoch uint64
}

// Option configures a Gatekeeper.
type Option func(*Gatekeeper)

// WithTimerFunc replaces the timer constructor used by timed review windows.
// The default is time.NewTimer. Pass a zero-duration factory in tests to make
// the window fire immediately without real-time dependency.
func WithTimerFunc(f func(time.Duration) *time.Timer) Option {
	return func(g *Gatekeeper) { g.timerFunc = f }
}

// Gatekeeper presents decisions to the developer and records their response
// as a DecisionRecord. Every gate call blocks until the developer responds or
// the context is cancelled.
type Gatekeeper struct {
	store *store.FileStore
	// lines is fed by a single goroutine, started lazily on the first Review call.
	// All gate calls receive from this channel, so only one goroutine ever reads
	// from the input at a time (no data races, no buffering loss across calls).
	// Channel capacity 1 lets the reader goroutine park one result while a gate
	// is busy.
	lines     chan lineResult
	in        io.Reader
	out       io.Writer
	startOnce sync.Once
	// reviewMu serializes concurrent Review* calls. Without it, concurrent callers
	// could interleave writes to s.out and steal each other's responses from s.lines.
	reviewMu sync.Mutex
	// epoch is incremented whenever a timed gate auto-proceeds. Lines tagged with
	// an older epoch are stale (typed during a window that already timed out) and
	// must be discarded by the next gate.
	epoch     atomic.Uint64
	stop      chan struct{}
	stopOnce  sync.Once
	timerFunc func(time.Duration) *time.Timer
}

// New returns a terminal-backed Gatekeeper.
func New(st *store.FileStore) *Gatekeeper {
	return NewWithIO(st, os.Stdin, os.Stdout)
}

// NewWithIO returns a Gatekeeper with injected streams for tests and embedding.
// Optional Option values (e.g. WithTimerFunc) may be passed to override defaults.
func NewWithIO(st *store.FileStore, in io.Reader, out io.Writer, opts ...Option) *Gatekeeper {
	if st == nil {
		panic("gate: store is required")
	}
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	lines := make(chan lineResult, 1)
	g := &Gatekeeper{
		store:     st,
		lines:     lines,
		in:        in,
		out:       out,
		stop:      make(chan struct{}),
		timerFunc: time.NewTimer,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Close stops the background reader goroutine if it was started. After Close,
// no new gate calls should be made. Safe to call multiple times.
func (s *Gatekeeper) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// startReader launches the background goroutine that feeds s.lines. It is
// called lazily on the first Review call so that runtimes that never invoke a
// gate (e.g. orca cancel, orca status) do not start a goroutine that would
// race on stdin.
func (s *Gatekeeper) startReader() {
	r := bufio.NewReader(s.in)
	send := func(res lineResult) bool {
		select {
		case s.lines <- res:
			return true
		case <-s.stop:
			return false
		}
	}
	go func() {
		for {
			// Snapshot the epoch BEFORE blocking on ReadString. If the timer fires
			// while the read is in progress, the epoch will increment after this
			// snapshot, so the result is tagged with the pre-timeout epoch and the
			// next gate correctly discards it as stale.
			//
			// ReadString on a terminal blocks until the next newline; it cannot be
			// interrupted by context cancellation or Close(). The goroutine exits
			// only after the next line arrives and send() observes s.stop is closed.
			// This is an inherent limitation of blocking terminal I/O in Go.
			epoch := s.epoch.Load()
			line, err := r.ReadString('\n')
			if err == io.EOF && line == "" {
				readErr := fmt.Errorf("gate: stdin closed unexpectedly: %w", io.ErrUnexpectedEOF)
				send(lineResult{err: readErr, epoch: epoch})
				return
			}
			if err != nil && err != io.EOF {
				send(lineResult{err: err, epoch: epoch})
				return
			}
			if !send(lineResult{line: line, epoch: epoch}) {
				return
			}
			if err == io.EOF {
				return
			}
		}
	}()
}

func (s *Gatekeeper) ReviewProjection(ctx context.Context, capsuleID string, reviewWindow time.Duration) (GateDecision, error) {
	projection, err := s.store.LoadHumanSummaryProjectionForCapsule(ctx, capsuleID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load human summary for capsule %s: %w", capsuleID, err)
	}
	display := renderProjection(projection, s.out)
	approved, proceeded, notes, err := s.review(ctx, display, reviewWindow, true)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "projection_review", capsuleID, approved, proceeded, notes)
}

func (s *Gatekeeper) ReviewMerge(ctx context.Context, patchID string) (GateDecision, error) {
	result, err := s.store.LoadVerifierResultForPatch(ctx, patchID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load verifier result for patch %s: %w", patchID, err)
	}
	patch, err := s.store.LoadPatch(ctx, patchID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load patch for merge review %s: %w", patchID, err)
	}
	display := renderMerge(result, patch, s.out)
	approved, proceeded, notes, err := s.review(ctx, display, 0, false)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "merge_review", patchID, approved, proceeded, notes)
}

func (s *Gatekeeper) ReviewWaiver(ctx context.Context, obligationID string, reason string) (GateDecision, error) {
	obligation, err := s.store.LoadObligation(ctx, obligationID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load obligation %s: %w", obligationID, err)
	}
	display := renderWaiver(obligation, reason, s.out)
	approved, proceeded, notes, err := s.review(ctx, display, 0, false)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "waiver_review", obligationID, approved, proceeded, notes)
}

func (s *Gatekeeper) review(ctx context.Context, display string, reviewWindow time.Duration, allowTimeout bool) (bool, bool, string, error) {
	s.startOnce.Do(s.startReader)
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	currentEpoch := s.epoch.Load()
	if _, err := fmt.Fprint(s.out, display); err != nil {
		return false, false, "", err
	}
	if reviewWindow <= 0 {
		sep := ui.Colorize(s.out, ui.OrcaBlue, strings.Repeat("─", 52))
		approve := ui.Colorize(s.out, ui.Green+ui.Bold, "ENTER")+" to approve"
		reject := ui.Colorize(s.out, ui.Red, "reject")
		cancel := ui.Colorize(s.out, ui.Yellow, "cancel")
		prompt := fmt.Sprintf("\n%s\n  %s  ·  %s  ·  %s\n%s\n", sep, approve, reject, cancel, sep)
		if _, err := fmt.Fprint(s.out, prompt); err != nil {
			return false, false, "", err
		}
		for {
			select {
			case result := <-s.lines:
				if result.err != nil {
					return false, false, "", result.err
				}
				if result.epoch < currentEpoch {
					// Stale line from a previous auto-proceed window. Notify the user
					// so they know to re-enter their response rather than hanging silently.
					if _, werr := fmt.Fprint(s.out, "[Your input was not received — the review window had already elapsed. Please re-enter your response.]\n"); werr != nil {
						return false, false, "", werr
					}
					continue
				}
				approved, proceeded, notes := parseApproval(result.line)
				return approved, proceeded, notes, nil
			case <-ctx.Done():
				return false, false, "", ctx.Err()
			case <-s.stop:
				return false, false, "", fmt.Errorf("gate: closed")
			}
		}
	}
	if !allowTimeout {
		return false, false, "", fmt.Errorf("gate: timeout is not allowed for this gate")
	}
	autoMsg := fmt.Sprintf("\n%s  ENTER to approve · %s to reject · Auto-proceeding in %v\n",
		ui.Colorize(s.out, ui.Green+ui.Bold, "›"),
		ui.Colorize(s.out, ui.Red, "reject"),
		reviewWindow)
	if _, err := fmt.Fprint(s.out, autoMsg); err != nil {
		return false, false, "", err
	}
	timer := s.timerFunc(reviewWindow)
	defer timer.Stop()
	for {
		select {
		case result := <-s.lines:
			if result.err != nil {
				return false, false, "", result.err
			}
			if result.epoch < currentEpoch {
				// Stale line from a previous auto-proceed window. Notify the user
				// so they know to re-enter their response rather than hanging silently.
				if _, werr := fmt.Fprint(s.out, "[Your input was not received — the review window had already elapsed. Please re-enter your response.]\n"); werr != nil {
					return false, false, "", werr
				}
				continue
			}
			approved, _, notes := parseApproval(result.line)
			return approved, false, notes, nil
		case <-timer.C:
			// Increment epoch so any line the goroutine sends after this point
			// (typed during the window but not yet in the channel) is tagged stale
			// and discarded by the next gate.
			s.epoch.Add(1)
			// Also drain any line already buffered in the channel.
			select {
			case <-s.lines:
			default:
			}
			return true, true, "", nil
		case <-ctx.Done():
			return false, false, "", ctx.Err()
		case <-s.stop:
			return false, false, "", fmt.Errorf("gate: closed")
		}
	}
}

// parseApproval interprets a trimmed input line as approve or reject.
// Rejections: text starting with "reject" (handles "reject because X"),
// the token "cancel" (abort), or the tokens "no" / "n". Everything else
// approves, with the text as notes.
func parseApproval(line string) (approved bool, proceeded bool, notes string) {
	text := strings.TrimSpace(line)
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "reject") || lower == "cancel" || lower == "no" || lower == "n" {
		return false, false, text
	}
	return true, false, text
}

func (s *Gatekeeper) saveDecision(ctx context.Context, gateContext, relatedID string, approved, proceeded bool, notes string) (GateDecision, error) {
	decision := "approved"
	if proceeded {
		decision = "auto_proceeded"
	} else if !approved {
		decision = "rejected"
	}
	record := &schema.DecisionRecord{
		DecisionID: idgen.New("DEC"),
		Context:    gateContext,
		Decision:   decision,
		Rationale:  notes,
		MadeBy:     "human",
		RelatedIDs: []string{relatedID},
		CreatedAt:  time.Now().UTC(),
	}
	if proceeded {
		record.MadeBy = "system"
		record.Rationale = "review window elapsed"
	}
	if err := s.store.SaveDecision(ctx, record); err != nil {
		return GateDecision{}, fmt.Errorf("gate: save decision: %w", err)
	}
	return GateDecision{
		Approved:   approved,
		Proceeded:  proceeded,
		DecisionID: record.DecisionID,
		Notes:      notes,
	}, nil
}

func renderProjection(p *schema.HumanSummaryProjection, w io.Writer) string {
	c := func(code, s string) string { return ui.Colorize(w, code, s) }
	var b strings.Builder

	sep := c(ui.OrcaBlue, strings.Repeat("═", 52))
	fmt.Fprintf(&b, "\n%s %s\n%s\n\n", ui.IconOrca, c(ui.OrcaBlue+ui.Bold, "Projection Review"), sep)

	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Goal:    "), p.GoalPlain)
	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Topology:"), c(ui.Cyan, string(p.Topology.Selected)))
	if p.Topology.Rationale != "" {
		fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Rationale:"), c(ui.Black+ui.Bold, p.Topology.Rationale))
	}
	fmt.Fprintln(&b)

	if p.ImplementationApproach != "" {
		fmt.Fprintf(&b, "  %s\n  %s\n\n", c(ui.Bold, "Approach:"), p.ImplementationApproach)
	}

	// Why approval is required
	fmt.Fprintf(&b, "  %s\n  ", c(ui.Yellow+ui.Bold, "Why approval is required:"))
	switch p.Topology.Selected {
	case schema.TopologyHumanGated:
		fmt.Fprintf(&b, "This goal is human-gated. Explicit approval is required before any agent runs.\n\n")
	case schema.TopologyImplementerReviewer:
		fmt.Fprintf(&b, "One or more obligations carry medium or high risk.\n  Approval is required before the implementer agent runs.\n\n")
	case schema.TopologySingle:
		fmt.Fprintf(&b, "Single-agent topology with an active review window. Approve or wait for auto-proceed.\n\n")
	default:
		fmt.Fprintf(&b, "A gate condition requires your review before execution continues.\n\n")
	}

	// Obligations
	fmt.Fprintf(&b, "  %s\n", c(ui.Bold, "Obligations:"))
	for _, obl := range p.ObligationsAddressed {
		icon, riskColor := gateRiskStyle(obl.RiskLevel)
		riskLabel := c(riskColor, fmt.Sprintf("[%-6s]", obl.RiskLevel))
		oblShort := gateShortID(string(obl.ObligationID))
		fmt.Fprintf(&b, "  %s %s %s  %s\n", icon, riskLabel, c(ui.Black+ui.Bold, oblShort), obl.Description)
	}

	// Verifier gates
	fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Verifier gates:"))
	if len(p.EvidencePlan.VerifierGates) > 0 {
		for _, g := range p.EvidencePlan.VerifierGates {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Cyan, ui.IconStep), g)
		}
	} else {
		fmt.Fprintf(&b, "  %s\n", c(ui.Black+ui.Bold, "(none configured)"))
	}

	// Expected files
	hasFiles := len(p.ExpectedFileScope.ToRead)+len(p.ExpectedFileScope.ToWrite)+len(p.ExpectedFileScope.ToCreate) > 0
	if hasFiles {
		fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Expected files:"))
		writeColorList(&b, w, "read", p.ExpectedFileScope.ToRead)
		writeColorList(&b, w, "write", p.ExpectedFileScope.ToWrite)
		writeColorList(&b, w, "create", p.ExpectedFileScope.ToCreate)
	}

	// Budget
	if p.Budget.MaxTokens > 0 || p.Budget.MaxWallTimeSeconds > 0 {
		fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Budget limits:"))
		if p.Budget.MaxTokens > 0 {
			fmt.Fprintf(&b, "  Max tokens:    %s\n", c(ui.Cyan, fmt.Sprintf("%d", p.Budget.MaxTokens)))
		}
		if p.Budget.MaxWallTimeSeconds > 0 {
			fmt.Fprintf(&b, "  Max wall time: %s\n", c(ui.Cyan, fmt.Sprintf("%ds", p.Budget.MaxWallTimeSeconds)))
		}
	}

	if len(p.EvidencePlan.AdvancedChecks) > 0 {
		fmt.Fprintln(&b)
		for _, check := range p.EvidencePlan.AdvancedChecks {
			fmt.Fprintf(&b, "  %s\n", check)
		}
	}

	// Risks
	if len(p.PreExecutionRisks) > 0 {
		fmt.Fprintf(&b, "\n  %s %s\n", ui.IconWarning, c(ui.Yellow+ui.Bold, "Risks:"))
		for _, risk := range p.PreExecutionRisks {
			fmt.Fprintf(&b, "  %s %s: %s\n", c(ui.Yellow, ui.IconStep), c(ui.Bold, risk.Source), risk.Description)
		}
	}

	// Required approvals
	if len(p.RequiredApprovals) > 0 {
		fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Required approvals:"))
		for _, a := range p.RequiredApprovals {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Yellow, ui.IconStep), a)
		}
	}

	fmt.Fprintln(&b)
	return b.String()
}

func renderMerge(r *schema.VerifierResult, p *schema.PatchArtifact, w io.Writer) string {
	c := func(code, s string) string { return ui.Colorize(w, code, s) }
	var b strings.Builder

	sep := c(ui.OrcaBlue, strings.Repeat("═", 52))
	fmt.Fprintf(&b, "\n%s %s\n%s\n\n", ui.IconCheck, c(ui.OrcaBlue+ui.Bold, "Merge Review"), sep)

	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Patch:   "), c(ui.Black+ui.Bold, gateShortID(r.PatchID)))
	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Capsule: "), c(ui.Black+ui.Bold, gateShortID(p.CapsuleID)))
	if p.Summary != "" {
		fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Summary: "), p.Summary)
	}
	if len(p.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Changed files:"))
		for _, f := range p.ChangedFiles {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Cyan, ui.IconStep), f)
		}
	}
	if p.DiffPath != "" {
		fmt.Fprintf(&b, "\n  %s  %s\n", c(ui.Bold, "Diff:"), c(ui.Black+ui.Bold, p.DiffPath))
	}

	// Recommended action with color
	actionColor := ui.Green
	actionIcon := ui.IconCheck
	switch r.RecommendedAction {
	case schema.ActionReject:
		actionColor = ui.Red + ui.Bold
		actionIcon = ui.IconCross
	case schema.ActionRetry, schema.ActionSplit:
		actionColor = ui.Yellow
		actionIcon = ui.IconWarning
	}
	fmt.Fprintf(&b, "\n  %s  %s %s\n", c(ui.Bold, "Action:  "), actionIcon, c(actionColor, string(r.RecommendedAction)))
	if r.RecommendationRationale != "" {
		fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Rationale:"), r.RecommendationRationale)
	}

	if len(r.BlockingFailures) > 0 {
		fmt.Fprintf(&b, "\n  %s %s\n", ui.IconCross, c(ui.Red+ui.Bold, "Blocking failures:"))
		for _, f := range r.BlockingFailures {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Red, ui.IconStep), f)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, "\n  %s %s\n", ui.IconWarning, c(ui.Yellow+ui.Bold, "Warnings:"))
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Yellow, ui.IconStep), w)
		}
	}

	if len(p.ObligationIDsClaimed) > 0 {
		fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Claimed obligations:"))
		for _, id := range p.ObligationIDsClaimed {
			fmt.Fprintf(&b, "  %s %s\n", c(ui.Cyan, ui.IconStep), c(ui.Black+ui.Bold, gateShortID(id)))
		}
	}

	fmt.Fprintf(&b, "\n  %s\n", c(ui.Bold, "Obligation results:"))
	for _, result := range r.ObligationResults {
		verdictColor := ui.Green
		verdictIcon := ui.IconCheck
		if result.Verdict != schema.VerdictSatisfied && result.Verdict != schema.VerdictWaived {
			verdictColor = ui.Red
			verdictIcon = ui.IconCross
		}
		line := fmt.Sprintf("  %s %s  %s", verdictIcon, c(verdictColor, string(result.Verdict)), c(ui.Black+ui.Bold, gateShortID(result.ObligationID)))
		if len(result.EvidenceIDs) > 0 {
			line += c(ui.Black+ui.Bold, fmt.Sprintf(" [evidence: %s]", strings.Join(result.EvidenceIDs, ", ")))
		}
		if result.WaivedBy != "" {
			line += c(ui.Yellow, fmt.Sprintf(" [waived by: %s]", result.WaivedBy))
		}
		if result.Notes != "" {
			line += "  " + result.Notes
		}
		fmt.Fprintln(&b, line)
	}

	if len(p.RiskNotes) > 0 {
		fmt.Fprintf(&b, "\n  %s %s\n", ui.IconWarning, c(ui.Yellow, "Risk notes: "+strings.Join(p.RiskNotes, "; ")))
	}
	if len(p.ScopeViolations) > 0 {
		fmt.Fprintf(&b, "  %s %s\n", ui.IconCross, c(ui.Red, "Scope violations: "+strings.Join(p.ScopeViolations, ", ")))
	}
	if len(r.Invalidates) > 0 {
		fmt.Fprintf(&b, "\n  Invalidates: %s\n", c(ui.Black+ui.Bold, strings.Join(r.Invalidates, ", ")))
	}
	fmt.Fprintln(&b)
	return b.String()
}

func renderWaiver(o *schema.Obligation, reason string, w io.Writer) string {
	c := func(code, s string) string { return ui.Colorize(w, code, s) }
	var b strings.Builder

	sep := c(ui.OrcaBlue, strings.Repeat("═", 52))
	fmt.Fprintf(&b, "\n%s %s\n%s\n\n", ui.IconWarning, c(ui.Yellow+ui.Bold, "Waiver Review"), sep)

	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Obligation: "), c(ui.Black+ui.Bold, gateShortID(string(o.ObligationID))))
	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Description:"), o.Description)
	_, riskColor := gateRiskStyle(o.RiskLevel)
	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Risk:       "), c(riskColor, string(o.RiskLevel)))
	fmt.Fprintf(&b, "  %s  %s\n", c(ui.Bold, "Reason:     "), reason)
	fmt.Fprintln(&b)
	return b.String()
}

func writeColorList(b *strings.Builder, w io.Writer, label string, values []string) {
	c := func(code, s string) string { return ui.Colorize(w, code, s) }
	if len(values) == 0 {
		fmt.Fprintf(b, "  %s %s: none\n", c(ui.Black+ui.Bold, ui.IconStep), label)
		return
	}
	fmt.Fprintf(b, "  %s %s: %s\n", c(ui.Cyan, ui.IconStep), label, strings.Join(values, ", "))
}

func gateShortID(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:14]
}

func gateRiskStyle(risk schema.RiskLevel) (icon, colorCode string) {
	switch risk {
	case schema.RiskHigh:
		return ui.IconCross, ui.Red + ui.Bold
	case schema.RiskMedium:
		return ui.IconWarning, ui.Yellow
	default:
		return ui.IconCheck, ui.Green
	}
}
