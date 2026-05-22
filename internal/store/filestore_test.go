package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── test helpers ─────────────────────────────────────────────────────────────

type testEnv struct {
	log  *eventlog.FileLog
	st   *store.FileStore
	root string
	ctx  context.Context
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	l, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("Open eventlog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	st, err := store.New(dir, l)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &testEnv{log: l, st: st, root: dir, ctx: context.Background()}
}

func (e *testEnv) seedGoal(t *testing.T, goalID string, conditionIDs ...string) *schema.GoalIR {
	t.Helper()
	var conditions []schema.GoalCondition
	for _, cid := range conditionIDs {
		conditions = append(conditions, schema.GoalCondition{
			ID:                   cid,
			Description:          "condition " + cid,
			EffectiveDescription: "condition " + cid,
			Status:               schema.GoalConditionUnmet,
		})
	}
	g := &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test goal",
		GoalConditions: conditions,
		RiskLevel:      schema.RiskLow,
		CreatedAt:      time.Now().UTC(),
		Status:         schema.GoalStatusActive,
	}
	if err := e.st.SaveGoal(e.ctx, g); err != nil {
		t.Fatalf("seedGoal %s: %v", goalID, err)
	}
	return g
}

func (e *testEnv) seedObligation(t *testing.T, oblID, conditionID string, status schema.ObligationStatus) *schema.Obligation {
	t.Helper()
	o := &schema.Obligation{
		ObligationID:    oblID,
		GoalConditionID: conditionID,
		Description:     "obligation " + oblID,
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          status,
	}
	if err := e.st.SaveObligation(e.ctx, o); err != nil {
		t.Fatalf("seedObligation %s: %v", oblID, err)
	}
	return o
}

func (e *testEnv) seedCapsule(t *testing.T, capsuleID string, oblIDs ...string) *schema.ExecutionCapsule {
	t.Helper()
	c := &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: oblIDs,
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
	}
	if err := e.st.SaveCapsule(e.ctx, c); err != nil {
		t.Fatalf("seedCapsule %s: %v", capsuleID, err)
	}
	return c
}

func (e *testEnv) seedPatch(t *testing.T, patchID, capsuleID string) *schema.PatchArtifact {
	t.Helper()
	p := &schema.PatchArtifact{
		PatchID:   patchID,
		CapsuleID: capsuleID,
		Status:    schema.PatchCandidate,
	}
	if err := e.st.SavePatch(e.ctx, p); err != nil {
		t.Fatalf("seedPatch %s: %v", patchID, err)
	}
	return p
}

func (e *testEnv) countEvents(t *testing.T, et schema.EventType) int {
	t.Helper()
	events, err := e.log.ReadByType(e.ctx, et, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType(%s): %v", et, err)
	}
	return len(events)
}

func marshalJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ── New ───────────────────────────────────────────────────────────────────────

func TestNew_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	l, _ := eventlog.Open(filepath.Join(dir, "events.log"))
	defer l.Close()

	if _, err := store.New(dir, l); err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, sub := range []string{
		"state/goals", "state/obligations", "artifacts/patches",
		"artifacts/projections/executor", "artifacts/projections/human_summary",
		"artifacts/failures", "artifacts/topology_outcomes",
	} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("expected dir %s: %v", sub, err)
		}
	}
}

func TestNew_Idempotent(t *testing.T) {
	dir := t.TempDir()
	l, _ := eventlog.Open(filepath.Join(dir, "events.log"))
	defer l.Close()
	for i := 0; i < 3; i++ {
		if _, err := store.New(dir, l); err != nil {
			t.Fatalf("New call %d: %v", i+1, err)
		}
	}
}

// ── Goal IR ───────────────────────────────────────────────────────────────────

func TestGoal_SaveLoad(t *testing.T) {
	e := newEnv(t)
	g := e.seedGoal(t, "G-1", "GC-1", "GC-2")
	got, err := e.st.LoadGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadGoal: %v", err)
	}
	if got.GoalID != g.GoalID || len(got.GoalConditions) != 2 {
		t.Errorf("goal mismatch: %+v", got)
	}
}

func TestGoal_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if n := e.countEvents(t, schema.EventGoalCreated); n != 1 {
		t.Errorf("expected 1 goal_created event, got %d", n)
	}
}

func TestGoal_EventCarriesGoalID(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	events, _ := e.log.ReadForGoal(e.ctx, "G-1", 0, 0)
	if len(events) != 1 {
		t.Errorf("expected 1 event for G-1, got %d", len(events))
	}
}

func TestGoal_LoadNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.st.LoadGoal(e.ctx, "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGoal_UpdateStatus(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if err := e.st.UpdateGoalStatus(e.ctx, "G-1", schema.GoalStatusComplete); err != nil {
		t.Fatalf("UpdateGoalStatus: %v", err)
	}
	got, _ := e.st.LoadGoal(e.ctx, "G-1")
	if got.Status != schema.GoalStatusComplete {
		t.Errorf("status = %s, want complete", got.Status)
	}
}

func TestGoal_SaveMaterializationFailureReturnsCommittedEvent(t *testing.T) {
	e := newEnv(t)
	blockedPath := filepath.Join(e.root, "state", "goals", "G-FAIL.json.tmp")
	if err := os.Mkdir(blockedPath, 0o755); err != nil {
		t.Fatalf("create directory at temp artifact path: %v", err)
	}

	err := e.st.SaveGoal(e.ctx, &schema.GoalIR{
		GoalID:         "G-FAIL",
		OriginalIntent: "test goal",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-FAIL",
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	})
	var materialized *store.MaterializationError
	if !errors.As(err, &materialized) {
		t.Fatalf("SaveGoal error = %v, want MaterializationError", err)
	}
	if materialized.Event.SequenceNum == 0 || materialized.Event.EventID == "" {
		t.Fatalf("committed event missing assigned fields: %+v", materialized.Event)
	}
	events, readErr := e.log.ReadAfter(e.ctx, 0, 0)
	if readErr != nil {
		t.Fatalf("ReadAfter: %v", readErr)
	}
	if len(events) != 1 || events[0].ArtifactID != "G-FAIL" {
		t.Fatalf("events = %+v, want committed G-FAIL event", events)
	}
}

func TestGoal_LoadGoalCondition(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1", "GC-2")
	cond, err := e.st.LoadGoalCondition(e.ctx, "GC-2")
	if err != nil {
		t.Fatalf("LoadGoalCondition: %v", err)
	}
	if cond.ID != "GC-2" {
		t.Errorf("condition ID = %s, want GC-2", cond.ID)
	}
}

func TestGoal_LoadGoalCondition_NotFound(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	_, err := e.st.LoadGoalCondition(e.ctx, "GC-999")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Obligations ───────────────────────────────────────────────────────────────

func TestObligation_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	o := e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	got, err := e.st.LoadObligation(e.ctx, "OB-1")
	if err != nil {
		t.Fatalf("LoadObligation: %v", err)
	}
	if got.ObligationID != o.ObligationID || got.GoalConditionID != o.GoalConditionID {
		t.Errorf("obligation mismatch: %+v", got)
	}
}

func TestObligation_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	if n := e.countEvents(t, schema.EventObligationCreated); n != 1 {
		t.Errorf("expected 1 obligation_created event, got %d", n)
	}
}

func TestObligation_EventCarriesGoalID(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	events, _ := e.log.ReadByType(e.ctx, schema.EventObligationCreated, 0, 0)
	if len(events) == 0 || events[0].GoalID != "G-1" {
		t.Errorf("obligation_created event missing GoalID=G-1")
	}
}

func TestObligation_LoadNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.st.LoadObligation(e.ctx, "OB-999")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestObligation_UpdateStatus(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	if err := e.st.UpdateObligationStatus(e.ctx, "OB-1", schema.ObligationSatisfied, []string{"EV-1"}); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}
	got, _ := e.st.LoadObligation(e.ctx, "OB-1")
	if got.Status != schema.ObligationSatisfied {
		t.Errorf("status = %s, want satisfied", got.Status)
	}
	if len(got.SatisfiedBy) != 1 || got.SatisfiedBy[0] != "EV-1" {
		t.Errorf("SatisfiedBy = %v, want [EV-1]", got.SatisfiedBy)
	}
}

func TestObligation_LoadOpenObligations(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-3", "GC-1", schema.ObligationSatisfied)
	e.seedObligation(t, "OB-4", "GC-2", schema.ObligationOpen) // different goal

	open, err := e.st.LoadOpenObligations(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("expected 2 open obligations for G-1, got %d", len(open))
	}
	for _, o := range open {
		if o.Status != schema.ObligationOpen {
			t.Errorf("non-open obligation returned: %s", o.ObligationID)
		}
	}
}

func TestObligation_LoadObligationsForCondition(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-1", schema.ObligationSatisfied)
	e.seedObligation(t, "OB-3", "GC-2", schema.ObligationOpen)

	obls, err := e.st.LoadObligationsForCondition(e.ctx, "GC-1")
	if err != nil {
		t.Fatalf("LoadObligationsForCondition: %v", err)
	}
	if len(obls) != 2 {
		t.Errorf("expected 2 obligations for GC-1, got %d", len(obls))
	}
}

// ── Execution Capsules ────────────────────────────────────────────────────────

func TestCapsule_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	c := e.seedCapsule(t, "CAP-1", "OB-1")

	got, err := e.st.LoadCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if got.CapsuleID != c.CapsuleID || got.State != schema.CapsuleStatePending {
		t.Errorf("capsule mismatch: %+v", got)
	}
}

func TestCapsule_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if n := e.countEvents(t, schema.EventCapsuleCreated); n != 1 {
		t.Errorf("expected 1 capsule_created event, got %d", n)
	}
}

func TestCapsule_UpdateState(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.UpdateCapsuleState(e.ctx, "CAP-1", schema.CapsuleStateAgentRunning); err != nil {
		t.Fatalf("UpdateCapsuleState: %v", err)
	}
	got, _ := e.st.LoadCapsule(e.ctx, "CAP-1")
	if got.State != schema.CapsuleStateAgentRunning {
		t.Errorf("state = %s, want agent_running", got.State)
	}
}

// ── Context Projections ───────────────────────────────────────────────────────

func TestProjection_SaveLoadExecutor(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	p := &schema.ContextProjection{
		ContextProjectionID: "CTX-1",
		Role:                schema.ProjectionRoleExecutor,
		SourceArtifactIDs:   []string{"CAP-1"},
		TokenBudget:         4096,
		CreatedAt:           time.Now().UTC(),
	}
	if err := e.st.SaveProjection(e.ctx, p); err != nil {
		t.Fatalf("SaveProjection: %v", err)
	}
	got, err := e.st.LoadProjection(e.ctx, "CTX-1")
	if err != nil {
		t.Fatalf("LoadProjection: %v", err)
	}
	if got.TokenBudget != 4096 || got.Role != schema.ProjectionRoleExecutor {
		t.Errorf("projection mismatch: %+v", got)
	}
}

func TestProjection_SaveLoadReviewerAndTester(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")

	for _, tt := range []struct {
		id   string
		role schema.ProjectionRole
	}{
		{id: "CTX-reviewer", role: schema.ProjectionRoleReviewer},
		{id: "CTX-tester", role: schema.ProjectionRoleTester},
	} {
		t.Run(string(tt.role), func(t *testing.T) {
			p := &schema.ContextProjection{
				ContextProjectionID: tt.id,
				Role:                tt.role,
				SourceArtifactIDs:   []string{"CAP-1"},
				TokenBudget:         1024,
				CreatedAt:           time.Now().UTC(),
			}
			if err := e.st.SaveProjection(e.ctx, p); err != nil {
				t.Fatalf("SaveProjection: %v", err)
			}
			got, err := e.st.LoadProjection(e.ctx, tt.id)
			if err != nil {
				t.Fatalf("LoadProjection: %v", err)
			}
			if got.Role != tt.role {
				t.Fatalf("Role = %s, want %s", got.Role, tt.role)
			}
		})
	}
}

func TestProjection_SaveLoadHumanSummary(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	p := &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-2",
			Role:                schema.ProjectionRoleHumanSummary,
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain:              "Fix the auth bug",
		ImplementationApproach: "Patch token validator",
	}
	if err := e.st.SaveHumanSummaryProjection(e.ctx, p); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}
	got, err := e.st.LoadHumanSummaryProjection(e.ctx, "CTX-2")
	if err != nil {
		t.Fatalf("LoadHumanSummaryProjection: %v", err)
	}
	if got.GoalPlain != "Fix the auth bug" {
		t.Errorf("GoalPlain = %q", got.GoalPlain)
	}
}

func TestProjection_LoadHumanSummaryForCapsule(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-1")
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-1",
			SourceArtifactIDs:   []string{"CAP-2"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain: "other capsule",
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection CTX-1: %v", err)
	}
	if err := e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-2",
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain: "target capsule",
	}); err != nil {
		t.Fatalf("SaveHumanSummaryProjection CTX-2: %v", err)
	}

	got, err := e.st.LoadHumanSummaryProjectionForCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadHumanSummaryProjectionForCapsule: %v", err)
	}
	if got.ContextProjectionID != "CTX-2" || got.GoalPlain != "target capsule" {
		t.Fatalf("projection = %+v, want CTX-2", got)
	}
}

func TestProjection_BothEmitEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	_ = e.st.SaveProjection(e.ctx, &schema.ContextProjection{
		ContextProjectionID: "CTX-1", Role: schema.ProjectionRoleExecutor, SourceArtifactIDs: []string{"CAP-1"}, CreatedAt: time.Now().UTC(),
	})
	_ = e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-2", Role: schema.ProjectionRoleHumanSummary, SourceArtifactIDs: []string{"CAP-1"}, CreatedAt: time.Now().UTC(),
		},
	})
	if n := e.countEvents(t, schema.EventContextProjectionCreated); n != 2 {
		t.Errorf("expected 2 context_projection_created events, got %d", n)
	}
}

func TestProjection_SaveRejectsHumanRoleAndReplaysAgentRoles(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	invalidAgentProjection := &schema.ContextProjection{
		ContextProjectionID: "CTX-invalid",
		Role:                schema.ProjectionRoleHumanSummary,
		SourceArtifactIDs:   []string{"CAP-1"},
		CreatedAt:           time.Now().UTC(),
	}
	reviewer := &schema.ContextProjection{
		ContextProjectionID: "CTX-reviewer",
		Role:                schema.ProjectionRoleReviewer,
		SourceArtifactIDs:   []string{"CAP-1"},
		CreatedAt:           time.Now().UTC(),
	}
	human := &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-human",
			Role:                schema.ProjectionRoleExecutor,
			SourceArtifactIDs:   []string{"CAP-1"},
			CreatedAt:           time.Now().UTC(),
		},
		GoalPlain: "review this",
	}
	if err := e.st.SaveProjection(e.ctx, invalidAgentProjection); err == nil {
		t.Fatal("SaveProjection accepted human_summary role")
	}
	if err := e.st.SaveProjection(e.ctx, reviewer); err != nil {
		t.Fatalf("SaveProjection reviewer: %v", err)
	}
	if err := e.st.SaveHumanSummaryProjection(e.ctx, human); err != nil {
		t.Fatalf("SaveHumanSummaryProjection: %v", err)
	}

	if human.Role != schema.ProjectionRoleExecutor {
		t.Fatalf("SaveHumanSummaryProjection must not mutate caller's struct: role changed to %s", human.Role)
	}
	loadedReviewer, err := e.st.LoadProjection(e.ctx, "CTX-reviewer")
	if err != nil {
		t.Fatalf("LoadProjection after save: %v", err)
	}
	if loadedReviewer.Role != schema.ProjectionRoleReviewer {
		t.Fatalf("stored reviewer role = %s, want reviewer", loadedReviewer.Role)
	}
	loadedHuman, err := e.st.LoadHumanSummaryProjection(e.ctx, "CTX-human")
	if err != nil {
		t.Fatalf("LoadHumanSummaryProjection after save: %v", err)
	}
	if loadedHuman.Role != schema.ProjectionRoleHumanSummary {
		t.Fatalf("SaveHumanSummaryProjection did not normalize role in stored artifact: got %s", loadedHuman.Role)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, err := e.st.LoadProjection(e.ctx, "CTX-reviewer"); err != nil {
		t.Fatalf("reviewer projection replayed to wrong directory: %v", err)
	} else if got.Role != schema.ProjectionRoleReviewer {
		t.Fatalf("replayed reviewer role = %s, want reviewer", got.Role)
	}
	if _, err := e.st.LoadHumanSummaryProjection(e.ctx, "CTX-human"); err != nil {
		t.Fatalf("human projection replayed to wrong directory: %v", err)
	}
}

// ── Patch Artifacts ───────────────────────────────────────────────────────────

func TestPatch_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	p := e.seedPatch(t, "PATCH-1", "CAP-1")

	got, err := e.st.LoadPatch(e.ctx, "PATCH-1")
	if err != nil {
		t.Fatalf("LoadPatch: %v", err)
	}
	if got.PatchID != p.PatchID || got.Status != schema.PatchCandidate {
		t.Errorf("patch mismatch: %+v", got)
	}
}

func TestPatch_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")
	if n := e.countEvents(t, schema.EventPatchArtifactCreated); n != 1 {
		t.Errorf("expected 1 patch_artifact_created event, got %d", n)
	}
}

func TestPatch_LoadPatchesForObligation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SavePatch(e.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-1",
		CapsuleID:            "CAP-1",
		ObligationIDsClaimed: []string{"OB-1"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch PATCH-1: %v", err)
	}
	if err := e.st.SavePatch(e.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-2",
		CapsuleID:            "CAP-1",
		ObligationIDsClaimed: []string{"OB-2"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch PATCH-2: %v", err)
	}

	patches, err := e.st.LoadPatchesForObligation(e.ctx, "OB-1")
	if err != nil {
		t.Fatalf("LoadPatchesForObligation: %v", err)
	}
	if len(patches) != 1 || patches[0].PatchID != "PATCH-1" {
		t.Fatalf("patches = %+v, want PATCH-1 only", patches)
	}
}

func TestPatch_UpdateStatus(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")
	if err := e.st.UpdatePatchStatus(e.ctx, "PATCH-1", schema.PatchAccepted); err != nil {
		t.Fatalf("UpdatePatchStatus: %v", err)
	}
	got, _ := e.st.LoadPatch(e.ctx, "PATCH-1")
	if got.Status != schema.PatchAccepted {
		t.Errorf("status = %s, want accepted", got.Status)
	}
}

func TestPatch_LoadForCapsule(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")
	e.seedPatch(t, "PATCH-2", "CAP-1")
	e.seedPatch(t, "PATCH-3", "CAP-2")

	patches, err := e.st.LoadPatchesForCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadPatchesForCapsule: %v", err)
	}
	if len(patches) != 2 {
		t.Errorf("expected 2 patches for CAP-1, got %d", len(patches))
	}
}

// ── Evidence Artifacts ────────────────────────────────────────────────────────

func TestEvidence_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	ev := &schema.EvidenceArtifact{
		EvidenceID: "EV-1", Type: schema.EvidenceTestResult,
		Command: "go test ./...", ExitCode: 0,
		Supports: []string{"OB-1"}, CreatedAt: time.Now().UTC(),
	}
	if err := e.st.SaveEvidence(e.ctx, ev); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	got, err := e.st.LoadEvidence(e.ctx, "EV-1")
	if err != nil {
		t.Fatalf("LoadEvidence: %v", err)
	}
	if got.ExitCode != 0 || got.EvidenceID != "EV-1" {
		t.Errorf("evidence mismatch: %+v", got)
	}
}

func TestEvidence_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	_ = e.st.SaveEvidence(e.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-1", Type: schema.EvidenceTestResult, Supports: []string{"OB-1"}, CreatedAt: time.Now().UTC(),
	})
	if n := e.countEvents(t, schema.EventEvidenceArtifactCreated); n != 1 {
		t.Errorf("expected 1 evidence_artifact_created event, got %d", n)
	}
}

func TestEvidence_LoadForObligation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	for _, ev := range []*schema.EvidenceArtifact{
		{EvidenceID: "EV-1", Type: schema.EvidenceTestResult, Supports: []string{"OB-1"}, CreatedAt: time.Now().UTC()},
		{EvidenceID: "EV-2", Type: schema.EvidenceLintResult, Weakens: []string{"OB-1"}, CreatedAt: time.Now().UTC()},
		{EvidenceID: "EV-3", Type: schema.EvidenceTestResult, Supports: []string{"OB-2"}, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveEvidence(e.ctx, ev); err != nil {
			t.Fatalf("SaveEvidence %s: %v", ev.EvidenceID, err)
		}
	}
	out, err := e.st.LoadEvidenceForObligation(e.ctx, "OB-1")
	if err != nil {
		t.Fatalf("LoadEvidenceForObligation: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 evidence items for OB-1, got %d", len(out))
	}
}

func TestEvidence_LoadReusableForObligation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	now := time.Now().UTC()
	for _, ev := range []*schema.EvidenceArtifact{
		{EvidenceID: "EV-failing", Type: schema.EvidenceTestResult, ExitCode: 1, Supports: []string{"OB-1"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-1", CreatedAt: now},
		{EvidenceID: "EV-wrong-obligation", Type: schema.EvidenceTestResult, ExitCode: 0, Supports: []string{"OB-2"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-1", CreatedAt: now},
		{EvidenceID: "EV-wrong-type", Type: schema.EvidenceLintResult, ExitCode: 0, Supports: []string{"OB-1"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-1", CreatedAt: now},
		{EvidenceID: "EV-wrong-key", Type: schema.EvidenceTestResult, ExitCode: 0, Supports: []string{"OB-1"}, ReuseKey: "other", ValidatedAgainst: "SNAP-1", CreatedAt: now},
		{EvidenceID: "EV-wrong-snapshot", Type: schema.EvidenceTestResult, ExitCode: 0, Supports: []string{"OB-1"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-2", CreatedAt: now},
		{EvidenceID: "EV-reusable-old", Type: schema.EvidenceTestResult, ExitCode: 0, Supports: []string{"OB-1"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-1", CreatedAt: now.Add(-time.Minute)},
		{EvidenceID: "EV-reusable-new", Type: schema.EvidenceTestResult, ExitCode: 0, Supports: []string{"OB-1"}, ReuseKey: "go-test", ValidatedAgainst: "SNAP-1", CreatedAt: now, ContentHash: "hash"},
	} {
		if err := e.st.SaveEvidence(e.ctx, ev); err != nil {
			t.Fatalf("SaveEvidence %s: %v", ev.EvidenceID, err)
		}
	}
	got, err := e.st.LoadReusableEvidenceForObligation(e.ctx, "OB-1", schema.EvidenceTestResult, "go-test", "SNAP-1")
	if err != nil {
		t.Fatalf("LoadReusableEvidenceForObligation: %v", err)
	}
	if got.EvidenceID != "EV-reusable-new" {
		t.Fatalf("reusable evidence = %s, want EV-reusable-new", got.EvidenceID)
	}
	if _, err := e.st.LoadReusableEvidenceForObligation(e.ctx, "OB-1", schema.EvidenceTestResult, "missing", "SNAP-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing reusable evidence error = %v, want ErrNotFound", err)
	}
}

// ── Claim Artifacts ───────────────────────────────────────────────────────────

func TestClaim_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")

	cl := &schema.ClaimArtifact{
		ClaimID: "CL-1", Text: "The handler is idempotent",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: "CAP-1",
		Status: schema.ClaimProposed,
	}
	if err := e.st.SaveClaim(e.ctx, cl); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	got, err := e.st.LoadClaim(e.ctx, "CL-1")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if got.Status != schema.ClaimProposed || got.ClaimType != schema.ClaimInvariant {
		t.Errorf("claim mismatch: %+v", got)
	}
}

func TestClaim_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	_ = e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
		ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimProposed,
	})
	if n := e.countEvents(t, schema.EventClaimCreated); n != 1 {
		t.Errorf("expected 1 claim_created event, got %d", n)
	}
}

func TestClaim_UpdateStatus(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	_ = e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
		ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimProposed,
	})
	if err := e.st.UpdateClaimStatus(e.ctx, "CL-1", schema.ClaimVerified); err != nil {
		t.Fatalf("UpdateClaimStatus: %v", err)
	}
	got, _ := e.st.LoadClaim(e.ctx, "CL-1")
	if got.Status != schema.ClaimVerified {
		t.Errorf("status = %s, want verified", got.Status)
	}
}

func TestClaim_LoadVerifiedForFiles(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	for _, cl := range []*schema.ClaimArtifact{
		{ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimVerified,
			AffectedFiles: []string{"internal/foo/foo.go"}},
		{ClaimID: "CL-2", SourceCapsuleID: "CAP-1", Status: schema.ClaimVerified,
			AffectedFiles: []string{"internal/bar/bar.go"}},
		{ClaimID: "CL-3", SourceCapsuleID: "CAP-1", Status: schema.ClaimProposed, // excluded
			AffectedFiles: []string{"internal/foo/foo.go"}},
		{ClaimID: "CL-4", SourceCapsuleID: "CAP-1", Status: schema.ClaimStale, // excluded
			AffectedFiles: []string{"internal/foo/foo.go"}},
	} {
		if err := e.st.SaveClaim(e.ctx, cl); err != nil {
			t.Fatalf("SaveClaim %s: %v", cl.ClaimID, err)
		}
	}
	out, err := e.st.LoadVerifiedClaimsForFiles(e.ctx, []string{"internal/foo/foo.go"})
	if err != nil {
		t.Fatalf("LoadVerifiedClaimsForFiles: %v", err)
	}
	if len(out) != 1 || out[0].ClaimID != "CL-1" {
		t.Errorf("expected [CL-1], got %v", claimIDs(out))
	}
}

func TestClaim_LoadForCapsule(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-1")
	for _, cl := range []*schema.ClaimArtifact{
		{ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimProposed},
		{ClaimID: "CL-2", SourceCapsuleID: "CAP-1", Status: schema.ClaimVerified},
		{ClaimID: "CL-3", SourceCapsuleID: "CAP-2", Status: schema.ClaimProposed},
	} {
		if err := e.st.SaveClaim(e.ctx, cl); err != nil {
			t.Fatalf("SaveClaim %s: %v", cl.ClaimID, err)
		}
	}
	out, err := e.st.LoadClaimsForCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadClaimsForCapsule: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 claims for CAP-1, got %d", len(out))
	}
	for _, c := range out {
		if c.SourceCapsuleID != "CAP-1" {
			t.Errorf("unexpected SourceCapsuleID %s in result", c.SourceCapsuleID)
		}
	}
}

func TestClaim_LoadForCapsule_Empty(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	out, err := e.st.LoadClaimsForCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadClaimsForCapsule empty: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected 0 claims, got %d", len(out))
	}
}

func TestClaim_LoadVerifiedForFiles_NormalizesWindowsPaths(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-1",
		SourceCapsuleID: "CAP-1",
		Status:          schema.ClaimVerified,
		AffectedFiles:   []string{`.\internal\foo\service.go`},
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	out, err := e.st.LoadVerifiedClaimsForFiles(e.ctx, []string{"internal/foo/service.go"})
	if err != nil {
		t.Fatalf("LoadVerifiedClaimsForFiles: %v", err)
	}
	if len(out) != 1 || out[0].ClaimID != "CL-1" {
		t.Fatalf("LoadVerifiedClaimsForFiles normalized = %v, want [CL-1]", claimIDs(out))
	}
}

func TestClaim_LoadClaimsForGoal(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-2")
	for _, claim := range []*schema.ClaimArtifact{
		{ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimVerified},
		{ClaimID: "CL-2", SourceCapsuleID: "CAP-2", Status: schema.ClaimVerified},
	} {
		if err := e.st.SaveClaim(e.ctx, claim); err != nil {
			t.Fatalf("SaveClaim %s: %v", claim.ClaimID, err)
		}
	}
	out, err := e.st.LoadClaimsForGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadClaimsForGoal: %v", err)
	}
	if len(out) != 1 || out[0].ClaimID != "CL-1" {
		t.Fatalf("LoadClaimsForGoal(G-1) = %v, want [CL-1]", claimIDs(out))
	}
	if _, err := e.st.LoadClaimsForGoal(e.ctx, "G-404"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadClaimsForGoal missing goal error = %v, want ErrNotFound", err)
	}
}

func TestClaim_LoadClaimsByStatus(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-2")
	for _, claim := range []*schema.ClaimArtifact{
		{ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimContested},
		{ClaimID: "CL-2", SourceCapsuleID: "CAP-1", Status: schema.ClaimInvalidated},
		{ClaimID: "CL-3", SourceCapsuleID: "CAP-2", Status: schema.ClaimContested},
	} {
		if err := e.st.SaveClaim(e.ctx, claim); err != nil {
			t.Fatalf("SaveClaim %s: %v", claim.ClaimID, err)
		}
	}
	out, err := e.st.LoadClaimsByStatus(e.ctx, "G-1", schema.ClaimContested)
	if err != nil {
		t.Fatalf("LoadClaimsByStatus: %v", err)
	}
	if len(out) != 1 || out[0].ClaimID != "CL-1" {
		t.Fatalf("LoadClaimsByStatus = %v, want [CL-1]", claimIDs(out))
	}
}

func TestClaim_UpdateDisputeAndValidation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-1",
		SourceCapsuleID: "CAP-1",
		Status:          schema.ClaimProposed,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	if err := e.st.UpdateClaimDispute(e.ctx, "CL-1", schema.ClaimContested, []string{"CL-2"}, []string{"CL-3"}); err != nil {
		t.Fatalf("UpdateClaimDispute: %v", err)
	}
	got, err := e.st.LoadClaim(e.ctx, "CL-1")
	if err != nil {
		t.Fatalf("LoadClaim after dispute: %v", err)
	}
	if got.Status != schema.ClaimContested || len(got.ContradictedBy) != 1 || got.ContradictedBy[0] != "CL-2" || len(got.InvalidatedBy) != 1 || got.InvalidatedBy[0] != "CL-3" {
		t.Fatalf("claim after dispute = %+v", got)
	}
	if err := e.st.UpdateClaimValidation(e.ctx, "CL-1", schema.ClaimVerified, "SNAP-1"); err != nil {
		t.Fatalf("UpdateClaimValidation: %v", err)
	}
	got, err = e.st.LoadClaim(e.ctx, "CL-1")
	if err != nil {
		t.Fatalf("LoadClaim after validation: %v", err)
	}
	if got.Status != schema.ClaimVerified || got.LastValidatedAgainst != "SNAP-1" {
		t.Fatalf("claim after validation = %+v", got)
	}
}

func TestGoal_LoadActiveGoal(t *testing.T) {
	e := newEnv(t)

	// No goal at all — must return nil, nil.
	active, err := e.st.LoadActiveGoal(e.ctx)
	if err != nil {
		t.Fatalf("LoadActiveGoal (none): %v", err)
	}
	if active != nil {
		t.Errorf("expected nil when no goals exist, got %+v", active)
	}

	// Seed an active goal.
	e.seedGoal(t, "G-1", "GC-1") // seedGoal creates status=active
	active, err = e.st.LoadActiveGoal(e.ctx)
	if err != nil {
		t.Fatalf("LoadActiveGoal: %v", err)
	}
	if active == nil || active.GoalID != "G-1" {
		t.Errorf("expected G-1, got %+v", active)
	}

	// After completing the goal it must no longer be returned.
	if err := e.st.UpdateGoalStatus(e.ctx, "G-1", schema.GoalStatusComplete); err != nil {
		t.Fatalf("UpdateGoalStatus: %v", err)
	}
	active, err = e.st.LoadActiveGoal(e.ctx)
	if err != nil {
		t.Fatalf("LoadActiveGoal after complete: %v", err)
	}
	if active != nil {
		t.Errorf("expected nil after completion, got %+v", active)
	}
}

// ── Failure Fingerprints ──────────────────────────────────────────────────────

func TestFailure_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")

	f := &schema.FailureFingerprint{
		FailureID: "FAIL-1", SourceCapsuleID: "CAP-1",
		FailureType: schema.FailureTest, Summary: "TestFoo failed",
		AffectedFiles:  []string{"internal/foo/foo_test.go"},
		ErrorSignature: "TestFoo: got nil, want error",
	}
	if err := e.st.SaveFailure(e.ctx, f); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}
	got, err := e.st.LoadFailure(e.ctx, "FAIL-1")
	if err != nil {
		t.Fatalf("LoadFailure: %v", err)
	}
	if got.FailureType != schema.FailureTest || got.ErrorSignature != f.ErrorSignature {
		t.Errorf("failure mismatch: %+v", got)
	}
}

func TestFailure_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	_ = e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
		FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureLint,
	})
	if n := e.countEvents(t, schema.EventFailureFingerprintCreated); n != 1 {
		t.Errorf("expected 1 failure_fingerprint_created event, got %d", n)
	}
}

func TestFailure_LoadForFiles(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	for _, f := range []*schema.FailureFingerprint{
		{FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest,
			AffectedFiles: []string{"internal/auth/auth.go"}},
		{FailureID: "FAIL-2", SourceCapsuleID: "CAP-1", FailureType: schema.FailureLint,
			AffectedFiles: []string{"internal/store/store.go"}},
		{FailureID: "FAIL-3", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest,
			AffectedFiles: []string{"internal/auth/auth.go", "internal/store/store.go"}},
	} {
		if err := e.st.SaveFailure(e.ctx, f); err != nil {
			t.Fatalf("SaveFailure: %v", err)
		}
	}
	out, err := e.st.LoadFailuresForFiles(e.ctx, []string{"internal/auth/auth.go"})
	if err != nil {
		t.Fatalf("LoadFailuresForFiles: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 failures, got %d", len(out))
	}
}

func TestFailure_LoadForFiles_NormalizesWindowsPaths(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
		FailureID:       "FAIL-1",
		SourceCapsuleID: "CAP-1",
		FailureType:     schema.FailureTest,
		AffectedFiles:   []string{`.\internal\auth\auth.go`},
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}
	out, err := e.st.LoadFailuresForFiles(e.ctx, []string{"internal/auth/auth.go"})
	if err != nil {
		t.Fatalf("LoadFailuresForFiles: %v", err)
	}
	if len(out) != 1 || out[0].FailureID != "FAIL-1" {
		t.Fatalf("LoadFailuresForFiles normalized = %+v, want FAIL-1", out)
	}
}

func TestFailure_LoadForCapsule(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-1")
	for _, f := range []*schema.FailureFingerprint{
		{FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest},
		{FailureID: "FAIL-2", SourceCapsuleID: "CAP-1", FailureType: schema.FailureLint},
		{FailureID: "FAIL-3", SourceCapsuleID: "CAP-2", FailureType: schema.FailureTest},
	} {
		if err := e.st.SaveFailure(e.ctx, f); err != nil {
			t.Fatalf("SaveFailure: %v", err)
		}
	}
	out, err := e.st.LoadFailuresForCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadFailuresForCapsule: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 failures for CAP-1, got %d", len(out))
	}
}

func TestFailure_LoadAllFailuresScopesByGoal(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-2")
	for _, f := range []*schema.FailureFingerprint{
		{FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest},
		{FailureID: "FAIL-2", SourceCapsuleID: "CAP-2", FailureType: schema.FailureLint},
	} {
		if err := e.st.SaveFailure(e.ctx, f); err != nil {
			t.Fatalf("SaveFailure: %v", err)
		}
	}

	out, err := e.st.LoadAllFailures(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadAllFailures: %v", err)
	}
	if len(out) != 1 || out[0].FailureID != "FAIL-1" {
		t.Fatalf("LoadAllFailures(G-1) = %+v, want only FAIL-1", out)
	}
	if _, err := e.st.LoadAllFailures(e.ctx, "G-404"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadAllFailures missing goal error = %v, want ErrNotFound", err)
	}
}

func TestFailure_LoadBySignatureReturnsOccurrencesOldestFirst(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedCapsule(t, "CAP-2", "OB-1")
	e.seedCapsule(t, "CAP-3", "OB-2")
	for _, f := range []*schema.FailureFingerprint{
		{FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest, ErrorSignature: "sig", PriorAttemptCount: 0},
		{FailureID: "FAIL-other-signature", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest, ErrorSignature: "other"},
		{FailureID: "FAIL-2", SourceCapsuleID: "CAP-2", FailureType: schema.FailureTest, ErrorSignature: "sig", PriorAttemptCount: 1, PriorCapsuleIDs: []string{"CAP-1"}, RecommendedNextAction: "run targeted test"},
		{FailureID: "FAIL-other-goal", SourceCapsuleID: "CAP-3", FailureType: schema.FailureTest, ErrorSignature: "sig"},
	} {
		if err := e.st.SaveFailure(e.ctx, f); err != nil {
			t.Fatalf("SaveFailure %s: %v", f.FailureID, err)
		}
	}
	out, err := e.st.LoadFailuresBySignature(e.ctx, "G-1", "sig")
	if err != nil {
		t.Fatalf("LoadFailuresBySignature: %v", err)
	}
	if len(out) != 2 || out[0].FailureID != "FAIL-1" || out[1].FailureID != "FAIL-2" {
		t.Fatalf("LoadFailuresBySignature = %+v, want FAIL-1 then FAIL-2", out)
	}
	if out[1].PriorAttemptCount != 1 || len(out[1].PriorCapsuleIDs) != 1 || out[1].PriorCapsuleIDs[0] != "CAP-1" || out[1].RecommendedNextAction == "" {
		t.Fatalf("recurring failure metadata not preserved: %+v", out[1])
	}
}

func TestFailure_LoadAllFailures_OrphanedCapsuleSkipped(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
		FailureID: "FAIL-1", SourceCapsuleID: "CAP-1", FailureType: schema.FailureTest,
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}
	// Write a failure whose capsule file does not exist (ErrNotFound should
	// cause it to be silently skipped, not returned as an error). This bypasses
	// SaveFailure because saves now reject unresolvable GoalID chains.
	capsuleJSON := filepath.Join(e.root, "state", "capsules", "CAP-orphan.json")
	if err := os.WriteFile(capsuleJSON, []byte(`{"capsule_id":"CAP-orphan","obligation_ids":[]}`), 0o644); err != nil {
		t.Fatalf("write orphan capsule file: %v", err)
	}
	orphanFailure := &schema.FailureFingerprint{
		FailureID: "FAIL-orphan", SourceCapsuleID: "CAP-orphan", FailureType: schema.FailureLint,
	}
	orphanBytes, err := json.Marshal(orphanFailure)
	if err != nil {
		t.Fatalf("marshal orphan failure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "artifacts", "failures", "FAIL-orphan.json"), orphanBytes, 0o644); err != nil {
		t.Fatalf("write orphan failure: %v", err)
	}
	// Now remove the orphan capsule file to simulate a missing-capsule scenario.
	if err := os.Remove(capsuleJSON); err != nil {
		t.Fatalf("remove orphan capsule: %v", err)
	}

	// LoadAllFailures should skip FAIL-orphan (ErrNotFound) and return only FAIL-1.
	out, err := e.st.LoadAllFailures(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadAllFailures: %v", err)
	}
	if len(out) != 1 || out[0].FailureID != "FAIL-1" {
		t.Errorf("LoadAllFailures = %+v, want only FAIL-1", out)
	}
}

// ── Verifier Results ──────────────────────────────────────────────────────────

func TestVerifierResult_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")

	vr := &schema.VerifierResult{
		VerifierResultID: "VR-1", PatchID: "PATCH-1", CapsuleID: "CAP-1",
		RecommendedAction: schema.ActionAccept, CreatedAt: time.Now().UTC(),
	}
	if err := e.st.SaveVerifierResult(e.ctx, vr); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	got, err := e.st.LoadVerifierResult(e.ctx, "VR-1")
	if err != nil {
		t.Fatalf("LoadVerifierResult: %v", err)
	}
	if got.RecommendedAction != schema.ActionAccept {
		t.Errorf("action = %s, want accept", got.RecommendedAction)
	}
}

func TestVerifierResult_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")
	_ = e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
		VerifierResultID: "VR-1", PatchID: "PATCH-1", CapsuleID: "CAP-1",
		RecommendedAction: schema.ActionAccept, CreatedAt: time.Now().UTC(),
	})
	if n := e.countEvents(t, schema.EventVerifierResultCreated); n != 1 {
		t.Errorf("expected 1 verifier_result_created event, got %d", n)
	}
}

func TestVerifierResult_LoadForPatch(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")
	e.seedPatch(t, "PATCH-2", "CAP-1")
	for _, vr := range []*schema.VerifierResult{
		{VerifierResultID: "VR-1", PatchID: "PATCH-1", CapsuleID: "CAP-1",
			RecommendedAction: schema.ActionAccept, CreatedAt: time.Now().UTC()},
		{VerifierResultID: "VR-2", PatchID: "PATCH-2", CapsuleID: "CAP-1",
			RecommendedAction: schema.ActionRetry, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveVerifierResult(e.ctx, vr); err != nil {
			t.Fatalf("SaveVerifierResult: %v", err)
		}
	}
	got, err := e.st.LoadVerifierResultForPatch(e.ctx, "PATCH-2")
	if err != nil {
		t.Fatalf("LoadVerifierResultForPatch: %v", err)
	}
	if got.RecommendedAction != schema.ActionRetry {
		t.Errorf("action = %s, want retry", got.RecommendedAction)
	}
}

func TestVerifierResult_LoadForPatch_NotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.st.LoadVerifierResultForPatch(e.ctx, "PATCH-999")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Decision Records ──────────────────────────────────────────────────────────

func TestDecision_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	d := &schema.DecisionRecord{
		DecisionID: "DEC-1", Context: "topology selection",
		Decision: "single", Rationale: "low risk, sequential",
		MadeBy: "system", RelatedIDs: []string{"G-1"}, CreatedAt: time.Now().UTC(),
	}
	if err := e.st.SaveDecision(e.ctx, d); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	got, err := e.st.LoadDecision(e.ctx, "DEC-1")
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if got.Decision != "single" {
		t.Errorf("decision = %s, want single", got.Decision)
	}
}

func TestDecision_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	_ = e.st.SaveDecision(e.ctx, &schema.DecisionRecord{DecisionID: "DEC-1", RelatedIDs: []string{"G-1"}, CreatedAt: time.Now().UTC()})
	if n := e.countEvents(t, schema.EventDecisionRecordCreated); n != 1 {
		t.Errorf("expected 1 decision_record_created event, got %d", n)
	}
}

// ── Budget Records ────────────────────────────────────────────────────────────

func TestBudget_SaveLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	b := &schema.BudgetRecord{
		BudgetID: "BUD-1", GoalID: "G-1", TokensSpent: 1024,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	got, err := e.st.LoadBudgetRecord(e.ctx, "BUD-1")
	if err != nil {
		t.Fatalf("LoadBudgetRecord: %v", err)
	}
	if got.TokensSpent != 1024 {
		t.Errorf("TokensSpent = %d, want 1024", got.TokensSpent)
	}
}

func TestBudget_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	_ = e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID: "BUD-1", GoalID: "G-1",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if n := e.countEvents(t, schema.EventBudgetRecordSaved); n != 1 {
		t.Errorf("expected 1 budget_record_saved event, got %d", n)
	}
}

func TestBudget_LoadForGoal(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	for _, b := range []*schema.BudgetRecord{
		{BudgetID: "BUD-1", GoalID: "G-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{BudgetID: "BUD-2", GoalID: "G-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{BudgetID: "BUD-3", GoalID: "G-2", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
			t.Fatalf("SaveBudgetRecord: %v", err)
		}
	}
	out, err := e.st.LoadBudgetForGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 budget records for G-1, got %d", len(out))
	}
}

func TestBudget_Update(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	b := &schema.BudgetRecord{
		BudgetID: "BUD-1", GoalID: "G-1", TokensSpent: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	b.TokensSpent = 500
	if err := e.st.UpdateBudgetRecord(e.ctx, b); err != nil {
		t.Fatalf("UpdateBudgetRecord: %v", err)
	}
	got, _ := e.st.LoadBudgetRecord(e.ctx, "BUD-1")
	if got.TokensSpent != 500 {
		t.Errorf("TokensSpent = %d, want 500", got.TokensSpent)
	}
	if n := e.countEvents(t, schema.EventBudgetRecordUpdated); n != 1 {
		t.Errorf("expected 1 budget_record_updated event, got %d", n)
	}
}

func TestBudget_UpdateMissingReturnsNotFound(t *testing.T) {
	e := newEnv(t)
	err := e.st.UpdateBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID: "BUD-404", GoalID: "G-1",
		UpdatedAt: time.Now().UTC(),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateBudgetRecord missing error = %v, want ErrNotFound", err)
	}
}

// ── State Snapshots ───────────────────────────────────────────────────────────

func TestSnapshot_LoadLatest(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedGoal(t, "G-2", "GC-2")
	for _, s := range []*schema.StateSnapshot{
		{SnapshotID: "SNAP-1", GoalID: "G-1", SequenceNum: 5, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-2", GoalID: "G-1", SequenceNum: 10, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-3", GoalID: "G-2", SequenceNum: 7, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveSnapshot(e.ctx, s); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}
	latest, err := e.st.LoadLatestSnapshot(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	if latest.SnapshotID != "SNAP-2" || latest.SequenceNum != 10 {
		t.Errorf("latest = %+v, want SNAP-2/10", latest)
	}
}

func TestSnapshot_LoadLatest_NotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.st.LoadLatestSnapshot(e.ctx, "G-999")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSnapshot_SaveEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	_ = e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-1", GoalID: "G-1", CreatedAt: time.Now().UTC(),
	})
	if n := e.countEvents(t, schema.EventStateSnapshotSaved); n != 1 {
		t.Errorf("expected 1 state_snapshot_saved event, got %d", n)
	}
}

func TestSnapshot_LoadSnapshot(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if err := e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-1", GoalID: "G-1", SequenceNum: 7, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, err := e.st.LoadSnapshot(e.ctx, "SNAP-1")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got.SnapshotID != "SNAP-1" || got.SequenceNum != 7 {
		t.Fatalf("snapshot = %+v, want SNAP-1/7", got)
	}
}

// ── Topology Outcomes ────────────────────────────────────────────────────────

func TestTopologyOutcome_SaveLoadAndEmitsEvent(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	record := &schema.TopologyOutcomeRecord{
		OutcomeID:       "TO-1",
		GoalID:          "G-1",
		Topology:        schema.TopologySingle,
		ObligationCount: 2,
		MaxRiskLevel:    schema.RiskLow,
		AffectedFiles:   []string{"internal/store/filestore.go"},
		PatchAccepted:   true,
		ObligationsMet:  2,
		TokensSpent:     1200,
		FailureCount:    0,
		RecordedAt:      time.Now().UTC(),
	}
	if err := e.st.SaveTopologyOutcome(e.ctx, record); err != nil {
		t.Fatalf("SaveTopologyOutcome: %v", err)
	}
	if n := e.countEvents(t, schema.EventTopologyOutcomeRecorded); n != 1 {
		t.Fatalf("expected 1 topology_outcome_recorded event, got %d", n)
	}
	byGoal, err := e.st.LoadTopologyOutcomesForGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
	}
	if len(byGoal) != 1 || byGoal[0].OutcomeID != "TO-1" {
		t.Fatalf("LoadTopologyOutcomesForGoal = %+v, want TO-1", byGoal)
	}
	byShape, err := e.st.LoadTopologyOutcomes(e.ctx, schema.TopologySingle, schema.RiskLow)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomes: %v", err)
	}
	if len(byShape) != 1 || byShape[0].OutcomeID != "TO-1" {
		t.Fatalf("LoadTopologyOutcomes = %+v, want TO-1", byShape)
	}
	if _, err := os.Stat(filepath.Join(e.root, "artifacts", "topology_outcomes", "TO-1.json")); err != nil {
		t.Fatalf("expected per-artifact topology outcome file: %v", err)
	}
}

func TestTopologyOutcome_ReplayReconstructsArtifact(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if err := e.st.SaveTopologyOutcome(e.ctx, &schema.TopologyOutcomeRecord{
		OutcomeID:       "TO-1",
		GoalID:          "G-1",
		Topology:        schema.TopologyImplementerReviewer,
		ObligationCount: 3,
		MaxRiskLevel:    schema.RiskMedium,
		RecordedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveTopologyOutcome: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	out, err := e.st.LoadTopologyOutcomes(e.ctx, schema.TopologyImplementerReviewer, schema.RiskMedium)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomes after replay: %v", err)
	}
	if len(out) != 1 || out[0].OutcomeID != "TO-1" {
		t.Fatalf("replayed topology outcomes = %+v, want TO-1", out)
	}
}

// ── Replay ────────────────────────────────────────────────────────────────────

func TestReplay_ReconstructsGoalAndObligation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1", "GC-2")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-2", schema.ObligationOpen)

	wipeArtifacts(t, e)

	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	g, err := e.st.LoadGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadGoal after replay: %v", err)
	}
	if len(g.GoalConditions) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(g.GoalConditions))
	}
	if _, err := e.st.LoadObligation(e.ctx, "OB-2"); err != nil {
		t.Fatalf("LoadObligation OB-2 after replay: %v", err)
	}
}

func TestReplay_ReconstructsCapsuleAndPatch(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := e.st.LoadCapsule(e.ctx, "CAP-1"); err != nil {
		t.Fatalf("LoadCapsule after replay: %v", err)
	}
	if _, err := e.st.LoadPatch(e.ctx, "PATCH-1"); err != nil {
		t.Fatalf("LoadPatch after replay: %v", err)
	}
}

func TestReplay_AppliesPatchAccepted(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")

	// Simulate reconciler appending patch_accepted.
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:    schema.EventPatchAccepted,
		GoalID:  "G-1",
		Payload: marshalJSON(t, schema.PatchStatusPayload{PatchID: "PATCH-1"}),
	}); err != nil {
		t.Fatalf("Append patch_accepted: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadPatch(e.ctx, "PATCH-1")
	if err != nil {
		t.Fatalf("LoadPatch after replay: %v", err)
	}
	if got.Status != schema.PatchAccepted {
		t.Errorf("patch status = %s after replay, want accepted", got.Status)
	}
}

func TestReplay_AppliesPatchRejected(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	e.seedPatch(t, "PATCH-1", "CAP-1")

	// Simulate reconciler appending patch_rejected.
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:    schema.EventPatchRejected,
		GoalID:  "G-1",
		Payload: marshalJSON(t, schema.PatchStatusPayload{PatchID: "PATCH-1"}),
	}); err != nil {
		t.Fatalf("Append patch_rejected: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadPatch(e.ctx, "PATCH-1")
	if err != nil {
		t.Fatalf("LoadPatch after replay: %v", err)
	}
	if got.Status != schema.PatchRejected {
		t.Errorf("patch status = %s after replay, want rejected", got.Status)
	}
}

func TestReplay_AppliesGoalStatusUpdated(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:   schema.EventGoalStatusUpdated,
		GoalID: "G-1",
		Payload: marshalJSON(t, schema.GoalStatusPayload{
			GoalID: "G-1",
			Status: schema.GoalStatusComplete,
		}),
	}); err != nil {
		t.Fatalf("Append goal_status_updated: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadGoal(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadGoal after replay: %v", err)
	}
	if got.Status != schema.GoalStatusComplete {
		t.Errorf("goal status = %s after replay, want complete", got.Status)
	}
}

func TestReplay_AppliesObligationStatusUpdated(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:   schema.EventObligationStatusUpdated,
		GoalID: "G-1",
		Payload: marshalJSON(t, schema.ObligationStatusPayload{
			ObligationID: "OB-1",
			Status:       schema.ObligationSatisfied,
			SatisfiedBy:  []string{"EV-1"},
		}),
	}); err != nil {
		t.Fatalf("Append obligation_status_updated: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadObligation(e.ctx, "OB-1")
	if err != nil {
		t.Fatalf("LoadObligation after replay: %v", err)
	}
	if got.Status != schema.ObligationSatisfied || len(got.SatisfiedBy) != 1 || got.SatisfiedBy[0] != "EV-1" {
		t.Errorf("obligation after replay = %+v, want satisfied by EV-1", got)
	}
}

func TestReplay_AppliesClaimStatusUpdated(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if err := e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
		ClaimID: "CL-1", SourceCapsuleID: "CAP-1", Status: schema.ClaimProposed,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:   schema.EventClaimStatusUpdated,
		GoalID: "G-1",
		Payload: marshalJSON(t, schema.ClaimStatusPayload{
			ClaimID:              "CL-1",
			Status:               schema.ClaimContested,
			LastValidatedAgainst: "SNAP-1",
			ContradictedBy:       []string{"CL-2"},
			InvalidatedBy:        []string{"CL-3"},
		}),
	}); err != nil {
		t.Fatalf("Append claim_status_updated: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadClaim(e.ctx, "CL-1")
	if err != nil {
		t.Fatalf("LoadClaim after replay: %v", err)
	}
	if got.Status != schema.ClaimContested ||
		got.LastValidatedAgainst != "SNAP-1" ||
		len(got.ContradictedBy) != 1 || got.ContradictedBy[0] != "CL-2" ||
		len(got.InvalidatedBy) != 1 || got.InvalidatedBy[0] != "CL-3" {
		t.Errorf("claim after replay = %+v, want contested with dispute metadata", got)
	}
}

func TestReplay_AppliesCapsuleStarted(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")

	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:   schema.EventCapsuleStarted,
		GoalID: "G-1",
		Payload: marshalJSON(t, schema.CapsuleTransitionPayload{
			CapsuleID: "CAP-1",
			State:     schema.CapsuleStateAgentRunning,
		}),
	}); err != nil {
		t.Fatalf("Append capsule_started: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule after replay: %v", err)
	}
	if got.State != schema.CapsuleStateAgentRunning {
		t.Errorf("capsule state = %s, want agent_running", got.State)
	}
}

func TestReplay_AppliesCapsuleProjectionLinked(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")

	if err := e.st.UpdateCapsuleProjectionID(e.ctx, "CAP-1", "PROJ-42"); err != nil {
		t.Fatalf("UpdateCapsuleProjectionID: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := e.st.LoadCapsule(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule after replay: %v", err)
	}
	if got.ContextProjectionID != "PROJ-42" {
		t.Errorf("ContextProjectionID = %q, want %q", got.ContextProjectionID, "PROJ-42")
	}
}

func TestReplay_RejectsUpdateBeforeCreate(t *testing.T) {
	e := newEnv(t)
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:   schema.EventPatchAccepted,
		GoalID: "G-1",
		Payload: marshalJSON(t, schema.PatchStatusPayload{
			PatchID: "PATCH-missing",
		}),
	}); err != nil {
		t.Fatalf("Append patch_accepted: %v", err)
	}

	if err := store.Replay(e.ctx, e.log, e.st, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Replay update-before-create error = %v, want ErrNotFound", err)
	}
}

func TestReplay_RejectsMalformedTransitionPayload(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:    schema.EventCapsuleStarted,
		GoalID:  "G-1",
		Payload: marshalJSON(t, map[string]string{"id": "CAP-1"}),
	}); err != nil {
		t.Fatalf("Append malformed capsule_started: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err == nil {
		t.Fatal("Replay succeeded with malformed capsule transition payload")
	}
}

func TestReplay_RejectsMalformedCreatePayload(t *testing.T) {
	e := newEnv(t)
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:    schema.EventGoalCreated,
		GoalID:  "G-1",
		Payload: marshalJSON(t, map[string]string{"original_intent": "missing id"}),
	}); err != nil {
		t.Fatalf("Append malformed goal_created: %v", err)
	}

	if err := store.Replay(e.ctx, e.log, e.st, 0); err == nil {
		t.Fatal("Replay succeeded with malformed goal_created payload")
	}
}

func TestReplay_ProjectionsByRole(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
	e.seedCapsule(t, "CAP-1", "OB-1")
	_ = e.st.SaveProjection(e.ctx, &schema.ContextProjection{
		ContextProjectionID: "CTX-exec", Role: schema.ProjectionRoleExecutor, SourceArtifactIDs: []string{"CAP-1"}, CreatedAt: time.Now().UTC(),
	})
	_ = e.st.SaveProjection(e.ctx, &schema.ContextProjection{
		ContextProjectionID: "CTX-reviewer", Role: schema.ProjectionRoleReviewer, SourceArtifactIDs: []string{"CAP-1"}, CreatedAt: time.Now().UTC(),
	})
	_ = e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: "CTX-human", Role: schema.ProjectionRoleHumanSummary, SourceArtifactIDs: []string{"CAP-1"}, CreatedAt: time.Now().UTC(),
		},
		GoalPlain: "do the thing",
	})

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	exec, err := e.st.LoadProjection(e.ctx, "CTX-exec")
	if err != nil {
		t.Fatalf("LoadProjection after replay: %v", err)
	}
	if exec.Role != schema.ProjectionRoleExecutor {
		t.Errorf("executor role = %s", exec.Role)
	}
	reviewer, err := e.st.LoadProjection(e.ctx, "CTX-reviewer")
	if err != nil {
		t.Fatalf("LoadProjection reviewer after replay: %v", err)
	}
	if reviewer.Role != schema.ProjectionRoleReviewer {
		t.Errorf("reviewer role = %s", reviewer.Role)
	}
	human, err := e.st.LoadHumanSummaryProjection(e.ctx, "CTX-human")
	if err != nil {
		t.Fatalf("LoadHumanSummaryProjection after replay: %v", err)
	}
	if human.GoalPlain != "do the thing" {
		t.Errorf("GoalPlain = %q", human.GoalPlain)
	}
}

func TestReplay_ReconstructsBudgetAndSnapshot(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID: "BUD-1", GoalID: "G-1", TokensSpent: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	if err := e.st.UpdateBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID: "BUD-1", GoalID: "G-1", TokensSpent: 250,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateBudgetRecord: %v", err)
	}
	if err := e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-1", GoalID: "G-1", SequenceNum: 2, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	b, err := e.st.LoadBudgetRecord(e.ctx, "BUD-1")
	if err != nil {
		t.Fatalf("LoadBudgetRecord after replay: %v", err)
	}
	if b.TokensSpent != 250 {
		t.Errorf("replayed budget TokensSpent = %d, want 250", b.TokensSpent)
	}
	snap, err := e.st.LoadLatestSnapshot(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot after replay: %v", err)
	}
	if snap.SnapshotID != "SNAP-1" {
		t.Errorf("replayed snapshot = %+v, want SNAP-1", snap)
	}
}

func TestReplay_FromSnapshotSeq(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")

	// Record snapshot seq.
	evts, _ := e.log.ReadAfter(e.ctx, 0, 0)
	snapshotSeq := evts[len(evts)-1].SequenceNum

	// Add more events after the snapshot.
	e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)

	// Simulate the caller restoring snapshot materialized state first: the goal
	// is already present, but the post-snapshot obligation must be replayed.
	oblDir := filepath.Join(e.root, "state", "obligations")
	if err := os.RemoveAll(oblDir); err != nil {
		t.Fatalf("RemoveAll obligations: %v", err)
	}
	if err := os.MkdirAll(oblDir, 0o755); err != nil {
		t.Fatalf("MkdirAll obligations: %v", err)
	}

	if err := store.Replay(e.ctx, e.log, e.st, snapshotSeq); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := e.st.LoadGoal(e.ctx, "G-1"); err != nil {
		t.Fatalf("snapshot-restored goal should still be present: %v", err)
	}
	if _, err := e.st.LoadObligation(e.ctx, "OB-1"); err != nil {
		t.Fatalf("obligation after snapshot should be replayed: %v", err)
	}
}

func TestSaveRejectsOrphanedCapsuleScopedArtifacts(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")
	if err := e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-orphan",
		ObligationIDs: []string{"OB-missing"},
		State:         schema.CapsuleStatePending,
	}); err == nil {
		t.Fatal("SaveCapsule succeeded with missing obligation")
	}
	if err := e.st.SavePatch(e.ctx, &schema.PatchArtifact{
		PatchID:   "PATCH-orphan",
		CapsuleID: "CAP-missing",
		Status:    schema.PatchCandidate,
	}); err == nil {
		t.Fatal("SavePatch succeeded with missing capsule")
	}
}

func TestSaveRejectsUnresolvableGoalReferences(t *testing.T) {
	tests := []struct {
		name      string
		eventType schema.EventType
		save      func(*testEnv) error
	}{
		{
			name:      "capsule without obligations",
			eventType: schema.EventCapsuleCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				return e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
					CapsuleID: "CAP-no-obligations",
					State:     schema.CapsuleStatePending,
				})
			},
		},
		{
			name:      "patch missing capsule",
			eventType: schema.EventPatchArtifactCreated,
			save: func(e *testEnv) error {
				return e.st.SavePatch(e.ctx, &schema.PatchArtifact{
					PatchID:   "PATCH-orphan",
					CapsuleID: "CAP-missing",
					Status:    schema.PatchCandidate,
				})
			},
		},
		{
			name:      "evidence without resolvable obligation",
			eventType: schema.EventEvidenceArtifactCreated,
			save: func(e *testEnv) error {
				return e.st.SaveEvidence(e.ctx, &schema.EvidenceArtifact{
					EvidenceID: "EV-orphan",
					Type:       schema.EvidenceTestResult,
					Supports:   []string{"OB-missing"},
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "claim missing capsule",
			eventType: schema.EventClaimCreated,
			save: func(e *testEnv) error {
				return e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
					ClaimID:         "CL-orphan",
					SourceCapsuleID: "CAP-missing",
					Status:          schema.ClaimProposed,
				})
			},
		},
		{
			name:      "failure missing capsule",
			eventType: schema.EventFailureFingerprintCreated,
			save: func(e *testEnv) error {
				return e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
					FailureID:       "FAIL-orphan",
					SourceCapsuleID: "CAP-missing",
					FailureType:     schema.FailureTest,
				})
			},
		},
		{
			name:      "verifier result missing capsule",
			eventType: schema.EventVerifierResultCreated,
			save: func(e *testEnv) error {
				return e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
					VerifierResultID:  "VR-orphan",
					CapsuleID:         "CAP-missing",
					RecommendedAction: schema.ActionRetry,
					CreatedAt:         time.Now().UTC(),
				})
			},
		},
		{
			name:      "executor projection without source",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				return e.st.SaveProjection(e.ctx, &schema.ContextProjection{
					ContextProjectionID: "CTX-orphan",
					Role:                schema.ProjectionRoleExecutor,
					CreatedAt:           time.Now().UTC(),
				})
			},
		},
		{
			name:      "human projection without source",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				return e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
					ContextProjection: schema.ContextProjection{
						ContextProjectionID: "CTX-human-orphan",
						Role:                schema.ProjectionRoleHumanSummary,
						CreatedAt:           time.Now().UTC(),
					},
				})
			},
		},
		{
			name:      "decision without related goal",
			eventType: schema.EventDecisionRecordCreated,
			save: func(e *testEnv) error {
				return e.st.SaveDecision(e.ctx, &schema.DecisionRecord{
					DecisionID: "DEC-orphan",
					MadeBy:     "system",
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "budget missing goal",
			eventType: schema.EventBudgetRecordSaved,
			save: func(e *testEnv) error {
				return e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
					BudgetID:  "BUD-orphan",
					GoalID:    "G-missing",
					CreatedAt: time.Now().UTC(),
					UpdatedAt: time.Now().UTC(),
				})
			},
		},
		{
			name:      "snapshot missing goal",
			eventType: schema.EventStateSnapshotSaved,
			save: func(e *testEnv) error {
				return e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
					SnapshotID:  "SNAP-orphan",
					GoalID:      "G-missing",
					SequenceNum: 1,
					CreatedAt:   time.Now().UTC(),
				})
			},
		},
		{
			name:      "topology outcome missing goal",
			eventType: schema.EventTopologyOutcomeRecorded,
			save: func(e *testEnv) error {
				return e.st.SaveTopologyOutcome(e.ctx, &schema.TopologyOutcomeRecord{
					OutcomeID:  "TO-orphan",
					GoalID:     "G-missing",
					Topology:   schema.TopologySingle,
					RecordedAt: time.Now().UTC(),
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.countEvents(t, tc.eventType)
			if err := tc.save(e); err == nil {
				t.Fatal("save succeeded with unresolvable goal reference")
			}
			if after := e.countEvents(t, tc.eventType); after != before {
				t.Fatalf("event count after failed save = %d, want %d", after, before)
			}
		})
	}
}

func TestChildArtifactEventsCarryResolvedGoalID(t *testing.T) {
	tests := []struct {
		name      string
		eventType schema.EventType
		save      func(*testEnv) error
	}{
		{
			name:      "capsule",
			eventType: schema.EventCapsuleCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				return e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
					CapsuleID:     "CAP-1",
					ObligationIDs: []string{"OB-1"},
					State:         schema.CapsuleStatePending,
				})
			},
		},
		{
			name:      "patch",
			eventType: schema.EventPatchArtifactCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SavePatch(e.ctx, &schema.PatchArtifact{
					PatchID:   "PATCH-1",
					CapsuleID: "CAP-1",
					Status:    schema.PatchCandidate,
				})
			},
		},
		{
			name:      "evidence",
			eventType: schema.EventEvidenceArtifactCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				return e.st.SaveEvidence(e.ctx, &schema.EvidenceArtifact{
					EvidenceID: "EV-1",
					Type:       schema.EvidenceTestResult,
					Supports:   []string{"OB-1"},
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "claim",
			eventType: schema.EventClaimCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
					ClaimID:         "CL-1",
					SourceCapsuleID: "CAP-1",
					Status:          schema.ClaimProposed,
				})
			},
		},
		{
			name:      "failure",
			eventType: schema.EventFailureFingerprintCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
					FailureID:       "FAIL-1",
					SourceCapsuleID: "CAP-1",
					FailureType:     schema.FailureTest,
				})
			},
		},
		{
			name:      "verifier result",
			eventType: schema.EventVerifierResultCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
					VerifierResultID:  "VR-1",
					CapsuleID:         "CAP-1",
					RecommendedAction: schema.ActionAccept,
					CreatedAt:         time.Now().UTC(),
				})
			},
		},
		{
			name:      "executor projection",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SaveProjection(e.ctx, &schema.ContextProjection{
					ContextProjectionID: "CTX-1",
					Role:                schema.ProjectionRoleExecutor,
					SourceArtifactIDs:   []string{"CAP-1"},
					CreatedAt:           time.Now().UTC(),
				})
			},
		},
		{
			name:      "human projection",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				e.seedCapsule(t, "CAP-1", "OB-1")
				return e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
					ContextProjection: schema.ContextProjection{
						ContextProjectionID: "CTX-2",
						Role:                schema.ProjectionRoleHumanSummary,
						SourceArtifactIDs:   []string{"CAP-1"},
						CreatedAt:           time.Now().UTC(),
					},
					GoalPlain: "review this",
				})
			},
		},
		{
			name:      "decision",
			eventType: schema.EventDecisionRecordCreated,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				return e.st.SaveDecision(e.ctx, &schema.DecisionRecord{
					DecisionID: "DEC-1",
					RelatedIDs: []string{"G-1"},
					MadeBy:     "system",
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "topology outcome",
			eventType: schema.EventTopologyOutcomeRecorded,
			save: func(e *testEnv) error {
				e.seedGoal(t, "G-1", "GC-1")
				return e.st.SaveTopologyOutcome(e.ctx, &schema.TopologyOutcomeRecord{
					OutcomeID:  "TO-1",
					GoalID:     "G-1",
					Topology:   schema.TopologySingle,
					RecordedAt: time.Now().UTC(),
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if err := tc.save(e); err != nil {
				t.Fatalf("save: %v", err)
			}
			events, err := e.log.ReadByType(e.ctx, tc.eventType, 0, 0)
			if err != nil {
				t.Fatalf("ReadByType(%s): %v", tc.eventType, err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].GoalID != "G-1" {
				t.Fatalf("event GoalID = %q, want G-1", events[0].GoalID)
			}
		})
	}
}

func TestSaveRejectsDuplicateIDsWithoutAppendingSecondCreateEvent(t *testing.T) {
	tests := []struct {
		name      string
		eventType schema.EventType
		save      func(*testEnv) error
	}{
		{
			name:      "goal",
			eventType: schema.EventGoalCreated,
			save: func(e *testEnv) error {
				return e.st.SaveGoal(e.ctx, &schema.GoalIR{
					GoalID: "G-dup",
					GoalConditions: []schema.GoalCondition{{
						ID:                   "GC-dup",
						Description:          "condition",
						EffectiveDescription: "condition",
						Status:               schema.GoalConditionUnmet,
					}},
					RiskLevel: schema.RiskLow,
					CreatedAt: time.Now().UTC(),
					Status:    schema.GoalStatusActive,
				})
			},
		},
		{
			name:      "obligation",
			eventType: schema.EventObligationCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
				}
				return e.st.SaveObligation(e.ctx, &schema.Obligation{
					ObligationID:    "OB-dup",
					GoalConditionID: "GC-1",
					Description:     "duplicate obligation",
					Blocking:        true,
					RiskLevel:       schema.RiskLow,
					Status:          schema.ObligationOpen,
				})
			},
		},
		{
			name:      "capsule",
			eventType: schema.EventCapsuleCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				}
				return e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
					CapsuleID:     "CAP-dup",
					ObligationIDs: []string{"OB-1"},
					State:         schema.CapsuleStatePending,
				})
			},
		},
		{
			name:      "executor projection",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SaveProjection(e.ctx, &schema.ContextProjection{
					ContextProjectionID: "CTX-dup",
					Role:                schema.ProjectionRoleExecutor,
					SourceArtifactIDs:   []string{"CAP-1"},
					CreatedAt:           time.Now().UTC(),
				})
			},
		},
		{
			name:      "human projection",
			eventType: schema.EventContextProjectionCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SaveHumanSummaryProjection(e.ctx, &schema.HumanSummaryProjection{
					ContextProjection: schema.ContextProjection{
						ContextProjectionID: "HCTX-dup",
						Role:                schema.ProjectionRoleHumanSummary,
						SourceArtifactIDs:   []string{"CAP-1"},
						CreatedAt:           time.Now().UTC(),
					},
					GoalPlain: "review this",
				})
			},
		},
		{
			name:      "patch",
			eventType: schema.EventPatchArtifactCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SavePatch(e.ctx, &schema.PatchArtifact{
					PatchID:   "PATCH-dup",
					CapsuleID: "CAP-1",
					Status:    schema.PatchCandidate,
				})
			},
		},
		{
			name:      "evidence",
			eventType: schema.EventEvidenceArtifactCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
				}
				return e.st.SaveEvidence(e.ctx, &schema.EvidenceArtifact{
					EvidenceID: "EV-dup",
					Type:       schema.EvidenceTestResult,
					Supports:   []string{"OB-1"},
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "claim",
			eventType: schema.EventClaimCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SaveClaim(e.ctx, &schema.ClaimArtifact{
					ClaimID:         "CL-dup",
					SourceCapsuleID: "CAP-1",
					Status:          schema.ClaimProposed,
				})
			},
		},
		{
			name:      "failure",
			eventType: schema.EventFailureFingerprintCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SaveFailure(e.ctx, &schema.FailureFingerprint{
					FailureID:       "FAIL-dup",
					SourceCapsuleID: "CAP-1",
					FailureType:     schema.FailureTest,
				})
			},
		},
		{
			name:      "verifier result",
			eventType: schema.EventVerifierResultCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
					e.seedObligation(t, "OB-1", "GC-1", schema.ObligationOpen)
					e.seedCapsule(t, "CAP-1", "OB-1")
				}
				return e.st.SaveVerifierResult(e.ctx, &schema.VerifierResult{
					VerifierResultID:  "VR-dup",
					CapsuleID:         "CAP-1",
					RecommendedAction: schema.ActionAccept,
					CreatedAt:         time.Now().UTC(),
				})
			},
		},
		{
			name:      "decision",
			eventType: schema.EventDecisionRecordCreated,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
				}
				return e.st.SaveDecision(e.ctx, &schema.DecisionRecord{
					DecisionID: "DEC-dup",
					RelatedIDs: []string{"G-1"},
					MadeBy:     "system",
					CreatedAt:  time.Now().UTC(),
				})
			},
		},
		{
			name:      "budget",
			eventType: schema.EventBudgetRecordSaved,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
				}
				return e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
					BudgetID:  "BUD-dup",
					GoalID:    "G-1",
					CreatedAt: time.Now().UTC(),
					UpdatedAt: time.Now().UTC(),
				})
			},
		},
		{
			name:      "snapshot",
			eventType: schema.EventStateSnapshotSaved,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
				}
				return e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
					SnapshotID:  "SNAP-dup",
					GoalID:      "G-1",
					SequenceNum: 1,
					CreatedAt:   time.Now().UTC(),
				})
			},
		},
		{
			name:      "topology outcome",
			eventType: schema.EventTopologyOutcomeRecorded,
			save: func(e *testEnv) error {
				if _, err := e.st.LoadGoal(e.ctx, "G-1"); errors.Is(err, store.ErrNotFound) {
					e.seedGoal(t, "G-1", "GC-1")
				}
				return e.st.SaveTopologyOutcome(e.ctx, &schema.TopologyOutcomeRecord{
					OutcomeID:  "TO-dup",
					GoalID:     "G-1",
					Topology:   schema.TopologySingle,
					RecordedAt: time.Now().UTC(),
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if err := tc.save(e); err != nil {
				t.Fatalf("first save: %v", err)
			}
			before := e.countEvents(t, tc.eventType)
			if err := tc.save(e); err == nil {
				t.Fatal("duplicate save succeeded")
			}
			if after := e.countEvents(t, tc.eventType); after != before {
				t.Fatalf("event count after duplicate = %d, want %d", after, before)
			}
		})
	}
}

func TestSaveRejectsUnsafeArtifactID(t *testing.T) {
	e := newEnv(t)
	err := e.st.SaveGoal(e.ctx, &schema.GoalIR{
		GoalID:         "..\\escape",
		OriginalIntent: "bad id",
		Status:         schema.GoalStatusActive,
	})
	if err == nil {
		t.Fatal("SaveGoal succeeded with path-like ID")
	}
}

func TestSaveRejectsWindowsReservedArtifactID(t *testing.T) {
	for _, id := range []string{"NUL", "con"} {
		t.Run(id, func(t *testing.T) {
			e := newEnv(t)
			err := e.st.SaveGoal(e.ctx, &schema.GoalIR{
				GoalID:         id,
				OriginalIntent: "bad id",
				Status:         schema.GoalStatusActive,
			})
			if err == nil {
				t.Fatalf("SaveGoal succeeded with Windows reserved ID %q", id)
			}
		})
	}
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestConcurrentSaves(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-1", "GC-1")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			oblID := fmt.Sprintf("OB-%03d", id)
			if err := e.st.SaveObligation(e.ctx, &schema.Obligation{
				ObligationID:    oblID,
				GoalConditionID: "GC-1",
				Status:          schema.ObligationOpen,
			}); err != nil {
				t.Errorf("SaveObligation %s: %v", oblID, err)
			}
		}(i)
	}
	wg.Wait()

	open, err := e.st.LoadOpenObligations(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(open) != 50 {
		t.Errorf("expected 50 open obligations, got %d", len(open))
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func wipeArtifacts(t *testing.T, e *testEnv) {
	t.Helper()
	for _, p := range store.ReplayDir(e.root) {
		if err := os.RemoveAll(p); err != nil {
			t.Fatalf("RemoveAll %s: %v", p, err)
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", p, err)
		}
	}
}

func claimIDs(cs []*schema.ClaimArtifact) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ClaimID
	}
	return ids
}
