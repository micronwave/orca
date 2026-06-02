package gate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/gate"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type gateEnv struct {
	ctx context.Context
	log *eventlog.FileLog
	st  *store.FileStore
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	dir := t.TempDir()
	log, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(dir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &gateEnv{ctx: context.Background(), log: log, st: st}
}

func (e *gateEnv) seedCore(t *testing.T) {
	t.Helper()
	if err := e.st.SaveGoal(e.ctx, &schema.GoalIR{
		GoalID:         "G-1",
		OriginalIntent: "test",
		GoalConditions: []schema.GoalCondition{{ID: "GC-1", Description: "condition", EffectiveDescription: "condition", Status: schema.GoalConditionUnmet}},
		RiskLevel:      schema.RiskLow,
		CreatedAt:      time.Now().UTC(),
		Status:         schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := e.st.SaveObligation(e.ctx, &schema.Obligation{
		ObligationID:    "OB-1",
		GoalConditionID: "GC-1",
		Description:     "prove behavior",
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-1",
		ObligationIDs: []string{"OB-1"},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
}

func (e *gateEnv) seedProjection(t *testing.T) {
	t.Helper()
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-1",
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain:              "Test goal",
		ImplementationApproach: "Change only the target code",
		ObligationsAddressed: []schema.ObligationRef{{
			ObligationID: "OB-1",
			Description:  "prove behavior",
			RiskLevel:    schema.RiskLow,
		}},
		Topology: schema.TopologyDecision{Selected: schema.TopologySingle, Rationale: "low risk"},
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}
}

func TestReviewProjectionRendersAdvancedVerification(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-ADV",
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain:              "Test goal",
		ImplementationApproach: "Change only the target code",
		ObligationsAddressed: []schema.ObligationRef{{
			ObligationID: "OB-1",
			Description:  "prove behavior",
			RiskLevel:    schema.RiskLow,
		}},
		Topology: schema.TopologyDecision{Selected: schema.TopologySingle, Rationale: "low risk"},
		EvidencePlan: schema.EvidencePlan{
			AdvancedChecks: []string{"Advanced verification: MAVEN=on Mutation=off Adversarial=on"},
		},
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, reader, &out, gate.WithTimerFunc(func(time.Duration) *time.Timer {
		return time.NewTimer(0)
	}))
	if _, err := g.ReviewProjection(e.ctx, "CAP-1", time.Millisecond); err != nil {
		t.Fatalf("ReviewProjection: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Advanced verification:") ||
		!strings.Contains(got, "MAVEN=on Mutation=off Adversarial=on") {
		t.Fatalf("projection output missing advanced verification:\n%s", got)
	}
}

func TestReviewProjection_AutoProceedsAfterWindow(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	e.seedProjection(t)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, reader, &out, gate.WithTimerFunc(func(time.Duration) *time.Timer {
		return time.NewTimer(0) // fires immediately — no real-time dependency
	}))

	decision, err := g.ReviewProjection(e.ctx, "CAP-1", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("ReviewProjection: %v", err)
	}
	if !decision.Approved || !decision.Proceeded || decision.DecisionID == "" {
		t.Fatalf("decision = %+v, want approved auto-proceed with decision ID", decision)
	}
	if !strings.Contains(out.String(), "Auto-proceeding") {
		t.Fatalf("output missing auto-proceed prompt: %s", out.String())
	}
	events, err := e.log.ReadByType(e.ctx, schema.EventDecisionRecordCreated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decision events = %d, want 1", len(events))
	}
}

func TestReviewMerge_RejectsExplicitReject(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	if err := e.st.SavePatch(e.ctx, &schema.PatchArtifact{
		PatchID:   "PATCH-1",
		CapsuleID: "CAP-1",
		Status:    schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
		VerifierResultID:  "VR-1",
		PatchID:           "PATCH-1",
		CapsuleID:         "CAP-1",
		RecommendedAction: schema.ActionHumanReview,
		ObligationResults: []schema.ObligationVerdict{{ObligationID: "OB-1", Verdict: schema.VerdictSatisfied}},
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, strings.NewReader("reject\n"), &out)

	decision, err := g.ReviewMerge(e.ctx, "PATCH-1")
	if err != nil {
		t.Fatalf("ReviewMerge: %v", err)
	}
	if decision.Approved || decision.Proceeded || decision.Notes != "reject" {
		t.Fatalf("decision = %+v, want explicit rejection", decision)
	}
}

func TestReviewProjection_BlocksUntilInput(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantApproved bool
	}{
		{"approve_enter", "\n", true},
		{"reject", "reject\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newGateEnv(t)
			e.seedCore(t)
			e.seedProjection(t)
			var out bytes.Buffer
			g := gate.NewWithIO(e.st, strings.NewReader(tt.input), &out)
			decision, err := g.ReviewProjection(e.ctx, "CAP-1", 0)
			if err != nil {
				t.Fatalf("ReviewProjection: %v", err)
			}
			if decision.Approved != tt.wantApproved {
				t.Fatalf("Approved = %v, want %v", decision.Approved, tt.wantApproved)
			}
			if decision.Proceeded {
				t.Fatal("Proceeded should be false for explicit input")
			}
			if decision.DecisionID == "" {
				t.Fatal("DecisionID should be set")
			}
		})
	}
}

func TestReviewProjectionRendersSavedContestedClaimRisk(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-contested",
			SourceArtifactIDs:   []string{"CAP-1", "CL-contested"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain:              "Test goal",
		ImplementationApproach: "Change only the target code",
		ObligationsAddressed: []schema.ObligationRef{{
			ObligationID: "OB-1",
			Description:  "prove behavior",
			RiskLevel:    schema.RiskLow,
		}},
		Topology: schema.TopologyDecision{Selected: schema.TopologySingle, Rationale: "low risk"},
		PreExecutionRisks: []schema.PreExecutionRisk{{
			Source:      "claim",
			Description: "contested claim CL-contested: API ownership is disputed",
		}},
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, strings.NewReader("\n"), &out)
	if _, err := g.ReviewProjection(e.ctx, "CAP-1", 0); err != nil {
		t.Fatalf("ReviewProjection: %v", err)
	}
	if !strings.Contains(out.String(), "contested claim CL-contested") {
		t.Fatalf("gate output did not render saved contested risk: %s", out.String())
	}
}

func TestReviewWaiver_ApprovesAndRejects(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantApproved bool
		wantNotes    string
	}{
		{"approve", "\n", true, ""},
		{"reject", "reject\n", false, "reject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newGateEnv(t)
			e.seedCore(t)
			var out bytes.Buffer
			g := gate.NewWithIO(e.st, strings.NewReader(tt.input), &out)
			decision, err := g.ReviewWaiver(e.ctx, "OB-1", "tests are flaky")
			if err != nil {
				t.Fatalf("ReviewWaiver: %v", err)
			}
			if decision.Approved != tt.wantApproved {
				t.Fatalf("Approved = %v, want %v", decision.Approved, tt.wantApproved)
			}
			if decision.Notes != tt.wantNotes {
				t.Fatalf("Notes = %q, want %q", decision.Notes, tt.wantNotes)
			}
			if decision.DecisionID == "" {
				t.Fatal("DecisionID should be set")
			}
			events, err := e.log.ReadByType(e.ctx, schema.EventDecisionRecordCreated, 0, 0)
			if err != nil {
				t.Fatalf("ReadByType: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("decision events = %d, want 1", len(events))
			}
		})
	}
}

func TestReviewProjection_ClosedStdin_Errors(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	e.seedProjection(t)
	// Empty reader simulates stdin closed immediately (EOF with no data).
	g := gate.NewWithIO(e.st, strings.NewReader(""), &bytes.Buffer{})
	_, err := g.ReviewProjection(e.ctx, "CAP-1", 0)
	if err == nil {
		t.Fatal("expected error on closed stdin, got nil — would have silently approved")
	}
}

func TestReviewProjection_RejectsAliases(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"no", "no\n"},
		{"n", "n\n"},
		{"reject_with_reason", "reject because it is unsafe\n"},
		{"REJECT_upper", "REJECT\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newGateEnv(t)
			e.seedCore(t)
			e.seedProjection(t)
			var out bytes.Buffer
			g := gate.NewWithIO(e.st, strings.NewReader(tt.input), &out)
			decision, err := g.ReviewProjection(e.ctx, "CAP-1", 0)
			if err != nil {
				t.Fatalf("ReviewProjection: %v", err)
			}
			if decision.Approved {
				t.Fatalf("input %q: Approved = true, want false (rejection)", tt.input)
			}
		})
	}
}

func TestReviewContextCancellation(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	e.seedProjection(t)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, reader, &out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.ReviewProjection(ctx, "CAP-1", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReviewMerge_RendersPatches(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	if err := e.st.SavePatch(e.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-2",
		CapsuleID:            "CAP-1",
		Status:               schema.PatchCandidate,
		Summary:              "add String() to RiskLevel",
		ChangedFiles:         []string{"internal/schema/common.go"},
		DiffPath:             ".orca/artifacts/patches/PATCH-2.diff",
		ObligationIDsClaimed: []string{"OB-1"},
		RiskNotes:            []string{"touches exported type"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
		VerifierResultID:  "VR-2",
		PatchID:           "PATCH-2",
		CapsuleID:         "CAP-1",
		RecommendedAction: schema.ActionAccept,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: "OB-1",
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{"EV-1"},
		}},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, strings.NewReader("\n"), &out)
	decision, err := g.ReviewMerge(e.ctx, "PATCH-2")
	if err != nil {
		t.Fatalf("ReviewMerge: %v", err)
	}
	if !decision.Approved {
		t.Fatalf("decision = %+v, want approved", decision)
	}
	rendered := out.String()
	for _, want := range []string{
		"add String() to RiskLevel",
		"internal/schema/common.go",
		"PATCH-2.diff",
		"OB-1",
		"EV-1",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("merge render missing %q:\n%s", want, rendered)
		}
	}
}

// TestReviewProjection_RendersHumanReviewModel verifies that ReviewProjection
// output includes all fields required by the Phase 3 human review model:
// topology rationale, planned agent approach, projected write scope,
// obligations with risks, verifier gates, budget limits, why approval is
// required, and action options.
func TestReviewProjection_RendersHumanReviewModel(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-HRM",
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain:              "Add unit tests for the auth middleware",
		ImplementationApproach: "Add table-driven tests covering the rounding edge case",
		Topology: schema.TopologyDecision{
			Selected:  schema.TopologyImplementerReviewer,
			Rationale: "medium-risk obligation requires dual-agent review",
		},
		ObligationsAddressed: []schema.ObligationRef{{
			ObligationID: "OB-1",
			Description:  "prove the middleware defect is fixed",
			RiskLevel:    schema.RiskMedium,
		}},
		ExpectedFileScope: schema.ExpectedFileScope{
			ToRead:   []string{"internal/auth/middleware.go"},
			ToWrite:  []string{"internal/auth/middleware_test.go"},
			ToCreate: []string{},
		},
		EvidencePlan: schema.EvidencePlan{
			VerifierGates: []string{"go test ./internal/auth/...", "go vet ./..."},
		},
		Budget: schema.ProjectionBudget{
			MaxTokens:          50000,
			MaxWallTimeSeconds: 600,
		},
		RequiredApprovals: []string{"projection_review: implementer capsule"},
		PreExecutionRisks: []schema.PreExecutionRisk{{
			Source:      "obligation_risk",
			Description: "medium-risk change to production auth path",
		}},
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}

	var out bytes.Buffer
	g := gate.NewWithIO(e.st, strings.NewReader("\n"), &out)
	decision, err := g.ReviewProjection(e.ctx, "CAP-1", 0)
	if err != nil {
		t.Fatalf("ReviewProjection: %v", err)
	}
	if !decision.Approved {
		t.Fatalf("decision = %+v, want approved", decision)
	}

	rendered := out.String()
	wants := []string{
		// Topology rationale.
		"medium-risk obligation requires dual-agent review",
		// Why approval is required (derived from topology).
		"Why approval is required",
		"medium or high risk",
		// Planned agent approach.
		"table-driven tests",
		// Projected write scope.
		"middleware_test.go",
		// Obligations with risk.
		"OB-1",
		"prove the middleware defect",
		// Verifier gates.
		"Verifier gates",
		"go test ./internal/auth",
		"go vet ./...",
		// Budget limits.
		"Budget limits",
		"50000",
		"600",
		// Required approvals.
		"projection_review",
		// Action options (from the gate prompt).
		"approve",
		"reject",
		"cancel",
	}
	for _, want := range wants {
		if !strings.Contains(rendered, want) {
			t.Errorf("human review model missing %q:\n%s", want, rendered)
		}
	}
}

// TestReviewProjection_Cancel_RejectsGate verifies that typing "cancel" in the
// gate prompt is treated as a rejection (same as "reject").
func TestReviewProjection_Cancel_RejectsGate(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	e.seedProjection(t)
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, strings.NewReader("cancel\n"), &out)
	decision, err := g.ReviewProjection(e.ctx, "CAP-1", 0)
	if err != nil {
		t.Fatalf("ReviewProjection: %v", err)
	}
	if decision.Approved {
		t.Fatalf("cancel input should reject the gate (Approved=true, want false)")
	}
	if decision.Notes != "cancel" {
		t.Fatalf("Notes = %q, want cancel", decision.Notes)
	}
}
