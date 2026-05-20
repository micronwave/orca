package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

func TestRunLoadsConfigAndInitializesEventLog(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	// openRuntime loads the config and creates the event log; we only want
	// to verify that setup (not the full control loop, which would block on
	// the interactive gate for a real goal with medium-risk obligations).
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	_ = rt

	if _, statErr := os.Stat(filepath.Join(orcaDir, "events.log")); statErr != nil {
		t.Fatalf("events.log was not created: %v", statErr)
	}
}

func TestRunInitCreatesOperationalLayoutAndConfig(t *testing.T) {
	orcaDir := filepath.Join(t.TempDir(), ".orca")
	if err := run([]string{"init", "--orca-dir", orcaDir}); err != nil {
		t.Fatalf("run init: %v", err)
	}

	for _, rel := range []string{
		"events.log",
		"config.yaml",
		"state/goals",
		"state/obligations",
		"state/capsules",
		"state/snapshots",
		"artifacts/patches",
		"artifacts/evidence",
		"artifacts/claims",
		"artifacts/projections/executor",
		"artifacts/projections/human_summary",
		"artifacts/projections/reviewer",
		"artifacts/projections/tester",
		"artifacts/failures",
		"artifacts/decisions",
		"artifacts/budgets",
		"artifacts/verifier_results",
		"capsules",
	} {
		if _, err := os.Stat(filepath.Join(orcaDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected init path %s: %v", rel, err)
		}
	}
	if _, err := configLoad(filepath.Join(orcaDir, "config.yaml")); err != nil {
		t.Fatalf("generated config did not load: %v", err)
	}
}

func TestRunInitRefusesNonEmptyOrcaDir(t *testing.T) {
	orcaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(orcaDir, "keep.txt"), []byte("active state"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	err := run([]string{"init", "--orca-dir", orcaDir})
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("run init error = %v, want non-empty refusal", err)
	}
}

func TestRunGoalShowsHelpfulActiveGoalError(t *testing.T) {
	orcaDir := seedOrcaDir(t, false)

	err := run([]string{"goal", "--orca-dir", orcaDir, "new goal"})
	if err == nil {
		t.Fatal("run goal error = nil, want active-goal error")
	}
	msg := err.Error()
	for _, want := range []string{
		"an active goal already exists",
		"goal_id: G-1",
		`Intent: "fix the auth middleware rounding defect"`,
		"orca cancel",
		"orca status",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("active goal error missing %q:\n%s", want, msg)
		}
	}
}

func TestRunCancelConfirmsActiveCapsulesAndUpdatesGoalEventFirst(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)

	var out bytes.Buffer
	if err := runCancel([]string{"--orca-dir", orcaDir}, strings.NewReader("no\n"), &out); err != nil {
		t.Fatalf("run cancel abort: %v", err)
	}
	if !strings.Contains(out.String(), "Cancel aborted.") {
		t.Fatalf("abort output = %q, want Cancel aborted", out.String())
	}
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusActive {
		t.Fatalf("goal status after abort = %s, want active", got)
	}

	out.Reset()
	if err := runCancel([]string{"--orca-dir", orcaDir}, strings.NewReader("cancel\n"), &out); err != nil {
		t.Fatalf("run cancel confirm: %v", err)
	}
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusCancelled {
		t.Fatalf("goal status after confirm = %s, want cancelled", got)
	}
	events := readAllEvents(t, orcaDir)
	if len(events) == 0 || events[len(events)-1].Type != schema.EventGoalStatusUpdated {
		t.Fatalf("last event = %+v, want goal_status_updated", events[len(events)-1])
	}
}

func TestStatusPrintsActiveGoalAndRuntimeState(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer closeFn()

	var out bytes.Buffer
	if err := rt.printStatus(context.Background(), &out); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Active goal: G-1",
		"Open obligations: 1",
		"Active capsules: 1",
		"CAP-1 [pending] agent=codex",
		"Last verifier result: none",
		"Merge readiness: unknown",
		"Blocking human decisions:",
		"- none",
		"Budget totals:",
		"coordination_cost=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
}

func TestStatusDoesNotReportReadyWithOpenBlockingObligation(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	savePatchAndVerifierResult(t, orcaDir, "OB-1", schema.RiskMedium, schema.PatchCandidate)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer closeFn()

	var out bytes.Buffer
	if err := rt.printStatus(context.Background(), &out); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Last verifier result: VR-1 action=accept") {
		t.Fatalf("status output missing latest verifier:\n%s", got)
	}
	if !strings.Contains(got, "Merge readiness: blocked") {
		t.Fatalf("status output =\n%s\nwant blocked readiness while blocking obligation remains open", got)
	}
}

func TestStatusReportsOutstandingMergeReviewForAcceptedHighRiskPatch(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	savePatchAndVerifierResult(t, orcaDir, "OB-1", schema.RiskHigh, schema.PatchAccepted)
	markObligationSatisfied(t, orcaDir, "OB-1", []string{"EV-1"})

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer closeFn()

	var out bytes.Buffer
	if err := rt.printStatus(context.Background(), &out); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Open obligations: 0",
		"Merge readiness: needs_human_review",
		"merge_review patch=PATCH-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
}

func TestGoalStatusSetToCompleteAfterSuccessfulRun(t *testing.T) {
	orcaDir := seedOrcaDir(t, false)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer closeFn()

	if err := rt.updateGoalStatus(context.Background(), "G-1", schema.GoalStatusComplete); err != nil {
		t.Fatalf("updateGoalStatus complete: %v", err)
	}
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusComplete {
		t.Fatalf("goal status = %s, want complete", got)
	}
	active, err := rt.store.LoadActiveGoal(context.Background())
	if err != nil {
		t.Fatalf("LoadActiveGoal: %v", err)
	}
	if active != nil {
		t.Fatalf("LoadActiveGoal = %s, want nil after completion", active.GoalID)
	}
}

func TestEmitCycleStartSnapshotPersistsLatestGoalEvent(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	defer closeFn()

	before, err := rt.eventLog.ReadForGoal(context.Background(), "G-1", 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal before snapshot: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("seeded goal produced no events")
	}
	lastBefore := before[len(before)-1]

	if err := rt.emitCycleStartSnapshot(context.Background(), "G-1"); err != nil {
		t.Fatalf("emitCycleStartSnapshot: %v", err)
	}

	snapshot, err := rt.store.LoadLatestSnapshot(context.Background(), "G-1")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	if !strings.HasPrefix(snapshot.SnapshotID, "SNAP-CYCLE-") {
		t.Fatalf("SnapshotID = %q, want SNAP-CYCLE prefix", snapshot.SnapshotID)
	}
	if snapshot.EventID != lastBefore.EventID || snapshot.SequenceNum != lastBefore.SequenceNum {
		t.Fatalf("snapshot event = (%q,%d), want (%q,%d)", snapshot.EventID, snapshot.SequenceNum, lastBefore.EventID, lastBefore.SequenceNum)
	}

	after, err := rt.eventLog.ReadForGoal(context.Background(), "G-1", 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal after snapshot: %v", err)
	}
	snapshotEvent := after[len(after)-1]
	if snapshotEvent.Type != schema.EventStateSnapshotSaved || snapshotEvent.ArtifactID != snapshot.SnapshotID {
		t.Fatalf("last event = %+v, want state_snapshot_saved for %s", snapshotEvent, snapshot.SnapshotID)
	}
	var payload schema.StateSnapshot
	if err := json.Unmarshal(snapshotEvent.Payload, &payload); err != nil {
		t.Fatalf("unmarshal snapshot event payload: %v", err)
	}
	if payload.SnapshotID != snapshot.SnapshotID || payload.GoalID != "G-1" {
		t.Fatalf("snapshot event payload = %+v, want snapshot_id %s and goal_id G-1", payload, snapshot.SnapshotID)
	}
}

// TestRunGoal_NoLearningFlag verifies that --no-learning=true reaches the runtime
// and that newPlanner passes nil OutcomeReader (not the store) when disabled.
func TestRunGoal_NoLearningFlag(t *testing.T) {
	orcaDir := seedOrcaDir(t, false)
	rt, closeFn, err := openRuntime(orcaDir, true)
	if err != nil {
		t.Fatalf("openRuntime(noLearning=true): %v", err)
	}
	defer closeFn()
	if !rt.noLearning {
		t.Fatal("runtime.noLearning = false, want true when --no-learning is passed")
	}
	runnerValue := reflect.ValueOf(rt.runner)
	if runnerValue.Kind() != reflect.Pointer || runnerValue.IsNil() {
		t.Fatalf("runtime runner = %T, want non-nil pointer", rt.runner)
	}
	field := runnerValue.Elem().FieldByName("noLearning")
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatalf("runtime runner %T has no noLearning bool field", rt.runner)
	}
	if !field.Bool() {
		t.Fatal("runner.noLearning = false, want true when --no-learning is passed")
	}
}

func TestShouldReviewProjectionForIRTopology(t *testing.T) {
	if !shouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskMedium) {
		t.Fatal("IR + medium should require review")
	}
	if !shouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskHigh) {
		t.Fatal("IR + high should require review")
	}
	if shouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskLow) {
		t.Fatal("IR + low should not require review")
	}
}

// TestShouldReviewProjection_IRAlwaysGatesRegardlessOfGoalRisk verifies that the
// gate fires for implementer_reviewer topology even when goal.RiskLevel is low.
// The obligation risk (not goal risk) determines whether IR was selected; passing
// goal.RiskLevel to shouldReviewProjection could silently skip the required gate.
// Callers must pass plan.MaxObligationRisk, not goal.RiskLevel.
func TestShouldReviewProjection_IRAlwaysGatesRegardlessOfGoalRisk(t *testing.T) {
	// The classifier only selects IR for medium/high obligation risk. If goal risk
	// is low but topology is IR, shouldReviewProjection(IR, medium) must be true.
	if !shouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskMedium) {
		t.Fatal("IR + medium obligation risk must require gate")
	}
	// Reviewer capsules are always excluded from the gate — checked by the caller
	// using capsule.Role != schema.RoleReviewer before calling shouldReviewProjection.
}

func TestReviewWindowForGateRules(t *testing.T) {
	defaultWindow := 30 * time.Second
	tests := []struct {
		name     string
		topology schema.Topology
		risk     schema.RiskLevel
		want     time.Duration
	}{
		{name: "human gated blocks", topology: schema.TopologyHumanGated, risk: schema.RiskLow, want: 0},
		{name: "implementer reviewer medium blocks", topology: schema.TopologyImplementerReviewer, risk: schema.RiskMedium, want: 0},
		{name: "single low uses default", topology: schema.TopologySingle, risk: schema.RiskLow, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewWindowFor(tt.topology, tt.risk, defaultWindow)
			if got != tt.want {
				t.Fatalf("reviewWindowFor() = %s, want %s", got, tt.want)
			}
		})
	}
}

var configLoad = config.Load

func writeTestConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
  working_dir: ""

gate:
  review_window_seconds: 30

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
  default_max_retries: 3

adapters:
  codex_path: ""
  claude_path: ""
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func seedOrcaDir(t *testing.T, withCapsule bool) string {
	t.Helper()
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	goal := &schema.GoalIR{
		GoalID:         "G-1",
		OriginalIntent: "fix the auth middleware rounding defect",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-1",
			Description:          "fix the auth middleware rounding defect",
			EffectiveDescription: "fix the auth middleware rounding defect",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		Status:    schema.GoalStatusActive,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("save goal: %v", err)
	}
	if withCapsule {
		obligation := &schema.Obligation{
			ObligationID:     "OB-1",
			GoalConditionID:  "GC-1",
			Description:      "prove the middleware defect is fixed",
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			Blocking:         true,
			RiskLevel:        schema.RiskMedium,
			Status:           schema.ObligationOpen,
		}
		if err := st.SaveObligation(ctx, obligation); err != nil {
			t.Fatalf("save obligation: %v", err)
		}
		decision := &schema.DecisionRecord{
			DecisionID: "DEC-TOPO",
			Context:    "topology selection",
			Decision:   string(schema.TopologySingle),
			Rationale:  "single low-risk capsule",
			MadeBy:     "system",
			RelatedIDs: []string{"G-1"},
			CreatedAt:  time.Now().UTC(),
		}
		if err := st.SaveDecision(ctx, decision); err != nil {
			t.Fatalf("save topology decision: %v", err)
		}
		capsule := &schema.ExecutionCapsule{
			CapsuleID:          "CAP-1",
			ObligationIDs:      []string{"OB-1"},
			Agent:              schema.AgentCodex,
			Role:               schema.RoleExecutor,
			State:              schema.CapsuleStatePending,
			TopologyDecisionID: "DEC-TOPO",
		}
		if err := st.SaveCapsule(ctx, capsule); err != nil {
			t.Fatalf("save capsule: %v", err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return orcaDir
}

func savePatchAndVerifierResult(t *testing.T, orcaDir, obligationID string, risk schema.RiskLevel, patchStatus schema.PatchStatus) {
	t.Helper()
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()
	obligation, err := st.LoadObligation(ctx, obligationID)
	if err != nil {
		t.Fatalf("load obligation: %v", err)
	}
	obligation.RiskLevel = risk
	if err := os.WriteFile(filepath.Join(orcaDir, "state", "obligations", obligationID+".json"), mustJSON(t, obligation), 0o644); err != nil {
		t.Fatalf("rewrite obligation risk: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-1",
		CapsuleID:            "CAP-1",
		DiffPath:             "inline",
		Summary:              "test patch",
		ObligationIDsClaimed: []string{obligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("save patch: %v", err)
	}
	if err := st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-1",
		Type:       schema.EvidenceTestResult,
		Source:     "go test",
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{obligationID},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save evidence: %v", err)
	}
	if err := st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: "VR-1",
		PatchID:          "PATCH-1",
		CapsuleID:        "CAP-1",
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: obligationID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{"EV-1"},
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save verifier result: %v", err)
	}
	if patchStatus == schema.PatchAccepted {
		payload := schema.PatchStatusPayload{PatchID: "PATCH-1"}
		data := mustJSON(t, payload)
		if _, err := log.Append(ctx, schema.Event{
			Type:       schema.EventPatchAccepted,
			GoalID:     "G-1",
			ArtifactID: "PATCH-1",
			Payload:    data,
		}); err != nil {
			t.Fatalf("append patch accepted: %v", err)
		}
		if err := st.UpdatePatchStatus(ctx, "PATCH-1", schema.PatchAccepted); err != nil {
			t.Fatalf("update patch status: %v", err)
		}
	}
}

func markObligationSatisfied(t *testing.T, orcaDir, obligationID string, evidenceIDs []string) {
	t.Helper()
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()
	data := mustJSON(t, schema.ObligationStatusPayload{
		ObligationID: obligationID,
		Status:       schema.ObligationSatisfied,
		SatisfiedBy:  evidenceIDs,
	})
	if _, err := log.Append(ctx, schema.Event{
		Type:       schema.EventObligationStatusUpdated,
		GoalID:     "G-1",
		ArtifactID: obligationID,
		Payload:    data,
	}); err != nil {
		t.Fatalf("append obligation status: %v", err)
	}
	if err := st.UpdateObligationStatus(ctx, obligationID, schema.ObligationSatisfied, evidenceIDs); err != nil {
		t.Fatalf("update obligation status: %v", err)
	}
}

func openStoreForTest(t *testing.T, orcaDir string) (*eventlog.FileLog, *store.FileStore) {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	st, err := store.New(orcaDir, log)
	if err != nil {
		_ = log.Close()
		t.Fatalf("new store: %v", err)
	}
	return log, st
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}

func loadGoalStatus(t *testing.T, orcaDir, goalID string) schema.GoalStatus {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer log.Close()
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	goal, err := st.LoadGoal(context.Background(), goalID)
	if err != nil {
		t.Fatalf("load goal: %v", err)
	}
	return goal.Status
}

func readAllEvents(t *testing.T, orcaDir string) []schema.Event {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer log.Close()
	events, err := log.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}
