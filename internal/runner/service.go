package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type service struct {
	store    store.ArtifactStore
	log      eventlog.EventLog
	orcaDir  string
	adapters map[schema.AgentType]Adapter
}

// New returns the default CapsuleRunner implementation.
func New(st store.ArtifactStore, log eventlog.EventLog, orcaDir string, adapters ...Adapter) CapsuleRunner {
	registry := make(map[schema.AgentType]Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			continue
		}
		registry[adapter.AgentType()] = adapter
	}
	return &service{
		store:    st,
		log:      log,
		orcaDir:  strings.TrimSpace(orcaDir),
		adapters: registry,
	}
}

func (s *service) Run(ctx context.Context, capsuleID string) (result RunResult, err error) {
	if s.store == nil {
		return RunResult{}, fmt.Errorf("runner: store is required")
	}
	if s.log == nil {
		return RunResult{}, fmt.Errorf("runner: event log is required")
	}
	if strings.TrimSpace(capsuleID) == "" {
		return RunResult{}, fmt.Errorf("runner: capsule ID is required")
	}

	capsule, err := s.store.LoadCapsule(ctx, capsuleID)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner: load capsule %s: %w", capsuleID, err)
	}
	projection, err := s.store.LoadProjection(ctx, capsule.ContextProjectionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner: load projection %s: %w", capsule.ContextProjectionID, err)
	}
	adapter, ok := s.adapters[capsule.Agent]
	if !ok {
		return RunResult{}, fmt.Errorf("runner: no adapter registered for agent %q", capsule.Agent)
	}
	goalID, err := s.resolveGoalID(ctx)
	if err != nil {
		return RunResult{}, err
	}

	result = RunResult{CapsuleID: capsule.CapsuleID}
	transitioned := false
	defer func() {
		if err == nil || !transitioned {
			return
		}
		failureID, failErr := s.failCapsule(ctx, goalID, capsule, err)
		if failErr != nil {
			err = errors.Join(err, failErr)
			return
		}
		result.FailureIDs = append(result.FailureIDs, failureID)
	}()

	if err = s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleStarted, schema.CapsuleStateWorktreeCreated); err != nil {
		return result, err
	}
	// Set transitioned immediately after the capsule_started event is logged so
	// the deferred failCapsule always emits a matching capsule_completed event if
	// any subsequent step fails — even if UpdateCapsuleState itself fails below.
	transitioned = true
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateWorktreeCreated); err != nil {
		return result, fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateWorktreeCreated, err)
	}

	if err = ensureWorktree(ctx, capsule.Sandbox.WorktreePath); err != nil {
		return result, fmt.Errorf("runner: ensure worktree for capsule %s: %w", capsule.CapsuleID, err)
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateWorkspaceAttached); err != nil {
		return result, fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateWorkspaceAttached, err)
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateSetupRun); err != nil {
		return result, fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateSetupRun, err)
	}

	runCtx := ctx
	cancel := func() {}
	if capsule.Budget.MaxWallTimeSeconds <= 0 {
		return result, fmt.Errorf("runner: capsule %s has invalid max_wall_time_seconds=%d", capsule.CapsuleID, capsule.Budget.MaxWallTimeSeconds)
	}
	runCtx, cancel = context.WithTimeout(ctx, time.Duration(capsule.Budget.MaxWallTimeSeconds)*time.Second)
	defer cancel()

	if err = adapter.Preflight(runCtx, capsule); err != nil {
		return result, fmt.Errorf("runner: adapter preflight for capsule %s: %w", capsule.CapsuleID, err)
	}

	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateAgentRunning); err != nil {
		return result, fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateAgentRunning, err)
	}

	output, sidecarUsed, err := s.runAdapter(runCtx, adapter, capsule, projection)
	if err != nil {
		return result, err
	}
	result.SidecarUsed = sidecarUsed

	diffPath, changedFiles, err := buildPatchDiff(runCtx, s.orcaDir, capsule.CapsuleID, capsule.Sandbox.WorktreePath)
	if err != nil {
		return result, fmt.Errorf("runner: build patch diff for capsule %s: %w", capsule.CapsuleID, err)
	}
	if len(output.FilesChanged) > 0 {
		changedFiles = append([]string(nil), output.FilesChanged...)
	}
	evidenceOnly := capsule.Role == schema.RoleReviewer || capsule.Role == schema.RoleTester
	if len(changedFiles) == 0 && !evidenceOnly {
		return result, fmt.Errorf("runner: capsule %s produced no changed files", capsule.CapsuleID)
	}

	obligationsClaimed := output.ObligationsAddressed
	if len(obligationsClaimed) == 0 {
		obligationsClaimed = append([]string(nil), capsule.ObligationIDs...)
	}
	scopeViolations := findScopeViolations(changedFiles, capsule.AllowedPaths, capsule.ForbiddenPaths)

	if len(changedFiles) > 0 || !evidenceOnly {
		patch := &schema.PatchArtifact{
			PatchID:              idgen.New("PATCH"),
			CapsuleID:            capsule.CapsuleID,
			BaseCommit:           strings.TrimSpace(currentCommit(runCtx, capsule.Sandbox.WorktreePath)),
			ChangedFiles:         changedFiles,
			DiffPath:             diffPath,
			Summary:              strings.TrimSpace(output.Summary),
			ObligationIDsClaimed: obligationsClaimed,
			RiskNotes:            append([]string(nil), output.Risks...),
			Status:               schema.PatchCandidate,
			ScopeViolations:      scopeViolations,
		}
		if patch.Summary == "" {
			patch.Summary = "capsule output recorded by runner"
		}
		if err = s.store.SavePatch(ctx, patch); err != nil {
			return result, fmt.Errorf("runner: save patch %s: %w", patch.PatchID, err)
		}
		result.PatchID = patch.PatchID
	}

	evidenceIDs, err := s.saveEvidence(ctx, capsule, output, obligationsClaimed)
	if err != nil {
		return result, err
	}
	result.EvidenceIDs = evidenceIDs

	claimIDs, err := s.saveClaims(ctx, capsule, output, evidenceIDs)
	if err != nil {
		return result, err
	}
	result.ClaimIDs = claimIDs

	if err = s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleCompleted, schema.CapsuleStateCompleted); err != nil {
		return result, err
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateCompleted); err != nil {
		return result, fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateCompleted, err)
	}
	return result, nil
}

func (s *service) runAdapter(
	ctx context.Context,
	adapter Adapter,
	capsule *schema.ExecutionCapsule,
	projection *schema.ContextProjection,
) (*schema.AgentSidecarOutput, bool, error) {
	output, err := adapter.Execute(ctx, capsule, projection)
	if err == nil {
		return output, true, nil
	}
	if !errors.Is(err, ErrNoSidecar) && !errors.Is(err, ErrInvalidSidecar) {
		return nil, false, fmt.Errorf("runner: execute capsule %s: %w", capsule.CapsuleID, err)
	}
	transcriptPath := orcapath.TranscriptPath(s.orcaDir, capsule.CapsuleID)
	output, extractErr := adapter.ExtractFromTranscript(ctx, capsule, transcriptPath)
	if extractErr != nil {
		return nil, false, fmt.Errorf("runner: transcript extraction for capsule %s after %v: %w", capsule.CapsuleID, err, extractErr)
	}
	return output, false, nil
}

func (s *service) resolveGoalID(ctx context.Context) (string, error) {
	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return "", fmt.Errorf("runner: load active goal: %w", err)
	}
	if goal == nil {
		return "", fmt.Errorf("runner: active goal not found: %w", store.ErrNotFound)
	}
	return goal.GoalID, nil
}

func (s *service) appendCapsuleTransition(
	ctx context.Context,
	goalID string,
	capsuleID string,
	eventType schema.EventType,
	state schema.CapsuleState,
) error {
	payload, err := json.Marshal(schema.CapsuleTransitionPayload{
		CapsuleID: capsuleID,
		State:     state,
	})
	if err != nil {
		return fmt.Errorf("runner: marshal %s payload for capsule %s: %w", eventType, capsuleID, err)
	}
	if _, err = s.log.Append(ctx, schema.Event{
		Type:       eventType,
		GoalID:     goalID,
		ArtifactID: capsuleID,
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("runner: append %s for capsule %s: %w", eventType, capsuleID, err)
	}
	return nil
}

func (s *service) failCapsule(
	ctx context.Context,
	goalID string,
	capsule *schema.ExecutionCapsule,
	runErr error,
) (string, error) {
	failure := &schema.FailureFingerprint{
		FailureID:       idgen.New("FAIL"),
		SourceCapsuleID: capsule.CapsuleID,
		FailureType:     schema.FailureInfra,
		Summary:         runErr.Error(),
		ErrorSignature:  errorSignature(runErr),
	}
	if err := s.store.SaveFailure(ctx, failure); err != nil {
		return "", fmt.Errorf("runner: save failure for capsule %s: %w", capsule.CapsuleID, err)
	}
	if err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleCompleted, schema.CapsuleStateFailed); err != nil {
		return "", err
	}
	if err := s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateFailed); err != nil {
		return "", fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateFailed, err)
	}
	return failure.FailureID, nil
}

func (s *service) saveEvidence(
	ctx context.Context,
	capsule *schema.ExecutionCapsule,
	output *schema.AgentSidecarOutput,
	obligations []string,
) ([]string, error) {
	if len(output.EvidencePaths) == 0 {
		output.EvidencePaths = []string{orcapath.TranscriptPath(s.orcaDir, capsule.CapsuleID)}
	}
	evidenceIDs := make([]string, 0, len(output.EvidencePaths))
	for _, p := range output.EvidencePaths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		ev := &schema.EvidenceArtifact{
			EvidenceID: idgen.New("EV"),
			Type:       schema.EvidenceTestResult,
			Source:     string(capsule.Agent),
			Command:    strings.Join(output.CommandsRun, " && "),
			ExitCode:   0,
			Summary:    "agent-provided evidence artifact",
			RawLogPath: trimmed,
			Supports:   append([]string(nil), obligations...),
			CreatedAt:  time.Now().UTC(),
		}
		if err := s.store.SaveEvidence(ctx, ev); err != nil {
			return nil, fmt.Errorf("runner: save evidence %s: %w", ev.EvidenceID, err)
		}
		evidenceIDs = append(evidenceIDs, ev.EvidenceID)
	}
	if len(evidenceIDs) == 0 {
		return nil, fmt.Errorf("runner: capsule %s produced no evidence artifacts", capsule.CapsuleID)
	}
	return evidenceIDs, nil
}

func (s *service) saveClaims(
	ctx context.Context,
	capsule *schema.ExecutionCapsule,
	output *schema.AgentSidecarOutput,
	evidenceIDs []string,
) ([]string, error) {
	claimIDs := make([]string, 0, len(output.Claims)+len(output.Assumptions)+len(output.Risks)+len(output.FollowUpNeeded))
	addClaim := func(text string, claimType schema.ClaimType, status schema.ClaimStatus, ids []string) error {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return nil
		}
		claim := &schema.ClaimArtifact{
			ClaimID:         idgen.New("CLM"),
			Text:            trimmed,
			ClaimType:       claimType,
			SourceCapsuleID: capsule.CapsuleID,
			AffectedFiles:   append([]string(nil), output.FilesChanged...),
			Status:          status,
			EvidenceIDs:     append([]string(nil), ids...),
		}
		if err := s.store.SaveClaim(ctx, claim); err != nil {
			return fmt.Errorf("runner: save claim %s: %w", claim.ClaimID, err)
		}
		claimIDs = append(claimIDs, claim.ClaimID)
		return nil
	}

	for _, c := range output.Claims {
		status := schema.ClaimProposed
		ids := []string(nil)
		if c.Type == schema.SidecarClaimVerified && len(evidenceIDs) > 0 {
			status = schema.ClaimVerified
			ids = []string{evidenceIDs[0]}
		}
		if err := addClaim(c.Claim, schema.ClaimInvariant, status, ids); err != nil {
			return nil, err
		}
	}
	for _, assumption := range output.Assumptions {
		if err := addClaim(assumption, schema.ClaimAssumption, schema.ClaimProposed, nil); err != nil {
			return nil, err
		}
	}
	for _, risk := range output.Risks {
		if err := addClaim(risk, schema.ClaimRisk, schema.ClaimProposed, nil); err != nil {
			return nil, err
		}
	}
	for _, followUp := range output.FollowUpNeeded {
		if err := addClaim(followUp, schema.ClaimOpenQuestion, schema.ClaimProposed, nil); err != nil {
			return nil, err
		}
	}
	return claimIDs, nil
}

func ensureWorktree(ctx context.Context, worktreePath string) error {
	path := strings.TrimSpace(worktreePath)
	if path == "" {
		return fmt.Errorf("runner: capsule sandbox worktree path is required")
	}
	if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && !info.IsDir() {
		return nil
	} else if err == nil && info.IsDir() {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runner: stat %s: %w", filepath.Join(path, ".git"), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("runner: create worktree parent %s: %w", filepath.Dir(path), err)
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", path, "HEAD")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("runner: git worktree add %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func buildPatchDiff(ctx context.Context, orcaDir, capsuleID, worktreePath string) (string, []string, error) {
	diffPath := filepath.Join(orcaDir, "capsules", capsuleID, "patch.diff")
	if err := os.MkdirAll(filepath.Dir(diffPath), 0o755); err != nil {
		return "", nil, fmt.Errorf("runner: create patch dir: %w", err)
	}
	diffCmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--binary")
	diffCmd.Dir = worktreePath
	diff, err := diffCmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("runner: git diff in %s: %w", worktreePath, err)
	}
	if err := os.WriteFile(diffPath, diff, 0o644); err != nil {
		return "", nil, fmt.Errorf("runner: write patch diff %s: %w", diffPath, err)
	}

	nameCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--relative")
	nameCmd.Dir = worktreePath
	namesRaw, err := nameCmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("runner: git diff --name-only in %s: %w", worktreePath, err)
	}
	names := strings.Split(strings.TrimSpace(string(namesRaw)), "\n")
	changed := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		changed = append(changed, filepath.Clean(name))
	}
	return diffPath, changed, nil
}

func currentCommit(ctx context.Context, worktreePath string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func findScopeViolations(changedFiles, allowedPaths, forbiddenPaths []string) []string {
	violations := make([]string, 0)
	for _, file := range changedFiles {
		if !isWithinAllowed(file, allowedPaths) || isForbidden(file, forbiddenPaths) {
			violations = append(violations, file)
		}
	}
	slices.Sort(violations)
	return slices.Compact(violations)
}

func isWithinAllowed(file string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, pattern := range allowed {
		pattern = filepath.Clean(strings.TrimSpace(pattern))
		if pattern == "." || pattern == "" {
			return true
		}
		if file == pattern {
			return true
		}
		prefix := pattern + string(filepath.Separator)
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

func isForbidden(file string, forbidden []string) bool {
	for _, pattern := range forbidden {
		pattern = filepath.Clean(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if file == pattern {
			return true
		}
		prefix := pattern + string(filepath.Separator)
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

func errorSignature(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "git"):
		return "git"
	case strings.Contains(msg, "sidecar"):
		return "sidecar"
	default:
		return "infra"
	}
}
