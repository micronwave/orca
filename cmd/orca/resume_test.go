package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── deriveCheckpoint table tests ──────────────────────────────────────────────

func TestDeriveCheckpoint_NoCapsulesYet(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointPlanFromStart {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointPlanFromStart)
	}
	if cp.GoalID != goal.GoalID {
		t.Errorf("GoalID = %q, want %q", cp.GoalID, goal.GoalID)
	}
	if cp.NextStep == "" {
		t.Error("NextStep is empty")
	}
}

func TestDeriveCheckpoint_PendingCapsule(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointRunCapsules {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.CapsuleIDs) != 1 {
		t.Errorf("CapsuleIDs len = %d, want 1", len(cp.CapsuleIDs))
	}
}

func TestDeriveCheckpoint_AbandonedCapsule_NoPatches(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateAgentRunning})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	// Abandoned capsule with no patch: resume should run capsules (but mark abandoned).
	if cp.Kind != CheckpointRunCapsules {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.AbandonedCapsuleIDs) != 1 {
		t.Errorf("AbandonedCapsuleIDs len = %d, want 1", len(cp.AbandonedCapsuleIDs))
	}
	// Abandoned capsule should NOT appear in CapsuleIDs (those are only pending).
	if len(cp.CapsuleIDs) != 0 {
		t.Errorf("CapsuleIDs len = %d, want 0 (abandoned capsules not in pending list)", len(cp.CapsuleIDs))
	}
}

func TestDeriveCheckpoint_PatchExistsNoVerifier(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointVerifyPatches {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointVerifyPatches)
	}
	if len(cp.PatchIDs) != 1 {
		t.Errorf("PatchIDs len = %d, want 1", len(cp.PatchIDs))
	}
}

func TestDeriveCheckpoint_VerifierResultNoReconcile(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointReconcile {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointReconcile)
	}
	if len(cp.PatchIDs) != 1 {
		t.Errorf("PatchIDs len = %d, want 1", len(cp.PatchIDs))
	}
}

func TestDeriveCheckpoint_PatchAcceptedNoMerge(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointMergeGate {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointMergeGate)
	}
	if len(cp.AcceptedPatchIDs) != 1 {
		t.Errorf("AcceptedPatchIDs len = %d, want 1", len(cp.AcceptedPatchIDs))
	}
}

func TestDeriveCheckpoint_MergeApplied_ReturnsFinalizeMerge(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true, withMergeApplied: true})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointFinalizeMerge {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointFinalizeMerge)
	}
	if len(cp.AcceptedPatchIDs) != 1 {
		t.Errorf("AcceptedPatchIDs len = %d, want 1", len(cp.AcceptedPatchIDs))
	}
}

// TestMarkCapsulesAbandoned verifies that marking a capsule abandoned sets its
// state to failed and appends a capsule_state_updated event.
func TestMarkCapsulesAbandoned(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateAgentRunning})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	ctx := context.Background()
	goal := loadResumeGoal(t, rt)

	eventsBefore := readAllEvents(t, orcaDir)

	if err := rt.markCapsulesAbandoned(ctx, goal.GoalID, []string{"CAP-R1"}); err != nil {
		t.Fatalf("markCapsulesAbandoned: %v", err)
	}

	capsule, err := rt.store.LoadCapsule(ctx, "CAP-R1")
	if err != nil {
		t.Fatalf("LoadCapsule after abandon: %v", err)
	}
	if capsule.State != schema.CapsuleStateFailed {
		t.Errorf("capsule state = %q, want %q", capsule.State, schema.CapsuleStateFailed)
	}

	eventsAfter := readAllEvents(t, orcaDir)
	if len(eventsAfter) <= len(eventsBefore) {
		t.Error("expected capsule_state_updated event to be appended")
	}
	last := eventsAfter[len(eventsAfter)-1]
	if last.Type != schema.EventCapsuleStateUpdated {
		t.Errorf("last event type = %q, want capsule_state_updated", last.Type)
	}
}

// TestDeriveCheckpoint_AllCapsulesFailed_FallsBackToPlanFromStart confirms that
// when all capsules are failed with no patches, checkpoint is PlanFromStart.
func TestDeriveCheckpoint_AllCapsulesFailed_FallsBackToPlanFromStart(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateFailed})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointPlanFromStart {
		t.Errorf("Kind = %q, want %q", cp.Kind, CheckpointPlanFromStart)
	}
}

func TestDeriveCheckpoint_MixedAcceptedAndCandidate_CarriesAcceptedWithoutDroppingCandidate(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true})
	addResumePatch(t, orcaDir, "CAP-R2", "PATCH-R2", "VR-R2", schema.PatchCandidate, true, false)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointReconcile {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointReconcile)
	}
	if strings.Join(cp.AcceptedPatchIDs, ",") != "PATCH-R1" {
		t.Fatalf("AcceptedPatchIDs = %v, want [PATCH-R1]", cp.AcceptedPatchIDs)
	}
	if strings.Join(cp.PatchIDs, ",") != "PATCH-R1,PATCH-R2" {
		t.Fatalf("PatchIDs = %v, want accepted plus candidate patches", cp.PatchIDs)
	}
}

func TestDeriveCheckpoint_MixedAcceptedAndUnverified_CarriesAcceptedForFinalApply(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true})
	addResumePatch(t, orcaDir, "CAP-R2", "PATCH-R2", "", schema.PatchCandidate, false, false)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointVerifyPatches {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointVerifyPatches)
	}
	if strings.Join(cp.PatchIDs, ",") != "PATCH-R1,PATCH-R2" {
		t.Fatalf("PatchIDs = %v, want accepted plus unverified patches", cp.PatchIDs)
	}
}

func TestFinalizeAppliedMergeMarksGoalCompleteWithoutDuplicatePR(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true, withMergeApplied: true})
	log, st := openStoreForTest(t, orcaDir)
	ctx := context.Background()
	if err := st.SavePRRecord(ctx, "G-R1", &schema.PRRecord{
		PRID:      "PR-R1",
		GoalID:    "G-R1",
		PatchID:   "PATCH-R1",
		PRURL:     "https://example.test/pr/1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePRRecord: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	prEventsBefore := countResumeEvents(t, orcaDir, schema.EventPRCreated)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	goal := loadResumeGoal(t, rt)
	if err := rt.finalizeAppliedMerge(context.Background(), goal, []string{"PATCH-R1"}); err != nil {
		t.Fatalf("finalizeAppliedMerge: %v", err)
	}
	if got := loadGoalStatus(t, orcaDir, "G-R1"); got != schema.GoalStatusComplete {
		t.Fatalf("goal status = %s, want complete", got)
	}
	if got := countResumeEvents(t, orcaDir, schema.EventPRCreated); got != prEventsBefore {
		t.Fatalf("pr_created events after finalize = %d, want %d", got, prEventsBefore)
	}
}

// TestDedupStrings verifies the dedup helper preserves order and eliminates duplicates.
func TestDedupStrings(t *testing.T) {
	in := []string{"a", "b", "a", "c", "b"}
	got := dedupStrings(in)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupStrings(%v) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupStrings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCheckpoint_NextStepNonEmpty asserts that all checkpoint kinds produce
// non-empty NextStep and LastStep fields for display.
func TestCheckpoint_NextStepNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		opts seedResumeOpts
	}{
		{"no_capsules", seedResumeOpts{}},
		{"pending_capsule", seedResumeOpts{withCapsule: true}},
		{"abandoned_capsule", seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateAgentRunning}},
		{"patch_no_verifier", seedResumeOpts{withCapsule: true, withPatch: true}},
		{"patch_accepted", seedResumeOpts{withCapsule: true, withPatch: true, withVerifier: true, patchAccepted: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orcaDir := seedResumeDir(t, tc.opts)
			rt, closeFn, err := openRuntime(orcaDir, false)
			if err != nil {
				t.Fatalf("openRuntime: %v", err)
			}
			defer closeFn()
			goal := loadResumeGoal(t, rt)
			cp, err := rt.deriveCheckpoint(context.Background(), goal)
			if err != nil {
				t.Fatalf("deriveCheckpoint: %v", err)
			}
			if strings.TrimSpace(cp.LastStep) == "" {
				t.Errorf("LastStep is empty for kind %q", cp.Kind)
			}
			if strings.TrimSpace(cp.NextStep) == "" {
				t.Errorf("NextStep is empty for kind %q", cp.Kind)
			}
		})
	}
}

// ── seedResumeDir helper ──────────────────────────────────────────────────────

type seedResumeOpts struct {
	withCapsule      bool
	capsuleState     schema.CapsuleState // default: CapsuleStatePending
	withPatch        bool
	withVerifier     bool
	patchAccepted    bool
	withMergeApplied bool
}

// seedResumeDir creates a temp .orca dir with an active goal and optionally
// capsule + patch + verifier artifacts for checkpoint derivation tests.
func seedResumeDir(t *testing.T, opts seedResumeOpts) string {
	t.Helper()
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()

	goal := &schema.GoalIR{
		GoalID:         "G-R1",
		OriginalIntent: "fix the resume test defect",
		GoalConditions: []schema.GoalCondition{{
			ID: "GC-R1", Description: "fix it", EffectiveDescription: "fix it",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		Status:    schema.GoalStatusActive,
		CreatedAt: now,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	if !opts.withCapsule {
		if err := log.Close(); err != nil {
			t.Fatalf("close log: %v", err)
		}
		return orcaDir
	}

	obl := &schema.Obligation{
		ObligationID:    "OB-R1",
		GoalConditionID: "GC-R1",
		Description:     "prove resume works",
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}
	if err := st.SaveObligation(ctx, obl); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	dec := &schema.DecisionRecord{
		DecisionID: "DEC-R1",
		Context:    "topology selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "single low-risk",
		MadeBy:     "system",
		RelatedIDs: []string{"G-R1"},
		CreatedAt:  now,
	}
	if err := st.SaveDecision(ctx, dec); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	capsuleState := schema.CapsuleStatePending
	if opts.capsuleState != "" {
		capsuleState = opts.capsuleState
	}
	capsule := &schema.ExecutionCapsule{
		CapsuleID:          "CAP-R1",
		ObligationIDs:      []string{"OB-R1"},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              capsuleState,
		TopologyDecisionID: "DEC-R1",
	}
	if err := st.SaveCapsule(ctx, capsule); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	if !opts.withPatch {
		if err := log.Close(); err != nil {
			t.Fatalf("close log: %v", err)
		}
		return orcaDir
	}

	// Advance capsule to completed via the proper lifecycle; intermediate
	// states are not logged as events but must be visited in strict order.
	if capsuleState != schema.CapsuleStateCompleted {
		capsuleLifecycle := []schema.CapsuleState{
			schema.CapsuleStatePending,
			schema.CapsuleStateWorktreeCreated,
			schema.CapsuleStateWorkspaceAttached,
			schema.CapsuleStateSetupRun,
			schema.CapsuleStateAgentRunning,
			schema.CapsuleStateCompleted,
		}
		advancing := false
		for _, s := range capsuleLifecycle {
			if s == capsuleState {
				advancing = true
				continue
			}
			if !advancing {
				continue
			}
			if err := st.UpdateCapsuleState(ctx, "CAP-R1", s); err != nil {
				t.Fatalf("UpdateCapsuleState to %s: %v", s, err)
			}
			if s == schema.CapsuleStateCompleted {
				break
			}
		}
	}

	patch := &schema.PatchArtifact{
		PatchID:              "PATCH-R1",
		CapsuleID:            "CAP-R1",
		DiffPath:             "inline",
		Summary:              "resume test patch",
		ObligationIDsClaimed: []string{"OB-R1"},
		Status:               schema.PatchCandidate,
	}
	if err := st.SavePatch(ctx, patch); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	if !opts.withVerifier {
		if err := log.Close(); err != nil {
			t.Fatalf("close log: %v", err)
		}
		return orcaDir
	}

	ev := &schema.EvidenceArtifact{
		EvidenceID: "EV-R1",
		Type:       schema.EvidenceTestResult,
		Source:     "go test",
		ExitCode:   0,
		Supports:   []string{"OB-R1"},
		CreatedAt:  now,
	}
	if err := st.SaveEvidence(ctx, ev); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	vr := &schema.VerifierResult{
		VerifierResultID: "VR-R1",
		PatchID:          "PATCH-R1",
		CapsuleID:        "CAP-R1",
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: "OB-R1",
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{"EV-R1"},
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         now,
	}
	if err := st.SaveVerifierResult(ctx, vr); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	if opts.patchAccepted {
		payload := mustJSON(t, schema.PatchStatusPayload{PatchID: "PATCH-R1"})
		if _, err := log.Append(ctx, schema.Event{
			Type:       schema.EventPatchAccepted,
			GoalID:     "G-R1",
			ArtifactID: "PATCH-R1",
			Payload:    payload,
		}); err != nil {
			t.Fatalf("append patch_accepted: %v", err)
		}
		if err := st.UpdatePatchStatus(ctx, "PATCH-R1", schema.PatchAccepted); err != nil {
			t.Fatalf("UpdatePatchStatus: %v", err)
		}
	}

	if opts.withMergeApplied {
		payload := mustJSON(t, schema.PatchStatusPayload{PatchID: "PATCH-R1"})
		if _, err := log.Append(ctx, schema.Event{
			Type:       schema.EventMergeApplied,
			GoalID:     "G-R1",
			ArtifactID: "PATCH-R1",
			Payload:    payload,
		}); err != nil {
			t.Fatalf("append merge_applied: %v", err)
		}
	}

	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return orcaDir
}

func addResumePatch(t *testing.T, orcaDir, capsuleID, patchID, verifierID string, patchStatus schema.PatchStatus, withVerifier, withMergeApplied bool) {
	t.Helper()
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	capsule := &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{"OB-R1"},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: "DEC-R1",
	}
	if err := st.SaveCapsule(ctx, capsule); err != nil {
		t.Fatalf("SaveCapsule %s: %v", capsuleID, err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsuleID,
		DiffPath:             "inline",
		Summary:              "additional resume patch",
		ObligationIDsClaimed: []string{"OB-R1"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch %s: %v", patchID, err)
	}
	if withVerifier {
		evidenceID := "EV-" + patchID
		if err := st.SaveEvidence(ctx, &schema.EvidenceArtifact{
			EvidenceID: evidenceID,
			Type:       schema.EvidenceTestResult,
			Source:     "go test",
			ExitCode:   0,
			Supports:   []string{"OB-R1"},
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("SaveEvidence %s: %v", evidenceID, err)
		}
		if err := st.SaveVerifierResult(ctx, &schema.VerifierResult{
			VerifierResultID: verifierID,
			PatchID:          patchID,
			CapsuleID:        capsuleID,
			ObligationResults: []schema.ObligationVerdict{{
				ObligationID: "OB-R1",
				Verdict:      schema.VerdictSatisfied,
				EvidenceIDs:  []string{evidenceID},
			}},
			RecommendedAction: schema.ActionAccept,
			CreatedAt:         now,
		}); err != nil {
			t.Fatalf("SaveVerifierResult %s: %v", verifierID, err)
		}
	}
	if patchStatus == schema.PatchAccepted {
		payload := mustJSON(t, schema.PatchStatusPayload{PatchID: patchID})
		if _, err := log.Append(ctx, schema.Event{
			Type:       schema.EventPatchAccepted,
			GoalID:     "G-R1",
			ArtifactID: patchID,
			Payload:    payload,
		}); err != nil {
			t.Fatalf("append patch_accepted %s: %v", patchID, err)
		}
		if err := st.UpdatePatchStatus(ctx, patchID, schema.PatchAccepted); err != nil {
			t.Fatalf("UpdatePatchStatus %s: %v", patchID, err)
		}
	}
	if withMergeApplied {
		payload := mustJSON(t, schema.PatchStatusPayload{PatchID: patchID})
		if _, err := log.Append(ctx, schema.Event{
			Type:       schema.EventMergeApplied,
			GoalID:     "G-R1",
			ArtifactID: patchID,
			Payload:    payload,
		}); err != nil {
			t.Fatalf("append merge_applied %s: %v", patchID, err)
		}
	}
}

func countResumeEvents(t *testing.T, orcaDir string, eventType schema.EventType) int {
	t.Helper()
	events := readAllEvents(t, orcaDir)
	var count int
	for _, ev := range events {
		if ev.Type == eventType {
			count++
		}
	}
	return count
}

func loadResumeGoal(t *testing.T, rt *runtime) *schema.GoalIR {
	t.Helper()
	goal, err := rt.store.LoadActiveGoal(context.Background())
	if err != nil {
		t.Fatalf("LoadActiveGoal: %v", err)
	}
	if goal == nil {
		t.Fatal("no active goal")
	}
	return goal
}

// ── deriveCheckpoint runtime-status (FN-7) tests ─────────────────────────────

// TestDeriveCheckpoint_ReadyForPrompt_PendingCapsule_ReturnsReattach confirms
// that a pending capsule whose latest runtime status is ready_for_prompt is
// moved to ReattachCapsuleIDs instead of CapsuleIDs.
func TestDeriveCheckpoint_ReadyForPrompt_PendingCapsule_ReturnsReattach(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true})
	seedRuntimeStatus(t, orcaDir, "CAP-R1", "G-R1", schema.RuntimeStatusReadyForPrompt)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointRunCapsules {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.ReattachCapsuleIDs) != 1 || cp.ReattachCapsuleIDs[0] != "CAP-R1" {
		t.Errorf("ReattachCapsuleIDs = %v, want [CAP-R1]", cp.ReattachCapsuleIDs)
	}
	if len(cp.CapsuleIDs) != 0 {
		t.Errorf("CapsuleIDs = %v, want [] (ready_for_prompt capsule must not appear in CapsuleIDs)", cp.CapsuleIDs)
	}
	if cp.LastStep == "" || cp.NextStep == "" {
		t.Error("LastStep or NextStep is empty")
	}
}

// TestDeriveCheckpoint_ReadyForPrompt_AbandonedCapsule_ReturnsReattach confirms
// that an active (abandoned) capsule at ready_for_prompt is moved to
// ReattachCapsuleIDs and removed from AbandonedCapsuleIDs.
func TestDeriveCheckpoint_ReadyForPrompt_AbandonedCapsule_ReturnsReattach(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateSetupRun})
	seedRuntimeStatus(t, orcaDir, "CAP-R1", "G-R1", schema.RuntimeStatusReadyForPrompt)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointRunCapsules {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.ReattachCapsuleIDs) != 1 || cp.ReattachCapsuleIDs[0] != "CAP-R1" {
		t.Errorf("ReattachCapsuleIDs = %v, want [CAP-R1]", cp.ReattachCapsuleIDs)
	}
	if len(cp.AbandonedCapsuleIDs) != 0 {
		t.Errorf("AbandonedCapsuleIDs = %v, want [] (ready_for_prompt capsule must not be abandoned)", cp.AbandonedCapsuleIDs)
	}
}

// TestDeriveCheckpoint_AgentRunning_AbandonedCapsule_NotInReattach confirms
// that a capsule at agent_running is treated as abandoned (full restart),
// not as a reattach candidate, because the agent may have already begun executing.
func TestDeriveCheckpoint_AgentRunning_AbandonedCapsule_NotInReattach(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true, capsuleState: schema.CapsuleStateAgentRunning})
	seedRuntimeStatus(t, orcaDir, "CAP-R1", "G-R1", schema.RuntimeStatusAgentRunning)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointRunCapsules {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.ReattachCapsuleIDs) != 0 {
		t.Errorf("ReattachCapsuleIDs = %v, want [] (agent_running must not reattach)", cp.ReattachCapsuleIDs)
	}
	if len(cp.AbandonedCapsuleIDs) != 1 {
		t.Errorf("AbandonedCapsuleIDs = %v, want [CAP-R1]", cp.AbandonedCapsuleIDs)
	}
}

// TestDeriveCheckpoint_MissingRuntimeStatus_PendingCapsule_StaysInCapsuleIDs confirms
// that a pending capsule with no runtime status is left in CapsuleIDs (full restart).
func TestDeriveCheckpoint_MissingRuntimeStatus_PendingCapsule_StaysInCapsuleIDs(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true})
	// No runtime event appended — LoadLatestRuntimeStatus returns ErrNotFound.

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(context.Background(), goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointRunCapsules {
		t.Fatalf("Kind = %q, want %q", cp.Kind, CheckpointRunCapsules)
	}
	if len(cp.CapsuleIDs) != 1 {
		t.Errorf("CapsuleIDs = %v, want [CAP-R1]", cp.CapsuleIDs)
	}
	if len(cp.ReattachCapsuleIDs) != 0 {
		t.Errorf("ReattachCapsuleIDs = %v, want [] (no runtime status → full restart)", cp.ReattachCapsuleIDs)
	}
}

// TestDeriveCheckpoint_RuntimeStatusReadError_ReturnsError confirms that
// non-ErrNotFound runtime status read failures are surfaced.
func TestDeriveCheckpoint_RuntimeStatusReadError_ReturnsError(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{withCapsule: true})
	runtimeFile := filepath.Join(orcaDir, "state", "capsule_runtime", "CAP-R1.json")
	if err := os.MkdirAll(filepath.Dir(runtimeFile), 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.WriteFile(runtimeFile, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write corrupt runtime status: %v", err)
	}

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	goal := loadResumeGoal(t, rt)
	_, err = rt.deriveCheckpoint(context.Background(), goal)
	if err == nil {
		t.Fatal("deriveCheckpoint error = nil, want runtime status read error")
	}
	if !strings.Contains(err.Error(), "load runtime status") {
		t.Fatalf("error = %q, want message to mention runtime status load", err.Error())
	}
}

// seedRuntimeStatus appends a CapsuleRuntimeEvent for the given capsule in the
// orcaDir event log and materialises the runtime status file. It is used by
// FN-7 checkpoint tests to simulate a specific pre-crash runtime state.
func seedRuntimeStatus(t *testing.T, orcaDir, capsuleID, goalID string, status schema.CapsuleRuntimeStatus) {
	t.Helper()
	log, st := openStoreForTest(t, orcaDir)
	defer func() {
		if err := log.Close(); err != nil {
			t.Fatalf("close log after seedRuntimeStatus: %v", err)
		}
	}()
	if err := st.AppendRuntimeEvent(context.Background(), &schema.CapsuleRuntimeEvent{
		CapsuleID:  capsuleID,
		GoalID:     goalID,
		Source:     "test",
		Status:     status,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendRuntimeEvent(%s): %v", status, err)
	}
}

// ── hasMergeAppliedEvent tests ────────────────────────────────────────────────

func TestHasMergeAppliedEvent_ReturnsTrueWhenEventExists(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true, withMergeApplied: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	found, err := rt.hasMergeAppliedEvent(context.Background(), "G-R1", "PATCH-R1")
	if err != nil {
		t.Fatalf("hasMergeAppliedEvent: %v", err)
	}
	if !found {
		t.Fatal("expected true for existing merge_applied event, got false")
	}
}

func TestHasMergeAppliedEvent_ReturnsFalseForWrongPatchID(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true, withMergeApplied: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	found, err := rt.hasMergeAppliedEvent(context.Background(), "G-R1", "PATCH-UNKNOWN")
	if err != nil {
		t.Fatalf("hasMergeAppliedEvent: %v", err)
	}
	if found {
		t.Fatal("expected false for non-existent patch ID, got true")
	}
}

func TestHasMergeAppliedEvent_ReturnsFalseWhenNoMergeEvent(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	found, err := rt.hasMergeAppliedEvent(context.Background(), "G-R1", "PATCH-R1")
	if err != nil {
		t.Fatalf("hasMergeAppliedEvent: %v", err)
	}
	if found {
		t.Fatal("expected false when no merge_applied event exists, got true")
	}
}

// ── resumeFromCheckpoint dispatch tests ──────────────────────────────────────

func TestResumeFromCheckpoint_UnknownKind_ReturnsError(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	cp := Checkpoint{Kind: CheckpointKind("totally_unknown"), GoalID: "G-R1"}
	if err := rt.resumeFromCheckpoint(context.Background(), cp); err == nil {
		t.Fatal("expected error for unknown checkpoint kind, got nil")
	}
}

func TestResumeFromCheckpoint_FinalizeMerge_MarksGoalComplete(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true, withMergeApplied: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	ctx := context.Background()
	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(ctx, goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointFinalizeMerge {
		t.Fatalf("expected CheckpointFinalizeMerge, got %s", cp.Kind)
	}

	if err := rt.resumeFromCheckpoint(ctx, cp); err != nil {
		t.Fatalf("resumeFromCheckpoint: %v", err)
	}
	if got := loadGoalStatus(t, orcaDir, "G-R1"); got != schema.GoalStatusComplete {
		t.Fatalf("goal status = %s, want complete", got)
	}
}

func TestResumeFromCheckpoint_MergeGate_RejectedByGate(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	ctx := context.Background()
	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(ctx, goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointMergeGate {
		t.Fatalf("expected CheckpointMergeGate, got %s", cp.Kind)
	}

	rt.gatekeeper = &stubGate{mergeApproved: false, mergeNotes: "not ready"}
	err = rt.resumeFromCheckpoint(ctx, cp)
	if err == nil {
		t.Fatal("expected error when gate rejects, got nil")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error %q does not mention rejection", err.Error())
	}
}

func TestResumeFromCheckpoint_MergeGate_ApprovedByGate_MarksGoalComplete(t *testing.T) {
	orcaDir := seedResumeDir(t, seedResumeOpts{
		withCapsule: true, withPatch: true, withVerifier: true,
		patchAccepted: true,
	})
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	ctx := context.Background()
	goal := loadResumeGoal(t, rt)
	cp, err := rt.deriveCheckpoint(ctx, goal)
	if err != nil {
		t.Fatalf("deriveCheckpoint: %v", err)
	}
	if cp.Kind != CheckpointMergeGate {
		t.Fatalf("expected CheckpointMergeGate, got %s", cp.Kind)
	}

	rt.gatekeeper = &stubGate{mergeApproved: true}
	if err := rt.resumeFromCheckpoint(ctx, cp); err != nil {
		t.Fatalf("resumeFromCheckpoint with approved gate: %v", err)
	}
	if got := loadGoalStatus(t, orcaDir, "G-R1"); got != schema.GoalStatusComplete {
		t.Fatalf("goal status = %s, want complete", got)
	}
	if got := countResumeEvents(t, orcaDir, schema.EventMergeApplied); got == 0 {
		t.Fatal("expected merge_applied event in log after approved gate, got none")
	}
}
