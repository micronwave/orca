package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/budget"
	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/gate"
	"github.com/micronwave/orca/internal/intent"
	"github.com/micronwave/orca/internal/planner"
	"github.com/micronwave/orca/internal/projector"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/runner/adapters/claude"
	"github.com/micronwave/orca/internal/runner/adapters/codex"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
	"github.com/micronwave/orca/internal/verifier"
)

// Phase 1 decision: the orchestrator wires deterministic, rule-based component
// implementations only. No model SDK, provider hook, or model config is part of
// this pre-build scaffold.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("orca: command is required (init, goal, status, cancel)")
	}
	if strings.HasPrefix(args[0], "-") {
		return runGoal(args)
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "goal":
		return runGoal(args[1:])
	case "status":
		return runStatus(args[1:])
	case "cancel":
		return runCancel(args[1:], os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("orca: unknown command %q", args[0])
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("orca init", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", ".orca", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := ensureInitTarget(*orcaDir); err != nil {
		return err
	}
	log, err := eventlog.Open(filepath.Join(*orcaDir, "events.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	if _, err := store.New(*orcaDir, log); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*orcaDir, "capsules"), 0o755); err != nil {
		return fmt.Errorf("orca init: create capsules dir: %w", err)
	}
	configPath := filepath.Join(*orcaDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(defaultConfigYAML()), 0o644); err != nil {
		return fmt.Errorf("orca init: write config.yaml: %w", err)
	}
	return nil
}

func runGoal(args []string) error {
	fs := flag.NewFlagSet("orca goal", flag.ContinueOnError)
	goalFlag := fs.String("goal", "", "user goal to execute")
	orcaDir := fs.String("orca-dir", ".orca", "path to the .orca directory")
	noLearning := fs.Bool("no-learning", false, "disable adaptive reuse (learning layer)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	goalText := strings.TrimSpace(*goalFlag)
	if goalText == "" {
		goalText = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if goalText == "" {
		return fmt.Errorf("orca goal: goal text is required")
	}
	rt, closeFn, err := openRuntime(*orcaDir, *noLearning)
	if err != nil {
		return err
	}
	defer closeFn()

	active, err := rt.store.LoadActiveGoal(context.Background())
	if err != nil {
		return fmt.Errorf("orca goal: load active goal: %w", err)
	}
	if active != nil {
		return activeGoalError(active)
	}
	return rt.runControlLoop(context.Background(), goalText)
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("orca status", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", ".orca", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, closeFn, err := openRuntime(*orcaDir, false)
	if err != nil {
		return err
	}
	defer closeFn()
	return rt.printStatus(context.Background(), os.Stdout)
}

func runCancel(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("orca cancel", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", ".orca", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, closeFn, err := openRuntime(*orcaDir, false)
	if err != nil {
		return err
	}
	defer closeFn()
	return rt.cancelActiveGoal(context.Background(), in, out)
}

func openRuntime(orcaDir string, noLearning bool) (*runtime, func(), error) {
	cfg, err := config.Load(filepath.Join(orcaDir, "config.yaml"))
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		return nil, nil, fmt.Errorf("orca: loaded nil config")
	}
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		return nil, nil, err
	}
	artifactStore, err := store.New(orcaDir, log)
	if err != nil {
		_ = log.Close()
		return nil, nil, err
	}
	rt, err := newRuntime(cfg, orcaDir, noLearning, log, artifactStore)
	if err != nil {
		_ = log.Close()
		return nil, nil, err
	}
	return rt, func() { _ = log.Close() }, nil
}

type runtime struct {
	cfg        *config.Config
	orcaDir    string
	noLearning bool

	eventLog eventlog.EventLog
	store    store.ArtifactStore

	intentCompiler intent.IntentCompiler
	verifierEngine verifier.VerifierEngine
	planner        planner.ObligationPlanner
	projector      projector.ContextCompiler
	gatekeeper     gate.HumanGatekeeper
	budget         budget.BudgetController
	runner         runner.CapsuleRunner
	reconciler     reconciler.Reconciler
}

func newRuntime(cfg *config.Config, orcaDir string, noLearning bool, log eventlog.EventLog, st store.ArtifactStore) (*runtime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("orca: config is required")
	}
	if log == nil {
		return nil, fmt.Errorf("orca: event log is required")
	}
	if st == nil {
		return nil, fmt.Errorf("orca: artifact store is required")
	}
	return &runtime{
		cfg:        cfg,
		orcaDir:    orcaDir,
		noLearning: noLearning,
		eventLog:   log,
		store:      st,

		intentCompiler: newIntentCompiler(st),
		verifierEngine: newVerifierEngine(st, cfg.Verifier),
		planner:        newPlanner(st, cfg.Budget, orcaDir),
		projector:      newProjector(st, cfg.Verifier),
		gatekeeper:     newGatekeeper(st, cfg.Gate),
		budget:         newBudgetController(log, cfg.Budget),
		runner:         newCapsuleRunner(st, log, orcaDir, cfg.Adapters),
		reconciler:     newReconciler(st, log),
	}, nil
}

func (rt *runtime) runControlLoop(ctx context.Context, rawIntent string) error {
	goal, err := rt.intentCompiler.Compile(ctx, rawIntent)
	if err != nil {
		return err
	}
	if _, err := rt.verifierEngine.ProposeObligations(ctx, goal.GoalID); err != nil {
		return err
	}

	for {
		plan, err := rt.planner.Plan(ctx, goal.GoalID)
		if err != nil {
			return err
		}
		if len(plan.CapsuleIDs) == 0 {
			return fmt.Errorf("orca: planner returned no capsules for goal %s", goal.GoalID)
		}
		if err := rt.emitTopologySelected(ctx, goal.GoalID, plan); err != nil {
			return err
		}

		var lastPatchID string
		var lastResult reconciler.ReconcileResult
		for _, capsuleID := range plan.CapsuleIDs {
			capsule, err := rt.store.LoadCapsule(ctx, capsuleID)
			if err != nil {
				return fmt.Errorf("orca: load capsule %s: %w", capsuleID, err)
			}
			if _, err := rt.projector.CompileHumanSummary(ctx, capsuleID); err != nil {
				return err
			}
			// Gate only executor capsules: spec says "ReviewProjection blocks before
			// implementer capsule" (module_boundaries.md). Reviewer capsules do not
			// require a separate pre-execution gate. Also use plan.MaxObligationRisk
			// rather than goal.RiskLevel: goal risk and obligation risk are set by
			// different components and may disagree.
			if capsule.Role != schema.RoleReviewer && shouldReviewProjection(plan.Topology, plan.MaxObligationRisk) {
				reviewWindow := reviewWindowFor(plan.Topology, plan.MaxObligationRisk, time.Duration(rt.cfg.Gate.ReviewWindowSeconds)*time.Second)
				decision, err := rt.gatekeeper.ReviewProjection(ctx, capsuleID, reviewWindow)
				if err != nil {
					return err
				}
				if !decision.Approved {
					return fmt.Errorf("orca: projection gate rejected capsule %s: %s", capsuleID, decision.Notes)
				}
			}

			check, err := rt.budget.CheckCapsuleBudget(ctx, capsuleID)
			if err != nil {
				return err
			}
			if !check.Allowed {
				return fmt.Errorf("orca: budget rejected capsule %s: %s", capsuleID, check.Reason)
			}
			executorProjection, err := rt.projector.CompileExecutor(ctx, capsuleID)
			if err != nil {
				return err
			}
			if err := rt.store.UpdateCapsuleProjectionID(ctx, capsuleID, executorProjection.ContextProjectionID); err != nil {
				return err
			}
			runResult, err := rt.runner.Run(ctx, capsuleID)
			if err != nil {
				return err
			}
			if runResult.PatchID == "" {
				return fmt.Errorf("orca: capsule %s produced no patch", capsuleID)
			}
			verifyResult, err := rt.verifierEngine.Verify(ctx, runResult.PatchID)
			if err != nil {
				return err
			}
			lastPatchID = verifyResult.PatchID
			lastResult, err = rt.reconciler.Reconcile(ctx, verifyResult.PatchID)
			if err != nil {
				return err
			}
			if _, err := rt.budget.ComputeROI(ctx, goal.GoalID); err != nil {
				return err
			}
		}

		if lastResult.MergeReady {
			if lastResult.HumanGateRequired {
				decision, err := rt.gatekeeper.ReviewMerge(ctx, lastPatchID)
				if err != nil {
					return err
				}
				if !decision.Approved {
					return fmt.Errorf("orca: merge gate rejected patch %s: %s", lastPatchID, decision.Notes)
				}
				if err := rt.appendMergeApplied(ctx, goal.GoalID, lastPatchID); err != nil {
					return err
				}
			}
			return rt.updateGoalStatus(ctx, goal.GoalID, schema.GoalStatusComplete)
		}
		if len(lastResult.FollowUpObligationIDs) > 0 {
			continue
		}
		return fmt.Errorf("orca: reconciliation stopped: %s", lastResult.BlockingReason)
	}
}

func ensureInitTarget(orcaDir string) error {
	if strings.TrimSpace(orcaDir) == "" {
		return fmt.Errorf("orca init: --orca-dir is required")
	}
	info, err := os.Stat(orcaDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(orcaDir, 0o755); err != nil {
			return fmt.Errorf("orca init: create %s: %w", orcaDir, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("orca init: stat %s: %w", orcaDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("orca init: %s exists and is not a directory", orcaDir)
	}
	entries, err := os.ReadDir(orcaDir)
	if err != nil {
		return fmt.Errorf("orca init: read %s: %w", orcaDir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("orca init: %s already exists and is non-empty", orcaDir)
	}
	return nil
}

func defaultConfigYAML() string {
	return `# Orca Phase 1 local runtime configuration.
# Keep this file in the simple shape supported by internal/config.Load:
# sections, scalar values, and verifier.gates list items only.

verifier:
  # Gates run from working_dir when set; empty means the current process directory.
  working_dir: ""
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
    - name: "go_vet"
      command: "go vet ./..."
      blocking: true
    - name: "go_build"
      command: "go build ./..."
      blocking: true

gate:
  review_window_seconds: 30

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
  default_max_retries: 3

adapters:
  # Leave empty to resolve from PATH.
  codex_path: ""
  claude_path: ""
`
}

func activeGoalError(goal *schema.GoalIR) error {
	if goal == nil {
		return nil
	}
	return fmt.Errorf(`Error: an active goal already exists (goal_id: %s).
  Intent: %q
  Status: %s

To start a new goal, first complete or cancel the current one:
  orca cancel
  orca status`, goal.GoalID, goal.OriginalIntent, goal.Status)
}

func (rt *runtime) printStatus(ctx context.Context, out io.Writer) error {
	goal, err := rt.store.LoadActiveGoal(ctx)
	if err != nil {
		return fmt.Errorf("orca status: load active goal: %w", err)
	}
	if goal == nil {
		_, err := fmt.Fprintln(out, "No active goal.")
		return err
	}
	obligations, err := rt.store.LoadOpenObligations(ctx, goal.GoalID)
	if err != nil {
		return fmt.Errorf("orca status: load open obligations: %w", err)
	}
	capsules, err := rt.activeCapsulesForGoal(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	latestVerifier, err := rt.latestVerifierResultForGoal(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	humanDecisions, err := rt.blockingHumanDecisions(ctx, goal, capsules, latestVerifier, obligations)
	if err != nil {
		return err
	}
	readiness, err := rt.mergeReadiness(ctx, latestVerifier, obligations, humanDecisions)
	if err != nil {
		return err
	}
	budgetRecords, err := rt.store.LoadBudgetForGoal(ctx, goal.GoalID)
	if err != nil {
		return fmt.Errorf("orca status: load budget records: %w", err)
	}
	roi, err := rt.budget.ComputeROI(ctx, goal.GoalID)
	if err != nil {
		return fmt.Errorf("orca status: compute budget ROI: %w", err)
	}

	fmt.Fprintf(out, "Active goal: %s\n", goal.GoalID)
	fmt.Fprintf(out, "Intent: %s\n", goal.OriginalIntent)
	fmt.Fprintf(out, "Status: %s\n", goal.Status)
	fmt.Fprintln(out, "Conditions:")
	for _, condition := range goal.GoalConditions {
		fmt.Fprintf(out, "- %s [%s]: %s\n", condition.ID, condition.Status, condition.Description)
	}
	fmt.Fprintf(out, "Open obligations: %d\n", len(obligations))
	sort.Slice(obligations, func(i, j int) bool { return obligations[i].ObligationID < obligations[j].ObligationID })
	for _, obligation := range obligations {
		fmt.Fprintf(out, "- %s [%s]\n", obligation.ObligationID, obligation.RiskLevel)
	}
	fmt.Fprintf(out, "Active capsules: %d\n", len(capsules))
	for _, capsule := range capsules {
		fmt.Fprintf(out, "- %s [%s] agent=%s\n", capsule.CapsuleID, capsule.State, capsule.Agent)
	}
	if latestVerifier == nil {
		fmt.Fprintln(out, "Last verifier result: none")
	} else {
		fmt.Fprintf(out, "Last verifier result: %s action=%s", latestVerifier.VerifierResultID, latestVerifier.RecommendedAction)
		if latestVerifier.RecommendationRationale != "" {
			fmt.Fprintf(out, " summary=%q", latestVerifier.RecommendationRationale)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "Merge readiness: %s\n", readiness)
	fmt.Fprintln(out, "Blocking human decisions:")
	if len(humanDecisions) == 0 {
		fmt.Fprintln(out, "- none")
	} else {
		for _, decision := range humanDecisions {
			fmt.Fprintf(out, "- %s\n", decision)
		}
	}
	fmt.Fprintln(out, "Budget spent per obligation:")
	writeBudgetByObligation(out, budgetRecords)
	fmt.Fprintf(out, "Budget totals: tokens=%d wall_time_seconds=%.2f coordination_cost=%d value_per_1k_tokens=%.2f\n",
		roi.TotalTokensSpent,
		roi.TotalWallTimeSeconds,
		roi.TotalCoordinationCost,
		roi.VerifiedValuePer1KTokens,
	)
	return nil
}

func (rt *runtime) cancelActiveGoal(ctx context.Context, in io.Reader, out io.Writer) error {
	goal, err := rt.store.LoadActiveGoal(ctx)
	if err != nil {
		return fmt.Errorf("orca cancel: load active goal: %w", err)
	}
	if goal == nil {
		_, err := fmt.Fprintln(out, "No active goal.")
		return err
	}
	capsules, err := rt.activeCapsulesForGoal(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	if len(capsules) > 0 {
		fmt.Fprintf(out, "Active capsules are still running or pending for goal %s.\n", goal.GoalID)
		fmt.Fprint(out, "Type 'cancel' to cancel the active goal: ")
		var response string
		if _, err := fmt.Fscan(in, &response); err != nil {
			return fmt.Errorf("orca cancel: read confirmation: %w", err)
		}
		if !strings.EqualFold(response, "cancel") {
			fmt.Fprintln(out, "Cancel aborted.")
			return nil
		}
	}
	if err := rt.updateGoalStatus(ctx, goal.GoalID, schema.GoalStatusCancelled); err != nil {
		return err
	}
	fmt.Fprintf(out, "Cancelled goal %s.\n", goal.GoalID)
	return nil
}

func (rt *runtime) updateGoalStatus(ctx context.Context, goalID string, status schema.GoalStatus) error {
	payload, err := json.Marshal(schema.GoalStatusPayload{GoalID: goalID, Status: status})
	if err != nil {
		return fmt.Errorf("orca: marshal goal_status_updated payload: %w", err)
	}
	if _, err := rt.eventLog.Append(ctx, schema.Event{
		Type:       schema.EventGoalStatusUpdated,
		GoalID:     goalID,
		ArtifactID: goalID,
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("orca: append goal_status_updated: %w", err)
	}
	if err := rt.store.UpdateGoalStatus(ctx, goalID, status); err != nil {
		return fmt.Errorf("orca: update goal status: %w", err)
	}
	return nil
}

func (rt *runtime) appendMergeApplied(ctx context.Context, goalID, patchID string) error {
	payload, err := json.Marshal(schema.PatchStatusPayload{PatchID: patchID})
	if err != nil {
		return fmt.Errorf("orca: marshal merge_applied payload: %w", err)
	}
	if _, err := rt.eventLog.Append(ctx, schema.Event{
		Type:       schema.EventMergeApplied,
		GoalID:     goalID,
		ArtifactID: patchID,
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("orca: append merge_applied: %w", err)
	}
	return nil
}

func (rt *runtime) activeCapsulesForGoal(ctx context.Context, goalID string) ([]*schema.ExecutionCapsule, error) {
	var capsules []*schema.ExecutionCapsule
	seen := make(map[string]bool)
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return nil, fmt.Errorf("orca: read events for active capsules: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			if event.Type != schema.EventCapsuleCreated || event.ArtifactID == "" || seen[event.ArtifactID] {
				continue
			}
			seen[event.ArtifactID] = true
			capsule, err := rt.store.LoadCapsule(ctx, event.ArtifactID)
			if err != nil {
				return nil, fmt.Errorf("orca: load capsule %s: %w", event.ArtifactID, err)
			}
			if isActiveCapsule(capsule.State) {
				capsules = append(capsules, capsule)
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	sort.Slice(capsules, func(i, j int) bool { return capsules[i].CapsuleID < capsules[j].CapsuleID })
	return capsules, nil
}

func (rt *runtime) latestVerifierResultForGoal(ctx context.Context, goalID string) (*schema.VerifierResult, error) {
	var latest *schema.VerifierResult
	var latestSeq int64
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return nil, fmt.Errorf("orca status: read events for verifier result: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			if event.Type != schema.EventVerifierResultCreated {
				continue
			}
			var result schema.VerifierResult
			if err := json.Unmarshal(event.Payload, &result); err != nil {
				return nil, fmt.Errorf("orca status: unmarshal verifier result %s: %w", event.ArtifactID, err)
			}
			if event.SequenceNum > latestSeq {
				latestSeq = event.SequenceNum
				latest = &result
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return latest, nil
}

func (rt *runtime) blockingHumanDecisions(
	ctx context.Context,
	goal *schema.GoalIR,
	capsules []*schema.ExecutionCapsule,
	latest *schema.VerifierResult,
	openObligations []*schema.Obligation,
) ([]string, error) {
	maxRisk := maxObligationRisk(openObligations)
	var decisions []string
	for _, capsule := range capsules {
		if strings.TrimSpace(capsule.TopologyDecisionID) == "" {
			continue
		}
		if capsule.Role == schema.RoleReviewer {
			continue
		}
		decision, err := rt.store.LoadDecision(ctx, capsule.TopologyDecisionID)
		if err != nil {
			return nil, fmt.Errorf("orca status: load topology decision %s: %w", capsule.TopologyDecisionID, err)
		}
		if shouldReviewProjection(schema.Topology(decision.Decision), maxRisk) {
			decided, err := rt.hasGateDecision(ctx, goal.GoalID, "projection_review", capsule.CapsuleID)
			if err != nil {
				return nil, err
			}
			if !decided {
				decisions = append(decisions, fmt.Sprintf("projection_review capsule=%s", capsule.CapsuleID))
			}
		}
	}
	if latest != nil && noOpenBlockingObligations(openObligations) {
		patch, err := rt.store.LoadPatch(ctx, latest.PatchID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("orca status: load patch %s: %w", latest.PatchID, err)
		}
		if err == nil && patch.Status == schema.PatchAccepted {
			highRisk, err := rt.verifierResultHasHighRiskObligation(ctx, latest)
			if err != nil {
				return nil, err
			}
			if highRisk {
				decided, err := rt.hasGateDecision(ctx, goal.GoalID, "merge_review", latest.PatchID)
				if err != nil {
					return nil, err
				}
				if !decided {
					decisions = append(decisions, fmt.Sprintf("merge_review patch=%s", latest.PatchID))
				}
			}
		}
	}
	sort.Strings(decisions)
	return decisions, nil
}

func (rt *runtime) hasGateDecision(ctx context.Context, goalID, gateContext, relatedID string) (bool, error) {
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return false, fmt.Errorf("orca status: read decisions: %w", err)
		}
		if len(events) == 0 {
			return false, nil
		}
		for _, event := range events {
			if event.Type != schema.EventDecisionRecordCreated {
				continue
			}
			var decision schema.DecisionRecord
			if err := json.Unmarshal(event.Payload, &decision); err != nil {
				return false, fmt.Errorf("orca status: unmarshal decision %s: %w", event.ArtifactID, err)
			}
			if decision.Context == gateContext && slicesContains(decision.RelatedIDs, relatedID) {
				return true, nil
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
}

func (rt *runtime) verifierResultHasHighRiskObligation(ctx context.Context, result *schema.VerifierResult) (bool, error) {
	for _, verdict := range result.ObligationResults {
		obligation, err := rt.store.LoadObligation(ctx, verdict.ObligationID)
		if err != nil {
			return false, fmt.Errorf("orca status: load obligation %s: %w", verdict.ObligationID, err)
		}
		if obligation.RiskLevel == schema.RiskHigh {
			return true, nil
		}
	}
	return false, nil
}

func isActiveCapsule(state schema.CapsuleState) bool {
	switch state {
	case schema.CapsuleStatePending,
		schema.CapsuleStateWorktreeCreated,
		schema.CapsuleStateWorkspaceAttached,
		schema.CapsuleStateSetupRun,
		schema.CapsuleStateAgentRunning:
		return true
	default:
		return false
	}
}

func (rt *runtime) mergeReadiness(
	ctx context.Context,
	result *schema.VerifierResult,
	openObligations []*schema.Obligation,
	humanDecisions []string,
) (string, error) {
	if result == nil {
		return "unknown", nil
	}
	if !noOpenBlockingObligations(openObligations) || len(result.BlockingFailures) > 0 {
		return "blocked", nil
	}
	patch, err := rt.store.LoadPatch(ctx, result.PatchID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "pending_reconciliation", nil
		}
		return "", fmt.Errorf("orca status: load patch %s: %w", result.PatchID, err)
	}
	switch patch.Status {
	case schema.PatchAccepted:
		if len(humanDecisions) > 0 {
			return "needs_human_review", nil
		}
		return "ready", nil
	case schema.PatchCandidate:
		return "pending_reconciliation", nil
	default:
		return "blocked", nil
	}
}

func noOpenBlockingObligations(obligations []*schema.Obligation) bool {
	for _, obligation := range obligations {
		if obligation.Blocking {
			return false
		}
	}
	return true
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func maxObligationRisk(obligations []*schema.Obligation) schema.RiskLevel {
	max := schema.RiskLow
	for _, o := range obligations {
		switch o.RiskLevel {
		case schema.RiskHigh:
			return schema.RiskHigh
		case schema.RiskMedium:
			max = schema.RiskMedium
		}
	}
	return max
}

func writeBudgetByObligation(out io.Writer, records []*schema.BudgetRecord) {
	byObligation := make(map[string]schema.BudgetRecord)
	for _, record := range records {
		if record.ObligationID == "" {
			continue
		}
		current := byObligation[record.ObligationID]
		current.ObligationID = record.ObligationID
		current.TokensSpent += record.TokensSpent
		current.WallTimeSeconds += record.WallTimeSeconds
		current.ToolCalls += record.ToolCalls
		current.Retries += record.Retries
		current.DuplicatedFileReads += record.DuplicatedFileReads
		current.OverlappingEdits += record.OverlappingEdits
		current.HumanInterventions += record.HumanInterventions
		current.ObligationsDischarged += record.ObligationsDischarged
		current.PatchesAccepted += record.PatchesAccepted
		current.PatchesRejected += record.PatchesRejected
		byObligation[record.ObligationID] = current
	}
	if len(byObligation) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	ids := make([]string, 0, len(byObligation))
	for id := range byObligation {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := byObligation[id]
		coordinationCost := record.Retries + record.DuplicatedFileReads + record.OverlappingEdits + record.HumanInterventions
		fmt.Fprintf(out, "- %s tokens=%d wall_time_seconds=%.2f tool_calls=%d retries=%d coordination_cost=%d discharged=%d accepted=%d rejected=%d\n",
			id,
			record.TokensSpent,
			record.WallTimeSeconds,
			record.ToolCalls,
			record.Retries,
			coordinationCost,
			record.ObligationsDischarged,
			record.PatchesAccepted,
			record.PatchesRejected,
		)
	}
}

type topologySelectedPayload struct {
	Topology   schema.Topology `json:"topology"`
	DecisionID string          `json:"decision_id"`
}

func (rt *runtime) emitTopologySelected(ctx context.Context, goalID string, plan planner.PlanResult) error {
	payload, err := json.Marshal(topologySelectedPayload{
		Topology:   plan.Topology,
		DecisionID: plan.DecisionID,
	})
	if err != nil {
		return fmt.Errorf("orca: marshal topology_selected payload: %w", err)
	}
	_, err = rt.eventLog.Append(ctx, schema.Event{
		Type:       schema.EventTopologySelected,
		GoalID:     goalID,
		ArtifactID: plan.DecisionID,
		Payload:    payload,
	})
	return err
}

func shouldReviewProjection(topology schema.Topology, risk schema.RiskLevel) bool {
	switch topology {
	case schema.TopologyHumanGated:
		return true
	case schema.TopologyImplementerReviewer:
		return risk == schema.RiskMedium || risk == schema.RiskHigh
	case schema.TopologySingle:
		return risk == schema.RiskLow
	default:
		return false
	}
}

func reviewWindowFor(topology schema.Topology, risk schema.RiskLevel, defaultWindow time.Duration) time.Duration {
	if topology == schema.TopologyHumanGated {
		return 0
	}
	// IR topology implies medium risk by classifier invariant; block indefinitely.
	if topology == schema.TopologyImplementerReviewer {
		return 0
	}
	return defaultWindow
}

func newIntentCompiler(st store.ArtifactStore) intent.IntentCompiler {
	return intent.New(st)
}

func newVerifierEngine(st store.ArtifactStore, cfg config.VerifierConfig) verifier.VerifierEngine {
	return verifier.New(st, cfg, nil)
}

func newPlanner(st store.ArtifactStore, cfg config.BudgetConfig, orcaDir string) planner.ObligationPlanner {
	return planner.New(st, planner.Config{
		OrcaDir:            orcaDir,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   cfg.DefaultMaxTokens,
		DefaultMaxWallTime: cfg.DefaultMaxWallTimeSeconds,
		DefaultMaxRetries:  cfg.DefaultMaxRetries,
	})
}

func newProjector(st store.ArtifactStore, cfg config.VerifierConfig) projector.ContextCompiler {
	return projector.New(st, cfg)
}

func newGatekeeper(st store.ArtifactStore, cfg config.GateConfig) gate.HumanGatekeeper {
	return gate.New(st)
}

func newBudgetController(log eventlog.EventLog, cfg config.BudgetConfig) budget.BudgetController {
	return budget.New(log)
}

func newCapsuleRunner(st store.ArtifactStore, log eventlog.EventLog, orcaDir string, cfg config.AdapterConfig) runner.CapsuleRunner {
	return runner.New(
		st,
		log,
		orcaDir,
		codex.New(orcaDir, cfg.CodexPath),
		claude.New(orcaDir, cfg.ClaudePath),
	)
}

func newReconciler(st store.ArtifactStore, log eventlog.EventLog) reconciler.Reconciler {
	return reconciler.New(st, log)
}
