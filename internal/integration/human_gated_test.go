package integration_test

import (
	"testing"
	"time"

	"github.com/micronwave/orca/internal/planner"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

// TestHumanGated_PlannerSelectsTopology verifies that the planner selects the
// human_gated topology when a goal contains a high-risk blocking obligation.
// A high-risk obligation always forces human_gated regardless of other factors.
func TestHumanGated_PlannerSelectsTopology(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID = "G-HUMANGATED-PLAN"
		condID = "GC-HUMANGATED-PLAN"
		oblID  = "OB-HUMANGATED-PLAN"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "high-risk schema migration requiring human approval",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "apply database migration",
			EffectiveDescription: "apply database migration",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskHigh,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "apply irreversible schema change",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskHigh,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	p := planner.New(env.st, planner.Config{
		OrcaDir:            env.root,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil)

	plan, err := p.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Topology != schema.TopologyHumanGated {
		t.Errorf("Topology = %s, want %s; high-risk obligation must force human_gated",
			plan.Topology, schema.TopologyHumanGated)
	}
	// human_gated uses a single capsule (the classifier does not split into IR).
	if len(plan.CapsuleIDs) != 1 {
		t.Errorf("CapsuleIDs len = %d, want 1; human_gated topology uses one executor capsule",
			len(plan.CapsuleIDs))
	}

	// Decision record must document the human_gated selection.
	dec, err := env.st.LoadDecision(ctx, plan.DecisionID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if dec.Decision != string(schema.TopologyHumanGated) {
		t.Errorf("decision.Decision = %q, want %q", dec.Decision, schema.TopologyHumanGated)
	}
}

// TestHumanGated_ReconcilerRequiresHumanApprovalBeforeMerge verifies that when
// all blocking obligations of a high-risk goal are satisfied, the reconciler:
//   - accepts the patch (PatchAccepted=true),
//   - marks it merge-ready (MergeReady=true), and
//   - requires human gate before auto-merge (HumanGateRequired=true).
//
// Critically, the reconciler must NOT emit a merge_applied event automatically;
// that event is only emitted after human approval is given.
func TestHumanGated_ReconcilerRequiresHumanApprovalBeforeMerge(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-HUMANGATED-RECON"
		condID  = "GC-HUMANGATED-RECON"
		oblID   = "OB-HUMANGATED-RECON"
		capsID  = "CAP-HUMANGATED-RECON"
		evID    = "EV-HUMANGATED-RECON"
		patchID = "PATCH-HUMANGATED-RECON"
		vrID    = "VR-HUMANGATED-RECON"
		decID   = "DEC-HUMANGATED-RECON"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "high-risk change needing human approval at merge",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "irreversible change",
			EffectiveDescription: "irreversible change",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskHigh,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "high-risk migration with evidence",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskHigh,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	if err := env.st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologyHumanGated),
		Rationale:  "high risk → human_gated",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsID,
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
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evID},
			Notes:        "all tests pass",
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	mergeEventsBefore := countEventType(t, env, schema.EventMergeApplied)

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// High-risk obligation has evidence → patch accepted.
	if !result.PatchAccepted {
		t.Errorf("PatchAccepted=false, want true; obligation has evidence; reason: %s", result.BlockingReason)
	}
	// No open blocking obligations remain → merge-ready.
	if !result.MergeReady {
		t.Errorf("MergeReady=false, want true; reason: %s", result.BlockingReason)
	}
	// High-risk → human gate required before auto-merge.
	if !result.HumanGateRequired {
		t.Errorf("HumanGateRequired=false, want true; high-risk obligation requires human approval before merge")
	}

	// The reconciler must NOT have emitted merge_applied automatically;
	// that only happens after the human approves via gate.ReviewMerge.
	if got := countEventType(t, env, schema.EventMergeApplied); got != mergeEventsBefore {
		t.Errorf("merge_applied event emitted automatically for high-risk patch; "+
			"HumanGateRequired=true patches must wait for explicit human approval (events before=%d after=%d)",
			mergeEventsBefore, got)
	}
}

// countEventType returns the number of events with the given type in the log.
func countEventType(t *testing.T, env *integEnv, eventType schema.EventType) int {
	t.Helper()
	events := mustReadAllEvents(t, env)
	count := 0
	for _, ev := range events {
		if ev.Type == eventType {
			count++
		}
	}
	return count
}
