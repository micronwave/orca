package integration_test

import (
	"testing"
	"time"

	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

// TestNonBlockingObligationFailure_PatchStillAccepted verifies that when a
// non-blocking obligation has a failed verdict, the patch is still accepted as
// long as every blocking obligation is satisfied with present evidence.
//
// A non-blocking obligation represents advisory/informational requirements
// (e.g. lint warnings). Failing one must never prevent delivery.
func TestNonBlockingObligationFailure_PatchStillAccepted(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-NONBLOCK"
		condID  = "GC-NONBLOCK"
		obl1    = "OB-NONBLOCK-BLOCKING"
		obl2    = "OB-NONBLOCK-ADVISORY"
		capsID  = "CAP-NONBLOCK"
		ev1     = "EV-NONBLOCK"
		patchID = "PATCH-NONBLOCK"
		vrID    = "VR-NONBLOCK"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "non-blocking obligation test",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "implement feature",
			EffectiveDescription: "implement feature",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	// OB-1: blocking — must pass for patch acceptance.
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obl1,
		GoalConditionID:  condID,
		Description:      "all tests pass",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation blocking: %v", err)
	}

	// OB-2: non-blocking — advisory lint obligation; its failure must not block.
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:    obl2,
		GoalConditionID: condID,
		Description:     "lint produces no warnings",
		Blocking:        false,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation non-blocking: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsID,
		ObligationIDs: []string{obl1, obl2},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	// Evidence for the blocking obligation only.
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: ev1,
		Type:       schema.EvidenceTestResult,
		ExitCode:   0,
		Supports:   []string{obl1},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{obl1, obl2},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// VerifierResult: blocking OB-1 satisfied with evidence; non-blocking OB-2 failed.
	// RecommendedAction is ActionAccept because the blocking obligation passed.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{
			{
				ObligationID: obl1,
				Verdict:      schema.VerdictSatisfied,
				EvidenceIDs:  []string{ev1},
				Notes:        "all tests pass",
			},
			{
				ObligationID: obl2,
				Verdict:      schema.VerdictFailed,
				EvidenceIDs:  nil,
				Notes:        "lint produced 3 warnings (advisory only)",
			},
		},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "blocking obligation satisfied; lint warnings are advisory",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Non-blocking failure must not prevent patch acceptance.
	if !result.PatchAccepted {
		t.Errorf("PatchAccepted=false, want true; non-blocking lint failure must not block delivery; reason: %s",
			result.BlockingReason)
	}

	// All blocking obligations satisfied → merge ready.
	if !result.MergeReady {
		t.Errorf("MergeReady=false, want true; blocking obligation satisfied; reason: %s",
			result.BlockingReason)
	}

	// Blocking obligation must be recorded as satisfied.
	if got := mustLoadObligation(t, env, obl1).Status; got != schema.ObligationSatisfied {
		t.Errorf("OB-1 (blocking) status = %s, want satisfied", got)
	}

	// Non-blocking obligation must be recorded as failed.
	if got := mustLoadObligation(t, env, obl2).Status; got != schema.ObligationFailed {
		t.Errorf("OB-2 (non-blocking) status = %s, want failed", got)
	}

	// Patch must be stored as accepted.
	assertPatchStatus(t, env, patchID, schema.PatchAccepted)
}
