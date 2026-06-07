package integration_test

import (
	"testing"
	"time"

	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

// TestMultiObligation_TwoCapsules_AllAccepted verifies the full pipeline for a
// goal with two independent blocking obligations, each served by a separate
// capsule and patch. After reconciling both patches:
//   - The first reconcile accepts patch1 but reports MergeReady=false because
//     OB-2 is still open.
//   - The second reconcile accepts patch2 and reports MergeReady=true because
//     no blocking obligations remain.
func TestMultiObligation_TwoCapsules_AllAccepted(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-MULTI-OBL"
		cond1   = "GC-MULTI-OBL-1"
		cond2   = "GC-MULTI-OBL-2"
		obl1    = "OB-MULTI-OBL-1"
		obl2    = "OB-MULTI-OBL-2"
		cap1    = "CAP-MULTI-OBL-1"
		cap2    = "CAP-MULTI-OBL-2"
		patch1  = "PATCH-MULTI-OBL-1"
		patch2  = "PATCH-MULTI-OBL-2"
		ev1     = "EV-MULTI-OBL-1"
		ev2     = "EV-MULTI-OBL-2"
		vr1     = "VR-MULTI-OBL-1"
		vr2     = "VR-MULTI-OBL-2"
		decID   = "DEC-MULTI-OBL"
	)

	// Goal with two independent conditions.
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "multi-obligation acceptance test",
		GoalConditions: []schema.GoalCondition{
			{
				ID:                   cond1,
				Description:          "fix auth middleware",
				EffectiveDescription: "fix auth middleware",
				Status:               schema.GoalConditionUnmet,
			},
			{
				ID:                   cond2,
				Description:          "add regression tests",
				EffectiveDescription: "add regression tests",
				Status:               schema.GoalConditionUnmet,
			},
		},
		RiskLevel: schema.RiskLow,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	// Two independent blocking obligations, one per condition.
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obl1,
		GoalConditionID:  cond1,
		Description:      "fix auth middleware rounding",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation %s: %v", obl1, err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obl2,
		GoalConditionID:  cond2,
		Description:      "add regression test coverage",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation %s: %v", obl2, err)
	}

	// Shared topology decision.
	if err := env.st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "two independent low-risk obligations",
		MadeBy:     "system",
		RelatedIDs: []string{obl1, obl2},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	// Capsule 1 handles OB-1; capsule 2 handles OB-2.
	for _, capsule := range []*schema.ExecutionCapsule{
		{
			CapsuleID:          cap1,
			ObligationIDs:      []string{obl1},
			Agent:              schema.AgentCodex,
			Role:               schema.RoleExecutor,
			State:              schema.CapsuleStateCompleted,
			TopologyDecisionID: decID,
		},
		{
			CapsuleID:          cap2,
			ObligationIDs:      []string{obl2},
			Agent:              schema.AgentCodex,
			Role:               schema.RoleExecutor,
			State:              schema.CapsuleStateCompleted,
			TopologyDecisionID: decID,
		},
	} {
		if err := env.st.SaveCapsule(ctx, capsule); err != nil {
			t.Fatalf("SaveCapsule %s: %v", capsule.CapsuleID, err)
		}
	}

	// Wire up evidence + patch + verifier result for each capsule.
	type capsuleFixture struct {
		capsID, evID, patchID, vrID, oblID string
	}
	for _, f := range []capsuleFixture{
		{cap1, ev1, patch1, vr1, obl1},
		{cap2, ev2, patch2, vr2, obl2},
	} {
		if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
			EvidenceID: f.evID,
			Type:       schema.EvidenceTestResult,
			ExitCode:   0,
			Supports:   []string{f.oblID},
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("SaveEvidence %s: %v", f.evID, err)
		}
		if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID:              f.patchID,
			CapsuleID:            f.capsID,
			ObligationIDsClaimed: []string{f.oblID},
			Status:               schema.PatchCandidate,
		}); err != nil {
			t.Fatalf("SavePatch %s: %v", f.patchID, err)
		}
		if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
			VerifierResultID: f.vrID,
			PatchID:          f.patchID,
			CapsuleID:        f.capsID,
			ObligationResults: []schema.ObligationVerdict{{
				ObligationID: f.oblID,
				Verdict:      schema.VerdictSatisfied,
				EvidenceIDs:  []string{f.evID},
			}},
			RecommendedAction: schema.ActionAccept,
			CreatedAt:         now,
		}); err != nil {
			t.Fatalf("SaveVerifierResult %s: %v", f.vrID, err)
		}
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})

	// Reconcile patch1: OB-1 satisfied, OB-2 still open → not yet merge-ready.
	result1, err := rec.Reconcile(ctx, patch1)
	if err != nil {
		t.Fatalf("Reconcile patch1: %v", err)
	}
	if !result1.PatchAccepted {
		t.Fatalf("patch1: PatchAccepted=false, want true; reason: %s", result1.BlockingReason)
	}
	if result1.MergeReady {
		t.Errorf("patch1: MergeReady=true, want false — OB-2 is still open")
	}

	// OB-1 must now be satisfied in the store.
	if got := mustLoadObligation(t, env, obl1).Status; got != schema.ObligationSatisfied {
		t.Errorf("OB-1 status = %s, want satisfied after first reconcile", got)
	}

	// Reconcile patch2: OB-2 satisfied, no open blocking obligations → merge-ready.
	result2, err := rec.Reconcile(ctx, patch2)
	if err != nil {
		t.Fatalf("Reconcile patch2: %v", err)
	}
	if !result2.PatchAccepted {
		t.Fatalf("patch2: PatchAccepted=false, want true; reason: %s", result2.BlockingReason)
	}
	if !result2.MergeReady {
		t.Errorf("patch2: MergeReady=false, want true — all blocking obligations satisfied; reason: %s",
			result2.BlockingReason)
	}

	// Both patches must be stored as accepted.
	assertPatchStatus(t, env, patch1, schema.PatchAccepted)
	assertPatchStatus(t, env, patch2, schema.PatchAccepted)

	// Both obligations must be satisfied in the store.
	if got := mustLoadObligation(t, env, obl2).Status; got != schema.ObligationSatisfied {
		t.Errorf("OB-2 status = %s, want satisfied after second reconcile", got)
	}

	// No open blocking obligations may remain.
	open, err := env.st.LoadOpenObligations(ctx, goalID)
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	for _, obl := range open {
		if obl.Blocking {
			t.Errorf("blocking obligation %s is still open after all patches reconciled", obl.ObligationID)
		}
	}
}
