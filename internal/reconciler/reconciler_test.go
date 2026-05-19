package reconciler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type testEnv struct {
	ctx context.Context
	log *eventlog.FileLog
	st  *store.FileStore
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("Open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &testEnv{ctx: context.Background(), log: log, st: st}
}

func TestReconcileRejectsBlockingObligationWithoutEvidenceIDs(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "NOEVID",
		evidenceIDs:  nil,
		saveEvidence: false,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	wantReason := "blocking obligation OB-NOEVID has no evidence IDs"
	if result.BlockingReason != wantReason {
		t.Fatalf("BlockingReason = %q, want %q", result.BlockingReason, wantReason)
	}

	patch, err := env.st.LoadPatch(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("LoadPatch: %v", err)
	}
	if patch.Status != schema.PatchRejected {
		t.Fatalf("patch status = %s, want %s", patch.Status, schema.PatchRejected)
	}
}

func TestReconcileRejectsAbsentEvidenceArtifact(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "GHOST",
		evidenceIDs:  []string{"EV-GHOST"},
		saveEvidence: false,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	if !strings.Contains(result.BlockingReason, "references absent evidence artifact EV-GHOST") {
		t.Fatalf("BlockingReason = %q, want absent evidence reason", result.BlockingReason)
	}
}

func TestReconcileRejectsClaimedBlockingObligationWithoutVerdict(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "MISSVERDICT",
		evidenceIDs:  nil,
		saveEvidence: false,
		omitVerdict:  true,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	wantReason := "patch PATCH-MISSVERDICT has no obligation verdicts"
	if result.BlockingReason != wantReason {
		t.Fatalf("BlockingReason = %q, want %q", result.BlockingReason, wantReason)
	}
}

func TestReconcileAcceptsPatchAndSnapshotsLastPreSnapshotEvent(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "OK",
		evidenceIDs:  []string{"EV-OK"},
		saveEvidence: true,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted = false, reason=%q", result.BlockingReason)
	}
	if !result.MergeReady {
		t.Fatalf("MergeReady = false, reason=%q", result.BlockingReason)
	}

	obligation, err := env.st.LoadObligation(env.ctx, ids.obligationID)
	if err != nil {
		t.Fatalf("LoadObligation: %v", err)
	}
	if obligation.Status != schema.ObligationSatisfied {
		t.Fatalf("obligation status = %s, want %s", obligation.Status, schema.ObligationSatisfied)
	}
	if len(obligation.SatisfiedBy) != 1 || obligation.SatisfiedBy[0] != ids.evidenceID {
		t.Fatalf("SatisfiedBy = %v, want [%s]", obligation.SatisfiedBy, ids.evidenceID)
	}

	snapshot, err := env.st.LoadLatestSnapshot(env.ctx, ids.goalID)
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	decisionEvents, err := env.log.ReadByType(env.ctx, schema.EventDecisionRecordCreated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType decision: %v", err)
	}
	if len(decisionEvents) != 1 {
		t.Fatalf("decision events = %d, want 1", len(decisionEvents))
	}
	if snapshot.EventID != decisionEvents[0].EventID || snapshot.SequenceNum != decisionEvents[0].SequenceNum {
		t.Fatalf("snapshot anchored at event (%s,%d), want decision event (%s,%d)",
			snapshot.EventID, snapshot.SequenceNum, decisionEvents[0].EventID, decisionEvents[0].SequenceNum)
	}

	mergeEvents, err := env.log.ReadByType(env.ctx, schema.EventMergeApplied, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType merge: %v", err)
	}
	if len(mergeEvents) != 1 {
		t.Fatalf("merge_applied events = %d, want 1", len(mergeEvents))
	}
}

type scenarioOptions struct {
	suffix       string
	evidenceIDs  []string
	saveEvidence bool
	omitVerdict  bool
}

type scenarioIDs struct {
	goalID, conditionID, obligationID string
	capsuleID, patchID, evidenceID    string
	verifierResultID                  string
}

func saveReconcileScenario(t *testing.T, env *testEnv, opts scenarioOptions) scenarioIDs {
	t.Helper()
	now := time.Now().UTC()
	ids := scenarioIDs{
		goalID:           "G-" + opts.suffix,
		conditionID:      "GC-" + opts.suffix,
		obligationID:     "OB-" + opts.suffix,
		capsuleID:        "CAP-" + opts.suffix,
		patchID:          "PATCH-" + opts.suffix,
		verifierResultID: "VR-" + opts.suffix,
	}
	if len(opts.evidenceIDs) > 0 {
		ids.evidenceID = opts.evidenceIDs[0]
	}
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         ids.goalID,
		OriginalIntent: "test reconcile",
		GoalConditions: []schema.GoalCondition{{
			ID:                   ids.conditionID,
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     ids.obligationID,
		GoalConditionID:  ids.conditionID,
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     ids.capsuleID,
		ObligationIDs: []string{ids.obligationID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if opts.saveEvidence {
		if ids.evidenceID == "" {
			t.Fatal("saveEvidence requires an evidence ID")
		}
		if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
			EvidenceID: ids.evidenceID,
			Type:       schema.EvidenceTestResult,
			Command:    "go test ./...",
			ExitCode:   0,
			Supports:   []string{ids.obligationID},
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("SaveEvidence: %v", err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              ids.patchID,
		CapsuleID:            ids.capsuleID,
		ObligationIDsClaimed: []string{ids.obligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	verdicts := []schema.ObligationVerdict{{
		ObligationID: ids.obligationID,
		Verdict:      schema.VerdictSatisfied,
		EvidenceIDs:  opts.evidenceIDs,
	}}
	if opts.omitVerdict {
		verdicts = nil
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID:        ids.verifierResultID,
		PatchID:                 ids.patchID,
		CapsuleID:               ids.capsuleID,
		ObligationResults:       verdicts,
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "tests passed",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	if _, err := env.st.LoadActiveGoal(env.ctx); errors.Is(err, store.ErrNotFound) {
		t.Fatal("scenario must create an active goal")
	}
	return ids
}
