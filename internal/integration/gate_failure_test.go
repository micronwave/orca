package integration_test

import (
	"testing"
	"time"

	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

// TestVerifierGateFailure_ReconcilerRejectsAndCreatesFollowUp verifies the
// failure-recovery loop: when a verifier gate fails for a blocking obligation,
// the reconciler must:
//   - reject the patch (PatchAccepted=false),
//   - convert the stored failure fingerprint into a follow-up blocking obligation
//     (FollowUpObligationIDs non-empty),
//   - persist the follow-up as open+blocking so the next planning cycle picks it up.
func TestVerifierGateFailure_ReconcilerRejectsAndCreatesFollowUp(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-GATEFAIL"
		condID  = "GC-GATEFAIL"
		oblID   = "OB-GATEFAIL"
		capsID  = "CAP-GATEFAIL"
		patchID = "PATCH-GATEFAIL"
		vrID    = "VR-GATEFAIL"
		failID  = "FAIL-GATEFAIL"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "gate failure follow-up test",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "all tests pass",
			EffectiveDescription: "all tests pass",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "go test ./... exits 0",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsID,
		ObligationIDs: []string{oblID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	// Failure fingerprint: the test gate exited non-zero.
	if err := env.st.SaveFailure(ctx, &schema.FailureFingerprint{
		FailureID:             failID,
		SourceCapsuleID:       capsID,
		FailureType:           schema.FailureTest,
		Summary:               "go test ./... exited with code 1",
		ErrorSignature:        "FAIL\tgithub.com/example/pkg\t0.123s",
		AffectedFiles:         []string{"internal/service_test.go"},
		RecommendedNextAction: "fix the failing test in internal/service_test.go",
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// VerifierResult: gate failed, blocking obligation has no evidence, retry needed.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictFailed,
			EvidenceIDs:  nil,
			Notes:        "go test ./... exited 1; 2 tests failed",
		}},
		RecommendedAction:       schema.ActionRetry,
		RecommendationRationale: "test gate failed; retry after fixing failures",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Gate failure on a blocking obligation must reject the patch.
	if result.PatchAccepted {
		t.Errorf("PatchAccepted=true, want false; blocking obligation with failed gate must reject the patch")
	}

	// The failure fingerprint must produce at least one follow-up obligation.
	if len(result.FollowUpObligationIDs) == 0 {
		t.Errorf("FollowUpObligationIDs is empty; reconciler must create follow-up obligations from failure fingerprints")
	}

	// Each follow-up must be open and blocking so the next planning cycle handles it.
	for _, followUpID := range result.FollowUpObligationIDs {
		followUp := mustLoadObligation(t, env, followUpID)
		if followUp.Status != schema.ObligationOpen {
			t.Errorf("follow-up %s status = %s, want open", followUpID, followUp.Status)
		}
		if !followUp.Blocking {
			t.Errorf("follow-up %s is not blocking; follow-ups from gate failures must be blocking", followUpID)
		}
	}

	// Original patch must be stored as rejected.
	assertPatchStatus(t, env, patchID, schema.PatchRejected)
}
