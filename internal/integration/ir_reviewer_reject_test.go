package integration_test

import (
	"testing"
	"time"

	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

// TestIRTopology_ReviewerRejects_PatchRejectedWithFollowUp exercises the
// implementer-reviewer rejection path end-to-end:
//   - The verifier recommends ActionRetry because the reviewer found a correctness
//     issue in the implementer's patch.
//   - A failure fingerprint on the implementer capsule records the issue.
//   - The reconciler rejects the patch and converts the fingerprint into a new
//     blocking follow-up obligation for the next planning cycle.
//
// Note: Reconcile is topology-agnostic — it does not inspect which topology
// created the capsule. These tests validate the ActionRetry + failure-fingerprint
// path that IR topology commonly produces. If IR-specific reconciler behavior is
// added in the future, it should have its own test coverage.
func TestIRTopology_ReviewerRejects_PatchRejectedWithFollowUp(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID     = "G-IR-REJECT"
		condID     = "GC-IR-REJECT"
		oblID      = "OB-IR-REJECT"
		implCapsID = "CAP-IR-REJECT-IMPL"
		patchID    = "PATCH-IR-REJECT"
		vrID       = "VR-IR-REJECT"
		failID     = "FAIL-IR-REJECT"
		decID      = "DEC-IR-REJECT"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "medium-risk feature under implementer-reviewer review",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "implement rate limiting",
			EffectiveDescription: "implement rate limiting",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "implement rate limiter with correct concurrent behavior",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	if err := env.st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologyImplementerReviewer),
		Rationale:  "medium risk → implementer_reviewer",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:          implCapsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: decID,
	}); err != nil {
		t.Fatalf("SaveCapsule implementer: %v", err)
	}

	// Failure fingerprint: the reviewer identified a correctness issue.
	if err := env.st.SaveFailure(ctx, &schema.FailureFingerprint{
		FailureID:             failID,
		SourceCapsuleID:       implCapsID,
		FailureType:           schema.FailureAgent,
		Summary:               "reviewer: RateLimiter does not lock before updating shared counter",
		ErrorSignature:        "concurrent_counter_no_lock",
		AffectedFiles:         []string{"internal/limiter.go"},
		RecommendedNextAction: "add sync.Mutex to RateLimiter.Allow",
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            implCapsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
		ChangedFiles:         []string{"internal/limiter.go"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// The reviewer rejected the approach — no tests were run, so no evidence.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        implCapsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictFailed,
			EvidenceIDs:  nil,
			Notes:        "reviewer: concurrent access not handled; retry required",
		}},
		RecommendedAction:       schema.ActionRetry,
		RecommendationRationale: "reviewer found correctness issue; implementation needs rework",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Reviewer rejection must cause the patch to be rejected.
	if result.PatchAccepted {
		t.Errorf("PatchAccepted=true, want false; reviewer rejected the implementation")
	}

	// The failure fingerprint must produce at least one follow-up obligation.
	if len(result.FollowUpObligationIDs) == 0 {
		t.Errorf("FollowUpObligationIDs empty; reviewer rejection with failure fingerprint must create follow-up")
	}

	// Every follow-up must be open and blocking so the next planner cycle handles it.
	for _, followUpID := range result.FollowUpObligationIDs {
		followUp := mustLoadObligation(t, env, followUpID)
		if followUp.Status != schema.ObligationOpen {
			t.Errorf("follow-up %s status = %s, want open", followUpID, followUp.Status)
		}
		if !followUp.Blocking {
			t.Errorf("follow-up %s Blocking=false, want true; follow-ups must gate the next run", followUpID)
		}
	}

	// Patch must be recorded as rejected in the store.
	assertPatchStatus(t, env, patchID, schema.PatchRejected)
}

// TestIRTopology_ReviewerApproves_MergeReady verifies the implementer-reviewer
// success path: when the reviewer approves (ActionAccept with evidence), the
// reconciler accepts the patch and reports MergeReady=true.
func TestIRTopology_ReviewerApproves_MergeReady(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID     = "G-IR-ACCEPT"
		condID     = "GC-IR-ACCEPT"
		oblID      = "OB-IR-ACCEPT"
		implCapsID = "CAP-IR-ACCEPT-IMPL"
		evID       = "EV-IR-ACCEPT"
		patchID    = "PATCH-IR-ACCEPT"
		vrID       = "VR-IR-ACCEPT"
		decID      = "DEC-IR-ACCEPT"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "medium-risk feature approved by reviewer",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "implement rate limiting",
			EffectiveDescription: "implement rate limiting",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "implement rate limiter",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	if err := env.st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologyImplementerReviewer),
		Rationale:  "medium risk → implementer_reviewer",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:          implCapsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: decID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID,
		Type:       schema.EvidenceTestResult,
		ExitCode:   0,
		Supports:   []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            implCapsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// Reviewer approved: tests pass, ActionAccept.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        implCapsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evID},
			Notes:        "tests pass; reviewer: implementation is correct",
		}},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "reviewer approved; all tests pass",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !result.PatchAccepted {
		t.Errorf("PatchAccepted=false, want true; reviewer approved; reason: %s", result.BlockingReason)
	}
	if !result.MergeReady {
		t.Errorf("MergeReady=false, want true; reason: %s", result.BlockingReason)
	}
	// Medium-risk IR topology must not require human gate at merge.
	if result.HumanGateRequired {
		t.Errorf("HumanGateRequired=true for medium-risk IR topology; only high-risk requires human gate at merge")
	}

	assertPatchStatus(t, env, patchID, schema.PatchAccepted)
}
