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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/failurehistory"
	"github.com/micronwave/orca/internal/intent"
	"github.com/micronwave/orca/internal/planner"
	"github.com/micronwave/orca/internal/projector"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
	"github.com/micronwave/orca/internal/verifier"
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
// It uses real intent/verifier calls for steps 1-2, then direct store/log writes
// for downstream components that are still implemented in later phases.
func buildCompleteScenario(t *testing.T, env *integEnv) scenarioIDs {
	t.Helper()
	ids := scenarioIDs{
		CapsuleID:        "CAP-INT-1",
		PatchID:          "PATCH-INT-1",
		EvidenceID:       "EV-INT-1",
		VerifierResultID: "VR-INT-1",
	}
	ctx := env.ctx

	// Step 1 — intent_compiler: creates GoalIR (store emits goal_created).
	intentCompiler := intent.New(env.st)
	goal, err := intentCompiler.Compile(ctx, "Write integration test coverage for the proof runtime")
	if err != nil {
		t.Fatalf("buildCompleteScenario: intent.Compile: %v", err)
	}
	if len(goal.GoalConditions) == 0 {
		t.Fatal("buildCompleteScenario: intent.Compile produced zero goal conditions")
	}
	ids.GoalID = goal.GoalID
	ids.ConditionID = goal.GoalConditions[0].ID

	// Step 2 — verifier.ProposeObligations: creates initial obligations
	// (store emits obligation_created).
	verifierEngine := verifier.New(env.st, config.VerifierConfig{}, nil)
	if _, err := verifierEngine.ProposeObligations(ctx, ids.GoalID); err != nil {
		t.Fatalf("buildCompleteScenario: verifier.ProposeObligations: %v", err)
	}
	obligations, err := env.st.LoadObligationsForCondition(ctx, ids.ConditionID)
	if err != nil {
		t.Fatalf("buildCompleteScenario: LoadObligationsForCondition: %v", err)
	}
	for _, obligation := range obligations {
		if obligation.Description == "Run all tests and confirm exit code 0" {
			ids.ObligationID = obligation.ObligationID
			break
		}
	}
	if ids.ObligationID == "" {
		t.Fatal("buildCompleteScenario: missing tests obligation created by verifier")
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
	conditionFound := false
	for _, condition := range goal.GoalConditions {
		if condition.ID == ids.ConditionID {
			conditionFound = true
			break
		}
	}
	if !conditionFound {
		t.Errorf("replayed goal conditions = %+v, want condition ID %s present", goal.GoalConditions, ids.ConditionID)
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
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
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
	rec := reconciler.New(env.st, env.log, reconciler.Config{})

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
	rec := reconciler.New(env.st, env.log, reconciler.Config{})

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

// ── Phase 3.6 integration tests ───────────────────────────────────────────────

// passGateRunner implements verifier.GateRunner and always returns exit 0.
// Used in integration tests that exercise the verifier without running real subprocesses.
type passGateRunner struct{}

func (passGateRunner) Run(_ context.Context, _, _ string) (int, string, error) {
	return 0, "pass", nil
}

// TestEvidenceReuse verifies that when the verifier runs a gate twice for the same
// obligation with the same snapshot, the second run creates evidence with ReusedFromID
// pointing to the first run's evidence, and the reconciler budget counts the reuse.
func TestEvidenceReuse(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID = "G-EVREUSE"
		condID = "GC-EVREUSE"
		oblID  = "OB-EVREUSE"
		cap1ID = "CAP-EVREUSE-1"
		patch1 = "PATCH-EVREUSE-1"
		cap2ID = "CAP-EVREUSE-2"
		patch2 = "PATCH-EVREUSE-2"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "evidence reuse integration test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "condition", EffectiveDescription: "condition",
			Status: schema.GoalConditionUnmet,
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
		Description:      "run lint",
		EvidenceRequired: []string{string(schema.EvidenceLintResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// Save a snapshot before the first verifier run so reuseKey includes a snapshot ID.
	if err := env.st.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-EVREUSE", GoalID: goalID,
		EventID: "EVT-EVREUSE", SequenceNum: 1, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// Configure verifier with a single lint gate using a pass runner.
	// WorkingDir is set to env.root so the reuseKey scope is deterministic between runs.
	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates:      []config.VerifierGate{{Name: "go_version", Command: "go version", Blocking: true}},
		WorkingDir: env.root,
		NoLearning: false,
	}, passGateRunner{})

	// First capsule + patch → first verification.
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     cap1ID,
		ObligationIDs: []string{oblID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule 1: %v", err)
	}
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patch1,
		CapsuleID:            cap1ID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch 1: %v", err)
	}

	firstResult, err := ve.Verify(ctx, patch1, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify 1: %v", err)
	}

	// Find the gate evidence (EvidenceLintResult) with a ReuseKey from the first run.
	var firstGateEvID string
	for _, verdict := range firstResult.ObligationResults {
		for _, evID := range verdict.EvidenceIDs {
			ev, loadErr := env.st.LoadEvidence(ctx, evID)
			if loadErr != nil {
				continue
			}
			if ev.Type == schema.EvidenceLintResult && ev.ReuseKey != "" {
				firstGateEvID = evID
			}
		}
	}
	if firstGateEvID == "" {
		t.Fatal("first verifier run produced no lint gate evidence with ReuseKey set")
	}

	// Second capsule + patch for the same obligation.
	// No reconciler runs between the two verifier calls so SNAP-EVREUSE stays latest.
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     cap2ID,
		ObligationIDs: []string{oblID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule 2: %v", err)
	}
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patch2,
		CapsuleID:            cap2ID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch 2: %v", err)
	}

	secondResult, err := ve.Verify(ctx, patch2, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify 2: %v", err)
	}

	// The second run must find the first evidence and create a new one with ReusedFromID.
	var reusedEvID string
	for _, verdict := range secondResult.ObligationResults {
		for _, evID := range verdict.EvidenceIDs {
			ev, loadErr := env.st.LoadEvidence(ctx, evID)
			if loadErr != nil {
				continue
			}
			if ev.ReusedFromID == firstGateEvID {
				reusedEvID = evID
			}
		}
	}
	if reusedEvID == "" {
		t.Fatalf("second verifier run did not produce evidence with ReusedFromID=%s; "+
			"check that the snapshot ID is stable between the two Verify calls", firstGateEvID)
	}

	// Run reconciler on the second patch and verify the budget counts the reuse.
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	if _, err := rec.Reconcile(ctx, patch2); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	records, err := env.st.LoadBudgetForGoal(ctx, goalID)
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	var totalReused int
	for _, r := range records {
		if r.CapsuleID == cap2ID {
			totalReused += r.EvidenceArtifactsReused
		}
	}
	if totalReused == 0 {
		t.Fatal("reconciler budget for second capsule did not count any evidence reuse")
	}
}

// TestFreshnessCheckMarksClaimsStaleBeforePlanning verifies that FreshnessCheck
// marks verified claims stale when accepted patches since their validation touched
// the same files. Phase 3.6 wires this call to the start of each planning iteration.
func TestFreshnessCheckMarksClaimsStaleBeforePlanning(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-FRESH-INTEG"
		condID  = "GC-FRESH-INTEG"
		oblID   = "OB-FRESH-INTEG"
		capsID  = "CAP-FRESH-INTEG"
		patchID = "PATCH-FRESH-INTEG"
		evID    = "EV-FRESH-INTEG"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "freshness test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "tests pass", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID: capsID, ObligationIDs: []string{oblID},
		Agent: schema.AgentCodex, Role: schema.RoleExecutor,
		State: schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID, Type: schema.EvidenceTestResult,
		ExitCode: 0, Supports: []string{oblID}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID: patchID, CapsuleID: capsID,
		ChangedFiles:         []string{"internal/fresh/service.go"},
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// Old snapshot: claim was validated against this.
	if err := env.st.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-FRESH-OLD", GoalID: goalID,
		EventID: "EVT-FRESH-OLD", SequenceNum: 1, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSnapshot old: %v", err)
	}

	// Verified claim touching the same file as the patch.
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-FRESH-INTEG", Text: "claim before patch",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles:        []string{"internal/fresh/service.go"},
		Status:               schema.ClaimVerified,
		EvidenceIDs:          []string{evID},
		LastValidatedAgainst: "SNAP-FRESH-OLD",
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	// Emit a patch_accepted event and a newer snapshot — FreshnessCheck reads these.
	if _, err := env.log.Append(ctx, schema.Event{
		Type: schema.EventPatchAccepted, GoalID: goalID, ArtifactID: patchID,
		Payload: mustMarshal(t, schema.PatchStatusPayload{PatchID: patchID}),
	}); err != nil {
		t.Fatalf("append patch_accepted: %v", err)
	}
	if err := env.st.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-FRESH-CURRENT", GoalID: goalID,
		EventID: "EVT-FRESH-CURRENT", SequenceNum: 100, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSnapshot current: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	if err := rec.FreshnessCheck(ctx, goalID); err != nil {
		t.Fatalf("FreshnessCheck: %v", err)
	}

	claim, err := env.st.LoadClaim(ctx, "CL-FRESH-INTEG")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status != schema.ClaimStale {
		t.Errorf("claim status = %s, want stale; FreshnessCheck must detect patch touched claim's files", claim.Status)
	}
}

// TestRecurringFailureDeduplication verifies that two failure fingerprints with the
// same ErrorSignature produce distinct artifacts, with the second having
// PriorAttemptCount=1 and PriorCapsuleIDs containing the first capsule ID.
func TestRecurringFailureDeduplication(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-FAILDEDUP"
		condID  = "GC-FAILDEDUP"
		oblID   = "OB-FAILDEDUP"
		cap1ID  = "CAP-FAILDEDUP-1"
		cap2ID  = "CAP-FAILDEDUP-2"
		fail1ID = "FAIL-FAILDEDUP-1"
		fail2ID = "FAIL-FAILDEDUP-2"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "failure dedup test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "tests pass", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	for _, capsID := range []string{cap1ID, cap2ID} {
		if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: capsID, ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor,
			State: schema.CapsuleStateCompleted,
		}); err != nil {
			t.Fatalf("SaveCapsule %s: %v", capsID, err)
		}
	}

	const sig = "go test ./...\ntest failed"

	// First failure: no prior history.
	failure1 := &schema.FailureFingerprint{
		FailureID:       fail1ID,
		SourceCapsuleID: cap1ID,
		FailureType:     schema.FailureTest,
		Summary:         "test gate failed",
		ErrorSignature:  sig,
		AffectedFiles:   []string{"internal/foo_test.go"},
	}
	if err := failurehistory.Prepare(ctx, env.st, goalID, failure1, false); err != nil {
		t.Fatalf("Prepare failure1: %v", err)
	}
	if failure1.PriorAttemptCount != 0 {
		t.Errorf("failure1 PriorAttemptCount = %d, want 0 (no prior history)", failure1.PriorAttemptCount)
	}
	if err := env.st.SaveFailure(ctx, failure1); err != nil {
		t.Fatalf("SaveFailure 1: %v", err)
	}

	// Second failure: same signature — should detect the first as prior attempt.
	failure2 := &schema.FailureFingerprint{
		FailureID:       fail2ID,
		SourceCapsuleID: cap2ID,
		FailureType:     schema.FailureTest,
		Summary:         "test gate failed again",
		ErrorSignature:  sig,
		AffectedFiles:   []string{"internal/foo_test.go"},
	}
	if err := failurehistory.Prepare(ctx, env.st, goalID, failure2, false); err != nil {
		t.Fatalf("Prepare failure2: %v", err)
	}
	if failure2.PriorAttemptCount != 1 {
		t.Errorf("failure2 PriorAttemptCount = %d, want 1", failure2.PriorAttemptCount)
	}
	found := false
	for _, id := range failure2.PriorCapsuleIDs {
		if id == cap1ID {
			found = true
		}
	}
	if !found {
		t.Errorf("failure2 PriorCapsuleIDs = %v, want to include %s", failure2.PriorCapsuleIDs, cap1ID)
	}
	if err := env.st.SaveFailure(ctx, failure2); err != nil {
		t.Fatalf("SaveFailure 2: %v", err)
	}

	// Both fingerprints must be distinct artifacts in the store.
	f1, err := env.st.LoadFailure(ctx, fail1ID)
	if err != nil {
		t.Fatalf("LoadFailure 1: %v", err)
	}
	f2, err := env.st.LoadFailure(ctx, fail2ID)
	if err != nil {
		t.Fatalf("LoadFailure 2: %v", err)
	}
	if f1.FailureID == f2.FailureID {
		t.Fatal("both failures have the same ID — they must be distinct artifacts")
	}
	if f2.PriorAttemptCount != 1 {
		t.Errorf("stored failure2 PriorAttemptCount = %d, want 1", f2.PriorAttemptCount)
	}
}

// TestTopologyOutcomeMemory verifies that the planner overrides a single-topology
// decision to implementer_reviewer when historical outcomes show IR acceptance
// rate exceeds single acceptance rate by more than the threshold.
func TestTopologyOutcomeMemory(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID = "G-TOPOUT-MEM-INTEG"
		condID = "GC-TOPOUT-MEM-INTEG"
		oblID  = "OB-TOPOUT-MEM-INTEG"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "topology outcome memory test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description:      "implement feature",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true, RiskLevel: schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// 3 IR successes + 3 single failures for low risk.
	// IR rate (1.0) > single rate (0.0) + threshold (0.15) → hint fires.
	for i := 0; i < 3; i++ {
		if err := env.st.SaveTopologyOutcome(ctx, &schema.TopologyOutcomeRecord{
			OutcomeID:       fmt.Sprintf("TOP-OUT-IR-MEM-%d", i),
			GoalID:          goalID,
			Topology:        schema.TopologyImplementerReviewer,
			MaxRiskLevel:    schema.RiskLow,
			PatchAccepted:   true,
			ObligationCount: 1,
			RecordedAt:      now,
		}); err != nil {
			t.Fatalf("SaveTopologyOutcome IR %d: %v", i, err)
		}
		if err := env.st.SaveTopologyOutcome(ctx, &schema.TopologyOutcomeRecord{
			OutcomeID:       fmt.Sprintf("TOP-OUT-SINGLE-MEM-%d", i),
			GoalID:          goalID,
			Topology:        schema.TopologySingle,
			MaxRiskLevel:    schema.RiskLow,
			PatchAccepted:   false,
			ObligationCount: 1,
			RecordedAt:      now,
		}); err != nil {
			t.Fatalf("SaveTopologyOutcome single %d: %v", i, err)
		}
	}

	p := planner.New(env.st, planner.Config{
		OrcaDir: env.root, ApprovalPolicy: "auto",
		DefaultMaxTokens: 32000, NoLearning: false,
	}, env.st) // env.st implements planner.OutcomeReader

	plan, err := p.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Topology != schema.TopologyImplementerReviewer {
		t.Errorf("Topology = %s, want implementer_reviewer (historical hint must override single -> IR)", plan.Topology)
	}
	decision, err := env.st.LoadDecision(ctx, plan.DecisionID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if !strings.Contains(decision.Rationale, "historical routing") {
		t.Errorf("decision rationale %q does not mention historical routing", decision.Rationale)
	}
}

// TestContestedClaimDetection verifies the full cross-component path:
// explicit Contradicts marks both claims contested via the reconciler,
// the projector labels them [contested] in PreExecutionRisks of the saved
// HumanSummaryProjection, and the gate reads contested risks from the saved
// projection (not by querying claims directly).
func TestContestedClaimDetection(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-CONTEST-INTEG"
		condID  = "GC-CONTEST-INTEG"
		oblID   = "OB-CONTEST-INTEG"
		decID   = "DEC-CONTEST-INTEG"
		capsID  = "CAP-CONTEST-INTEG"
		evID    = "EV-CONTEST-INTEG"
		patchID = "PATCH-CONTEST-INTEG"
		vrID    = "VR-CONTEST-INTEG"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "contested claim detection test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "implement", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: decID, Context: "topology_selection",
		Decision:  string(schema.TopologySingle),
		Rationale: "low risk -> single",
		MadeBy:    "system", RelatedIDs: []string{oblID}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID: capsID, ObligationIDs: []string{oblID},
		Agent: schema.AgentCodex, Role: schema.RoleExecutor,
		State: schema.CapsuleStateCompleted, TopologyDecisionID: decID,
		Budget: schema.CapsuleBudget{MaxTokens: 32000},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID, Type: schema.EvidenceTestResult,
		ExitCode: 0, Supports: []string{oblID}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	// Two verified claims that contradict each other.
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-CONTEST-A", Text: "claim A",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles: []string{"internal/foo.go"},
		Status:        schema.ClaimVerified,
		EvidenceIDs:   []string{evID},
		Contradicts:   []string{"CL-CONTEST-B"},
	}); err != nil {
		t.Fatalf("SaveClaim A: %v", err)
	}
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-CONTEST-B", Text: "claim B",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles: []string{"internal/foo.go"},
		Status:        schema.ClaimVerified,
		EvidenceIDs:   []string{evID},
	}); err != nil {
		t.Fatalf("SaveClaim B: %v", err)
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID: patchID, CapsuleID: capsID,
		ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID, PatchID: patchID, CapsuleID: capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID, Verdict: schema.VerdictSatisfied,
			EvidenceIDs: []string{evID},
		}},
		RecommendedAction: schema.ActionAccept, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	// Reconcile: detectClaimDisputes marks both claims contested.
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	if _, err := rec.Reconcile(ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	clA, err := env.st.LoadClaim(ctx, "CL-CONTEST-A")
	if err != nil {
		t.Fatalf("LoadClaim A: %v", err)
	}
	if clA.Status != schema.ClaimContested {
		t.Fatalf("CL-CONTEST-A status = %s, want contested after reconcile", clA.Status)
	}

	// Projector must label contested claims in PreExecutionRisks of the saved projection.
	proj := projector.New(env.st, config.VerifierConfig{})
	summary, err := proj.CompileHumanSummary(ctx, capsID)
	if err != nil {
		t.Fatalf("CompileHumanSummary: %v", err)
	}

	var hasContestedRisk bool
	for _, risk := range summary.PreExecutionRisks {
		if strings.Contains(risk.Description, "contested claim") && risk.Source == "claim" {
			hasContestedRisk = true
		}
	}
	if !hasContestedRisk {
		t.Errorf("HumanSummaryProjection.PreExecutionRisks does not include any contested-claim entry; "+
			"got %+v", summary.PreExecutionRisks)
	}

	// Gate reads from the saved projection — not directly from claims.
	saved, err := env.st.LoadHumanSummaryProjectionForCapsule(ctx, capsID)
	if err != nil {
		t.Fatalf("LoadHumanSummaryProjectionForCapsule: %v", err)
	}
	var savedHasRisk bool
	for _, risk := range saved.PreExecutionRisks {
		if strings.Contains(risk.Description, "contested claim") && risk.Source == "claim" {
			savedHasRisk = true
		}
	}
	if !savedHasRisk {
		t.Error("saved HumanSummaryProjection does not contain contested-claim risk; gate cannot render it")
	}
}

// TestNoContestedClaimFromFileOverlapAlone verifies that overlapping AffectedFiles
// alone does not cause the reconciler to mark claims contested. Only an explicit
// Contradicts edge triggers contestation.
func TestNoContestedClaimFromFileOverlapAlone(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-NOCONTEST-INTEG"
		condID  = "GC-NOCONTEST-INTEG"
		oblID   = "OB-NOCONTEST-INTEG"
		capsID  = "CAP-NOCONTEST-INTEG"
		evID    = "EV-NOCONTEST-INTEG"
		patchID = "PATCH-NOCONTEST-INTEG"
		vrID    = "VR-NOCONTEST-INTEG"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "no contest from overlap",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "run tests", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID: capsID, ObligationIDs: []string{oblID},
		Agent: schema.AgentCodex, Role: schema.RoleExecutor,
		State: schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID, Type: schema.EvidenceTestResult,
		ExitCode: 0, Supports: []string{oblID}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	// Two verified claims with overlapping AffectedFiles but NO Contradicts edge.
	for _, id := range []string{"CL-NOCONTEST-A", "CL-NOCONTEST-B"} {
		if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
			ClaimID: id, Text: "claim " + id,
			ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
			AffectedFiles: []string{"internal/shared.go"}, // same file
			Status:        schema.ClaimVerified,
			EvidenceIDs:   []string{evID},
			// Contradicts is intentionally empty.
		}); err != nil {
			t.Fatalf("SaveClaim %s: %v", id, err)
		}
	}

	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID: patchID, CapsuleID: capsID,
		ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID, PatchID: patchID, CapsuleID: capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID, Verdict: schema.VerdictSatisfied,
			EvidenceIDs: []string{evID},
		}},
		RecommendedAction: schema.ActionAccept, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	if _, err := rec.Reconcile(ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for _, id := range []string{"CL-NOCONTEST-A", "CL-NOCONTEST-B"} {
		cl, err := env.st.LoadClaim(ctx, id)
		if err != nil {
			t.Fatalf("LoadClaim %s: %v", id, err)
		}
		if cl.Status == schema.ClaimContested {
			t.Errorf("claim %s became contested from file overlap alone; "+
				"only explicit Contradicts edges must trigger contestation", id)
		}
	}
}

// TestNoLearningIsolation verifies that --no-learning disables all adaptive paths:
// evidence reuse, failure-history counting, topology outcome recording, and
// historical routing hints.
func TestNoLearningIsolation(t *testing.T) {
	t.Run("NoEvidenceReuse", func(t *testing.T) {
		env := newIntegEnv(t)
		ctx := env.ctx
		now := time.Now().UTC()

		const goalID, condID, oblID = "G-NOLEARN-EV", "GC-NOLEARN-EV", "OB-NOLEARN-EV"

		env.st.SaveGoal(ctx, &schema.GoalIR{
			GoalID: goalID, OriginalIntent: "no-learning ev",
			GoalConditions: []schema.GoalCondition{{
				ID: condID, Description: "c", EffectiveDescription: "c",
				Status: schema.GoalConditionUnmet,
			}},
			RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
		})
		env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID: oblID, GoalConditionID: condID,
			Description: "run lint", Blocking: true,
			EvidenceRequired: []string{string(schema.EvidenceLintResult)},
			RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
		})
		env.st.SaveSnapshot(ctx, &schema.StateSnapshot{
			SnapshotID: "SNAP-NOLEARN-EV", GoalID: goalID,
			EventID: "EVT-NOLEARN-EV", SequenceNum: 1, CreatedAt: now,
		})

		// First verifier run (NoLearning=false) to create reusable evidence.
		ve1 := verifier.NewWithConfig(env.st, verifier.Config{
			Gates:      []config.VerifierGate{{Name: "go_version", Command: "go version", Blocking: true}},
			WorkingDir: env.root,
			NoLearning: false,
		}, passGateRunner{})
		env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: "CAP-NOLEARN-EV-1", ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor, State: schema.CapsuleStateCompleted,
		})
		env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID: "PATCH-NOLEARN-EV-1", CapsuleID: "CAP-NOLEARN-EV-1",
			ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
		})
		if _, err := ve1.Verify(ctx, "PATCH-NOLEARN-EV-1", verifier.VerifyInput{}); err != nil {
			t.Fatalf("Verify 1: %v", err)
		}

		// Second verifier run with NoLearning=true: must NOT reuse evidence.
		ve2 := verifier.NewWithConfig(env.st, verifier.Config{
			Gates:      []config.VerifierGate{{Name: "go_version", Command: "go version", Blocking: true}},
			WorkingDir: env.root,
			NoLearning: true,
		}, passGateRunner{})
		env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: "CAP-NOLEARN-EV-2", ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor, State: schema.CapsuleStateCompleted,
		})
		env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID: "PATCH-NOLEARN-EV-2", CapsuleID: "CAP-NOLEARN-EV-2",
			ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
		})
		secondResult, err := ve2.Verify(ctx, "PATCH-NOLEARN-EV-2", verifier.VerifyInput{})
		if err != nil {
			t.Fatalf("Verify 2: %v", err)
		}
		for _, verdict := range secondResult.ObligationResults {
			for _, evID := range verdict.EvidenceIDs {
				ev, _ := env.st.LoadEvidence(ctx, evID)
				if ev != nil && ev.ReusedFromID != "" {
					t.Errorf("NoLearning=true: second verifier run produced reused evidence %s (ReusedFromID=%s); expected no reuse",
						evID, ev.ReusedFromID)
				}
			}
		}
	})

	t.Run("NoFailureHistory", func(t *testing.T) {
		env := newIntegEnv(t)
		ctx := env.ctx
		now := time.Now().UTC()

		const goalID, condID, oblID = "G-NOLEARN-FAIL", "GC-NOLEARN-FAIL", "OB-NOLEARN-FAIL"
		env.st.SaveGoal(ctx, &schema.GoalIR{
			GoalID: goalID, OriginalIntent: "no-learning fail",
			GoalConditions: []schema.GoalCondition{{
				ID: condID, Description: "c", EffectiveDescription: "c",
				Status: schema.GoalConditionUnmet,
			}},
			RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
		})
		env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID: oblID, GoalConditionID: condID,
			Description: "tests pass", Blocking: true,
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
		})
		env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: "CAP-NOLEARN-FAIL-1", ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor, State: schema.CapsuleStateCompleted,
		})
		env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: "CAP-NOLEARN-FAIL-2", ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor, State: schema.CapsuleStateCompleted,
		})

		const sig = "test failed"
		fail1 := &schema.FailureFingerprint{
			FailureID: "FAIL-NOLEARN-1", SourceCapsuleID: "CAP-NOLEARN-FAIL-1",
			FailureType: schema.FailureTest, Summary: "fail1", ErrorSignature: sig,
		}
		failurehistory.Prepare(ctx, env.st, goalID, fail1, false)
		env.st.SaveFailure(ctx, fail1)

		// With noLearning=true, Prepare must zero out PriorAttemptCount.
		fail2 := &schema.FailureFingerprint{
			FailureID: "FAIL-NOLEARN-2", SourceCapsuleID: "CAP-NOLEARN-FAIL-2",
			FailureType: schema.FailureTest, Summary: "fail2", ErrorSignature: sig,
		}
		if err := failurehistory.Prepare(ctx, env.st, goalID, fail2, true); err != nil {
			t.Fatalf("Prepare noLearning: %v", err)
		}
		if fail2.PriorAttemptCount != 0 {
			t.Errorf("PriorAttemptCount = %d, want 0 when noLearning=true", fail2.PriorAttemptCount)
		}
		if len(fail2.PriorCapsuleIDs) != 0 {
			t.Errorf("PriorCapsuleIDs = %v, want empty when noLearning=true", fail2.PriorCapsuleIDs)
		}
	})

	t.Run("NoTopologyOutcomeRecording", func(t *testing.T) {
		env := newIntegEnv(t)
		ctx := env.ctx
		now := time.Now().UTC()

		const (
			goalID  = "G-NOLEARN-TOPO"
			condID  = "GC-NOLEARN-TOPO"
			oblID   = "OB-NOLEARN-TOPO"
			decID   = "DEC-NOLEARN-TOPO"
			capsID  = "CAP-NOLEARN-TOPO"
			evID    = "EV-NOLEARN-TOPO"
			patchID = "PATCH-NOLEARN-TOPO"
		)
		env.st.SaveGoal(ctx, &schema.GoalIR{
			GoalID: goalID, OriginalIntent: "no-learning topo",
			GoalConditions: []schema.GoalCondition{{
				ID: condID, Description: "c", EffectiveDescription: "c",
				Status: schema.GoalConditionUnmet,
			}},
			RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
		})
		env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID: oblID, GoalConditionID: condID,
			Description: "run tests", Blocking: true,
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
		})
		env.st.SaveDecision(ctx, &schema.DecisionRecord{
			DecisionID: decID, Context: "topology_selection",
			Decision: string(schema.TopologySingle), Rationale: "low risk",
			MadeBy: "system", RelatedIDs: []string{oblID}, CreatedAt: now,
		})
		env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID: capsID, ObligationIDs: []string{oblID},
			Agent: schema.AgentCodex, Role: schema.RoleExecutor,
			State: schema.CapsuleStateCompleted, TopologyDecisionID: decID,
		})
		env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
			EvidenceID: evID, Type: schema.EvidenceTestResult,
			ExitCode: 0, Supports: []string{oblID}, CreatedAt: now,
		})
		env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID: patchID, CapsuleID: capsID,
			ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
		})
		env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
			VerifierResultID: "VR-NOLEARN-TOPO", PatchID: patchID, CapsuleID: capsID,
			ObligationResults: []schema.ObligationVerdict{{
				ObligationID: oblID, Verdict: schema.VerdictSatisfied,
				EvidenceIDs: []string{evID},
			}},
			RecommendedAction: schema.ActionAccept, CreatedAt: now,
		})

		rec := reconciler.New(env.st, env.log, reconciler.Config{NoLearning: true})
		if _, err := rec.Reconcile(ctx, patchID); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		outcomes, err := env.st.LoadTopologyOutcomesForGoal(ctx, goalID)
		if err != nil {
			t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
		}
		if len(outcomes) != 0 {
			t.Errorf("NoLearning=true: %d topology outcomes recorded, want 0", len(outcomes))
		}
	})

	t.Run("NoRoutingHints", func(t *testing.T) {
		env := newIntegEnv(t)
		ctx := env.ctx
		now := time.Now().UTC()

		const goalID, condID, oblID = "G-NOLEARN-ROUTE", "GC-NOLEARN-ROUTE", "OB-NOLEARN-ROUTE"
		env.st.SaveGoal(ctx, &schema.GoalIR{
			GoalID: goalID, OriginalIntent: "no-learning routing",
			GoalConditions: []schema.GoalCondition{{
				ID: condID, Description: "c", EffectiveDescription: "c",
				Status: schema.GoalConditionUnmet,
			}},
			RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
		})
		env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID: oblID, GoalConditionID: condID,
			Description: "implement", Blocking: true,
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
		})

		// 3 IR successes + 3 single failures — would trigger routing hint if NoLearning=false.
		for i := 0; i < 3; i++ {
			env.st.SaveTopologyOutcome(ctx, &schema.TopologyOutcomeRecord{
				OutcomeID: fmt.Sprintf("TOP-OUT-NOLEARN-IR-%d", i), GoalID: goalID,
				Topology: schema.TopologyImplementerReviewer, MaxRiskLevel: schema.RiskLow,
				PatchAccepted: true, ObligationCount: 1, RecordedAt: now,
			})
			env.st.SaveTopologyOutcome(ctx, &schema.TopologyOutcomeRecord{
				OutcomeID: fmt.Sprintf("TOP-OUT-NOLEARN-SINGLE-%d", i), GoalID: goalID,
				Topology: schema.TopologySingle, MaxRiskLevel: schema.RiskLow,
				PatchAccepted: false, ObligationCount: 1, RecordedAt: now,
			})
		}

		// Planner with NoLearning=true and nil OutcomeReader: must not apply historical hint.
		p := planner.New(env.st, planner.Config{
			OrcaDir: env.root, ApprovalPolicy: "auto",
			DefaultMaxTokens: 32000, NoLearning: true,
		}, nil) // nil OutcomeReader mirrors newPlanner when noLearning=true

		plan, err := p.Plan(ctx, goalID)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		// Low-risk obligation with no special conditions → classifier picks single.
		// Historical hint would override to IR but must be suppressed.
		if plan.Topology == schema.TopologyImplementerReviewer {
			t.Errorf("NoLearning=true: planner selected implementer_reviewer via historical hint; " +
				"historical routing must be disabled when noLearning=true")
		}
	})
}

// TestDeterministicStateReconstruction_Phase3 verifies that all Phase 3 artifact
// fields survive a wipe-and-replay cycle: validation metadata (LastValidatedAgainst),
// dispute edges (ContradictedBy), failure dedup metadata (PriorAttemptCount,
// PriorCapsuleIDs), evidence reuse fields (ReusedFromID), and topology outcomes.
func TestDeterministicStateReconstruction_Phase3(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-PHASE3-REPLAY"
		condID  = "GC-PHASE3-REPLAY"
		oblID   = "OB-PHASE3-REPLAY"
		capsID  = "CAP-PHASE3-REPLAY"
		evID    = "EV-PHASE3-REPLAY"
		evSrcID = "EV-PHASE3-REPLAY-SRC"
		patchID = "PATCH-PHASE3-REPLAY"
		vrID    = "VR-PHASE3-REPLAY"
		failID  = "FAIL-PHASE3-REPLAY"
	)

	// Foundation: goal + obligation + capsule.
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "phase3 replay test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "tests pass", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID: capsID, ObligationIDs: []string{oblID},
		Agent: schema.AgentCodex, Role: schema.RoleExecutor,
		State: schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	// Phase 3 reuse field: evidence with ReusedFromID.
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID, Type: schema.EvidenceTestResult,
		ExitCode: 0, Supports: []string{oblID},
		ReusedFromID:     evSrcID, // Phase 3 reuse field
		ReuseKey:         "rk-phase3",
		ValidatedAgainst: "SNAP-PHASE3",
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	// Phase 3 validation metadata: claim with LastValidatedAgainst.
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-PHASE3-VALID", Text: "validated claim",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles:        []string{"internal/foo.go"},
		Status:               schema.ClaimVerified,
		EvidenceIDs:          []string{evID},
		LastValidatedAgainst: "SNAP-PHASE3", // Phase 3 field
	}); err != nil {
		t.Fatalf("SaveClaim validated: %v", err)
	}

	// Two claims that will gain ContradictedBy via reconciler.detectClaimDisputes.
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-PHASE3-A", Text: "claim A",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles: []string{"internal/bar.go"},
		Status:        schema.ClaimVerified,
		EvidenceIDs:   []string{evID},
		Contradicts:   []string{"CL-PHASE3-B"},
	}); err != nil {
		t.Fatalf("SaveClaim A: %v", err)
	}
	if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID: "CL-PHASE3-B", Text: "claim B",
		ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsID,
		AffectedFiles: []string{"internal/bar.go"},
		Status:        schema.ClaimVerified,
		EvidenceIDs:   []string{evID},
	}); err != nil {
		t.Fatalf("SaveClaim B: %v", err)
	}

	// Phase 3 failure dedup metadata: failure with PriorAttemptCount.
	if err := env.st.SaveFailure(ctx, &schema.FailureFingerprint{
		FailureID:         failID,
		SourceCapsuleID:   capsID,
		FailureType:       schema.FailureTest,
		Summary:           "test failed",
		ErrorSignature:    "test failed",
		PriorAttemptCount: 1,                     // Phase 3 field
		PriorCapsuleIDs:   []string{"CAP-PRIOR"}, // Phase 3 field
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}

	// Phase 3 topology outcome.
	if err := env.st.SaveTopologyOutcome(ctx, &schema.TopologyOutcomeRecord{
		OutcomeID:       "TOP-OUT-PHASE3-REPLAY",
		GoalID:          goalID,
		Topology:        schema.TopologySingle,
		MaxRiskLevel:    schema.RiskLow,
		PatchAccepted:   true,
		ObligationCount: 1,
		RecordedAt:      now,
	}); err != nil {
		t.Fatalf("SaveTopologyOutcome: %v", err)
	}

	// Run reconciler to emit dispute-edge events (ContradictedBy) for CL-PHASE3-A/B.
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID: patchID, CapsuleID: capsID,
		ObligationIDsClaimed: []string{oblID}, Status: schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: vrID, PatchID: patchID, CapsuleID: capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID, Verdict: schema.VerdictSatisfied,
			EvidenceIDs: []string{evID},
		}},
		RecommendedAction: schema.ActionAccept, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	if _, err := rec.Reconcile(ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Record event count before wipe.
	preEvCount := len(mustReadAllEvents(t, env))

	// Helper that validates all Phase 3 fields are correctly set.
	assertPhase3Fields := func(label string) {
		t.Helper()

		ev, err := env.st.LoadEvidence(ctx, evID)
		if err != nil {
			t.Fatalf("%s: LoadEvidence: %v", label, err)
		}
		if ev.ReusedFromID != evSrcID {
			t.Errorf("%s: ReusedFromID = %q, want %s", label, ev.ReusedFromID, evSrcID)
		}

		clV, err := env.st.LoadClaim(ctx, "CL-PHASE3-VALID")
		if err != nil {
			t.Fatalf("%s: LoadClaim validated: %v", label, err)
		}
		if clV.LastValidatedAgainst != "SNAP-PHASE3" {
			t.Errorf("%s: LastValidatedAgainst = %q, want SNAP-PHASE3", label, clV.LastValidatedAgainst)
		}

		clA, err := env.st.LoadClaim(ctx, "CL-PHASE3-A")
		if err != nil {
			t.Fatalf("%s: LoadClaim A: %v", label, err)
		}
		if clA.Status != schema.ClaimContested {
			t.Errorf("%s: CL-PHASE3-A status = %s, want contested", label, clA.Status)
		}
		if len(clA.ContradictedBy) == 0 {
			t.Errorf("%s: CL-PHASE3-A ContradictedBy is empty", label)
		}

		fail, err := env.st.LoadFailure(ctx, failID)
		if err != nil {
			t.Fatalf("%s: LoadFailure: %v", label, err)
		}
		if fail.PriorAttemptCount != 1 {
			t.Errorf("%s: PriorAttemptCount = %d, want 1", label, fail.PriorAttemptCount)
		}
		if len(fail.PriorCapsuleIDs) == 0 {
			t.Errorf("%s: PriorCapsuleIDs is empty", label)
		}

		outcomes, err := env.st.LoadTopologyOutcomesForGoal(ctx, goalID)
		if err != nil {
			t.Fatalf("%s: LoadTopologyOutcomesForGoal: %v", label, err)
		}
		if len(outcomes) == 0 {
			t.Errorf("%s: no topology outcomes after replay", label)
		}
	}

	assertPhase3Fields("before wipe")

	wipeArtifactFiles(t, env)

	if _, err := env.st.LoadClaim(ctx, "CL-PHASE3-A"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("claim must be absent after wipe (test-setup error)")
	}

	if err := store.Replay(ctx, env.log, env.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	postEvCount := len(mustReadAllEvents(t, env))
	if postEvCount != preEvCount {
		t.Errorf("event count changed during replay: before=%d after=%d (Replay must be read-only)",
			preEvCount, postEvCount)
	}

	assertPhase3Fields("after replay")
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
