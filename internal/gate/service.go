package gate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type service struct {
	store store.ArtifactStore
	in    io.Reader
	out   io.Writer
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
	return &service{store: st, in: in, out: out}
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
	display := renderMerge(result)
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
	if _, err := fmt.Fprint(s.out, display); err != nil {
		return false, false, "", err
	}
	if reviewWindow <= 0 {
		if _, err := fmt.Fprint(s.out, "\n[Press ENTER to approve, type 'reject' + ENTER to reject.]\n"); err != nil {
			return false, false, "", err
		}
		line, err := readLine(ctx, s.in)
		if err != nil {
			return false, false, "", err
		}
		approved, proceeded, notes := parseApproval(line)
		return approved, proceeded, notes, nil
	}
	if !allowTimeout {
		return false, false, "", fmt.Errorf("gate: timeout is not allowed for this gate")
	}
	if _, err := fmt.Fprintf(s.out, "\n[Press ENTER to approve, type 'reject' + ENTER to reject. Auto-proceeding in %v...]\n", reviewWindow); err != nil {
		return false, false, "", err
	}
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(s.in).ReadString('\n')
		if err != nil && err != io.EOF {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	timer := time.NewTimer(reviewWindow)
	defer timer.Stop()
	select {
	case line := <-lineCh:
		approved, _, notes := parseApproval(line)
		return approved, false, notes, nil
	case err := <-errCh:
		return false, false, "", err
	case <-timer.C:
		return true, true, "", nil
	case <-ctx.Done():
		return false, false, "", ctx.Err()
	}
}

func readLine(ctx context.Context, in io.Reader) (string, error) {
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && err != io.EOF {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		return line, nil
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func parseApproval(line string) (approved bool, proceeded bool, notes string) {
	text := strings.TrimSpace(line)
	if strings.EqualFold(text, "reject") {
		return false, false, "reject"
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

func renderMerge(r *schema.VerifierResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Merge Review\n\n")
	fmt.Fprintf(&b, "Patch: %s\n", r.PatchID)
	fmt.Fprintf(&b, "Recommended action: %s\n", r.RecommendedAction)
	if r.RecommendationRationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n", r.RecommendationRationale)
	}
	if len(r.BlockingFailures) > 0 {
		fmt.Fprintf(&b, "\nBlocking failures:\n")
		for _, failure := range r.BlockingFailures {
			fmt.Fprintf(&b, "- %s\n", failure)
		}
	}
	fmt.Fprintf(&b, "\nObligation results:\n")
	for _, result := range r.ObligationResults {
		fmt.Fprintf(&b, "- %s: %s", result.ObligationID, result.Verdict)
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
