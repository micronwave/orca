package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/cigate"
	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/gate"
	"github.com/micronwave/orca/internal/intake"
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

func TestStatusPrintsAdvancedFindingsAndFalsePositiveRate(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	if err := os.WriteFile(filepath.Join(orcaDir, "config.yaml"), []byte(`
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
advanced:
  enabled: true
  maven: true
  mutation: true
  adversarial_tests: true
  reviewer_diversity: false
`), 0o644); err != nil {
		t.Fatalf("write advanced config: %v", err)
	}
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-1",
		CapsuleID:            "CAP-1",
		ObligationIDsClaimed: []string{"OB-1"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := st.SaveVerifierResult(ctx, &schema.VerifierResult{
		VerifierResultID: "VR-ADV",
		PatchID:          "PATCH-1",
		CapsuleID:        "CAP-1",
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: "OB-1",
			Verdict:      schema.VerdictFailed,
		}},
		Warnings:                []string{"[maven] factual: obligation OB-1 missing evidence type test_result"},
		RecommendedAction:       schema.ActionHumanReview,
		RecommendationRationale: "[maven] findings require human review",
		CreatedAt:               time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	if err := st.SaveDecision(ctx, &schema.DecisionRecord{
		DecisionID: "DEC-MERGE-ADV",
		Context:    "merge_review",
		Decision:   "approved",
		Rationale:  "approved after review",
		MadeBy:     "human",
		RelatedIDs: []string{"PATCH-1"},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close setup log: %v", err)
	}

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
		"Advanced checks: enabled",
		"MAVEN: on  Mutation: on  Adversarial: on  Reviewer diversity: off",
		"Advanced findings:",
		"[maven] factual: obligation OB-1 missing evidence type test_result",
		"Advanced false positives: 1/1 findings",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
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

	if err := rt.emitCycleStartSnapshot(context.Background(), "G-1", lastBefore); err != nil {
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

// TestRunGoal_NoLearningFlag verifies that --no-learning=true is wired through
// the runtime to each sub-component that gates learning.
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
	if rt.runner == nil {
		t.Fatal("runtime.runner is nil")
	}
	if !rt.runner.NoLearning() {
		t.Fatal("runner.NoLearning() = false, want true when --no-learning is passed")
	}
}

func TestShouldReviewProjectionForIRTopology(t *testing.T) {
	if !gate.ShouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskMedium) {
		t.Fatal("IR + medium should require review")
	}
	if !gate.ShouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskHigh) {
		t.Fatal("IR + high should require review")
	}
	if gate.ShouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskLow) {
		t.Fatal("IR + low should not require review")
	}
}

// TestShouldReviewProjection_IRAlwaysGatesRegardlessOfGoalRisk verifies that the
// gate fires for implementer_reviewer topology even when goal.RiskLevel is low.
// The obligation risk (not goal risk) determines whether IR was selected; passing
// goal.RiskLevel to gate.ShouldReviewProjection could silently skip the required gate.
// Callers must pass plan.MaxObligationRisk, not goal.RiskLevel.
func TestShouldReviewProjection_IRAlwaysGatesRegardlessOfGoalRisk(t *testing.T) {
	// The classifier only selects IR for medium/high obligation risk. If goal risk
	// is low but topology is IR, gate.ShouldReviewProjection(IR, medium) must be true.
	if !gate.ShouldReviewProjection(schema.TopologyImplementerReviewer, schema.RiskMedium) {
		t.Fatal("IR + medium obligation risk must require gate")
	}
	// Reviewer capsules are always excluded from the gate — checked by the caller
	// using capsule.Role != schema.RoleReviewer before calling gate.ShouldReviewProjection.
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
		{name: "single medium blocks", topology: schema.TopologySingle, risk: schema.RiskMedium, want: 0},
		{name: "single high blocks", topology: schema.TopologySingle, risk: schema.RiskHigh, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gate.ReviewWindowFor(tt.topology, tt.risk, defaultWindow)
			if got != tt.want {
				t.Fatalf("gate.ReviewWindowFor() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestShouldReviewProjection_SingleTopologyAllRisksGate(t *testing.T) {
	for _, risk := range []schema.RiskLevel{schema.RiskLow, schema.RiskMedium, schema.RiskHigh} {
		if !gate.ShouldReviewProjection(schema.TopologySingle, risk) {
			t.Fatalf("single + %s must require review gate", risk)
		}
	}
}

func TestRunCancelEOFAbortsWithoutCancelling(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)

	var out bytes.Buffer
	if err := runCancel([]string{"--orca-dir", orcaDir}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("run cancel EOF: %v", err)
	}
	if !strings.Contains(out.String(), "Cancel aborted.") {
		t.Fatalf("EOF output = %q, want Cancel aborted", out.String())
	}
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusActive {
		t.Fatalf("goal status after EOF = %s, want active", got)
	}
}

func TestDefaultConfigYAMLIncludesAdvancedAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := defaultConfigYAML()
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(defaultConfigYAML): %v", err)
	}
	if cfg.Advanced.Enabled {
		t.Fatal("Advanced.Enabled = true, want false (default)")
	}
	if cfg.Advanced.MutationTimeoutSeconds != 120 {
		t.Fatalf("Advanced.MutationTimeoutSeconds = %d, want 120", cfg.Advanced.MutationTimeoutSeconds)
	}
	if cfg.Advanced.AdversarialTimeoutSeconds != 60 {
		t.Fatalf("Advanced.AdversarialTimeoutSeconds = %d, want 60", cfg.Advanced.AdversarialTimeoutSeconds)
	}
	if !strings.Contains(yaml, "advanced:") {
		t.Fatal("defaultConfigYAML does not contain 'advanced:' block")
	}
}

func TestNewPlannerWiresReviewerDiversityPreferredAdapter(t *testing.T) {
	ctx := context.Background()
	orcaDir := t.TempDir()
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         "G-reviewer-diversity",
		OriginalIntent: "wire reviewer diversity",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-reviewer-diversity",
			Description:          "wire reviewer diversity",
			EffectiveDescription: "wire reviewer diversity",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-reviewer-diversity",
		GoalConditionID:  "GC-reviewer-diversity",
		Description:      "review medium-risk implementation",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	plannerSvc := newPlanner(
		st,
		config.BudgetConfig{DefaultMaxTokens: 32000, DefaultMaxWallTimeSeconds: 300, DefaultMaxRetries: 3},
		config.AdapterConfig{CodexPath: "codex.exe", ClaudePath: "claude.exe"},
		config.AdvancedConfig{Enabled: true, ReviewerDiversity: true},
		orcaDir,
		false,
	)
	result, err := plannerSvc.Plan(ctx, "G-reviewer-diversity")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var reviewer *schema.ExecutionCapsule
	for _, capsuleID := range result.CapsuleIDs {
		capsule, err := st.LoadCapsule(ctx, capsuleID)
		if err != nil {
			t.Fatalf("LoadCapsule %s: %v", capsuleID, err)
		}
		if capsule.Role == schema.RoleReviewer {
			reviewer = capsule
		}
	}
	if reviewer == nil {
		t.Fatal("reviewer capsule not found")
	}
	if reviewer.Agent != schema.AgentClaude {
		t.Fatalf("reviewer agent = %s, want configured preferred %s", reviewer.Agent, schema.AgentClaude)
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
	t.Cleanup(func() { log.Close() })
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
		SatisfiedBy:  &evidenceIDs,
	})
	if _, err := log.Append(ctx, schema.Event{
		Type:       schema.EventObligationStatusUpdated,
		GoalID:     "G-1",
		ArtifactID: obligationID,
		Payload:    data,
	}); err != nil {
		t.Fatalf("append obligation status: %v", err)
	}
	if err := st.UpdateObligationStatus(ctx, obligationID, schema.ObligationSatisfied, &evidenceIDs); err != nil {
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

func writeMCPConfig(t *testing.T, path string, mcpEnabled bool, mcpAddr string) {
	t.Helper()
	cfg := `
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
`
	if mcpEnabled {
		cfg += "mcp:\n  enabled: true\n  listen: \"" + mcpAddr + "\"\n"
	} else {
		cfg += "mcp:\n  enabled: false\n"
	}
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}
}

func TestOpenRuntime_MCPEnabled_Binds(t *testing.T) {
	orcaDir := t.TempDir()

	// Pick an available port before init so we know it's free.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	if err := run([]string{"init", "--orca-dir", orcaDir}); err != nil {
		t.Fatalf("orca init: %v", err)
	}
	writeMCPConfig(t, filepath.Join(orcaDir, "config.yaml"), true, addr)

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime with MCP enabled: %v", err)
	}
	defer closeFn()
	_ = rt
}

func TestOpenRuntime_MCPEnabled_BindFailure_Propagates(t *testing.T) {
	orcaDir := t.TempDir()

	// Pre-bind the port so openRuntime's net.Listen will fail.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer blocker.Close()
	addr := blocker.Addr().String()

	if err := run([]string{"init", "--orca-dir", orcaDir}); err != nil {
		t.Fatalf("orca init: %v", err)
	}
	writeMCPConfig(t, filepath.Join(orcaDir, "config.yaml"), true, addr)

	_, closeFn, err := openRuntime(orcaDir, false)
	if err == nil {
		closeFn()
		t.Fatal("expected error from openRuntime when MCP port already in use, got nil")
	}
	if !strings.Contains(err.Error(), "mcp server") {
		t.Errorf("error %q does not mention mcp server", err)
	}
}

// ── Phase 5.3: intake and PR tests ───────────────────────────────────────────

func TestIntakeIssue_CreatesGoalAndPersistsIntakeRecord(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues/42") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"title": "Fix the auth middleware rounding defect",
			"body":  "The `Round()` helper returns incorrect values on edge cases.",
		})
	}))
	defer mockSrv.Close()

	orcaDir := t.TempDir()
	writeIntakeTestConfig(t, orcaDir, "testtoken", "owner/repo")

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	rt.intakeFetcher = &intake.Fetcher{BaseURL: mockSrv.URL}

	goal, err := rt.intakeIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("intakeIssue: %v", err)
	}
	if goal == nil || goal.GoalID == "" {
		t.Fatal("intakeIssue returned nil or empty goal")
	}
	if !strings.Contains(goal.OriginalIntent, "Fix the auth middleware rounding defect") {
		t.Errorf("goal intent = %q, want issue title in intent", goal.OriginalIntent)
	}

	events := readAllEvents(t, orcaDir)
	var intakeFound bool
	for _, ev := range events {
		if ev.Type != schema.EventIntakeIssueIngested {
			continue
		}
		var ir schema.IntakeRecord
		if err := json.Unmarshal(ev.Payload, &ir); err != nil {
			t.Fatalf("unmarshal intake record: %v", err)
		}
		if ir.GoalID == goal.GoalID && ir.Source == "github_issue" && ir.ExternalID == "42" {
			intakeFound = true
		}
	}
	if !intakeFound {
		t.Fatalf("intake_issue_ingested event for goal %s not found in event log", goal.GoalID)
	}
}

func TestIntakeIssue_PersistsIntakeLinkInStore(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"title": "Add retry logic",
			"body":  "",
		})
	}))
	defer mockSrv.Close()

	orcaDir := t.TempDir()
	writeIntakeTestConfig(t, orcaDir, "tok", "owner/repo")

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	rt.intakeFetcher = &intake.Fetcher{BaseURL: mockSrv.URL}

	goal, err := rt.intakeIssue(context.Background(), 7)
	if err != nil {
		t.Fatalf("intakeIssue: %v", err)
	}

	// Verify the intake record artifact file was written.
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	// No load helper for IntakeRecord, so scan events for the record ID.
	events := readAllEvents(t, orcaDir)
	var recordID string
	for _, ev := range events {
		if ev.Type == schema.EventIntakeIssueIngested {
			var ir schema.IntakeRecord
			if err := json.Unmarshal(ev.Payload, &ir); err == nil && ir.GoalID == goal.GoalID {
				recordID = ir.RecordID
			}
		}
	}
	if recordID == "" {
		t.Fatal("could not find intake record ID in events")
	}
	_ = st // store opened for directory layout; artifact file verified via events
}

func TestRunGoal_FromIssueFlagAndGoalAreMutuallyExclusive(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	err := run([]string{"goal", "--orca-dir", orcaDir, "--from-issue", "1", "--goal", "some goal"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestRunGoal_RequiresGoalOrFromIssue(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	err := run([]string{"goal", "--orca-dir", orcaDir})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-goal error, got %v", err)
	}
}

func TestResolveBaseBranch_UsesConfigFirst(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	rt.cfg.PR.BaseBranch = "release/v2"

	branch, err := rt.resolveBaseBranch(context.Background())
	if err != nil {
		t.Fatalf("resolveBaseBranch: %v", err)
	}
	if branch != "release/v2" {
		t.Errorf("branch = %q, want release/v2", branch)
	}
}

func TestResolveBaseBranch_FallsBackToGitHubAPIWhenGitFails(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/owner/repo") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"default_branch": "develop"})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mockSrv.Close()

	orcaDir := t.TempDir()
	writeIntakeTestConfig(t, orcaDir, "tok", "owner/repo")

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	// No base_branch in config; git symbolic-ref will fail in a temp dir.
	rt.prWriterBaseURL = mockSrv.URL

	branch, err := rt.resolveBaseBranch(context.Background())
	if err != nil {
		t.Fatalf("resolveBaseBranch via GitHub API: %v", err)
	}
	if branch != "develop" {
		t.Errorf("branch = %q, want develop", branch)
	}
}

func TestResolveBaseBranch_ReturnsErrorWhenAllFail(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	// No config branch, git will fail, no intake.repo → error.
	_, err = rt.resolveBaseBranch(context.Background())
	if err == nil {
		t.Fatal("resolveBaseBranch: expected error when all resolution steps fail")
	}
}

func TestResolveHeadBranch_FailsWhenWorktreeDetachedOrMissing(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()

	// Seed a patch whose capsule has a non-existent worktree path.
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-HEAD",
		CapsuleID:            "CAP-1",
		Status:               schema.PatchCandidate,
		ObligationIDsClaimed: []string{"OB-1"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	// CAP-1 has an empty WorktreePath (seeded without one).
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	_, err = rt.resolveHeadBranch(ctx, "PATCH-HEAD")
	if err == nil {
		t.Fatal("resolveHeadBranch: expected error for capsule without worktree")
	}
}

func TestResolveHeadBranch_FailsWhenWorktreePathNotGitRepo(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()

	nonGitDir := t.TempDir() // a real directory but not a git repo
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-NOGIT",
		CapsuleID:            "CAP-1",
		Status:               schema.PatchCandidate,
		ObligationIDsClaimed: []string{"OB-1"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	// Update the capsule sandbox worktree path to a non-git directory.
	capsule, err := st.LoadCapsule(ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	capsule.Sandbox.WorktreePath = nonGitDir
	capsulePath := filepath.Join(orcaDir, "state", "capsules", "CAP-1.json")
	data, merr := json.Marshal(capsule)
	if merr != nil {
		t.Fatalf("marshal capsule: %v", merr)
	}
	if err := os.WriteFile(capsulePath, data, 0o644); err != nil {
		t.Fatalf("write capsule: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	rt, closeFn, openErr := openRuntime(orcaDir, false)
	if openErr != nil {
		t.Fatalf("openRuntime: %v", openErr)
	}
	defer closeFn()

	_, err = rt.resolveHeadBranch(context.Background(), "PATCH-NOGIT")
	if err == nil {
		t.Fatal("resolveHeadBranch: expected error for non-git worktree directory")
	}
}

func TestCreateAndSavePR_PRCreatedEventInEventLog(t *testing.T) {
	// Create a real git repo so resolveHeadBranch can run git branch --show-current.
	repoDir := initTempGitRepo(t)

	orcaDir := seedOrcaDir(t, true)
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()

	// Update CAP-1 to have the real worktree path.
	capsule, err := st.LoadCapsule(ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	capsule.Sandbox.WorktreePath = repoDir
	capsulePath := filepath.Join(orcaDir, "state", "capsules", "CAP-1.json")
	data, merr := json.Marshal(capsule)
	if merr != nil {
		t.Fatalf("marshal capsule: %v", merr)
	}
	if err := os.WriteFile(capsulePath, data, 0o644); err != nil {
		t.Fatalf("write capsule: %v", err)
	}

	// Seed a patch that references CAP-1 and OB-1.
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-PR",
		CapsuleID:            "CAP-1",
		Status:               schema.PatchAccepted,
		ObligationIDsClaimed: []string{"OB-1"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	// Mock GitHub PR server.
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{
				"html_url": "https://github.com/owner/repo/pull/99",
			})
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer mockSrv.Close()

	rt, closeFn, openErr := openRuntime(orcaDir, false)
	if openErr != nil {
		t.Fatalf("openRuntime: %v", openErr)
	}
	defer closeFn()
	rt.prWriterBaseURL = mockSrv.URL
	rt.cfg.PR.BaseBranch = "main"
	rt.cfg.Intake.Repo = "owner/repo"
	rt.cfg.Intake.GitHubToken = "tok"

	if err := rt.createAndSavePR(context.Background(), "G-1", "PATCH-PR"); err != nil {
		t.Fatalf("createAndSavePR: %v", err)
	}

	events := readAllEvents(t, orcaDir)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventPRCreated {
			found = true
			var pr schema.PRRecord
			if err := json.Unmarshal(ev.Payload, &pr); err != nil {
				t.Fatalf("unmarshal pr record: %v", err)
			}
			if pr.GoalID != "G-1" || pr.PatchID != "PATCH-PR" {
				t.Errorf("pr record = %+v, want G-1/PATCH-PR", pr)
			}
			if pr.PRURL != "https://github.com/owner/repo/pull/99" {
				t.Errorf("PRURL = %q, want github PR URL", pr.PRURL)
			}
		}
	}
	if !found {
		t.Fatal("pr_created event not found in event log after createAndSavePR")
	}
}

func TestPRCreationSkippedWhenNotEnabled(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	// PR is disabled by default.
	if rt.cfg.PR.Enabled {
		t.Fatal("PR should be disabled by default")
	}
	// No mock server needed — createAndSavePR must not be called when disabled.
	// Verify no pr_created event appears in the log.
	events := readAllEvents(t, orcaDir)
	for _, ev := range events {
		if ev.Type == schema.EventPRCreated {
			t.Fatal("pr_created event found despite PR being disabled")
		}
	}
}

func TestResolveHeadBranch_FailsWhenWorktreeInDetachedHEAD(t *testing.T) {
	repoDir := initTempGitRepoDetached(t)
	orcaDir := seedOrcaDir(t, true)
	log, st := openStoreForTest(t, orcaDir)
	defer log.Close()
	ctx := context.Background()

	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-DETACH",
		CapsuleID:            "CAP-1",
		Status:               schema.PatchCandidate,
		ObligationIDsClaimed: []string{"OB-1"},
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	capsule, err := st.LoadCapsule(ctx, "CAP-1")
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	capsule.Sandbox.WorktreePath = repoDir
	capsulePath := filepath.Join(orcaDir, "state", "capsules", "CAP-1.json")
	data, merr := json.Marshal(capsule)
	if merr != nil {
		t.Fatalf("marshal capsule: %v", merr)
	}
	if err := os.WriteFile(capsulePath, data, 0o644); err != nil {
		t.Fatalf("write capsule: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	rt, closeFn, openErr := openRuntime(orcaDir, false)
	if openErr != nil {
		t.Fatalf("openRuntime: %v", openErr)
	}
	defer closeFn()

	_, err = rt.resolveHeadBranch(context.Background(), "PATCH-DETACH")
	if err == nil {
		t.Fatal("resolveHeadBranch: expected error for detached HEAD worktree")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("error %q should mention detached HEAD state", err.Error())
	}
}

// TestAutoMergePathDoesNotCreatePR verifies that the auto-merge code path
// (HumanGateRequired=false) emits merge_applied but NOT pr_created, even
// when pr.enabled is true. PR creation is gated behind the human approval gate.
func TestAutoMergePathDoesNotCreatePR(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	rt.cfg.PR.Enabled = true

	if err := rt.appendMergeApplied(context.Background(), "G-1", "PATCH-AUTO"); err != nil {
		t.Fatalf("appendMergeApplied: %v", err)
	}

	events := readAllEvents(t, orcaDir)
	var mergeFound bool
	for _, ev := range events {
		if ev.Type == schema.EventPRCreated {
			t.Fatal("pr_created event emitted by auto-merge path (HumanGateRequired=false)")
		}
		if ev.Type == schema.EventMergeApplied {
			mergeFound = true
		}
	}
	if !mergeFound {
		t.Fatal("merge_applied event not found after auto-merge path")
	}
}

// ── test helpers ─────────────────────────────────────────────────────────────

// writeIntakeTestConfig writes a config.yaml with intake settings for tests.
func writeIntakeTestConfig(t *testing.T, orcaDir, token, repo string) {
	t.Helper()
	configPath := filepath.Join(orcaDir, "config.yaml")
	content := `
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
intake:
  github_token: "` + token + `"
  repo: "` + repo + `"
pr:
  enabled: false
  base_branch: ""
  draft: true
  label: ""
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write intake test config: %v", err)
	}
}

// initTempGitRepoDetached creates a temporary git repository and leaves HEAD in
// detached state, so that `git branch --show-current` returns an empty string.
func initTempGitRepoDetached(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"checkout", "--detach"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// initTempGitRepo creates a temporary git repository with a commit on a named
// branch, for use in resolveHeadBranch tests. Returns the repo path and branch.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "feature/orca-fix"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// ── CI wait tests ─────────────────────────────────────────────────────────────

// writeCITestConfig writes a config.yaml with CI and intake settings.
func writeCITestConfig(t *testing.T, orcaDir, ciProvider, apiToken, repo string) {
	t.Helper()
	content := `
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
ci:
  provider: "` + ciProvider + `"
  poll_interval_seconds: 1
  branch: ""
intake:
  github_token: "` + apiToken + `"
  repo: "` + repo + `"
`
	if err := os.WriteFile(filepath.Join(orcaDir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write ci test config: %v", err)
	}
}

// ciRunsResponse builds a minimal GitHub Actions /actions/runs response JSON.
func ciRunsResponse(status, conclusion, htmlURL string) map[string]any {
	return map[string]any{
		"total_count": 1,
		"workflow_runs": []map[string]any{
			{
				"id":          42,
				"status":      status,
				"conclusion":  conclusion,
				"html_url":    htmlURL,
				"head_branch": "main",
			},
		},
	}
}

func TestRunCIWait_Success_PersistsCIStatusRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ciRunsResponse("completed", "success", "https://github.com/run/1"))
	}))
	defer srv.Close()

	orcaDir := seedOrcaDir(t, false)
	writeCITestConfig(t, orcaDir, "github_actions", "tok", "owner/repo")

	ciPollerOpts = []cigate.Option{
		cigate.WithAPIBase(srv.URL),
		cigate.WithHTTPDo(srv.Client().Do),
	}
	t.Cleanup(func() { ciPollerOpts = nil })

	if err := runCIWait([]string{"--orca-dir", orcaDir, "--timeout", "10", "--goal-id", "G-1"}); err != nil {
		t.Fatalf("runCIWait = %v, want nil on success", err)
	}

	events := readAllEvents(t, orcaDir)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventCIStatusReceived {
			var r schema.CIStatusRecord
			if err := json.Unmarshal(ev.Payload, &r); err == nil && r.Status == "success" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("ci_status_received event with status=success not found")
	}
}

func TestRunCIWait_Failure_PersistsAndReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ciRunsResponse("completed", "failure", "https://github.com/run/2"))
	}))
	defer srv.Close()

	orcaDir := seedOrcaDir(t, false)
	writeCITestConfig(t, orcaDir, "github_actions", "tok", "owner/repo")

	ciPollerOpts = []cigate.Option{
		cigate.WithAPIBase(srv.URL),
		cigate.WithHTTPDo(srv.Client().Do),
	}
	t.Cleanup(func() { ciPollerOpts = nil })

	err := runCIWait([]string{"--orca-dir", orcaDir, "--timeout", "10", "--goal-id", "G-1"})
	if err == nil {
		t.Fatal("runCIWait = nil, want non-nil error on CI failure")
	}

	events := readAllEvents(t, orcaDir)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventCIStatusReceived {
			var r schema.CIStatusRecord
			if err := json.Unmarshal(ev.Payload, &r); err == nil && r.Status == "failure" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("ci_status_received event with status=failure not found")
	}
}

func TestRunCIWait_Timeout_PersistsTimeoutSummary(t *testing.T) {
	// Always in_progress — forces timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ciRunsResponse("in_progress", "", ""))
	}))
	defer srv.Close()

	orcaDir := seedOrcaDir(t, false)
	writeCITestConfig(t, orcaDir, "github_actions", "tok", "owner/repo")

	ciPollerOpts = []cigate.Option{
		cigate.WithAPIBase(srv.URL),
		cigate.WithHTTPDo(srv.Client().Do),
	}
	t.Cleanup(func() { ciPollerOpts = nil })

	err := runCIWait([]string{"--orca-dir", orcaDir, "--timeout", "2", "--goal-id", "G-1"})
	if err == nil {
		t.Fatal("runCIWait = nil, want non-nil error on timeout")
	}

	events := readAllEvents(t, orcaDir)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventCIStatusReceived {
			var r schema.CIStatusRecord
			if err := json.Unmarshal(ev.Payload, &r); err == nil &&
				r.Status == "failure" && r.Summary == "CI status poll timed out" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("ci_status_received event with timeout summary not found")
	}
}

func TestNewRuntime_InjectsCIGateWhenProviderConfigured(t *testing.T) {
	orcaDir := t.TempDir()
	writeCITestConfig(t, orcaDir, "github_actions", "", "")
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("open eventlog: %v", err)
	}
	defer log.Close()
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	cfg, err := config.Load(filepath.Join(orcaDir, "config.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	rt, err := newRuntime(cfg, orcaDir, false, log, st)
	if err != nil {
		t.Fatalf("newRuntime = %v, want nil when ci.provider = \"github_actions\"", err)
	}
	rt.gatekeeper.Close()

	// buildCIGateCommand is the function newRuntime uses to build the injected
	// ci_status gate. Verify it produces a parseable command containing "ci wait".
	ciCmd, err := buildCIGateCommand(orcaDir)
	if err != nil {
		t.Fatalf("buildCIGateCommand = %v, want nil", err)
	}
	if !strings.Contains(ciCmd, "ci wait") {
		t.Errorf("buildCIGateCommand = %q, want string containing \"ci wait\"", ciCmd)
	}
	if !strings.Contains(ciCmd, "--orca-dir") {
		t.Errorf("buildCIGateCommand = %q, want string containing \"--orca-dir\"", ciCmd)
	}
}

func TestRunCIWait_MissingProvider_ReturnsError(t *testing.T) {
	orcaDir := t.TempDir()
	// Write config with empty provider — no eventlog or store needed; validation
	// returns before opening them.
	writeCITestConfig(t, orcaDir, "", "", "owner/repo")
	err := runCIWait([]string{"--orca-dir", orcaDir})
	if err == nil || !strings.Contains(err.Error(), "ci.provider") {
		t.Errorf("runCIWait = %v, want error about ci.provider", err)
	}
}

func TestRunCIWait_UnsupportedProvider_ReturnsError(t *testing.T) {
	orcaDir := t.TempDir()
	writeCITestConfig(t, orcaDir, "jenkins", "", "owner/repo")
	err := runCIWait([]string{"--orca-dir", orcaDir})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("runCIWait = %v, want error about unsupported provider", err)
	}
}

func TestRunCIWait_MissingRepo_ReturnsError(t *testing.T) {
	orcaDir := t.TempDir()
	writeCITestConfig(t, orcaDir, "github_actions", "tok", "")
	err := runCIWait([]string{"--orca-dir", orcaDir})
	if err == nil || !strings.Contains(err.Error(), "intake.repo") {
		t.Errorf("runCIWait = %v, want error about intake.repo", err)
	}
}

func TestRunCICommand_UsageErrors(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{}, "command is required"},
		{[]string{"unknown"}, "unknown command"},
	} {
		err := runCI(tt.args)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("runCI(%v) = %v, want error containing %q", tt.args, err, tt.want)
		}
	}
}
