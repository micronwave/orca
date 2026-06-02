package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/failurehistory"
	"github.com/micronwave/orca/internal/hooks"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/permission"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// Runner executes an agent inside a bounded execution capsule. It selects an
// Adapter by capsule.Agent, runs the agent under contract, collects outputs via
// sidecar or transcript extraction, normalizes output into schema artifacts,
// persists them, and returns the IDs of all produced artifacts.
type Runner struct {
	store           *store.FileStore
	log             *eventlog.FileLog
	orcaDir         string
	noLearning      bool
	permissionRules []permission.Rule
	preCapsuleHook  *hooks.Config // nil means no pre-capsule hook
	hookRunner      *hooks.Runner
	adapters        map[schema.AgentType]Adapter
	nowFn           func() time.Time
}

// Config holds runner-local options not part of the repo config file contract.
type Config struct {
	NoLearning      bool
	PermissionRules []permission.Rule // global config rules applied after capsule-level checks
	// PreCapsuleHook, when non-nil, fires after the permission check passes and
	// before adapter.Preflight. A deny result blocks capsule launch.
	PreCapsuleHook *hooks.Config
}

// New returns a Runner.
func New(st *store.FileStore, log *eventlog.FileLog, orcaDir string, adapters ...Adapter) *Runner {
	return NewWithConfig(st, log, orcaDir, Config{}, adapters...)
}

// NewWithConfig returns a Runner with runner-local options.
func NewWithConfig(st *store.FileStore, log *eventlog.FileLog, orcaDir string, cfg Config, adapters ...Adapter) *Runner {
	registry := make(map[schema.AgentType]Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			continue
		}
		if v := reflect.ValueOf(adapter); v.Kind() == reflect.Pointer && v.IsNil() {
			continue
		}
		registry[adapter.AgentType()] = adapter
	}
	return &Runner{
		store:           st,
		log:             log,
		orcaDir:         strings.TrimSpace(orcaDir),
		noLearning:      cfg.NoLearning,
		permissionRules: append([]permission.Rule(nil), cfg.PermissionRules...),
		preCapsuleHook:  cfg.PreCapsuleHook,
		hookRunner:      hooks.New(),
		adapters:        registry,
		nowFn:           time.Now,
	}
}

// NoLearning reports whether this runner was constructed with learning disabled.
func (s *Runner) NoLearning() bool { return s.noLearning }

func (s *Runner) Run(ctx context.Context, capsuleID string) (result RunResult, err error) {
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
	if capsule.CapsuleID != capsuleID {
		return RunResult{}, fmt.Errorf("runner: capsule file %s contains mismatched capsule_id %q", capsuleID, capsule.CapsuleID)
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

	// Track the last emitted runtime status for failure classification.
	var lastRuntimeStatus schema.CapsuleRuntimeStatus

	emitRuntime := func(status schema.CapsuleRuntimeStatus, failClass schema.CapsuleRuntimeFailureClass, detail string) {
		lastRuntimeStatus = status
		ev := &schema.CapsuleRuntimeEvent{
			CapsuleID:  capsule.CapsuleID,
			GoalID:     goalID,
			Source:     "runner",
			Status:     status,
			FailClass:  failClass,
			Detail:     detail,
			OccurredAt: s.nowFn(),
		}
		// Errors emitting runtime events are non-fatal; they are diagnostic only.
		_ = s.store.AppendRuntimeEvent(ctx, ev)
	}

	result = RunResult{CapsuleID: capsule.CapsuleID}
	transitioned := false
	defer func() {
		if err == nil || !transitioned {
			return
		}
		failureID, failErr := s.failCapsule(ctx, goalID, capsule, err, lastRuntimeStatus)
		if failErr != nil {
			err = errors.Join(err, failErr)
			return
		}
		result.FailureIDs = append(result.FailureIDs, failureID)
	}()

	emitRuntime(schema.RuntimeStatusSpawning, "", "")

	startEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleStarted, schema.CapsuleStateWorktreeCreated)
	if err != nil {
		return result, err
	}
	// Set transitioned immediately after the capsule_started event is logged so
	// the deferred failCapsule always emits a matching capsule_completed event if
	// any subsequent step fails — even if UpdateCapsuleState itself fails below.
	transitioned = true
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateWorktreeCreated); err != nil {
		return result, &store.MaterializationError{Event: startEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateWorktreeCreated, err)}
	}

	if err = ensureWorktree(ctx, capsule.Sandbox.WorktreePath); err != nil {
		return result, fmt.Errorf("runner: ensure worktree for capsule %s: %w", capsule.CapsuleID, err)
	}
	wsEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleStateUpdated, schema.CapsuleStateWorkspaceAttached)
	if err != nil {
		return result, err
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateWorkspaceAttached); err != nil {
		return result, &store.MaterializationError{Event: wsEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateWorkspaceAttached, err)}
	}
	setupEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleStateUpdated, schema.CapsuleStateSetupRun)
	if err != nil {
		return result, err
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateSetupRun); err != nil {
		return result, &store.MaterializationError{Event: setupEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateSetupRun, err)}
	}

	runCtx := ctx
	cancel := func() {}
	if capsule.Budget.MaxWallTimeSeconds <= 0 {
		return result, fmt.Errorf("runner: capsule %s has invalid max_wall_time_seconds=%d", capsule.CapsuleID, capsule.Budget.MaxWallTimeSeconds)
	}
	runCtx, cancel = context.WithTimeout(ctx, time.Duration(capsule.Budget.MaxWallTimeSeconds)*time.Second)
	defer cancel()

	// Permission check: enforce the capsule's policy before calling the adapter.
	enforcer := permission.NewEnforcer(
		permission.Mode(capsule.PermissionMode),
		capsule.AllowedTools,
		capsule.ForbiddenActions,
		capsule.AllowedPaths,
		capsule.ForbiddenPaths,
		capsule.Sandbox.WorktreePath,
		s.permissionRules,
	)
	permReq := permission.Request{
		CapsuleID:    capsule.CapsuleID,
		ToolName:     string(capsule.Agent),
		RequiredMode: permission.ModeWorkspaceWrite,
		ActiveMode:   permission.Mode(capsule.PermissionMode),
		PathScope:    capsule.Sandbox.WorktreePath,
		Reason:       "execute capsule agent",
	}
	decision := enforcer.Check(permReq)
	if decision.Effect == permission.EffectDeny {
		emitRuntime(schema.RuntimeStatusPermissionRequired, schema.RuntimeFailurePermissionGate, decision.Reason)
		return result, fmt.Errorf("runner: permission denied for capsule %s: %s", capsule.CapsuleID, decision.Reason)
	}
	if decision.Effect == permission.EffectAsk {
		emitRuntime(schema.RuntimeStatusPermissionRequired, schema.RuntimeFailurePermissionGate, decision.AskPrompt)
		return result, fmt.Errorf("runner: capsule %s requires human approval (prompt mode) before execution: %s", capsule.CapsuleID, decision.AskPrompt)
	}

	emitRuntime(schema.RuntimeStatusReadyForPrompt, "", "")

	// pre_capsule hook: fires after permission check, before adapter preflight.
	if s.preCapsuleHook != nil {
		hookInput := hooks.Input{
			HookPoint:     hooks.PointPreCapsule,
			CapsuleID:     capsule.CapsuleID,
			GoalID:        goalID,
			ObligationIDs: append([]string(nil), capsule.ObligationIDs...),
			WorktreePath:  capsule.Sandbox.WorktreePath,
		}
		hookRes, hookErr := s.hookRunner.Run(ctx, *s.preCapsuleHook, hookInput)
		if hookErr != nil {
			emitRuntime(schema.RuntimeStatusPreflightWarning, schema.RuntimeFailureToolRuntime,
				"pre_capsule hook error: "+hookErr.Error())
			return result, fmt.Errorf("runner: pre_capsule hook for capsule %s: %w", capsule.CapsuleID, hookErr)
		}
		if hookRunErr := s.handlePreCapsuleHookResult(ctx, capsule, goalID, hookRes, emitRuntime); hookRunErr != nil {
			return result, hookRunErr
		}
	}

	if err = adapter.Preflight(runCtx, capsule); err != nil {
		failClass := classifyPreflightError(err)
		emitRuntime(schema.RuntimeStatusPreflightWarning, failClass, err.Error())
		return result, fmt.Errorf("runner: adapter preflight for capsule %s: %w", capsule.CapsuleID, err)
	}

	agentEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleStateUpdated, schema.CapsuleStateAgentRunning)
	if err != nil {
		return result, err
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateAgentRunning); err != nil {
		return result, &store.MaterializationError{Event: agentEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateAgentRunning, err)}
	}
	emitRuntime(schema.RuntimeStatusAgentRunning, "", "")

	output, sidecarUsed, err := s.runAdapter(runCtx, adapter, capsule, projection, emitRuntime)
	if err != nil {
		return result, err
	}
	emitRuntime(schema.RuntimeStatusOutputCollecting, "", "")
	result.SidecarUsed = sidecarUsed
	result.TokensUsed = output.TokensUsed
	result.WallTimeSeconds = output.WallTimeSeconds

	diffPath, changedFiles, err := buildPatchDiff(runCtx, s.orcaDir, capsule.CapsuleID, capsule.Sandbox.WorktreePath)
	if err != nil {
		return result, fmt.Errorf("runner: build patch diff for capsule %s: %w", capsule.CapsuleID, err)
	}
	evidenceOnly := capsule.Role == schema.RoleReviewer || capsule.Role == schema.RoleTester
	if len(changedFiles) == 0 && !evidenceOnly {
		return result, fmt.Errorf("runner: capsule %s produced no changed files", capsule.CapsuleID)
	}

	obligationsClaimed := filterObligations(output.ObligationsAddressed, capsule.ObligationIDs)
	if len(obligationsClaimed) == 0 {
		obligationsClaimed = append([]string(nil), capsule.ObligationIDs...)
	}
	scopeViolations := findScopeViolations(changedFiles, capsule.AllowedPaths, capsule.ForbiddenPaths)

	if !evidenceOnly {
		baseCommit, err := currentCommit(runCtx, capsule.Sandbox.WorktreePath)
		if err != nil {
			return result, err
		}
		patch := &schema.PatchArtifact{
			PatchID:              idgen.New("PATCH"),
			CapsuleID:            capsule.CapsuleID,
			BaseCommit:           baseCommit,
			ChangedFiles:         changedFiles,
			DiffPath:             diffPath,
			Summary:              strings.TrimSpace(output.Summary),
			ObligationIDsClaimed: obligationsClaimed,
			RiskNotes:            append([]string(nil), output.Risks...),
			Status:               schema.PatchCandidate,
			ScopeViolations:      scopeViolations,
			TokensUsed:           output.TokensUsed,
			WallTimeSeconds:      output.WallTimeSeconds,
			SupersededClaimIDs:   append([]string(nil), output.ContradictedClaimIDs...),
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

	claimIDs, err := s.saveClaims(ctx, capsule, output, evidenceIDs, changedFiles)
	if err != nil {
		return result, err
	}
	result.ClaimIDs = claimIDs

	completeEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleCompleted, schema.CapsuleStateCompleted)
	if err != nil {
		return result, err
	}
	if err = s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateCompleted); err != nil {
		return result, &store.MaterializationError{Event: completeEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateCompleted, err)}
	}
	return result, nil
}

func (s *Runner) runAdapter(
	ctx context.Context,
	adapter Adapter,
	capsule *schema.ExecutionCapsule,
	projection *schema.ContextProjection,
	emitRuntime func(schema.CapsuleRuntimeStatus, schema.CapsuleRuntimeFailureClass, string),
) (*schema.AgentSidecarOutput, bool, error) {
	start := s.nowFn()
	output, err := adapter.Execute(ctx, capsule, projection)
	if err == nil {
		if output == nil {
			return nil, false, fmt.Errorf("runner: execute capsule %s: adapter returned nil output with no error", capsule.CapsuleID)
		}
		if output.WallTimeSeconds <= 0 {
			output.WallTimeSeconds = s.nowFn().Sub(start).Seconds()
		}
		return output, true, nil
	}
	if !errors.Is(err, ErrNoSidecar) && !errors.Is(err, ErrInvalidSidecar) {
		return nil, false, fmt.Errorf("runner: execute capsule %s: %w", capsule.CapsuleID, err)
	}
	// Sidecar parse/missing failure: emit adapter_protocol before falling back.
	if errors.Is(err, ErrInvalidSidecar) {
		emitRuntime(schema.RuntimeStatusAgentRunning, schema.RuntimeFailureAdapterProtocol,
			"sidecar output failed schema validation; falling back to transcript extraction")
	}
	transcriptPath := orcapath.TranscriptPath(s.orcaDir, capsule.CapsuleID)
	output, extractErr := adapter.ExtractFromTranscript(ctx, capsule, transcriptPath)
	if extractErr != nil {
		return nil, false, fmt.Errorf("runner: transcript extraction for capsule %s after %v: %w", capsule.CapsuleID, err, extractErr)
	}
	if output == nil {
		return nil, false, fmt.Errorf("runner: transcript extraction for capsule %s: adapter returned nil output", capsule.CapsuleID)
	}
	if output.WallTimeSeconds <= 0 {
		output.WallTimeSeconds = s.nowFn().Sub(start).Seconds()
	}
	return output, false, nil
}

func (s *Runner) resolveGoalID(ctx context.Context) (string, error) {
	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return "", fmt.Errorf("runner: load active goal: %w", err)
	}
	if goal == nil {
		return "", fmt.Errorf("runner: active goal not found: %w", store.ErrNotFound)
	}
	return goal.GoalID, nil
}

func (s *Runner) appendCapsuleTransition(
	ctx context.Context,
	goalID string,
	capsuleID string,
	eventType schema.EventType,
	state schema.CapsuleState,
) (schema.Event, error) {
	payload, err := json.Marshal(schema.CapsuleTransitionPayload{
		CapsuleID: capsuleID,
		State:     state,
	})
	if err != nil {
		return schema.Event{}, fmt.Errorf("runner: marshal %s payload for capsule %s: %w", eventType, capsuleID, err)
	}
	ev, err := s.log.Append(ctx, schema.Event{
		Type:       eventType,
		GoalID:     goalID,
		ArtifactID: capsuleID,
		Payload:    payload,
	})
	if err != nil {
		return schema.Event{}, fmt.Errorf("runner: append %s for capsule %s: %w", eventType, capsuleID, err)
	}
	return ev, nil
}

func (s *Runner) failCapsule(
	ctx context.Context,
	goalID string,
	capsule *schema.ExecutionCapsule,
	runErr error,
	lastStatus schema.CapsuleRuntimeStatus,
) (string, error) {
	// On startup timeout, persist a StartupEvidenceBundle before the generic
	// failure fingerprint so diagnosis can answer what was actually observed.
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		bundle := &schema.StartupEvidenceBundle{
			CapsuleID:      capsule.CapsuleID,
			LastStatus:     lastStatus,
			FailureClass:   schema.RuntimeFailureStartupNoEvidence,
			ProcessCommand: string(capsule.Agent),
			HealthChecks:   []string{},
			CreatedAt:      s.nowFn().UTC(),
		}
		// Save on best-effort; don't let bundle save failure mask the primary error.
		_ = s.store.SaveStartupBundle(ctx, bundle)
	}

	failure := &schema.FailureFingerprint{
		FailureID:       idgen.New("FAIL"),
		SourceCapsuleID: capsule.CapsuleID,
		FailureType:     schema.FailureInfra,
		Summary:         runErr.Error(),
		ErrorSignature:  errorSignature(runErr),
	}
	if err := failurehistory.Prepare(ctx, s.store, goalID, failure, s.noLearning); err != nil {
		return "", fmt.Errorf("runner: prepare failure history for capsule %s: %w", capsule.CapsuleID, err)
	}
	if err := s.store.SaveFailure(ctx, failure); err != nil {
		return "", fmt.Errorf("runner: save failure for capsule %s: %w", capsule.CapsuleID, err)
	}
	failEv, err := s.appendCapsuleTransition(ctx, goalID, capsule.CapsuleID, schema.EventCapsuleCompleted, schema.CapsuleStateFailed)
	if err != nil {
		return "", err
	}
	if err := s.store.UpdateCapsuleState(ctx, capsule.CapsuleID, schema.CapsuleStateFailed); err != nil {
		return "", &store.MaterializationError{Event: failEv, Err: fmt.Errorf("runner: set capsule %s state %s: %w", capsule.CapsuleID, schema.CapsuleStateFailed, err)}
	}
	return failure.FailureID, nil
}

// handlePreCapsuleHookResult stores the hook result and returns an error when
// the hook blocks capsule launch (deny or ask). Allow and attach results are
// stored and treated as non-blocking.
func (s *Runner) handlePreCapsuleHookResult(
	ctx context.Context,
	capsule *schema.ExecutionCapsule,
	goalID string,
	res hooks.Result,
	emitRuntime func(schema.CapsuleRuntimeStatus, schema.CapsuleRuntimeFailureClass, string),
) error {
	switch res.Kind {
	case hooks.ResultDeny, hooks.ResultAsk:
		// Persist the block as a DecisionRecord for auditability.
		reason := res.Reason
		if reason == "" {
			reason = res.Prompt
		}
		if reason == "" {
			reason = "hook blocked capsule launch"
		}
		dec := &schema.DecisionRecord{
			DecisionID: idgen.New("DEC"),
			Context:    "hook_pre_capsule_deny",
			Decision:   "blocked",
			Rationale:  reason,
			MadeBy:     "hook",
			RelatedIDs: append([]string(nil), capsule.ObligationIDs...),
			CreatedAt:  s.nowFn().UTC(),
		}
		if err := s.store.SaveDecision(ctx, dec); err != nil {
			return fmt.Errorf("runner: save pre_capsule hook decision for capsule %s: %w", capsule.CapsuleID, err)
		}
		emitRuntime(schema.RuntimeStatusPreflightWarning, schema.RuntimeFailurePermissionGate,
			"pre_capsule hook denied: "+reason)
		return fmt.Errorf("runner: pre_capsule hook denied capsule %s: %s", capsule.CapsuleID, reason)

	case hooks.ResultAttachEvidence:
		summary := res.EvidenceSummary
		if summary == "" {
			summary = "hook pre_capsule evidence"
		}
		source := res.EvidenceSource
		if source == "" {
			source = "hook"
		}
		ev := &schema.EvidenceArtifact{
			EvidenceID:   idgen.New("EV"),
			Type:         schema.EvidenceAgentOutput,
			Source:       source,
			Summary:      summary,
			InlineOutput: summary,
			Supports:     append([]string(nil), capsule.ObligationIDs...),
			CreatedAt:    s.nowFn().UTC(),
		}
		if err := s.store.SaveEvidence(ctx, ev); err != nil {
			return fmt.Errorf("runner: save pre_capsule hook evidence for capsule %s: %w", capsule.CapsuleID, err)
		}
		return nil

	case hooks.ResultAttachWarning:
		warning := res.Warning
		if warning == "" {
			warning = "hook pre_capsule warning"
		}
		claim := &schema.ClaimArtifact{
			ClaimID:         idgen.New("CLM"),
			Text:            warning,
			ClaimType:       schema.ClaimRisk,
			SourceCapsuleID: capsule.CapsuleID,
			Status:          schema.ClaimProposed,
		}
		if err := s.store.SaveClaim(ctx, claim); err != nil {
			return fmt.Errorf("runner: save pre_capsule hook warning for capsule %s: %w", capsule.CapsuleID, err)
		}
		return nil

	default:
		// ResultAllow and unknown kinds: continue normally.
		return nil
	}
}

func (s *Runner) saveEvidence(
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
			Type:       schema.EvidenceAgentOutput,
			Source:     string(capsule.Agent),
			Command:    strings.Join(output.CommandsRun, " && "),
			Summary:    "agent-provided output artifact",
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

func (s *Runner) saveClaims(
	ctx context.Context,
	capsule *schema.ExecutionCapsule,
	output *schema.AgentSidecarOutput,
	evidenceIDs []string,
	changedFiles []string,
) ([]string, error) {
	claimIDs := make([]string, 0, len(output.Claims)+len(output.Assumptions)+len(output.Risks)+len(output.FollowUpNeeded))
	addClaim := func(text string, claimType schema.ClaimType, status schema.ClaimStatus, ids []string, contradicts, invalidates []string) error {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return nil
		}
		claim := &schema.ClaimArtifact{
			ClaimID:         idgen.New("CLM"),
			Text:            trimmed,
			ClaimType:       claimType,
			SourceCapsuleID: capsule.CapsuleID,
			AffectedFiles:   append([]string(nil), changedFiles...),
			Status:          status,
			EvidenceIDs:     append([]string(nil), ids...),
			Contradicts:     append([]string(nil), contradicts...),
			Invalidates:     append([]string(nil), invalidates...),
		}
		if err := s.store.SaveClaim(ctx, claim); err != nil {
			return fmt.Errorf("runner: save claim %s: %w", claim.ClaimID, err)
		}
		claimIDs = append(claimIDs, claim.ClaimID)
		return nil
	}

	for _, c := range output.Claims {
		ids := []string(nil)
		if c.Type == schema.SidecarClaimVerified && len(evidenceIDs) > 0 {
			ids = append([]string(nil), evidenceIDs...)
		}
		if err := addClaim(c.Claim, schema.ClaimInvariant, schema.ClaimProposed, ids, c.Contradicts, c.Invalidates); err != nil {
			return nil, err
		}
	}
	for _, assumption := range output.Assumptions {
		if err := addClaim(assumption, schema.ClaimAssumption, schema.ClaimProposed, nil, nil, nil); err != nil {
			return nil, err
		}
	}
	for _, risk := range output.Risks {
		if err := addClaim(risk, schema.ClaimRisk, schema.ClaimProposed, nil, nil, nil); err != nil {
			return nil, err
		}
	}
	for _, followUp := range output.FollowUpNeeded {
		if err := addClaim(followUp, schema.ClaimOpenQuestion, schema.ClaimProposed, nil, nil, nil); err != nil {
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
	// git diff HEAD covers both staged and unstaged changes relative to the last
	// commit, so new files added to the index are included in the patch diff.
	diffCmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--binary", "HEAD")
	diffCmd.Dir = worktreePath
	diff, err := diffCmd.CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("runner: git diff in %s: %w: %s", worktreePath, err, strings.TrimSpace(string(diff)))
	}
	if err := os.WriteFile(diffPath, diff, 0o644); err != nil {
		return "", nil, fmt.Errorf("runner: write patch diff %s: %w", diffPath, err)
	}

	nameCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--relative", "HEAD")
	nameCmd.Dir = worktreePath
	namesRaw, err := nameCmd.CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("runner: git diff --name-only in %s: %w: %s", worktreePath, err, strings.TrimSpace(string(namesRaw)))
	}
	seen := make(map[string]bool)
	changed := make([]string, 0)
	for _, name := range strings.Split(strings.TrimSpace(string(namesRaw)), "\n") {
		name = filepath.Clean(strings.TrimSpace(strings.TrimRight(name, "\r")))
		if name == "" || name == "." || seen[name] {
			continue
		}
		seen[name] = true
		changed = append(changed, name)
	}

	return diffPath, changed, nil
}

func currentCommit(ctx context.Context, worktreePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("runner: git rev-parse HEAD in %s: %w: %s", worktreePath, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
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

func filterObligations(claimed, allowed []string) []string {
	if len(allowed) == 0 {
		return append([]string(nil), claimed...)
	}
	set := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	out := make([]string, 0, len(claimed))
	for _, id := range claimed {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
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

// classifyPreflightError maps a preflight error to a CapsuleRuntimeFailureClass.
// Worktree errors (git status, clean check) map to worktree_state.
// Permission-related errors map to permission_gate. Others map to tool_runtime.
func classifyPreflightError(err error) schema.CapsuleRuntimeFailureClass {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "worktree") ||
		strings.Contains(msg, "git status") ||
		strings.Contains(msg, "uncommitted") ||
		strings.Contains(msg, "clean worktree"):
		return schema.RuntimeFailureWorktreeState
	case strings.Contains(msg, "permission") ||
		strings.Contains(msg, "denied") ||
		strings.Contains(msg, "not allowed"):
		return schema.RuntimeFailurePermissionGate
	default:
		return schema.RuntimeFailureToolRuntime
	}
}
