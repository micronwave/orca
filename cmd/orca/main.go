package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
	"github.com/micronwave/orca/internal/verifier"
)

var errNotYetImplemented = errors.New("not yet implemented")

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
	fs := flag.NewFlagSet("orca", flag.ContinueOnError)
	goal := fs.String("goal", "", "user goal to execute")
	orcaDir := fs.String("orca-dir", ".orca", "path to the .orca directory")
	noLearning := fs.Bool("no-learning", false, "disable adaptive reuse; no-op in Phase 1 scaffold")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*goal) == "" {
		return fmt.Errorf("orca: --goal is required")
	}

	cfg, err := config.Load(filepath.Join(*orcaDir, "config.yaml"))
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("orca: loaded nil config")
	}

	log, err := eventlog.Open(filepath.Join(*orcaDir, "events.log"))
	if err != nil {
		return err
	}
	defer log.Close()

	artifactStore, err := store.New(*orcaDir, log)
	if err != nil {
		return err
	}

	rt, err := newRuntime(cfg, *orcaDir, *noLearning, log, artifactStore)
	if err != nil {
		return err
	}
	return rt.runControlLoop(context.Background(), *goal)
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
		runner:         newCapsuleRunner(st, log, cfg.Adapters),
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
			if _, err := rt.projector.CompileHumanSummary(ctx, capsuleID); err != nil {
				return err
			}
			if shouldReviewProjection(plan.Topology, goal.RiskLevel) {
				reviewWindow := reviewWindowFor(plan.Topology, goal.RiskLevel, time.Duration(rt.cfg.Gate.ReviewWindowSeconds)*time.Second)
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
			if _, err := rt.projector.CompileExecutor(ctx, capsuleID); err != nil {
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
			}
			return nil
		}
		if len(lastResult.FollowUpObligationIDs) > 0 {
			continue
		}
		return fmt.Errorf("orca: reconciliation stopped: %s", lastResult.BlockingReason)
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
		// IR topology is only selected when obligations contain medium risk (count > 1),
		// so the gate always applies regardless of the goal-level risk field.
		return true
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

func notYetImplemented(name string) error {
	return fmt.Errorf("%s: %w", name, errNotYetImplemented)
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

type gatekeeperStub struct {
	store  store.ArtifactStore
	config config.GateConfig
}

func newGatekeeper(st store.ArtifactStore, cfg config.GateConfig) gate.HumanGatekeeper {
	return gatekeeperStub{store: st, config: cfg}
}

func (s gatekeeperStub) ReviewProjection(context.Context, string, time.Duration) (gate.GateDecision, error) {
	return gate.GateDecision{}, notYetImplemented("human projection gate Phase 1 implementation")
}

func (s gatekeeperStub) ReviewMerge(context.Context, string) (gate.GateDecision, error) {
	return gate.GateDecision{}, notYetImplemented("human merge gate Phase 1 implementation")
}

func (s gatekeeperStub) ReviewWaiver(context.Context, string, string) (gate.GateDecision, error) {
	return gate.GateDecision{}, notYetImplemented("human waiver gate Phase 1 implementation")
}

type budgetControllerStub struct {
	log    eventlog.EventLog
	config config.BudgetConfig
}

func newBudgetController(log eventlog.EventLog, cfg config.BudgetConfig) budget.BudgetController {
	return budgetControllerStub{log: log, config: cfg}
}

func (s budgetControllerStub) CheckCapsuleBudget(context.Context, string) (budget.BudgetCheck, error) {
	return budget.BudgetCheck{}, notYetImplemented("budget controller Phase 1 implementation")
}

func (s budgetControllerStub) ComputeROI(context.Context, string) (budget.ROI, error) {
	return budget.ROI{}, notYetImplemented("budget ROI Phase 1 implementation")
}

type capsuleRunnerStub struct {
	store  store.ArtifactStore
	log    eventlog.EventLog
	config config.AdapterConfig
}

func newCapsuleRunner(st store.ArtifactStore, log eventlog.EventLog, cfg config.AdapterConfig) runner.CapsuleRunner {
	return capsuleRunnerStub{store: st, log: log, config: cfg}
}

func (s capsuleRunnerStub) Run(context.Context, string) (runner.RunResult, error) {
	return runner.RunResult{}, notYetImplemented("capsule runner Phase 1 implementation")
}

type reconcilerStub struct {
	store store.ArtifactStore
	log   eventlog.EventLog
}

func newReconciler(st store.ArtifactStore, log eventlog.EventLog) reconciler.Reconciler {
	return reconcilerStub{store: st, log: log}
}

func (s reconcilerStub) Reconcile(context.Context, string) (reconciler.ReconcileResult, error) {
	return reconciler.ReconcileResult{}, notYetImplemented("reconciler Phase 1 implementation")
}
