//go:build integration

// Package integration_test encodes the Phase 1 acceptance tests.
// These tests are gated behind the "integration" build tag so that plain
// `go test ./...` does not exit non-zero due to intentionally failing gates.
// Run with: go test -tags=integration ./internal/integration/...
//
// Each test exercises the full artifact lifecycle across the event log and
// materialized store, not any single component in isolation.
//
// Phase 1 success criteria (orca.md §23):
//
//  1. Crash-resumable runs: after losing all artifact files, replaying the event
//     log reconstructs every artifact, and a Reconciler operating on that
//     reconstructed state can complete reconciliation without re-running the agent.
//
//  2. Deterministic state reconstruction: replaying from seq=0 produces exactly
//     the same materialized state as the original save sequence — field for field,
//     and without appending any new events to the log.
//
//  3. Accepted patch has attached evidence bundle: the Reconciler must not advance
//     a patch to accepted status unless every blocking obligation has at least one
//     evidence artifact ID recorded in the VerifierResult's ObligationResults.
//
//  4. Accepted patch has present evidence artifacts: the Reconciler must verify
//     that each EvidenceID in ObligationResults resolves to a stored artifact,
//     not just that the EvidenceIDs list is non-empty.
//
// Current pass/fail (run with -tags=integration):
//
//	TestCrashResumableRun
//	TestDeterministicStateReconstruction
//	TestAcceptedPatchRequiresEvidenceBundle
//	TestAcceptedPatchRequiresPresentEvidenceArtifacts
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── Test environment ──────────────────────────────────────────────────────────

type integEnv struct {
	ctx  context.Context
	root string
	log  *eventlog.FileLog
	st   *store.FileStore
}

func newIntegEnv(t *testing.T) *integEnv {
	t.Helper()
	dir := t.TempDir()
	l, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("Open log: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	st, err := store.New(dir, l)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &integEnv{ctx: context.Background(), root: dir, log: l, st: st}
}

// ── Scenario IDs ──────────────────────────────────────────────────────────────

// scenarioIDs holds the stable artifact IDs used across a scenario.
// EvidenceID is empty in the no-evidence scenario.
type scenarioIDs struct {
	GoalID, ConditionID, ObligationID string
	CapsuleID, PatchID, EvidenceID    string
	VerifierResultID                  string
}

// ── Complete scenario ─────────────────────────────────────────────────────────

// buildCompleteScenario creates a full run state in the store as if every
// component had executed in sequence:
//
//	intent_compiler → verifier.ProposeObligations → obligation_planner →
//	context_compiler → capsule_runner → verifier.Verify
//
// It uses direct store and log calls because real component implementations
// do not yet exist. The state it produces is identical to what a correct
// implementation would produce — same events, same artifact files, same field
// values. When components are built, this helper can be replaced with real
// orchestrator calls and the tests must continue to pass.
func buildCompleteScenario(t *testing.T, env *integEnv) scenarioIDs {
	t.Helper()
	ids := scenarioIDs{
		GoalID:           "G-INT-1",
		ConditionID:      "GC-INT-1",
		ObligationID:     "OB-INT-1",
		CapsuleID:        "CAP-INT-1",
		PatchID:          "PATCH-INT-1",
		EvidenceID:       "EV-INT-1",
		VerifierResultID: "VR-INT-1",
	}
	ctx := env.ctx

	// Step 1 — intent_compiler: creates GoalIR (store emits goal_created).
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         ids.GoalID,
		OriginalIntent: "Write integration test coverage for the proof runtime",
		GoalConditions: []schema.GoalCondition{{
			ID:                   ids.ConditionID,
			Description:          "integration tests pass with zero failures",
			EffectiveDescription: "integration tests pass with zero failures",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SaveGoal: %v", err)
	}

	// Step 2 — verifier.ProposeObligations: creates initial Obligation
	// (store emits obligation_created).
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     ids.ObligationID,
		GoalConditionID:  ids.ConditionID,
		Description:      "go test ./... exits 0 with coverage ≥ 80%",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SaveObligation: %v", err)
	}

	// Step 3 — obligation_planner: creates ExecutionCapsule (store emits capsule_created).
	// Capsule is created with State = pending; runner owns the first transition.
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     ids.CapsuleID,
		ObligationIDs: []string{ids.ObligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
		Budget:        schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300},
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SaveCapsule: %v", err)
	}

	// Step 4 — capsule_runner: emits capsule_started before the first state
	// transition, then advances intermediate states directly through the store.
	startCapsuleRun(t, env, ids, "buildCompleteScenario")

	// Step 5 — capsule_runner: saves EvidenceArtifact produced by the agent
	// (store emits evidence_artifact_created). Evidence supports the obligation.
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: ids.EvidenceID,
		Type:       schema.EvidenceTestResult,
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{ids.ObligationID},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SaveEvidence: %v", err)
	}

	// Step 6 — capsule_runner: saves PatchArtifact (store emits patch_artifact_created).
	// The patch claims the obligation as addressed.
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              ids.PatchID,
		CapsuleID:            ids.CapsuleID,
		ObligationIDsClaimed: []string{ids.ObligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SavePatch: %v", err)
	}

	// Step 7 — capsule_runner: emits capsule_completed before the final state update.
	finishCapsuleRun(t, env, ids, "buildCompleteScenario")

	// Step 8 — verifier.Verify: saves VerifierResult with evidence mapped to
	// the obligation (store emits verifier_result_created).
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: ids.VerifierResultID,
		PatchID:          ids.PatchID,
		CapsuleID:        ids.CapsuleID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: ids.ObligationID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{ids.EvidenceID}, // evidence bundle is attached
			Notes:        "go test passed; coverage 85%",
		}},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "all blocking obligations satisfied by evidence",
		CreatedAt:               time.Now().UTC(),
	}); err != nil {
		t.Fatalf("buildCompleteScenario: SaveVerifierResult: %v", err)
	}

	return ids
}

// ── No-evidence-bundle scenario ───────────────────────────────────────────────

// buildNoEvidenceBundleScenario creates a run state where the capsule produced
// a patch and the verifier issued a VerifierResult, but the ObligationResults
// list contains NO evidence IDs for the blocking obligation.
//
// This models a corrupted or incorrect verifier result that recommends "accept"
// without actually mapping any evidence. The Reconciler must detect this and
// reject the patch regardless of RecommendedAction. orca.md §11.
func buildNoEvidenceBundleScenario(t *testing.T, env *integEnv) scenarioIDs {
	t.Helper()
	ids := scenarioIDs{
		GoalID:           "G-INT-2",
		ConditionID:      "GC-INT-2",
		ObligationID:     "OB-INT-2",
		CapsuleID:        "CAP-INT-2",
		PatchID:          "PATCH-INT-2",
		VerifierResultID: "VR-INT-2",
		// EvidenceID intentionally left empty; no evidence saved to the store
	}
	ctx := env.ctx

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         ids.GoalID,
		OriginalIntent: "Test evidence bundle enforcement",
		GoalConditions: []schema.GoalCondition{{
			ID:                   ids.ConditionID,
			Description:          "patch carries evidence",
			EffectiveDescription: "patch carries evidence",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("buildNoEvidenceBundleScenario: SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:    ids.ObligationID,
		GoalConditionID: ids.ConditionID,
		Description:     "all tests pass",
		Blocking:        true, // blocking — reconciler must verify evidence
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("buildNoEvidenceBundleScenario: SaveObligation: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     ids.CapsuleID,
		ObligationIDs: []string{ids.ObligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("buildNoEvidenceBundleScenario: SaveCapsule: %v", err)
	}

	// The capsule has completed and produced a patch; the only invalid condition
	// is the verifier's missing evidence mapping below.
	startCapsuleRun(t, env, ids, "buildNoEvidenceBundleScenario")

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              ids.PatchID,
		CapsuleID:            ids.CapsuleID,
		ObligationIDsClaimed: []string{ids.ObligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("buildNoEvidenceBundleScenario: SavePatch: %v", err)
	}
	finishCapsuleRun(t, env, ids, "buildNoEvidenceBundleScenario")

	// The verifier result exists and recommends "accept", but maps ZERO evidence
	// IDs to the blocking obligation. The Reconciler must reject despite the
	// ActionAccept recommendation because the evidence bundle is absent.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: ids.VerifierResultID,
		PatchID:          ids.PatchID,
		CapsuleID:        ids.CapsuleID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: ids.ObligationID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  nil, // ← EMPTY: no evidence bundle despite satisfied verdict
			Notes:        "agent claimed tests pass but produced no evidence",
		}},
		RecommendedAction:       schema.ActionAccept, // incorrect recommendation
		RecommendationRationale: "agent asserted tests pass",
		CreatedAt:               time.Now().UTC(),
	}); err != nil {
		t.Fatalf("buildNoEvidenceBundleScenario: SaveVerifierResult: %v", err)
	}

	return ids
}

// buildGhostEvidenceScenario creates a scenario where the VerifierResult
// references a non-empty EvidenceID list, but none of the referenced
// EvidenceArtifacts were ever saved to the store ("ghost evidence").
//
// This tests that the Reconciler verifies artifact existence, not just that
// the EvidenceIDs list is non-empty. A Reconciler that only checks
// len(EvidenceIDs) > 0 will pass TestAcceptedPatchRequiresEvidenceBundle but
// still violate criterion 4. orca.md §11 step 3: the Reconciler must call
// store.LoadEvidence for each EvidenceID and confirm the artifact exists.
func buildGhostEvidenceScenario(t *testing.T, env *integEnv) scenarioIDs {
	t.Helper()
	ids := scenarioIDs{
		GoalID:           "G-INT-3",
		ConditionID:      "GC-INT-3",
		ObligationID:     "OB-INT-3",
		CapsuleID:        "CAP-INT-3",
		PatchID:          "PATCH-INT-3",
		EvidenceID:       "EV-INT-GHOST", // referenced in VerifierResult but never saved
		VerifierResultID: "VR-INT-3",
	}
	ctx := env.ctx

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         ids.GoalID,
		OriginalIntent: "Test ghost evidence rejection",
		GoalConditions: []schema.GoalCondition{{
			ID:                   ids.ConditionID,
			Description:          "patch carries verifiable evidence",
			EffectiveDescription: "patch carries verifiable evidence",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("buildGhostEvidenceScenario: SaveGoal: %v", err)
	}

	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:    ids.ObligationID,
		GoalConditionID: ids.ConditionID,
		Description:     "all tests pass",
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("buildGhostEvidenceScenario: SaveObligation: %v", err)
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     ids.CapsuleID,
		ObligationIDs: []string{ids.ObligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("buildGhostEvidenceScenario: SaveCapsule: %v", err)
	}

	// The capsule has completed and produced a patch; the only invalid condition
	// is that the verifier references evidence that is absent from the store.
	startCapsuleRun(t, env, ids, "buildGhostEvidenceScenario")

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              ids.PatchID,
		CapsuleID:            ids.CapsuleID,
		ObligationIDsClaimed: []string{ids.ObligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("buildGhostEvidenceScenario: SavePatch: %v", err)
	}
	finishCapsuleRun(t, env, ids, "buildGhostEvidenceScenario")

	// The VerifierResult references EV-INT-GHOST as evidence for the blocking
	// obligation. That artifact is intentionally never saved to the store.
	// A Reconciler must reject the patch because it cannot resolve the referenced
	// artifact — a non-empty EvidenceIDs list alone is not sufficient.
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: ids.VerifierResultID,
		PatchID:          ids.PatchID,
		CapsuleID:        ids.CapsuleID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: ids.ObligationID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{ids.EvidenceID}, // ← non-empty but artifact absent from store
			Notes:        "agent claimed tests pass; sidecar listed an evidence ID",
		}},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "agent reported evidence",
		CreatedAt:               time.Now().UTC(),
	}); err != nil {
		t.Fatalf("buildGhostEvidenceScenario: SaveVerifierResult: %v", err)
	}

	// Intentionally do NOT save any EvidenceArtifact with ids.EvidenceID.
	return ids
}

func startCapsuleRun(t *testing.T, env *integEnv, ids scenarioIDs, label string) {
	t.Helper()
	ctx := env.ctx
	if _, err := env.log.Append(ctx, schema.Event{
		Type:   schema.EventCapsuleStarted,
		GoalID: ids.GoalID,
		Payload: mustMarshal(t, schema.CapsuleTransitionPayload{
			CapsuleID: ids.CapsuleID,
			State:     schema.CapsuleStateWorktreeCreated,
		}),
	}); err != nil {
		t.Fatalf("%s: Append capsule_started: %v", label, err)
	}
	for _, state := range []schema.CapsuleState{
		schema.CapsuleStateWorktreeCreated,
		schema.CapsuleStateWorkspaceAttached,
		schema.CapsuleStateSetupRun,
		schema.CapsuleStateAgentRunning,
	} {
		if err := env.st.UpdateCapsuleState(ctx, ids.CapsuleID, state); err != nil {
			t.Fatalf("%s: UpdateCapsuleState(%s): %v", label, state, err)
		}
	}
}

func finishCapsuleRun(t *testing.T, env *integEnv, ids scenarioIDs, label string) {
	t.Helper()
	ctx := env.ctx
	if _, err := env.log.Append(ctx, schema.Event{
		Type:   schema.EventCapsuleCompleted,
		GoalID: ids.GoalID,
		Payload: mustMarshal(t, schema.CapsuleTransitionPayload{
			CapsuleID: ids.CapsuleID,
			State:     schema.CapsuleStateCompleted,
		}),
	}); err != nil {
		t.Fatalf("%s: Append capsule_completed: %v", label, err)
	}
	if err := env.st.UpdateCapsuleState(ctx, ids.CapsuleID, schema.CapsuleStateCompleted); err != nil {
		t.Fatalf("%s: UpdateCapsuleState(completed): %v", label, err)
	}
}

// ── Captured run state ────────────────────────────────────────────────────────

// capturedRun holds all artifacts from a complete scenario for comparison.
type capturedRun struct {
	goal     *schema.GoalIR
	obl      *schema.Obligation
	cap      *schema.ExecutionCapsule
	patch    *schema.PatchArtifact
	evidence *schema.EvidenceArtifact
	vr       *schema.VerifierResult
}

// captureCompleteRun loads every artifact from a complete scenario.
func captureCompleteRun(t *testing.T, env *integEnv, ids scenarioIDs) capturedRun {
	t.Helper()
	ev, err := env.st.LoadEvidence(env.ctx, ids.EvidenceID)
	if err != nil {
		t.Fatalf("captureCompleteRun: LoadEvidence(%s): %v", ids.EvidenceID, err)
	}
	vr, err := env.st.LoadVerifierResultForPatch(env.ctx, ids.PatchID)
	if err != nil {
		t.Fatalf("captureCompleteRun: LoadVerifierResultForPatch(%s): %v", ids.PatchID, err)
	}
	return capturedRun{
		goal:     mustLoadGoal(t, env, ids.GoalID),
		obl:      mustLoadObligation(t, env, ids.ObligationID),
		cap:      mustLoadCapsule(t, env, ids.CapsuleID),
		patch:    mustLoadPatch(t, env, ids.PatchID),
		evidence: ev,
		vr:       vr,
	}
}

// ── Acceptance tests ──────────────────────────────────────────────────────────

// TestCrashResumableRun encodes Phase 1 success criterion 1:
// "crash-resumable runs".
//
// The test runs in two phases:
//
// Phase A — data layer: builds a complete run state, simulates crash by deleting
// all artifact files (the event log survives), replays from seq=0, and asserts
// that every artifact is fully reconstructed with correct field values and state
// transitions. This phase PASSES because the data layer is fully implemented.
//
// Phase B — reconciliation gate: calls Reconcile on the replayed state and
// asserts that reconciliation completes successfully. A correct Reconciler must
// be able to read the VerifierResult and evidence from the replayed store and
// accept the patch.
func TestCrashResumableRun(t *testing.T) {
	env := newIntegEnv(t)
	ids := buildCompleteScenario(t, env)

	// ── Phase A: crash simulation and data-layer reconstruction ──────────────

	// Assert pre-crash state is what we expect.
	assertCapsuleState(t, env, ids.CapsuleID, schema.CapsuleStateCompleted)
	assertPatchStatus(t, env, ids.PatchID, schema.PatchCandidate)

	// Confirm the event count before crash so we can verify replay is read-only.
	preEvents := mustReadAllEvents(t, env)

	// Simulate crash: delete all materialized artifact files.
	// The event log at events.log is untouched — it is the durable source of truth.
	wipeArtifactFiles(t, env)

	// Artifacts must be absent after the wipe.
	if _, err := env.st.LoadPatch(env.ctx, ids.PatchID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("patch artifact must be absent after crash simulation")
	}
	if _, err := env.st.LoadGoal(env.ctx, ids.GoalID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("goal artifact must be absent after crash simulation")
	}

	// Replay the entire event log from seq=0 to reconstruct materialized state.
	if err := store.Replay(env.ctx, env.log, env.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Replay must be read-only: no new events appended to the log.
	postEvents := mustReadAllEvents(t, env)
	if len(postEvents) != len(preEvents) {
		t.Errorf("replay appended events to the log: before=%d after=%d (must be read-only)",
			len(preEvents), len(postEvents))
	}

	// Every artifact must be fully reconstructed with its original field values.
	goal := mustLoadGoal(t, env, ids.GoalID)
	if goal.Status != schema.GoalStatusActive {
		t.Errorf("replayed goal status = %s, want active", goal.Status)
	}
	if len(goal.GoalConditions) != 1 || goal.GoalConditions[0].ID != ids.ConditionID {
		t.Errorf("replayed goal conditions = %+v, want [{ID:%s}]", goal.GoalConditions, ids.ConditionID)
	}

	obl := mustLoadObligation(t, env, ids.ObligationID)
	if obl.Status != schema.ObligationOpen {
		t.Errorf("replayed obligation status = %s, want open (reconciler has not run yet)", obl.Status)
	}
	if !obl.Blocking {
		t.Errorf("replayed obligation blocking = false, want true")
	}

	cap := mustLoadCapsule(t, env, ids.CapsuleID)
	if cap.State != schema.CapsuleStateCompleted {
		t.Errorf("replayed capsule state = %s, want completed (capsule_completed event was logged)", cap.State)
	}
	if len(cap.ObligationIDs) != 1 || cap.ObligationIDs[0] != ids.ObligationID {
		t.Errorf("replayed capsule obligation IDs = %v, want [%s]", cap.ObligationIDs, ids.ObligationID)
	}

	evidence, err := env.st.LoadEvidence(env.ctx, ids.EvidenceID)
	if err != nil {
		t.Fatalf("LoadEvidence(%s) after crash replay: %v", ids.EvidenceID, err)
	}
	if len(evidence.Supports) == 0 || evidence.Supports[0] != ids.ObligationID {
		t.Errorf("replayed evidence supports = %v, want [%s]", evidence.Supports, ids.ObligationID)
	}

	patch := mustLoadPatch(t, env, ids.PatchID)
	if patch.Status != schema.PatchCandidate {
		t.Errorf("replayed patch status = %s, want candidate (reconciler has not run yet)", patch.Status)
	}
	if len(patch.ObligationIDsClaimed) != 1 || patch.ObligationIDsClaimed[0] != ids.ObligationID {
		t.Errorf("replayed patch obligation IDs claimed = %v, want [%s]",
			patch.ObligationIDsClaimed, ids.ObligationID)
	}

	vr, err := env.st.LoadVerifierResultForPatch(env.ctx, ids.PatchID)
	if err != nil {
		t.Fatalf("LoadVerifierResultForPatch(%s) after crash replay: %v", ids.PatchID, err)
	}
	if vr.RecommendedAction != schema.ActionAccept {
		t.Errorf("replayed verifier action = %s, want accept", vr.RecommendedAction)
	}
	if len(vr.ObligationResults) != 1 || len(vr.ObligationResults[0].EvidenceIDs) == 0 {
		t.Errorf("replayed verifier result has no evidence IDs for obligation — evidence bundle not preserved")
	}

	// Verify that LoadEvidenceForObligation returns the evidence after replay.
	// The reconciler will call this to build the mapping.
	evidenceForObl, err := env.st.LoadEvidenceForObligation(env.ctx, ids.ObligationID)
	if err != nil {
		t.Fatalf("LoadEvidenceForObligation after crash replay: %v", err)
	}
	if len(evidenceForObl) != 1 || evidenceForObl[0].EvidenceID != ids.EvidenceID {
		t.Errorf("evidence for obligation after replay = %v, want [%s]", evidenceForObl, ids.EvidenceID)
	}

	// ── Phase B: reconciliation gate ─────────────────────────────────────────
	//
	// ACCEPTANCE GATE: the following call fails until a real Reconciler is wired.
	//
	// A correct Reconciler operating on the replayed state must:
	//   1. Load the VerifierResult for PATCH-INT-1.
	//   2. Find EvidenceIDs:[EV-INT-1] for OB-INT-1 in ObligationResults.
	//   3. Verify the EvidenceArtifact EV-INT-1 exists in the store.
	//   4. Emit obligation_status_updated, patch_accepted to the log.
	//   5. Call UpdateObligationStatus(OB-INT-1, satisfied, [EV-INT-1]).
	//   6. Call UpdatePatchStatus(PATCH-INT-1, accepted).
	//   7. Return ReconcileResult{PatchAccepted: true}.
	//
	rec := reconciler.New(env.st, env.log)
	result, reconcileErr := rec.Reconcile(env.ctx, ids.PatchID)
	if reconcileErr != nil {
		t.Fatalf("crash-resume Reconcile: %v\n\n"+
			"ACCEPTANCE GATE: implement internal/reconciler.Reconciler "+
			"to satisfy the 'crash-resumable runs' Phase 1 criterion.",
			reconcileErr)
	}
	if !result.PatchAccepted {
		t.Errorf("crash-resume: Reconcile returned PatchAccepted=false, want true " +
			"(all evidence is present in replayed store; reconciler should accept)")
	}
}

// TestDeterministicStateReconstruction encodes Phase 1 success criterion 2:
// "deterministic state reconstruction".
//
// It verifies that replaying the event log from seq=0 produces exactly the
// same materialized state as the original save sequence — every artifact is
// field-for-field identical, and no new events are appended during replay.
//
// This test PASSES because the data layer (FileStore + FileLog + Replay) is
// fully implemented. It documents the invariant and guards against regressions.
func TestDeterministicStateReconstruction(t *testing.T) {
	env := newIntegEnv(t)
	ids := buildCompleteScenario(t, env)

	// Record event count before replay. Replay must not append new events.
	preReplayEvents := mustReadAllEvents(t, env)

	// Capture the pre-wipe materialized state for comparison.
	original := captureCompleteRun(t, env, ids)

	// Wipe all artifact files; the event log remains the sole source of state.
	wipeArtifactFiles(t, env)

	// Verify artifacts are absent (confirms the wipe worked).
	if _, err := env.st.LoadGoal(env.ctx, ids.GoalID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("goal must be absent after wipe (test setup error)")
	}

	// Replay from seq=0 to reconstruct the full materialized state.
	if err := store.Replay(env.ctx, env.log, env.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Replay must be read-only: event count must be unchanged.
	postReplayEvents := mustReadAllEvents(t, env)
	if len(postReplayEvents) != len(preReplayEvents) {
		t.Errorf("event count changed during replay: before=%d after=%d "+
			"(Replay must not emit events)", len(preReplayEvents), len(postReplayEvents))
	}
	// Sequence numbers must also be unchanged.
	if len(postReplayEvents) > 0 {
		lastPre := preReplayEvents[len(preReplayEvents)-1].SequenceNum
		lastPost := postReplayEvents[len(postReplayEvents)-1].SequenceNum
		if lastPre != lastPost {
			t.Errorf("last sequence number changed during replay: before=%d after=%d",
				lastPre, lastPost)
		}
	}

	// Load the replayed state and compare field-by-field with the original.
	replayed := captureCompleteRun(t, env, ids)

	assertJSONEqual(t, "GoalIR", original.goal, replayed.goal)
	assertJSONEqual(t, "Obligation", original.obl, replayed.obl)
	assertJSONEqual(t, "ExecutionCapsule", original.cap, replayed.cap)
	assertJSONEqual(t, "PatchArtifact", original.patch, replayed.patch)
	assertJSONEqual(t, "EvidenceArtifact", original.evidence, replayed.evidence)
	assertJSONEqual(t, "VerifierResult", original.vr, replayed.vr)

	// The event log itself must be byte-for-byte identical before and after replay.
	// Verify by re-reading all events and checking they match the pre-replay set.
	for i, pre := range preReplayEvents {
		post := postReplayEvents[i]
		if pre.EventID != post.EventID || pre.SequenceNum != post.SequenceNum || pre.Type != post.Type {
			t.Errorf("event[%d] changed during replay: before=%+v after=%+v", i, pre, post)
		}
	}
}

// TestAcceptedPatchRequiresEvidenceBundle encodes Phase 1 success criterion 3:
// "accepted patch has attached evidence bundle".
//
// It verifies that the Reconciler enforces the invariant from orca.md §11:
// "Must NOT accept a patch without mapping evidence to every blocking obligation."
//
// Setup: a patch exists with a VerifierResult that recommends "accept" but maps
// ZERO evidence artifact IDs to the blocking obligation. The Reconciler must
// examine ObligationResults[].EvidenceIDs and reject the patch because the
// evidence bundle is absent, regardless of the RecommendedAction field.
//
// This test verifies that the real Reconciler enforces the evidence-bundle
// invariant.
func TestAcceptedPatchRequiresEvidenceBundle(t *testing.T) {
	env := newIntegEnv(t)
	ids := buildNoEvidenceBundleScenario(t, env)

	// Precondition: verify the obligation is blocking and has no evidence
	// in the store. The test is only meaningful if the store confirms this.
	obl := mustLoadObligation(t, env, ids.ObligationID)
	if !obl.Blocking {
		t.Fatal("precondition: obligation must be blocking for this test to be meaningful")
	}
	evidenceForObl, err := env.st.LoadEvidenceForObligation(env.ctx, ids.ObligationID)
	if err != nil {
		t.Fatalf("LoadEvidenceForObligation: %v", err)
	}
	if len(evidenceForObl) != 0 {
		t.Fatalf("precondition: expected zero evidence for obligation, got %d "+
			"(scenario setup error)", len(evidenceForObl))
	}

	// Precondition: verify the VerifierResult exists but has no evidence IDs
	// for the blocking obligation.
	vr, err := env.st.LoadVerifierResultForPatch(env.ctx, ids.PatchID)
	if err != nil {
		t.Fatalf("LoadVerifierResultForPatch: %v", err)
	}
	if len(vr.ObligationResults) == 0 {
		t.Fatal("precondition: VerifierResult must have at least one ObligationVerdict")
	}
	if len(vr.ObligationResults[0].EvidenceIDs) != 0 {
		t.Fatalf("precondition: ObligationVerdict must have zero EvidenceIDs, got %v "+
			"(scenario setup error)", vr.ObligationResults[0].EvidenceIDs)
	}

	// Attempt reconciliation via the real Reconciler.
	//
	// The real Reconciler must return PatchAccepted:false when ObligationResults
	// contains no EvidenceIDs for a blocking obligation.
	rec := reconciler.New(env.st, env.log)

	result, reconcileErr := rec.Reconcile(env.ctx, ids.PatchID)
	if reconcileErr != nil {
		// Infrastructure errors (I/O, missing artifacts) are not policy rejections.
		// A lazy implementation returning errors.New("todo") would silently pass
		// this gate. The real Reconciler must return PatchAccepted:false, not an
		// error, to signal evidence-bundle rejection.
		t.Fatalf("Reconcile returned unexpected error — infrastructure errors are not "+
			"a valid evidence-bundle rejection signal; return "+
			"ReconcileResult{PatchAccepted:false} instead: %v", reconcileErr)
	}

	// A correct Reconciler MUST return PatchAccepted:false when a blocking
	// obligation has no evidence IDs in the VerifierResult.
	if result.PatchAccepted {
		t.Errorf(
			"Reconciler accepted patch %q without evidence bundle — "+
				"blocking obligation %q has zero EvidenceIDs in the VerifierResult.\n"+
				"Invariant (orca.md §11): 'Must NOT accept a patch without mapping "+
				"evidence to every blocking obligation.'\n"+
				"Reconciler must reject patches whose blocking obligations lack evidence.",
			ids.PatchID, ids.ObligationID,
		)
	}

	// Secondary check: even if the stub returned PatchAccepted:true, the patch
	// status in the store must NOT have been updated to "accepted". The store is
	// the durable source of truth — if the stub accepted in memory but did not
	// update the store, the invariant still holds at the persistence layer.
	patch := mustLoadPatch(t, env, ids.PatchID)
	if patch.Status == schema.PatchAccepted {
		t.Errorf(
			"patch %q stored as accepted without evidence — "+
				"data invariant violated: patch status must remain candidate "+
				"when evidence bundle is absent",
			ids.PatchID,
		)
	}
}

// TestAcceptedPatchRequiresPresentEvidenceArtifacts verifies Phase 1 criterion 4:
// the Reconciler must confirm that each EvidenceID in ObligationResults resolves
// to an artifact that exists in the store, not merely that the list is non-empty.
//
// This test verifies that the real Reconciler calls store.LoadEvidence for each
// EvidenceID and rejects when any referenced artifact is absent.
func TestAcceptedPatchRequiresPresentEvidenceArtifacts(t *testing.T) {
	env := newIntegEnv(t)
	ids := buildGhostEvidenceScenario(t, env)

	// Precondition: VerifierResult must have a non-empty EvidenceIDs list.
	vr, err := env.st.LoadVerifierResult(env.ctx, ids.VerifierResultID)
	if err != nil {
		t.Fatalf("precondition: LoadVerifierResult: %v", err)
	}
	if len(vr.ObligationResults) == 0 {
		t.Fatal("precondition: VerifierResult must have at least one ObligationVerdict")
	}
	if len(vr.ObligationResults[0].EvidenceIDs) == 0 {
		t.Fatal("precondition: ObligationVerdict must have at least one EvidenceID (ghost scenario setup error)")
	}

	// Precondition: the referenced EvidenceArtifact must NOT exist in the store.
	_, loadErr := env.st.LoadEvidence(env.ctx, ids.EvidenceID)
	if loadErr == nil {
		t.Fatalf("precondition: EvidenceArtifact %q must NOT exist in the store — "+
			"ghost-evidence scenario requires the artifact to be absent", ids.EvidenceID)
	}

	// Attempt reconciliation via the real Reconciler.
	//
	// The real Reconciler must call store.LoadEvidence(id) for each EvidenceID
	// and return PatchAccepted:false when any artifact is missing.
	rec := reconciler.New(env.st, env.log)

	result, reconcileErr := rec.Reconcile(env.ctx, ids.PatchID)
	if reconcileErr != nil {
		// Infrastructure errors are not a valid ghost-evidence rejection signal.
		// The real Reconciler must return PatchAccepted:false, not an error.
		t.Fatalf("Reconcile returned unexpected error — infrastructure errors are not "+
			"a valid ghost-evidence rejection signal; return "+
			"ReconcileResult{PatchAccepted:false} instead: %v", reconcileErr)
	}

	// A correct Reconciler MUST return PatchAccepted:false when a referenced
	// EvidenceArtifact does not exist in the store.
	if result.PatchAccepted {
		t.Errorf(
			"Reconciler accepted patch %q with ghost evidence — "+
				"obligation %q references EvidenceID %q which is absent from the store.\n"+
				"Invariant (orca.md §11 step 3): Reconciler must verify each "+
				"EvidenceArtifact exists in the store before accepting a patch.\n"+
				"Reconciler must reject patches whose blocking obligations reference absent evidence.",
			ids.PatchID, ids.ObligationID, ids.EvidenceID,
		)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return b
}

func wipeArtifactFiles(t *testing.T, env *integEnv) {
	t.Helper()
	for _, dir := range store.ReplayDir(env.root) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("wipeArtifactFiles: RemoveAll %s: %v", dir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("wipeArtifactFiles: MkdirAll %s: %v", dir, err)
		}
	}
}

func mustReadAllEvents(t *testing.T, env *integEnv) []schema.Event {
	t.Helper()
	events, err := env.log.ReadAfter(env.ctx, 0, 0)
	if err != nil {
		t.Fatalf("mustReadAllEvents: ReadAfter: %v", err)
	}
	return events
}

func mustLoadGoal(t *testing.T, env *integEnv, id string) *schema.GoalIR {
	t.Helper()
	g, err := env.st.LoadGoal(env.ctx, id)
	if err != nil {
		t.Fatalf("LoadGoal(%s): %v", id, err)
	}
	return g
}

func mustLoadObligation(t *testing.T, env *integEnv, id string) *schema.Obligation {
	t.Helper()
	o, err := env.st.LoadObligation(env.ctx, id)
	if err != nil {
		t.Fatalf("LoadObligation(%s): %v", id, err)
	}
	return o
}

func mustLoadCapsule(t *testing.T, env *integEnv, id string) *schema.ExecutionCapsule {
	t.Helper()
	c, err := env.st.LoadCapsule(env.ctx, id)
	if err != nil {
		t.Fatalf("LoadCapsule(%s): %v", id, err)
	}
	return c
}

func mustLoadPatch(t *testing.T, env *integEnv, id string) *schema.PatchArtifact {
	t.Helper()
	p, err := env.st.LoadPatch(env.ctx, id)
	if err != nil {
		t.Fatalf("LoadPatch(%s): %v", id, err)
	}
	return p
}

func assertCapsuleState(t *testing.T, env *integEnv, id string, want schema.CapsuleState) {
	t.Helper()
	c := mustLoadCapsule(t, env, id)
	if c.State != want {
		t.Errorf("capsule %s state = %s, want %s", id, c.State, want)
	}
}

func assertPatchStatus(t *testing.T, env *integEnv, id string, want schema.PatchStatus) {
	t.Helper()
	p := mustLoadPatch(t, env, id)
	if p.Status != want {
		t.Errorf("patch %s status = %s, want %s", id, p.Status, want)
	}
}

// assertJSONEqual marshals both want and got to JSON and compares the strings.
// JSON marshaling is deterministic for the schema types used here because all
// time.Time values are stored in UTC and serialized via encoding/json's RFC3339Nano
// format. A mismatch indicates a field was not correctly preserved during replay.
func assertJSONEqual(t *testing.T, label string, want, got any) {
	t.Helper()
	wantBytes, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("assertJSONEqual %s: marshal want: %v", label, err)
	}
	gotBytes, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("assertJSONEqual %s: marshal got: %v", label, err)
	}
	if string(wantBytes) != string(gotBytes) {
		t.Errorf("%s not identical after replay:\n  want: %s\n   got: %s",
			label, wantBytes, gotBytes)
	}
}

// Verify that the EvidenceArtifact schema used in the no-evidence scenario
// has an empty path from the store (sanity-check the test isolation).
var _ = filepath.Join // import used in wipeArtifactFiles
