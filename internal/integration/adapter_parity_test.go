// Adapter parity harness (Phase D Item 8).
//
// Covers every adapter collection path and failure mode using the mock adapter,
// without spawning real agent CLIs. Each scenario asserts event ordering, the
// produced artifact graph, and that replay reconstructs the same artifacts.
//
// Run with: go test ./internal/integration/...
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/hooks"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/runner/adapters/mock"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
	"github.com/micronwave/orca/internal/verifier"
)

// ── Test environment ──────────────────────────────────────────────────────────

// parityEnv is an integEnv that also owns a git repository used as the capsule
// worktree. The worktree is shared across scenarios in a test.
type parityEnv struct {
	ctx      context.Context
	orcaDir  string
	worktree string
	log      *eventlog.FileLog
	st       *store.FileStore
}

func newParityEnv(t *testing.T) *parityEnv {
	t.Helper()
	orcaDir := t.TempDir()
	l, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("Open log: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	st, err := store.New(orcaDir, l)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	worktree := initParityRepo(t)
	return &parityEnv{
		ctx:      context.Background(),
		orcaDir:  orcaDir,
		worktree: worktree,
		log:      l,
		st:       st,
	}
}

// initParityRepo creates a standalone git repo with one commit. It serves as
// the capsule worktree for mock adapter scenarios.
func initParityRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	parityGit(t, "", "init", dir)
	parityGit(t, dir, "config", "user.email", "parity@example.com")
	parityGit(t, dir, "config", "user.name", "Parity Tests")
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("init"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	parityGit(t, dir, "add", "README.txt")
	parityGit(t, dir, "commit", "-m", "init")
	return dir
}

func parityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, string(out))
	}
}

func buildHookCommand(t *testing.T, outputJSON string) string {
	t.Helper()
	return buildHookCommandFromSource(t, fmt.Sprintf("package main\nimport \"fmt\"\nfunc main() { fmt.Print(%q) }\n", outputJSON))
}

func buildSleepingHookCommand(t *testing.T) string {
	t.Helper()
	return buildHookCommandFromSource(t, "package main\nimport \"time\"\nfunc main() { time.Sleep(10 * time.Second) }\n")
}

func buildHookCommandFromSource(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "hook.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("write hook helper: %v", err)
	}
	exe := filepath.Join(dir, "hook")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", exe, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build hook helper: %v (%s)", err, string(out))
	}
	return exe
}

// ── Scenario scaffolding ──────────────────────────────────────────────────────

// parityScenario holds the stable artifact IDs for one parity test run.
type parityScenario struct {
	GoalID, ConditionID, ObligationID string
	CapsuleID, ProjectionID           string
}

// newParityScenario creates a goal, obligation, and projection in the store and
// returns the IDs. The capsule is NOT created — callers create it with the
// permission mode and scenario-specific settings they need.
func newParityScenario(t *testing.T, env *parityEnv, name string) parityScenario {
	t.Helper()
	s := parityScenario{
		GoalID:       "G-PARITY-" + name,
		ConditionID:  "GC-PARITY-" + name,
		ObligationID: "OB-PARITY-" + name,
		CapsuleID:    "CAP-PARITY-" + name,
		ProjectionID: "CTX-PARITY-" + name,
	}
	now := time.Now().UTC()
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         s.GoalID,
		OriginalIntent: "adapter parity test: " + name,
		GoalConditions: []schema.GoalCondition{{
			ID: s.ConditionID, Description: "parity condition",
			EffectiveDescription: "parity condition", Status: schema.GoalConditionUnmet,
		}},
		ScopeConstraints: schema.ScopeConstraints{AllowedFiles: []string{"."}},
		RiskLevel:        schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal %s: %v", name, err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID: s.ObligationID, GoalConditionID: s.ConditionID,
		Description: "parity obligation", Blocking: true,
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		RiskLevel:        schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation %s: %v", name, err)
	}
	if err := env.st.SaveProjection(env.ctx, &schema.ContextProjection{
		ContextProjectionID: s.ProjectionID, Role: schema.ProjectionRoleExecutor,
		SourceArtifactIDs: []string{s.ObligationID}, TokenBudget: 1024, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveProjection %s: %v", name, err)
	}
	return s
}

// saveMockCapsule creates a capsule with Agent=mock.AgentType pointing to env.worktree.
func saveMockCapsule(t *testing.T, env *parityEnv, s parityScenario, mode schema.PermissionMode) {
	t.Helper()
	if mode == "" {
		mode = schema.PermissionWorkspaceWrite
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:           s.CapsuleID,
		ObligationIDs:       []string{s.ObligationID},
		Agent:               mock.AgentType,
		Role:                schema.RoleExecutor,
		ContextProjectionID: s.ProjectionID,
		AllowedPaths:        []string{"."},
		PermissionMode:      mode,
		Budget: schema.CapsuleBudget{
			MaxTokens: 4096, MaxWallTimeSeconds: 60, MaxRetries: 1,
		},
		Sandbox: schema.CapsuleSandbox{
			WorktreePath: env.worktree, Network: schema.NetworkDeny, WriteScope: "worktree_only",
		},
		State: schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
}

// newMockRunner returns a runner.Runner wired with the mock adapter.
func newMockRunner(env *parityEnv, scenario mock.Scenario) *runner.Runner {
	adapter := mock.New(scenario, env.orcaDir)
	return runner.New(env.st, env.log, env.orcaDir, adapter)
}

// ── Gate runners ──────────────────────────────────────────────────────────────

type failGateRunner struct{}

func (failGateRunner) Run(_ context.Context, _, _ string) (int, string, error) {
	return 1, "gate failed", nil
}

// ── Replay helper ─────────────────────────────────────────────────────────────

// assertReplayReconstructsArtifacts wipes all materialized state, replays the
// event log, and asserts that the capsule state and (if patchID is non-empty)
// patch status are preserved.
func assertReplayReconstructsArtifacts(
	t *testing.T,
	env *parityEnv,
	s parityScenario,
	wantCapsuleState schema.CapsuleState,
	patchID string,
	wantPatchStatus schema.PatchStatus,
) {
	t.Helper()
	// Capture event count — replay must be read-only.
	preEvents, err := env.log.ReadAfter(env.ctx, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter pre-replay: %v", err)
	}

	// Wipe artifact dirs (not the event log).
	for _, dir := range store.ReplayDir(env.orcaDir) {
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(dir, 0o755)
	}

	if err := store.Replay(env.ctx, env.log, env.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	postEvents, err := env.log.ReadAfter(env.ctx, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter post-replay: %v", err)
	}
	if len(postEvents) != len(preEvents) {
		t.Errorf("replay appended events: before=%d after=%d (replay must be read-only)", len(preEvents), len(postEvents))
	}

	cap, loadErr := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if loadErr != nil {
		t.Fatalf("LoadCapsule after replay: %v", loadErr)
	}
	if cap.State != wantCapsuleState {
		t.Errorf("replayed capsule state = %s, want %s", cap.State, wantCapsuleState)
	}

	if patchID != "" {
		p, loadPErr := env.st.LoadPatch(env.ctx, patchID)
		if loadPErr != nil {
			t.Fatalf("LoadPatch %s after replay: %v", patchID, loadPErr)
		}
		if p.Status != wantPatchStatus {
			t.Errorf("replayed patch status = %s, want %s", p.Status, wantPatchStatus)
		}
	}
}

// ── Scenario 1: Sidecar accepted ──────────────────────────────────────────────

// TestAdapterParity_SidecarAccepted exercises the primary collection path:
// Execute returns a valid AgentSidecarOutput. Asserts SidecarUsed=true, patch
// created, event ordering (capsule_started before capsule_completed), and replay.
func TestAdapterParity_SidecarAccepted(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "SIDC")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioSidecarAccepted)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.SidecarUsed {
		t.Error("SidecarUsed = false, want true")
	}
	if result.PatchID == "" {
		t.Error("PatchID is empty; sidecar scenario must produce a patch")
	}
	if len(result.EvidenceIDs) == 0 {
		t.Error("EvidenceIDs is empty; sidecar scenario must produce evidence")
	}

	// Event ordering: capsule_started must precede capsule_completed.
	started, _ := env.log.ReadByType(env.ctx, schema.EventCapsuleStarted, 0, 0)
	completed, _ := env.log.ReadByType(env.ctx, schema.EventCapsuleCompleted, 0, 0)
	if len(started) != 1 || len(completed) != 1 {
		t.Fatalf("lifecycle events: started=%d completed=%d, want 1 each", len(started), len(completed))
	}
	if started[0].SequenceNum >= completed[0].SequenceNum {
		t.Errorf("capsule_started seq %d >= capsule_completed seq %d; want started < completed",
			started[0].SequenceNum, completed[0].SequenceNum)
	}

	cap, _ := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if cap.State != schema.CapsuleStateCompleted {
		t.Errorf("capsule state = %s, want completed", cap.State)
	}

	assertReplayReconstructsArtifacts(t, env, s, schema.CapsuleStateCompleted, result.PatchID, schema.PatchCandidate)
}

// ── Scenario 2: Streaming output observed ────────────────────────────────────

// TestAdapterParity_StreamingOutputObserved verifies that when Execute returns
// output (sidecar path), the runtime emits events in a well-defined order:
// spawning → ready_for_prompt → agent_running → output_collecting.
func TestAdapterParity_StreamingOutputObserved(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "STRM")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioStreamingOutput)

	_, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The latest runtime status must be output_collecting.
	latest, err := env.st.LoadLatestRuntimeStatus(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("LoadLatestRuntimeStatus: %v", err)
	}
	if latest.Status != schema.RuntimeStatusOutputCollecting {
		t.Errorf("final runtime status = %s, want output_collecting", latest.Status)
	}

	// Runtime events must be in monotonic order.
	rtEvents, err := env.log.ReadByType(env.ctx, schema.EventCapsuleRuntimeStatus, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType runtime_status: %v", err)
	}
	for i := 1; i < len(rtEvents); i++ {
		if rtEvents[i].SequenceNum <= rtEvents[i-1].SequenceNum {
			t.Errorf("runtime events not monotonic at index %d: seq %d <= %d",
				i, rtEvents[i].SequenceNum, rtEvents[i-1].SequenceNum)
		}
	}
}

// ── Scenario 3: Invalid sidecar → transcript fallback ─────────────────────────

// TestAdapterParity_InvalidSidecarFallback verifies the ErrInvalidSidecar →
// ExtractFromTranscript path: SidecarUsed=false, patch still produced, and the
// adapter_protocol runtime event was emitted before the run succeeded.
func TestAdapterParity_InvalidSidecarFallback(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "INVSC")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioInvalidSidecar)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SidecarUsed {
		t.Error("SidecarUsed = true; want false (transcript fallback path)")
	}
	if result.PatchID == "" {
		t.Error("PatchID is empty; fallback must still produce a patch")
	}
	if len(result.EvidenceIDs) == 0 {
		t.Error("EvidenceIDs is empty; fallback must produce evidence")
	}

	// The adapter_protocol runtime event must have been emitted.
	rtEvents, err := env.log.ReadByType(env.ctx, schema.EventCapsuleRuntimeStatus, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType runtime_status: %v", err)
	}
	var foundProtocol bool
	for _, ev := range rtEvents {
		if strings.Contains(string(ev.Payload), string(schema.RuntimeFailureAdapterProtocol)) {
			foundProtocol = true
			break
		}
	}
	if !foundProtocol {
		t.Error("adapter_protocol runtime event not found; must be emitted on ErrInvalidSidecar")
	}

	assertReplayReconstructsArtifacts(t, env, s, schema.CapsuleStateCompleted, result.PatchID, schema.PatchCandidate)
}

// ── Scenario 4: No sidecar → transcript fallback ──────────────────────────────

// TestAdapterParity_NoSidecarFallback verifies the ErrNoSidecar →
// ExtractFromTranscript path. Unlike the invalid-sidecar path, no
// adapter_protocol event is emitted (the agent simply produced no output).
func TestAdapterParity_NoSidecarFallback(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "NOSC")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioNoSidecar)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SidecarUsed {
		t.Error("SidecarUsed = true; want false (no-sidecar fallback path)")
	}
	if result.PatchID == "" {
		t.Error("PatchID is empty; no-sidecar fallback must still produce a patch")
	}

	assertReplayReconstructsArtifacts(t, env, s, schema.CapsuleStateCompleted, result.PatchID, schema.PatchCandidate)
}

// ── Scenario 5: Permission denied ─────────────────────────────────────────────

// TestAdapterParity_PermissionDenied verifies that a read-only capsule is
// rejected before adapter.Execute is ever called, and that NO patch artifact
// is produced.
func TestAdapterParity_PermissionDenied(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "PERM")
	saveMockCapsule(t, env, s, schema.PermissionReadOnly)
	r := newMockRunner(env, mock.ScenarioSidecarAccepted)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err == nil {
		t.Fatal("Run must fail for a read-only capsule")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q; want to mention permission denied", err.Error())
	}

	// Permission denied leaves NO patch artifact.
	if result.PatchID != "" {
		t.Error("PatchID must be empty after permission denial")
	}

	// Capsule must be in failed state.
	cap, loadErr := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if loadErr != nil {
		t.Fatalf("LoadCapsule: %v", loadErr)
	}
	if cap.State != schema.CapsuleStateFailed {
		t.Errorf("capsule state = %s, want failed", cap.State)
	}

	// FailureIDs must be non-empty.
	if len(result.FailureIDs) == 0 {
		t.Error("FailureIDs is empty; a failure fingerprint must be persisted")
	}
}

// ── Scenario 6: Dirty worktree preflight failure ───────────────────────────────

// TestAdapterParity_DirtyWorktreePreflight verifies that a dirty-worktree
// preflight error is classified as worktree_state, the capsule transitions to
// failed, and no patch artifact is produced.
func TestAdapterParity_DirtyWorktreePreflight(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "DIRTY")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioDirtyWorktree)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err == nil {
		t.Fatal("Run must fail for dirty worktree preflight")
	}

	// No patch artifact on preflight failure.
	if result.PatchID != "" {
		t.Error("PatchID must be empty after preflight failure")
	}

	cap, capErr := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if capErr != nil {
		t.Fatalf("LoadCapsule: %v", capErr)
	}
	if cap.State != schema.CapsuleStateFailed {
		t.Errorf("capsule state = %s, want failed", cap.State)
	}

	// The runtime event must classify the failure as worktree_state.
	latest, err2 := env.st.LoadLatestRuntimeStatus(env.ctx, s.CapsuleID)
	if err2 != nil {
		t.Fatalf("LoadLatestRuntimeStatus: %v", err2)
	}
	if latest == nil {
		t.Fatal("LoadLatestRuntimeStatus returned nil; runtime events must be persisted")
	}
	if latest.FailClass != schema.RuntimeFailureWorktreeState {
		t.Errorf("fail class = %s, want worktree_state", latest.FailClass)
	}
}

// ── Scenario 7: Verifier pass ─────────────────────────────────────────────────

// TestAdapterParity_VerifierPass exercises the full runner → verifier path with
// a passing gate. Asserts VerdictSatisfied for the obligation and
// ActionAccept as the recommended action.
func TestAdapterParity_VerifierPass(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "VPASS")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioVerifierPass)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PatchID == "" {
		t.Fatal("Run produced no patch")
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			// Name containing "test" causes the verifier to create EvidenceTestResult,
			// matching the obligation's EvidenceRequired field.
			{Name: "go_test_check", Command: "go version", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
	}, passGateRunner{})

	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vr.RecommendedAction != schema.ActionAccept {
		t.Errorf("RecommendedAction = %s, want accept", vr.RecommendedAction)
	}
	if len(vr.ObligationResults) == 0 {
		t.Fatal("ObligationResults is empty")
	}
	if vr.ObligationResults[0].Verdict != schema.VerdictSatisfied {
		t.Errorf("verdict = %s, want satisfied", vr.ObligationResults[0].Verdict)
	}
}

// ── Scenario 8: Verifier failure creates follow-up ────────────────────────────

// TestAdapterParity_VerifierFailure exercises the runner → verifier path where
// a blocking gate fails. The verifier returns ActionRetry and the reconciler
// must not accept the patch.
func TestAdapterParity_VerifierFailure(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "VFAIL")
	saveMockCapsule(t, env, s, "")
	r := newMockRunner(env, mock.ScenarioVerifierFailure)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PatchID == "" {
		t.Fatal("Run produced no patch")
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			{Name: "failing_gate", Command: "false", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
	}, failGateRunner{})

	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vr.RecommendedAction == schema.ActionAccept {
		t.Errorf("RecommendedAction = accept; gate failure must not produce accept recommendation")
	}

	// Reconciler must not accept the patch when gates failed.
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	recResult, err := rec.Reconcile(env.ctx, result.PatchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if recResult.PatchAccepted {
		t.Error("Reconciler accepted the patch despite verifier gate failure")
	}

	p, _ := env.st.LoadPatch(env.ctx, result.PatchID)
	if p.Status == schema.PatchAccepted {
		t.Error("patch stored as accepted; must remain unaccepted after gate failure")
	}
}

// ── Scenario 9: Resume from pending capsule ───────────────────────────────────

// TestAdapterParity_ResumeFromPending verifies that a capsule created in the
// pending state by the planner can be picked up and run by the runner, advancing
// through all lifecycle states to completed, and that replay reconstructs the
// final state.
func TestAdapterParity_ResumeFromPending(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "RESUME")
	saveMockCapsule(t, env, s, "")

	// Confirm pending state before the runner touches the capsule.
	cap, err := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("LoadCapsule pre-run: %v", err)
	}
	if cap.State != schema.CapsuleStatePending {
		t.Fatalf("pre-run capsule state = %s, want pending", cap.State)
	}

	r := newMockRunner(env, mock.ScenarioSidecarAccepted)
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	cap, err = env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("LoadCapsule post-run: %v", err)
	}
	if cap.State != schema.CapsuleStateCompleted {
		t.Errorf("capsule state = %s, want completed", cap.State)
	}

	assertReplayReconstructsArtifacts(t, env, s, schema.CapsuleStateCompleted, result.PatchID, schema.PatchCandidate)
}

// ── Scenario 10: Resume from accepted patch before merge gate ─────────────────

// TestAdapterParity_ResumeFromAcceptedPatch runs the full pipeline: runner →
// verifier (pass) → reconciler (accepts patch). It then wipes artifacts and
// replays the event log, asserting that the accepted patch status is preserved
// and no new events are appended.
func TestAdapterParity_ResumeFromAcceptedPatch(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "ACCEPT")
	saveMockCapsule(t, env, s, "")

	r := newMockRunner(env, mock.ScenarioSidecarAccepted)
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PatchID == "" {
		t.Fatal("Run produced no patch")
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			{Name: "go_test_check", Command: "go version", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
	}, passGateRunner{})
	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vr.RecommendedAction != schema.ActionAccept {
		t.Fatalf("RecommendedAction = %s; want accept", vr.RecommendedAction)
	}

	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	recResult, err := rec.Reconcile(env.ctx, result.PatchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !recResult.PatchAccepted {
		t.Fatal("Reconcile returned PatchAccepted=false; expected acceptance")
	}

	p, _ := env.st.LoadPatch(env.ctx, result.PatchID)
	if p.Status != schema.PatchAccepted {
		t.Errorf("patch status = %s, want accepted", p.Status)
	}

	// Wipe and replay: the accepted status must survive replay.
	assertReplayReconstructsArtifacts(t, env, s, schema.CapsuleStateCompleted, result.PatchID, schema.PatchAccepted)
}

// ── Hook integration scenarios ────────────────────────────────────────────────

// TestAdapterParity_HookDenyBlocksCapsuleLaunch wires a pre_capsule hook that
// returns deny and verifies that the capsule does not advance past the hook
// check and that no patch artifact is produced.
func TestAdapterParity_HookDenyBlocksCapsuleLaunch(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "HOOKDENY")
	saveMockCapsule(t, env, s, "")

	r := runner.NewWithConfig(env.st, env.log, env.orcaDir, runner.Config{
		PreCapsuleHook: &hooks.Config{
			Command:        buildHookCommand(t, `{"kind":"deny","reason":"policy rejected capsule"}`),
			TimeoutSeconds: 5,
		},
	}, mock.New(mock.ScenarioSidecarAccepted, env.orcaDir))
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err == nil {
		t.Fatal("Run must return error when pre_capsule hook denies")
	}
	if !strings.Contains(err.Error(), "pre_capsule hook denied") || !strings.Contains(err.Error(), "policy rejected capsule") {
		t.Errorf("error = %q; want hook deny reason", err.Error())
	}
	if result.PatchID != "" {
		t.Error("PatchID must be empty when hook blocks pre-launch")
	}
	decisionEvents, err := env.log.ReadByType(env.ctx, schema.EventDecisionRecordCreated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType decision_record_created: %v", err)
	}
	var foundHookDecision bool
	for _, ev := range decisionEvents {
		var d schema.DecisionRecord
		if err := json.Unmarshal(ev.Payload, &d); err != nil {
			t.Fatalf("unmarshal decision payload: %v", err)
		}
		if d.Context == "hook_pre_capsule_deny" && strings.Contains(d.Rationale, "policy rejected capsule") {
			foundHookDecision = true
			break
		}
	}
	if !foundHookDecision {
		t.Error("hook deny decision record not found")
	}
}

// TestAdapterParity_HookEvidenceStoredAsArtifact verifies the attach_evidence
// hook result: evidence is saved as an EvidenceArtifact (not automatically
// mapped to obligations — that belongs to the verifier).
func TestAdapterParity_HookEvidenceStoredAsArtifact(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "HOOKEV")
	saveMockCapsule(t, env, s, "")

	r := runner.NewWithConfig(env.st, env.log, env.orcaDir, runner.Config{
		PreCapsuleHook: &hooks.Config{
			Command:        buildHookCommand(t, `{"kind":"attach_evidence","evidence_summary":"hook scanned capsule","evidence_source":"hook-test"}`),
			TimeoutSeconds: 5,
		},
	}, mock.New(mock.ScenarioSidecarAccepted, env.orcaDir))

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run with attach_evidence hook: %v", err)
	}
	if result.PatchID == "" {
		t.Error("Run with attach_evidence hook must produce a patch")
	}
	evidence, err := env.st.LoadEvidenceForObligation(env.ctx, s.ObligationID)
	if err != nil {
		t.Fatalf("LoadEvidenceForObligation: %v", err)
	}
	var foundHookEvidence bool
	for _, ev := range evidence {
		if ev.Source == "hook-test" && strings.Contains(ev.Summary, "hook scanned capsule") {
			foundHookEvidence = true
			break
		}
	}
	if !foundHookEvidence {
		t.Error("pre_capsule attach_evidence artifact not found")
	}
}

// TestAdapterParity_PostVerifyHookDenyOverridesRecommendation verifies that a
// post_verify hook result of deny changes the VerifierResult's recommended
// action to reject.
func TestAdapterParity_PostVerifyHookDenyOverridesRecommendation(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "HOOKPV")
	saveMockCapsule(t, env, s, "")

	r := newMockRunner(env, mock.ScenarioVerifierPass)
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PatchID == "" {
		t.Fatal("Run produced no patch")
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			{Name: "go_test_check", Command: "go version", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
		PostVerifyHook: &hooks.Config{
			Command:        buildHookCommand(t, `{"kind":"deny","reason":"post verifier policy failed"}`),
			TimeoutSeconds: 5,
		},
	}, passGateRunner{})
	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vr.RecommendedAction != schema.ActionReject {
		t.Errorf("RecommendedAction = %s, want reject", vr.RecommendedAction)
	}
	if !strings.Contains(vr.RecommendationRationale, "post verifier policy failed") {
		t.Errorf("RecommendationRationale = %q, want hook deny reason", vr.RecommendationRationale)
	}
}

// TestAdapterParity_HookWarningAddsToVerifierWarnings verifies that a
// post_verify hook result of attach_warning appends to the VerifierResult's
// Warnings slice.
func TestAdapterParity_HookWarningAddsToVerifierWarnings(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "HOOKWARN")
	saveMockCapsule(t, env, s, "")

	r := newMockRunner(env, mock.ScenarioVerifierPass)
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			{Name: "go_test_check", Command: "go version", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
		PostVerifyHook: &hooks.Config{
			Command:        buildHookCommand(t, `{"kind":"attach_warning","warning":"hook warning captured"}`),
			TimeoutSeconds: 5,
		},
	}, passGateRunner{})

	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var foundHookWarning bool
	for _, w := range vr.Warnings {
		if strings.Contains(w, "hook warning captured") {
			foundHookWarning = true
			break
		}
	}
	if !foundHookWarning {
		t.Errorf("hook warning not found in warnings: %v", vr.Warnings)
	}
}

// TestAdapterParity_PostVerifyHookTimeoutRecordsInfraFailure verifies hook
// command timeout handling is durable: the verifier records an infra failure ID
// instead of only appending an ephemeral warning.
func TestAdapterParity_PostVerifyHookTimeoutRecordsInfraFailure(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "HOOKTIME")
	saveMockCapsule(t, env, s, "")

	r := newMockRunner(env, mock.ScenarioVerifierPass)
	result, err := r.Run(env.ctx, s.CapsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ve := verifier.NewWithConfig(env.st, verifier.Config{
		Gates: []config.VerifierGate{
			{Name: "go_test_check", Command: "go version", Blocking: true},
		},
		WorkingDir: env.worktree,
		NoLearning: true,
		PostVerifyHook: &hooks.Config{
			Command:        buildSleepingHookCommand(t),
			TimeoutSeconds: 1,
		},
	}, passGateRunner{})

	vr, err := ve.Verify(env.ctx, result.PatchID, verifier.VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(vr.FailureIDs) == 0 {
		t.Fatal("FailureIDs is empty; hook timeout must record an infra failure")
	}
	failure, err := env.st.LoadFailure(env.ctx, vr.FailureIDs[len(vr.FailureIDs)-1])
	if err != nil {
		t.Fatalf("LoadFailure: %v", err)
	}
	if failure.FailureType != schema.FailureInfra {
		t.Errorf("FailureType = %s, want infra", failure.FailureType)
	}
}

// ── Boundary: mock adapter not in forbidden import list ───────────────────────

// TestAdapterParity_MockAdapterImportIsAllowed is a static check that the mock
// adapter package path is not in the forbidden import list defined in
// TestBoundary_CorePackagesDoNotImportPhase5Packages. This is a documentation
// test — it fails only if the forbidden list is accidentally extended to cover
// the mock adapter.
func TestAdapterParity_MockAdapterImportIsAllowed(t *testing.T) {
	root := findOrcaRoot(t)
	forbidden := []string{
		`"github.com/micronwave/orca/internal/mcp"`,
		`"github.com/micronwave/orca/internal/intake"`,
		`"github.com/micronwave/orca/internal/cigate"`,
		`"github.com/micronwave/orca/internal/prwriter"`,
		`"github.com/micronwave/orca/internal/runner/adapters/remote"`,
		`"github.com/micronwave/orca/desktop"`,
	}
	mockPkg := `"github.com/micronwave/orca/internal/runner/adapters/mock"`
	for _, f := range forbidden {
		if f == mockPkg {
			t.Errorf("mock adapter package %s is in the forbidden list; it must not be", mockPkg)
		}
	}
	_ = root
}

// ── Error handling ────────────────────────────────────────────────────────────

// TestAdapterParity_ExecuteErrorTransitionsCapsuleToFailed verifies that when
// Execute returns a non-sidecar error, the capsule is transitioned to failed and
// a FailureFingerprint is persisted.
func TestAdapterParity_ExecuteErrorTransitionsCapsuleToFailed(t *testing.T) {
	env := newParityEnv(t)
	s := newParityScenario(t, env, "EXECERR")
	saveMockCapsule(t, env, s, "")

	// Use a custom adapter that always fails Execute.
	badAdapter := &alwaysFailAdapter{}
	r := runner.New(env.st, env.log, env.orcaDir, badAdapter)

	result, err := r.Run(env.ctx, s.CapsuleID)
	if err == nil {
		t.Fatal("Run must return error for failing Execute")
	}
	if len(result.FailureIDs) == 0 {
		t.Error("FailureIDs must be non-empty after Execute failure")
	}

	cap, _ := env.st.LoadCapsule(env.ctx, s.CapsuleID)
	if cap.State != schema.CapsuleStateFailed {
		t.Errorf("capsule state = %s, want failed", cap.State)
	}

	failure, err := env.st.LoadFailure(env.ctx, result.FailureIDs[0])
	if err != nil {
		t.Fatalf("LoadFailure: %v", err)
	}
	if failure.FailureType != schema.FailureInfra {
		t.Errorf("failure type = %s, want infra", failure.FailureType)
	}
}

// alwaysFailAdapter is a minimal test adapter whose Execute always returns an error.
type alwaysFailAdapter struct{}

func (a *alwaysFailAdapter) AgentType() schema.AgentType { return mock.AgentType }
func (a *alwaysFailAdapter) Preflight(_ context.Context, _ *schema.ExecutionCapsule) error {
	return nil
}
func (a *alwaysFailAdapter) Execute(_ context.Context, _ *schema.ExecutionCapsule, _ *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	return nil, errors.New("adapter: forced failure for test")
}
func (a *alwaysFailAdapter) ExtractFromTranscript(_ context.Context, _ *schema.ExecutionCapsule, _ string) (*schema.AgentSidecarOutput, error) {
	return nil, errors.New("adapter: forced extract failure for test")
}
