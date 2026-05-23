package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type runnerEnv struct {
	ctx      context.Context
	orcaDir  string
	worktree string
	log      *eventlog.FileLog
	st       *store.FileStore
}

func newRunnerEnv(t *testing.T) *runnerEnv {
	t.Helper()
	orcaDir := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "repo")
	initGitRepo(t, worktree)

	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(orcaDir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &runnerEnv{
		ctx:      context.Background(),
		orcaDir:  orcaDir,
		worktree: worktree,
		log:      log,
		st:       st,
	}
}

func TestRunUsesSidecarPath(t *testing.T) {
	env := newRunnerEnv(t)
	capsuleID := saveRunnerScenario(t, env)
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			if err := os.WriteFile(filepath.Join(capsule.Sandbox.WorktreePath, "runner_sidecar.txt"), []byte("updated"), 0o644); err != nil {
				t.Fatalf("write file in execute: %v", err)
			}
			runGit(t, capsule.Sandbox.WorktreePath, "add", "runner_sidecar.txt")
			return &schema.AgentSidecarOutput{
				ObligationsAddressed: []string{"OB-1"},
				FilesChanged:         []string{"runner_sidecar.txt"},
				CommandsRun:          []string{"go test ./..."},
				Claims: []schema.SidecarClaim{
					{Claim: "tests pass", Type: schema.SidecarClaimVerified},
				},
				Risks:           []string{"none"},
				FollowUpNeeded:  nil,
				EvidencePaths:   []string{orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID)},
				Summary:         "runner updated one file",
				TokensUsed:      123,
				WallTimeSeconds: 4.5,
			}, nil
		},
	}
	r := New(env.st, env.log, env.orcaDir, adapter)
	result, err := r.Run(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.SidecarUsed {
		t.Fatal("SidecarUsed = false, want true")
	}
	if result.PatchID == "" || len(result.EvidenceIDs) == 0 {
		t.Fatalf("RunResult = %+v", result)
	}
	if result.TokensUsed != 123 || result.WallTimeSeconds != 4.5 {
		t.Fatalf("RunResult cost metadata tokens=%d wall=%.2f, want 123 and 4.50", result.TokensUsed, result.WallTimeSeconds)
	}
	patch, err := env.st.LoadPatch(env.ctx, result.PatchID)
	if err != nil {
		t.Fatalf("LoadPatch: %v", err)
	}
	if patch.TokensUsed != 123 || patch.WallTimeSeconds != 4.5 {
		t.Fatalf("patch cost metadata tokens=%d wall=%.2f, want 123 and 4.50", patch.TokensUsed, patch.WallTimeSeconds)
	}
	capsule, err := env.st.LoadCapsule(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.State != schema.CapsuleStateCompleted {
		t.Fatalf("capsule state = %s, want %s", capsule.State, schema.CapsuleStateCompleted)
	}
	started, err := env.log.ReadByType(env.ctx, schema.EventCapsuleStarted, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType capsule_started: %v", err)
	}
	completed, err := env.log.ReadByType(env.ctx, schema.EventCapsuleCompleted, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType capsule_completed: %v", err)
	}
	if len(started) != 1 || len(completed) != 1 {
		t.Fatalf("capsule lifecycle events started=%d completed=%d, want 1 each", len(started), len(completed))
	}
}

func TestRunFillsWallTimeWhenAdapterReturnsZero(t *testing.T) {
	env := newRunnerEnv(t)
	capsuleID := saveRunnerScenario(t, env)
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			if err := os.WriteFile(filepath.Join(capsule.Sandbox.WorktreePath, "runner_walltime.txt"), []byte("updated"), 0o644); err != nil {
				t.Fatalf("write file in execute: %v", err)
			}
			runGit(t, capsule.Sandbox.WorktreePath, "add", "runner_walltime.txt")
			time.Sleep(time.Millisecond) // ensure measurable duration for the fallback timer
			return &schema.AgentSidecarOutput{
				ObligationsAddressed: []string{"OB-1"},
				FilesChanged:         []string{"runner_walltime.txt"},
				CommandsRun:          []string{"go test ./..."},
				Claims:               []schema.SidecarClaim{{Claim: "ok", Type: schema.SidecarClaimVerified}},
				Risks:                []string{"none"},
				EvidencePaths:        []string{orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID)},
				Summary:              "runner wall time fallback test",
				TokensUsed:           0,
				WallTimeSeconds:      0, // adapter does not report wall time; runAdapter must fill it
			}, nil
		},
	}
	before := time.Now()
	result, err := New(env.st, env.log, env.orcaDir, adapter).Run(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(before).Seconds()
	if result.WallTimeSeconds <= 0 || result.WallTimeSeconds > elapsed+1 {
		t.Fatalf("RunResult.WallTimeSeconds = %.6f, want > 0 and <= %.6f", result.WallTimeSeconds, elapsed+1)
	}
	patch, err := env.st.LoadPatch(env.ctx, result.PatchID)
	if err != nil {
		t.Fatalf("LoadPatch: %v", err)
	}
	if patch.WallTimeSeconds <= 0 {
		t.Fatalf("patch.WallTimeSeconds = %.6f, want > 0 (fallback should propagate)", patch.WallTimeSeconds)
	}
}

func TestRunPreservesClaimDisputeEdgesAndLeavesVerificationToReconciler(t *testing.T) {
	env := newRunnerEnv(t)
	capsuleID := saveRunnerScenario(t, env)
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			if err := os.WriteFile(filepath.Join(capsule.Sandbox.WorktreePath, "runner_claims.txt"), []byte("updated"), 0o644); err != nil {
				t.Fatalf("write file in execute: %v", err)
			}
			runGit(t, capsule.Sandbox.WorktreePath, "add", "runner_claims.txt")
			return &schema.AgentSidecarOutput{
				ObligationsAddressed: []string{"OB-1"},
				FilesChanged:         []string{"runner_claims.txt"},
				CommandsRun:          []string{"go test ./..."},
				Claims: []schema.SidecarClaim{{
					Claim:       "new evidence supersedes old invariant",
					Type:        schema.SidecarClaimVerified,
					Contradicts: []string{"CLM-old"},
					Invalidates: []string{"CLM-stale"},
				}},
				EvidencePaths: []string{orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID)},
				Summary:       "runner updated claims",
			}, nil
		},
	}
	result, err := New(env.st, env.log, env.orcaDir, adapter).Run(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.ClaimIDs) != 1 {
		t.Fatalf("ClaimIDs = %v, want one claim", result.ClaimIDs)
	}
	claim, err := env.st.LoadClaim(env.ctx, result.ClaimIDs[0])
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status != schema.ClaimProposed {
		t.Fatalf("claim status = %s, want proposed until reconciliation validation", claim.Status)
	}
	if len(claim.EvidenceIDs) != 1 || claim.EvidenceIDs[0] != result.EvidenceIDs[0] {
		t.Fatalf("EvidenceIDs = %v, want first runner evidence %s", claim.EvidenceIDs, result.EvidenceIDs[0])
	}
	if len(claim.Contradicts) != 1 || claim.Contradicts[0] != "CLM-old" {
		t.Fatalf("Contradicts = %v", claim.Contradicts)
	}
	if len(claim.Invalidates) != 1 || claim.Invalidates[0] != "CLM-stale" {
		t.Fatalf("Invalidates = %v", claim.Invalidates)
	}
}

func TestRunFallsBackToTranscript(t *testing.T) {
	env := newRunnerEnv(t)
	capsuleID := saveRunnerScenario(t, env)
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			path := orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll transcript dir: %v", err)
			}
			if err := os.WriteFile(path, []byte("fallback transcript"), 0o644); err != nil {
				t.Fatalf("WriteFile transcript: %v", err)
			}
			return nil, ErrNoSidecar
		},
		extractFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
			if transcriptPath != orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID) {
				t.Fatalf("transcriptPath = %q, want %q", transcriptPath, orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID))
			}
			if err := os.WriteFile(filepath.Join(capsule.Sandbox.WorktreePath, "runner_fallback.txt"), []byte("updated"), 0o644); err != nil {
				t.Fatalf("write fallback file: %v", err)
			}
			runGit(t, capsule.Sandbox.WorktreePath, "add", "runner_fallback.txt")
			return &schema.AgentSidecarOutput{
				ObligationsAddressed: []string{"OB-1"},
				FilesChanged:         []string{"runner_fallback.txt"},
				CommandsRun:          []string{"go test ./..."},
				Assumptions:          []string{"fallback path"},
				Claims:               []schema.SidecarClaim{{Claim: "fallback claim", Type: schema.SidecarClaimProposed}},
				Risks:                []string{"manual review required"},
				FollowUpNeeded:       []string{"add structured output"},
				EvidencePaths:        []string{transcriptPath},
			}, nil
		},
	}
	r := New(env.st, env.log, env.orcaDir, adapter)
	result, err := r.Run(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SidecarUsed {
		t.Fatal("SidecarUsed = true, want false")
	}
	if result.PatchID == "" || len(result.EvidenceIDs) == 0 {
		t.Fatalf("RunResult = %+v", result)
	}
}

func TestRunReviewerCanProduceEvidenceWithoutPatch(t *testing.T) {
	env := newRunnerEnv(t)
	// saveRunnerScenario creates GOAL-1, GC-1, OB-1, and an executor capsule CAP-1.
	// The reviewer capsule is a separate artifact with Role set from creation.
	_ = saveRunnerScenario(t, env)

	reviewerProjectionID := "CTX-reviewer"
	reviewerCapsuleID := "CAP-reviewer"

	if err := env.st.SaveProjection(env.ctx, &schema.ContextProjection{
		ContextProjectionID: reviewerProjectionID,
		Role:                schema.ProjectionRoleReviewer,
		SourceArtifactIDs:   []string{"OB-1"},
		IncludedSections:    []string{"role contract: review the implementer output"},
		TokenBudget:         1200,
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveProjection reviewer: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:           reviewerCapsuleID,
		ObligationIDs:       []string{"OB-1"},
		Agent:               schema.AgentCodex,
		Role:                schema.RoleReviewer,
		ContextProjectionID: reviewerProjectionID,
		AllowedPaths:        []string{"."},
		Budget: schema.CapsuleBudget{
			MaxTokens:          4096,
			MaxWallTimeSeconds: 60,
			MaxRetries:         1,
		},
		Sandbox: schema.CapsuleSandbox{
			WorktreePath: env.worktree,
			Network:      schema.NetworkDeny,
			WriteScope:   "worktree_only",
		},
		State: schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule reviewer: %v", err)
	}

	capsuleID := reviewerCapsuleID
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			if projection.Role != schema.ProjectionRoleReviewer {
				t.Fatalf("projection role = %s, want reviewer", projection.Role)
			}
			if err := os.WriteFile(filepath.Join(capsule.Sandbox.WorktreePath, "review_notes.txt"), []byte("review edits"), 0o644); err != nil {
				t.Fatalf("write reviewer file: %v", err)
			}
			return &schema.AgentSidecarOutput{
				ObligationsAddressed: []string{"OB-1"},
				FilesChanged:         []string{"review_notes.txt"},
				CommandsRun:          []string{"review patch evidence"},
				Claims:               []schema.SidecarClaim{{Claim: "review found no scope issue", Type: schema.SidecarClaimVerified}},
				EvidencePaths:        []string{orcapath.TranscriptPath(env.orcaDir, capsule.CapsuleID)},
				Summary:              "review evidence only",
			}, nil
		},
	}
	r := New(env.st, env.log, env.orcaDir, adapter)
	result, err := r.Run(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PatchID != "" {
		t.Fatalf("PatchID = %q, want empty for evidence-only reviewer run", result.PatchID)
	}
	if len(result.EvidenceIDs) == 0 || len(result.ClaimIDs) == 0 {
		t.Fatalf("RunResult = %+v, want evidence and claims", result)
	}
	if patches, err := env.st.LoadPatchesForCapsule(env.ctx, capsuleID); err != nil {
		t.Fatalf("LoadPatchesForCapsule: %v", err)
	} else if len(patches) != 0 {
		t.Fatalf("reviewer created %d patch artifacts, want none", len(patches))
	}
}

func TestRunFailureTransitionsCapsuleAndPersistsInfraFailure(t *testing.T) {
	env := newRunnerEnv(t)
	capsuleID := saveRunnerScenario(t, env)
	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			return nil, errors.New("adapter exploded")
		},
	}
	r := New(env.st, env.log, env.orcaDir, adapter)
	result, err := r.Run(env.ctx, capsuleID)
	if err == nil {
		t.Fatal("Run returned nil error for failing adapter")
	}
	if len(result.FailureIDs) != 1 {
		t.Fatalf("FailureIDs = %v, want exactly one", result.FailureIDs)
	}
	capsule, loadErr := env.st.LoadCapsule(env.ctx, capsuleID)
	if loadErr != nil {
		t.Fatalf("LoadCapsule: %v", loadErr)
	}
	if capsule.State != schema.CapsuleStateFailed {
		t.Fatalf("capsule state = %s, want %s", capsule.State, schema.CapsuleStateFailed)
	}
	failure, loadFailureErr := env.st.LoadFailure(env.ctx, result.FailureIDs[0])
	if loadFailureErr != nil {
		t.Fatalf("LoadFailure: %v", loadFailureErr)
	}
	if failure.FailureType != schema.FailureInfra {
		t.Fatalf("failure type = %s, want %s", failure.FailureType, schema.FailureInfra)
	}
}

func TestRunFailureAddsPriorAttemptHistoryWithoutOverwriting(t *testing.T) {
	env := newRunnerEnv(t)
	firstCapsuleID := saveRunnerScenario(t, env)
	secondCapsuleID := "CAP-2"
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:           secondCapsuleID,
		ObligationIDs:       []string{"OB-1"},
		Agent:               schema.AgentCodex,
		Role:                schema.RoleExecutor,
		ContextProjectionID: "CTX-1",
		AllowedPaths:        []string{"."},
		Budget: schema.CapsuleBudget{
			MaxTokens:          4096,
			MaxWallTimeSeconds: 60,
			MaxRetries:         1,
		},
		Sandbox: schema.CapsuleSandbox{
			WorktreePath: env.worktree,
			Network:      schema.NetworkDeny,
			WriteScope:   "worktree_only",
		},
		State: schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule second: %v", err)
	}

	adapter := &fakeAdapter{
		agent: schema.AgentCodex,
		executeFn: func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
			return nil, errors.New("adapter exploded")
		},
	}
	r := New(env.st, env.log, env.orcaDir, adapter)
	firstResult, firstErr := r.Run(env.ctx, firstCapsuleID)
	if firstErr == nil {
		t.Fatal("first Run returned nil error")
	}
	secondResult, secondErr := r.Run(env.ctx, secondCapsuleID)
	if secondErr == nil {
		t.Fatal("second Run returned nil error")
	}
	if len(firstResult.FailureIDs) != 1 || len(secondResult.FailureIDs) != 1 {
		t.Fatalf("failure IDs first=%v second=%v, want one each", firstResult.FailureIDs, secondResult.FailureIDs)
	}
	if firstResult.FailureIDs[0] == secondResult.FailureIDs[0] {
		t.Fatalf("failure ID was reused: %s", firstResult.FailureIDs[0])
	}
	firstFailure, err := env.st.LoadFailure(env.ctx, firstResult.FailureIDs[0])
	if err != nil {
		t.Fatalf("LoadFailure first: %v", err)
	}
	secondFailure, err := env.st.LoadFailure(env.ctx, secondResult.FailureIDs[0])
	if err != nil {
		t.Fatalf("LoadFailure second: %v", err)
	}
	if firstFailure.PriorAttemptCount != 0 {
		t.Fatalf("first PriorAttemptCount = %d, want 0", firstFailure.PriorAttemptCount)
	}
	if secondFailure.PriorAttemptCount != 1 {
		t.Fatalf("second PriorAttemptCount = %d, want 1", secondFailure.PriorAttemptCount)
	}
	if len(secondFailure.PriorCapsuleIDs) != 1 || secondFailure.PriorCapsuleIDs[0] != firstCapsuleID {
		t.Fatalf("second PriorCapsuleIDs = %v, want [%s]", secondFailure.PriorCapsuleIDs, firstCapsuleID)
	}
	failuresForSecond, err := env.st.LoadFailuresForCapsule(env.ctx, secondCapsuleID)
	if err != nil {
		t.Fatalf("LoadFailuresForCapsule second: %v", err)
	}
	if len(failuresForSecond) != 1 || failuresForSecond[0].FailureID != secondResult.FailureIDs[0] {
		t.Fatalf("LoadFailuresForCapsule second = %+v, want current capsule failure only", failuresForSecond)
	}
}

func saveRunnerScenario(t *testing.T, env *runnerEnv) string {
	t.Helper()
	now := time.Now().UTC()
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         "GOAL-1",
		OriginalIntent: "runner scenario",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-1",
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		ScopeConstraints: schema.ScopeConstraints{
			AllowedFiles: []string{"."},
		},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     "OB-1",
		GoalConditionID:  "GC-1",
		Description:      "prove runner output",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveProjection(env.ctx, &schema.ContextProjection{
		ContextProjectionID: "CTX-1",
		Role:                schema.ProjectionRoleExecutor,
		SourceArtifactIDs:   []string{"OB-1"},
		IncludedSections:    []string{"obligations", "scope"},
		OmittedSections:     []string{"raw_transcript"},
		TokenBudget:         1200,
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("SaveProjection: %v", err)
	}
	capsuleID := "CAP-1"
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:           capsuleID,
		ObligationIDs:       []string{"OB-1"},
		Agent:               schema.AgentCodex,
		Role:                schema.RoleExecutor,
		ContextProjectionID: "CTX-1",
		AllowedPaths:        []string{"."},
		Budget: schema.CapsuleBudget{
			MaxTokens:          4096,
			MaxWallTimeSeconds: 60,
			MaxRetries:         1,
		},
		Sandbox: schema.CapsuleSandbox{
			WorktreePath: env.worktree,
			Network:      schema.NetworkDeny,
			WriteScope:   "worktree_only",
		},
		State: schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	return capsuleID
}

type fakeAdapter struct {
	agent     schema.AgentType
	executeFn func(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error)
	extractFn func(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error)
}

func (f *fakeAdapter) AgentType() schema.AgentType { return f.agent }

func (f *fakeAdapter) Preflight(ctx context.Context, capsule *schema.ExecutionCapsule) error {
	return nil
}

func (f *fakeAdapter) Execute(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	if f.executeFn == nil {
		return nil, errors.New("execute not implemented")
	}
	return f.executeFn(ctx, capsule, projection)
}

func (f *fakeAdapter) ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
	if f.extractFn == nil {
		return nil, errors.New("extract not implemented")
	}
	return f.extractFn(ctx, capsule, transcriptPath)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	runGit(t, "", "init", dir)
	runGit(t, dir, "config", "user.email", "runner-tests@example.com")
	runGit(t, dir, "config", "user.name", "Runner Tests")
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("initial"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	runGit(t, dir, "add", "README.txt")
	runGit(t, dir, "commit", "-m", "init")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}

func TestFindScopeViolations(t *testing.T) {
	sep := string([]byte{filepath.Separator})
	cases := []struct {
		name      string
		changed   []string
		allowed   []string
		forbidden []string
		want      []string
	}{
		{
			name:    "no restrictions",
			changed: []string{"a.go", "b.go"},
			want:    nil,
		},
		{
			name:    "all within allowed",
			changed: []string{"src" + sep + "a.go", "src" + sep + "b.go"},
			allowed: []string{"src"},
			want:    nil,
		},
		{
			name:    "outside allowed",
			changed: []string{"src" + sep + "a.go", "docs" + sep + "x.md"},
			allowed: []string{"src"},
			want:    []string{"docs" + sep + "x.md"},
		},
		{
			name:      "forbidden overrides allowed",
			changed:   []string{"src" + sep + "secret.go", "src" + sep + "ok.go"},
			allowed:   []string{"src"},
			forbidden: []string{"src" + sep + "secret.go"},
			want:      []string{"src" + sep + "secret.go"},
		},
		{
			name:      "forbidden subtree",
			changed:   []string{"pkg" + sep + "internal" + sep + "x.go", "pkg" + sep + "a.go"},
			allowed:   []string{"pkg"},
			forbidden: []string{"pkg" + sep + "internal"},
			want:      []string{"pkg" + sep + "internal" + sep + "x.go"},
		},
		{
			name:    "dot allowed covers everything",
			changed: []string{"anywhere" + sep + "file.go"},
			allowed: []string{"."},
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findScopeViolations(tc.changed, tc.allowed, tc.forbidden)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("violations = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("violations[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
