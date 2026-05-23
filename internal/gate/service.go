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
)

type lineResult struct {
	line  string
	err   error
	epoch uint64
}

type service struct {
	store store.ArtifactStore
	// lines is fed by a single goroutine, started lazily on the first Review call.
	// All gate calls receive from this channel, so only one goroutine ever reads
	// from the input at a time (no data races, no buffering loss across calls).
	// Channel capacity 1 lets the reader goroutine park one result while a gate
	// is busy.
	lines     chan lineResult
	in        io.Reader
	out       io.Writer
	startOnce sync.Once
	// epoch is incremented whenever a timed gate auto-proceeds. Lines tagged with
	// an older epoch are stale (typed during a window that already timed out) and
	// must be discarded by the next gate.
	epoch    atomic.Uint64
	stop     chan struct{}
	stopOnce sync.Once
}

// New returns a terminal-backed human gatekeeper.
func New(st store.ArtifactStore) HumanGatekeeper {
	return NewWithIO(st, os.Stdin, os.Stdout)
}

// NewWithIO returns a gatekeeper with injected streams for tests and embedding.
func NewWithIO(st store.ArtifactStore, in io.Reader, out io.Writer) HumanGatekeeper {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	lines := make(chan lineResult, 1)
	return &service{store: st, lines: lines, in: in, out: out, stop: make(chan struct{})}
}

// Close stops the background reader goroutine if it was started. After Close,
// no new gate calls should be made. Safe to call multiple times.
func (s *service) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// startReader launches the background goroutine that feeds s.lines. It is
// called lazily on the first Review call so that runtimes that never invoke a
// gate (e.g. orca cancel, orca status) do not start a goroutine that would
// race on stdin.
func (s *service) startReader() {
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
				send(lineResult{err: fmt.Errorf("gate: stdin closed unexpectedly: %w", io.ErrUnexpectedEOF), epoch: epoch})
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

func (s *service) ReviewProjection(ctx context.Context, capsuleID string, reviewWindow time.Duration) (GateDecision, error) {
	projection, err := s.store.LoadHumanSummaryProjectionForCapsule(ctx, capsuleID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load human summary for capsule %s: %w", capsuleID, err)
	}
	display := renderProjection(projection)
	approved, proceeded, notes, err := s.review(ctx, display, reviewWindow, true)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "projection_review", capsuleID, approved, proceeded, notes)
}

func (s *service) ReviewMerge(ctx context.Context, patchID string) (GateDecision, error) {
	result, err := s.store.LoadVerifierResultForPatch(ctx, patchID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load verifier result for patch %s: %w", patchID, err)
	}
	patch, err := s.store.LoadPatch(ctx, patchID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load patch for merge review %s: %w", patchID, err)
	}
	display := renderMerge(result, patch)
	approved, proceeded, notes, err := s.review(ctx, display, 0, false)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "merge_review", patchID, approved, proceeded, notes)
}

func (s *service) ReviewWaiver(ctx context.Context, obligationID string, reason string) (GateDecision, error) {
	obligation, err := s.store.LoadObligation(ctx, obligationID)
	if err != nil {
		return GateDecision{}, fmt.Errorf("gate: load obligation %s: %w", obligationID, err)
	}
	display := renderWaiver(obligation, reason)
	approved, proceeded, notes, err := s.review(ctx, display, 0, false)
	if err != nil {
		return GateDecision{Approved: false}, err
	}
	return s.saveDecision(ctx, "waiver_review", obligationID, approved, proceeded, notes)
}

func (s *service) review(ctx context.Context, display string, reviewWindow time.Duration, allowTimeout bool) (bool, bool, string, error) {
	s.startOnce.Do(s.startReader)
	currentEpoch := s.epoch.Load()
	if _, err := fmt.Fprint(s.out, display); err != nil {
		return false, false, "", err
	}
	if reviewWindow <= 0 {
		if _, err := fmt.Fprint(s.out, "\n[Press ENTER to approve, type 'reject' + ENTER to reject.]\n"); err != nil {
			return false, false, "", err
		}
		for {
			select {
			case result := <-s.lines:
				if result.err != nil {
					return false, false, "", result.err
				}
				if result.epoch < currentEpoch {
					// Stale line tagged before a previous auto-proceed; discard.
					continue
				}
				approved, proceeded, notes := parseApproval(result.line)
				return approved, proceeded, notes, nil
			case <-ctx.Done():
				return false, false, "", ctx.Err()
			}
		}
	}
	if !allowTimeout {
		return false, false, "", fmt.Errorf("gate: timeout is not allowed for this gate")
	}
	if _, err := fmt.Fprintf(s.out, "\n[Press ENTER to approve, type 'reject' + ENTER to reject. Auto-proceeding in %v...]\n", reviewWindow); err != nil {
		return false, false, "", err
	}
	timer := time.NewTimer(reviewWindow)
	defer timer.Stop()
	for {
		select {
		case result := <-s.lines:
			if result.err != nil {
				return false, false, "", result.err
			}
			if result.epoch < currentEpoch {
				// Stale line tagged before a previous auto-proceed; discard.
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
		}
	}
}

// parseApproval interprets a trimmed input line as approve or reject.
// Rejections: text starting with "reject" (handles "reject because X"),
// or the tokens "no" / "n". Everything else approves, with the text as notes.
func parseApproval(line string) (approved bool, proceeded bool, notes string) {
	text := strings.TrimSpace(line)
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "reject") || lower == "no" || lower == "n" {
		return false, false, text
	}
	return true, false, text
}

func (s *service) saveDecision(ctx context.Context, gateContext, relatedID string, approved, proceeded bool, notes string) (GateDecision, error) {
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

func renderProjection(p *schema.HumanSummaryProjection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Human Summary Projection\n\n")
	fmt.Fprintf(&b, "Goal: %s\n\n", p.GoalPlain)
	fmt.Fprintf(&b, "Approach: %s\n\n", p.ImplementationApproach)
	fmt.Fprintf(&b, "Topology: %s\n", p.Topology.Selected)
	if p.Topology.Rationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n", p.Topology.Rationale)
	}
	fmt.Fprintf(&b, "\nObligations:\n")
	for _, obligation := range p.ObligationsAddressed {
		fmt.Fprintf(&b, "- %s (%s): %s\n", obligation.ObligationID, obligation.RiskLevel, obligation.Description)
	}
	fmt.Fprintf(&b, "\nExpected files:\n")
	writeList(&b, "read", p.ExpectedFileScope.ToRead)
	writeList(&b, "write", p.ExpectedFileScope.ToWrite)
	writeList(&b, "create", p.ExpectedFileScope.ToCreate)
	if len(p.PreExecutionRisks) > 0 {
		fmt.Fprintf(&b, "\nRisks:\n")
		for _, risk := range p.PreExecutionRisks {
			fmt.Fprintf(&b, "- %s: %s\n", risk.Source, risk.Description)
		}
	}
	return b.String()
}

func renderMerge(r *schema.VerifierResult, p *schema.PatchArtifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Merge Review\n\n")
	fmt.Fprintf(&b, "Patch: %s (capsule %s)\n", r.PatchID, p.CapsuleID)
	if p.Summary != "" {
		fmt.Fprintf(&b, "Summary: %s\n", p.Summary)
	}
	if len(p.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "Changed files: %s\n", strings.Join(p.ChangedFiles, ", "))
	}
	if p.DiffPath != "" {
		fmt.Fprintf(&b, "Diff: %s\n", p.DiffPath)
	}
	if len(p.ObligationIDsClaimed) > 0 {
		fmt.Fprintf(&b, "Claimed obligations: %s\n", strings.Join(p.ObligationIDsClaimed, ", "))
	}
	if len(p.RiskNotes) > 0 {
		fmt.Fprintf(&b, "Risk notes: %s\n", strings.Join(p.RiskNotes, "; "))
	}
	if len(p.ScopeViolations) > 0 {
		fmt.Fprintf(&b, "Scope violations: %s\n", strings.Join(p.ScopeViolations, ", "))
	}
	fmt.Fprintf(&b, "Recommended action: %s\n", r.RecommendedAction)
	if r.RecommendationRationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n", r.RecommendationRationale)
	}
	if len(r.BlockingFailures) > 0 {
		fmt.Fprintf(&b, "\nBlocking failures:\n")
		for _, f := range r.BlockingFailures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, "\nWarnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if len(r.Invalidates) > 0 {
		fmt.Fprintf(&b, "\nInvalidates: %s\n", strings.Join(r.Invalidates, ", "))
	}
	if len(r.FailureIDs) > 0 {
		fmt.Fprintf(&b, "Failure records: %s\n", strings.Join(r.FailureIDs, ", "))
	}
	fmt.Fprintf(&b, "\nObligation results:\n")
	for _, result := range r.ObligationResults {
		fmt.Fprintf(&b, "- %s: %s", result.ObligationID, result.Verdict)
		if len(result.EvidenceIDs) > 0 {
			fmt.Fprintf(&b, " [evidence: %s]", strings.Join(result.EvidenceIDs, ", "))
		}
		if result.WaivedBy != "" {
			fmt.Fprintf(&b, " [waived by: %s]", result.WaivedBy)
		}
		if result.Notes != "" {
			fmt.Fprintf(&b, " - %s", result.Notes)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func renderWaiver(o *schema.Obligation, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Waiver Review\n\n")
	fmt.Fprintf(&b, "Obligation: %s\n", o.ObligationID)
	fmt.Fprintf(&b, "Description: %s\n", o.Description)
	fmt.Fprintf(&b, "Risk: %s\n", o.RiskLevel)
	fmt.Fprintf(&b, "Waiver reason: %s\n", reason)
	return b.String()
}

func writeList(b *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		fmt.Fprintf(b, "- %s: none\n", label)
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", label, strings.Join(values, ", "))
}
