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

func TestReviewProjection_AutoProceedsAfterWindow(t *testing.T) {
	e := newGateEnv(t)
	e.seedCore(t)
	e.seedProjection(t)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	var out bytes.Buffer
	g := gate.NewWithIO(e.st, reader, &out)

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

func TestReviewProjection_ContextCancellation(t *testing.T) {
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
