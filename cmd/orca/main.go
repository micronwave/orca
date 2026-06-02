package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goos "runtime"
	"sort"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/budget"
	"github.com/micronwave/orca/internal/cigate"
	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/gate"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/intake"
	"github.com/micronwave/orca/internal/intent"
	"github.com/micronwave/orca/internal/mcp"
	"github.com/micronwave/orca/internal/planner"
	"github.com/micronwave/orca/internal/projector"
	"github.com/micronwave/orca/internal/prwriter"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/runner/adapters/claude"
	"github.com/micronwave/orca/internal/runner/adapters/codex"
	"github.com/micronwave/orca/internal/runner/adapters/remote"
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
		if !isatty(os.Stdin) {
			return fmt.Errorf("orca: command is required (init, goal, status, cancel)")
		}
		orcaDir := filepath.Join(findProjectRoot("."), ".orca")
		return runInteractive(orcaDir)
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
	case "ci":
		return runCI(args[1:])
	case "ui":
		return runUI(args[1:])
	default:
		return fmt.Errorf("orca: unknown command %q", args[0])
	}
}

func runInit(args []string) (err error) {
	fs := flag.NewFlagSet("orca init", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}
	if err := ensureInitTarget(*orcaDir); err != nil {
		return err
	}
	log, err := eventlog.Open(filepath.Join(*orcaDir, "events.log"))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if _, err := store.New(*orcaDir, log); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*orcaDir, "capsules"), 0o755); err != nil {
		return fmt.Errorf("orca init: create capsules dir: %w", err)
	}
	configPath := filepath.Join(*orcaDir, "config.yaml")
	projectType := config.DetectProjectType(".")
	if err := os.WriteFile(configPath, []byte(config.DefaultConfigYAML(projectType)), 0o644); err != nil {
		return fmt.Errorf("orca init: write config.yaml: %w", err)
	}
	return nil
}

func runGoal(args []string) (err error) {
	fs := flag.NewFlagSet("orca goal", flag.ContinueOnError)
	goalFlag := fs.String("goal", "", "user goal to execute")
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	noLearning := fs.Bool("no-learning", false, "disable adaptive reuse (learning layer)")
	fromIssue := fs.Int("from-issue", 0, "GitHub issue number to use as goal input")
	flagPlain := fs.Bool("plain", false, "use plain text progress output (default)")
	flagVerbose := fs.Bool("verbose", false, "use plain text progress output with extra detail")
	flagJSON := fs.Bool("json", false, "emit lifecycle events as newline-delimited JSON to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}
	goalText := strings.TrimSpace(*goalFlag)
	if goalText == "" {
		goalText = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if *fromIssue > 0 && goalText != "" {
		return fmt.Errorf("orca goal: --from-issue and --goal are mutually exclusive")
	}
	if *fromIssue <= 0 && goalText == "" {
		return fmt.Errorf("orca goal: goal text is required (or use --from-issue)")
	}
	if err := autoInitWithConfirmation(*orcaDir, os.Stdin, os.Stderr); err != nil {
		return err
	}
	rt, closeFn, err := openRuntime(*orcaDir, *noLearning)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeFn(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	switch {
	case *flagJSON:
		rt.notifier = newJSONNotifier(os.Stderr)
	case *flagVerbose:
		rt.notifier = newPlainNotifier(os.Stderr, true)
	default:
		// --plain is the default; preserves the previous [orca] stderr lines.
		_ = flagPlain
		rt.notifier = newPlainNotifier(os.Stderr, false)
	}
	if err := rt.cfg.Verifier.ValidateGates(); err != nil {
		return err
	}

	active, err := rt.store.LoadActiveGoal(context.Background())
	if err != nil {
		return fmt.Errorf("orca goal: load active goal: %w", err)
	}
	if active != nil {
		return activeGoalError(active)
	}
	if *fromIssue > 0 {
		return rt.runFromIssue(context.Background(), *fromIssue)
	}
	return rt.runControlLoop(context.Background(), goalText)
}

func runStatus(args []string) (err error) {
	fs := flag.NewFlagSet("orca status", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	raw := fs.Bool("raw", false, "show detailed operational dump (artifact IDs, budget, MCP, CI)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}
	rt, closeFn, openErr := openRuntime(*orcaDir, false)
	if openErr != nil {
		return openErr
	}
	defer func() {
		if closeErr := closeFn(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if *raw {
		return rt.printStatus(context.Background(), os.Stdout)
	}
	return rt.printStatusConcise(context.Background(), os.Stdout)
}

func runCancel(args []string, in io.Reader, out io.Writer) (err error) {
	fs := flag.NewFlagSet("orca cancel", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}
	rt, closeFn, openErr := openRuntime(*orcaDir, false)
	if openErr != nil {
		return openErr
	}
	defer func() {
		if closeErr := closeFn(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	return rt.cancelActiveGoal(context.Background(), in, out)
}

// ── CI subcommands ────────────────────────────────────────────────────────────

func runCI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("orca ci: command is required (wait)")
	}
	switch args[0] {
	case "wait":
		return runCIWait(args[1:])
	default:
		return fmt.Errorf("orca ci: unknown command %q", args[0])
	}
}

func runCIWait(args []string) (err error) {
	fs := flag.NewFlagSet("orca ci wait", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	timeoutSec := fs.Int("timeout", 600, "poll timeout in seconds")
	branch := fs.String("branch", "", "branch to poll (overrides config branch)")
	goalID := fs.String("goal-id", "", "goal ID (auto-resolved from active goal if empty)")
	capsuleID := fs.String("capsule-id", "", "capsule ID for the CI status record")
	if parseErr := fs.Parse(args); parseErr != nil {
		return parseErr
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}

	cfg, err := config.Load(filepath.Join(*orcaDir, "config.yaml"))
	if err != nil {
		return err
	}
	if cfg.CI.Provider == "" {
		return fmt.Errorf("orca ci wait: ci.provider is not configured")
	}
	if cfg.CI.Provider != "github_actions" {
		return fmt.Errorf("orca ci wait: unsupported ci.provider %q (only \"github_actions\" is supported)", cfg.CI.Provider)
	}
	if strings.TrimSpace(cfg.Intake.Repo) == "" {
		return fmt.Errorf("orca ci wait: intake.repo must be set for CI polling (format: owner/repo)")
	}

	log, err := eventlog.Open(filepath.Join(*orcaDir, "events.log"))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	st, err := store.New(*orcaDir, log)
	if err != nil {
		return err
	}

	ctx := context.Background()
	gID := strings.TrimSpace(*goalID)
	if gID == "" {
		goal, loadErr := st.LoadActiveGoal(ctx)
		if loadErr != nil {
			return fmt.Errorf("orca ci wait: load active goal: %w", loadErr)
		}
		if goal != nil {
			gID = goal.GoalID
		}
	}

	token := cfg.Intake.GitHubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	poller := cigate.New(cfg.CI, token, cfg.Intake.Repo, ciPollerOpts...)
	timeout := time.Duration(*timeoutSec) * time.Second
	status, runURL, summary, rawLogPath, waitErr := poller.Wait(ctx, gID, *capsuleID, *branch, timeout)

	if gID != "" {
		record := &schema.CIStatusRecord{
			RecordID:   idgen.New("CISTAT"),
			GoalID:     gID,
			CapsuleID:  *capsuleID,
			Provider:   cfg.CI.Provider,
			Branch:     *branch,
			Status:     status,
			RunURL:     runURL,
			Summary:    summary,
			RawLogPath: rawLogPath,
			RecordedAt: time.Now().UTC(),
		}
		if saveErr := st.SaveCIStatusRecord(ctx, gID, record); saveErr != nil {
			fmt.Fprintf(os.Stderr, "[orca ci wait] save ci status record: %v\n", saveErr)
		}
	}

	if waitErr != nil {
		return fmt.Errorf("orca ci wait: %w", waitErr)
	}
	if status != "success" {
		return fmt.Errorf("orca ci wait: CI %s: %s", status, summary)
	}
	return nil
}

// ciPollerOpts is set by tests to inject options into cigate.New inside
// runCIWait (e.g., to redirect API calls to a mock HTTP server).
var ciPollerOpts []cigate.Option

// buildCIGateCommand returns the verifier gate command string that invokes
// the CI wait helper. Paths with spaces are not supported by the verifier's
// command parser (a known Phase 5 limitation).
func buildCIGateCommand(orcaDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("ci gate: get executable path: %w", err)
	}
	absOrcaDir, absErr := filepath.Abs(orcaDir)
	if absErr != nil {
		absOrcaDir = orcaDir
	}
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	return fmt.Sprintf("%s ci wait --orca-dir %s --timeout 600", exe, absOrcaDir), nil
}

func openRuntime(orcaDir string, noLearning bool) (*runtime, func() error, error) {
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
		return nil, nil, errors.Join(err, log.Close())
	}
	rt, err := newRuntime(cfg, orcaDir, noLearning, log, artifactStore)
	if err != nil {
		return nil, nil, errors.Join(err, log.Close())
	}

	var mcpCancel context.CancelFunc
	if cfg.MCP.Enabled {
		addr := cfg.MCP.Listen
		if addr == "" {
			addr = "127.0.0.1:7070"
		}
		ln, listenErr := net.Listen("tcp", addr)
		if listenErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("orca: mcp server: %w", listenErr), log.Close())
		}
		mcpServer := mcp.New(artifactStore, log)
		mcpCtx, cancel := context.WithCancel(context.Background())
		mcpCancel = cancel
		go func() {
			if serveErr := mcpServer.Serve(mcpCtx, ln); serveErr != nil {
				fmt.Fprintf(os.Stderr, "[orca] mcp server: %v\n", serveErr)
			}
		}()
		fmt.Fprintf(os.Stderr, "[orca] mcp server listening on %s\n", addr)
	}

	return rt, func() error {
		if mcpCancel != nil {
			mcpCancel()
		}
		rt.gatekeeper.Close()
		return log.Close()
	}, nil
}

type runtime struct {
	cfg        *config.Config
	orcaDir    string
	noLearning bool

	// notifier is set by the CLI layer before any control-loop method runs.
	// A nil notifier is treated as no-op; use rt.emit rather than calling
	// rt.notifier directly.
	notifier Notifier

	eventLog *eventlog.FileLog
	store    *store.FileStore

	intentCompiler *intent.Compiler
	verifierEngine *verifier.Engine
	planner        *planner.Planner
	projector      *projector.Compiler
	gatekeeper     gateService
	budget         *budget.Controller
	runner         *runner.Runner
	reconciler     *reconciler.Reconciler

	// intakeFetcher is the GitHub issue fetcher. Tests replace it with a
	// Fetcher pointing at a mock HTTP server.
	intakeFetcher *intake.Fetcher
	// prWriterBaseURL overrides the GitHub API base URL used by prwriter.
	// Empty in production; set to a mock server URL in tests.
	prWriterBaseURL string
}

// emit delivers a lifecycle event through the notifier. A nil notifier is a no-op.
func (rt *runtime) emit(ctx context.Context, ev UIEvent) {
	if rt.notifier != nil {
		rt.notifier.Step(ctx, ev)
	}
}

func newRuntime(cfg *config.Config, orcaDir string, noLearning bool, log *eventlog.FileLog, st *store.FileStore) (*runtime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("orca: config is required")
	}
	if log == nil {
		return nil, fmt.Errorf("orca: event log is required")
	}
	if st == nil {
		return nil, fmt.Errorf("orca: artifact store is required")
	}

	// When ci.provider is configured, append a verifier gate that calls the
	// orca ci wait helper. The gate uses the existing GateRunner flow so no
	// verifier interfaces change.
	verifierCfg := cfg.Verifier
	if cfg.CI.Provider == "github_actions" {
		ciCmd, err := buildCIGateCommand(orcaDir)
		if err != nil {
			return nil, fmt.Errorf("orca: %w", err)
		}
		verifierCfg.Gates = append(verifierCfg.Gates, config.VerifierGate{
			Name:     "ci_status",
			Command:  ciCmd,
			Blocking: true,
		})
	}

	return &runtime{
		cfg:        cfg,
		orcaDir:    orcaDir,
		noLearning: noLearning,
		eventLog:   log,
		store:      st,

		intentCompiler: newIntentCompiler(st),
		verifierEngine: newVerifierEngine(st, verifierCfg, cfg.Advanced, noLearning),
		planner:        newPlanner(st, cfg.Budget, cfg.Adapters, cfg.Advanced, orcaDir, noLearning),
		projector:      newProjector(st, verifierCfg, cfg.Advanced),
		gatekeeper:     newGatekeeper(st, cfg.Gate),
		budget:         newBudgetController(log, cfg.Budget),
		runner:         newCapsuleRunner(st, log, orcaDir, cfg.Adapters, cfg.Remote, noLearning),
		reconciler:     newReconciler(st, log, noLearning),
		intakeFetcher:  &intake.Fetcher{},
	}, nil
}

// compileGoal compiles the raw intent into a GoalIR and proposes initial obligations.
// It does not run the planning loop.
func (rt *runtime) compileGoal(ctx context.Context, rawIntent string) (*schema.GoalIR, error) {
	rt.emit(ctx, UIEvent{Kind: EventKindGoalCompiling, Summary: "compiling intent"})
	goal, err := rt.intentCompiler.Compile(ctx, rawIntent)
	if err != nil {
		return nil, err
	}
	rt.emit(ctx, UIEvent{Kind: EventKindGoalPlanning, GoalID: goal.GoalID, Summary: fmt.Sprintf("goal %s: proposing obligations", goal.GoalID)})
	if _, err := rt.verifierEngine.ProposeObligations(ctx, goal.GoalID); err != nil {
		return nil, err
	}
	return goal, nil
}

// runFromIssue fetches the GitHub issue, compiles it as a goal, saves the
// intake record linking the issue to the goal, and then runs the planning loop.
func (rt *runtime) runFromIssue(ctx context.Context, issueNumber int) error {
	goal, err := rt.intakeIssue(ctx, issueNumber)
	if err != nil {
		return err
	}
	return rt.runPlanLoop(ctx, goal.GoalID)
}

// intakeIssue fetches a GitHub issue, compiles it as a goal, saves the intake
// record, and returns the created GoalIR. It does not run the planning loop.
// Tests call this directly to verify intake behavior without running agents.
func (rt *runtime) intakeIssue(ctx context.Context, issueNumber int) (*schema.GoalIR, error) {
	rt.emit(ctx, UIEvent{Kind: EventKindSetupStarted, Summary: fmt.Sprintf("fetching github issue #%d", issueNumber)})
	text, err := rt.intakeFetcher.Fetch(ctx, rt.cfg.Intake, issueNumber)
	if err != nil {
		return nil, fmt.Errorf("orca: fetch issue %d: %w", issueNumber, err)
	}
	goal, err := rt.compileGoal(ctx, text)
	if err != nil {
		return nil, err
	}
	externalURL := fmt.Sprintf("https://github.com/%s/issues/%d", rt.cfg.Intake.Repo, issueNumber)
	ir := &schema.IntakeRecord{
		RecordID:    idgen.New("INTAKE"),
		GoalID:      goal.GoalID,
		Source:      "github_issue",
		ExternalID:  fmt.Sprintf("%d", issueNumber),
		ExternalURL: externalURL,
		IngestedAt:  time.Now().UTC(),
	}
	if err := rt.store.SaveIntakeRecord(ctx, goal.GoalID, ir); err != nil {
		return nil, fmt.Errorf("orca: save intake record for issue %d: %w", issueNumber, err)
	}
	rt.emit(ctx, UIEvent{Kind: EventKindSetupReady, GoalID: goal.GoalID, Summary: fmt.Sprintf("goal %s created from issue #%d", goal.GoalID, issueNumber)})
	return goal, nil
}

func (rt *runtime) runControlLoop(ctx context.Context, rawIntent string) error {
	goal, err := rt.compileGoal(ctx, rawIntent)
	if err != nil {
		return err
	}
	return rt.runPlanLoop(ctx, goal.GoalID)
}

func (rt *runtime) runPlanLoop(ctx context.Context, goalID string) error {
	goal, err := rt.store.LoadGoal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("orca: load goal %s: %w", goalID, err)
	}
	for {
		if err := rt.reconciler.FreshnessCheck(ctx, goal.GoalID); err != nil {
			return fmt.Errorf("orca: freshness check for goal %s: %w", goal.GoalID, err)
		}
		rt.emit(ctx, UIEvent{Kind: EventKindGoalPlanning, GoalID: goal.GoalID, Summary: fmt.Sprintf("goal %s: planning", goal.GoalID)})
		plan, err := rt.planner.Plan(ctx, goal.GoalID)
		if err != nil {
			return err
		}
		if len(plan.CapsuleIDs) == 0 {
			return fmt.Errorf("orca: planner returned no capsules for goal %s", goal.GoalID)
		}
		topologyEvent, err := rt.emitTopologySelected(ctx, goal.GoalID, plan)
		if err != nil {
			return err
		}
		rt.emit(ctx, UIEvent{
			Kind:    EventKindTopologySelected,
			GoalID:  goal.GoalID,
			Summary: fmt.Sprintf("goal %s: selected topology %s", goal.GoalID, plan.Topology),
			Fields: map[string]string{
				"topology":    string(plan.Topology),
				"decision_id": plan.DecisionID,
			},
		})
		if err := rt.emitCycleStartSnapshot(ctx, goal.GoalID, topologyEvent); err != nil {
			return err
		}

		var patchIDs []string
		var supplementalEvidenceIDs []string
		var supplementalClaimIDs []string
		for _, capsuleID := range plan.CapsuleIDs {
			capsule, err := rt.store.LoadCapsule(ctx, capsuleID)
			if err != nil {
				return fmt.Errorf("orca: load capsule %s: %w", capsuleID, err)
			}
			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleCreated, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": compiling projections"})
			if _, err := rt.projector.CompileHumanSummary(ctx, capsuleID); err != nil {
				return err
			}
			// Gate only executor capsules: spec says "ReviewProjection blocks before
			// implementer capsule" (module_boundaries.md). Reviewer capsules do not
			// require a separate pre-execution gate. Also use plan.MaxObligationRisk
			// rather than goal.RiskLevel: goal risk and obligation risk are set by
			// different components and may disagree.
			if capsule.Role == schema.RoleExecutor && gate.ShouldReviewProjection(plan.Topology, plan.MaxObligationRisk) {
				reviewWindow := gate.ReviewWindowFor(plan.Topology, plan.MaxObligationRisk, time.Duration(rt.cfg.Gate.ReviewWindowSeconds)*time.Second)
				rt.emit(ctx, UIEvent{Kind: EventKindCapsuleWaitingForGate, CapsuleID: capsuleID, Summary: fmt.Sprintf("capsule %s: awaiting projection review (window %s)", capsuleID, reviewWindow)})
				decision, err := rt.gatekeeper.ReviewProjection(ctx, capsuleID, reviewWindow)
				if err != nil {
					return err
				}
				if !decision.Approved {
					return fmt.Errorf("orca: projection gate rejected capsule %s: %s", capsuleID, decision.Notes)
				}
			}

			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": checking budget"})
			check, err := rt.budget.CheckCapsuleBudget(ctx, capsuleID)
			if err != nil {
				return err
			}
			if !check.Allowed {
				return fmt.Errorf("orca: budget rejected capsule %s: %s", capsuleID, check.Reason)
			}
			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: fmt.Sprintf("capsule %s (%s): compiling agent projection", capsuleID, capsule.Role)})
			agentProjection, err := rt.compileAgentProjection(ctx, capsule)
			if err != nil {
				return err
			}
			if err := rt.store.UpdateCapsuleProjectionID(ctx, capsuleID, agentProjection.ContextProjectionID); err != nil {
				return err
			}
			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": running agent"})
			runResult, err := rt.runner.Run(ctx, capsuleID)
			if err != nil {
				rt.emit(ctx, UIEvent{
					Kind:      EventKindCapsuleFailed,
					GoalID:    goal.GoalID,
					CapsuleID: capsuleID,
					Summary:   "capsule " + capsuleID + ": failed",
					Detail:    err.Error(),
					Status:    "failed",
					Severity:  "error",
				})
				return err
			}
			rt.emit(ctx, UIEvent{
				Kind:      EventKindCapsuleCompleted,
				GoalID:    goal.GoalID,
				CapsuleID: capsuleID,
				PatchID:   runResult.PatchID,
				Summary:   "capsule " + capsuleID + ": completed",
				Status:    "completed",
			})
			if capsule.Role != schema.RoleExecutor {
				if len(runResult.EvidenceIDs) == 0 && len(runResult.ClaimIDs) == 0 {
					return fmt.Errorf("orca: %s capsule %s produced no review evidence or claims", capsule.Role, capsuleID)
				}
				supplementalEvidenceIDs = append(supplementalEvidenceIDs, runResult.EvidenceIDs...)
				supplementalClaimIDs = append(supplementalClaimIDs, runResult.ClaimIDs...)
				continue
			}
			if runResult.PatchID == "" {
				return fmt.Errorf("orca: capsule %s produced no patch", capsuleID)
			}
			patchIDs = append(patchIDs, runResult.PatchID)
		}
		if len(patchIDs) == 0 {
			return fmt.Errorf("orca: plan produced no implementer patch for goal %s", goal.GoalID)
		}
		var readyPatchID string
		var readyResult reconciler.ReconcileResult
		// acceptedPatchIDs tracks every patch the reconciler accepted this cycle.
		// In parallel topology the reconciler can only emit merge_applied for the
		// last-reconciled patch (when all obligations finally clear). Earlier accepted
		// patches must receive their merge_applied event from the orchestrator.
		var acceptedPatchIDs []string
		var followUpIDs []string
		var blockingReason string
		// reconcilerMergeEmitted tracks patches for which the reconciler already
		// emitted merge_applied (MergeReady=true && !HumanGateRequired). The
		// orchestrator must not re-emit for these when backfilling earlier accepted patches.
		reconcilerMergeEmitted := make(map[string]bool)
		for _, patchID := range patchIDs {
			rt.emit(ctx, UIEvent{Kind: EventKindVerifierRunning, PatchID: patchID, Summary: "patch " + patchID + ": verifying"})
			verifyResult, err := rt.verifyPatch(ctx, patchID, supplementalEvidenceIDs, supplementalClaimIDs)
			if err != nil {
				return err
			}
			rt.emit(ctx, verifierUIEvent(goal.GoalID, verifyResult))
			var reconcileIn reconciler.ReconcileInput
			if len(verifyResult.BlockingFailures) > 0 {
				waivers, waiverErr := rt.collectWaivers(ctx, verifyResult)
				if waiverErr != nil {
					return waiverErr
				}
				reconcileIn.Waivers = waivers
			}
			rt.emit(ctx, UIEvent{
				Kind:    EventKindReconcileRunning,
				GoalID:  goal.GoalID,
				PatchID: verifyResult.PatchID,
				Summary: fmt.Sprintf("patch %s: reconciling (recommended action: %s)", verifyResult.PatchID, verifyResult.RecommendedAction),
				Fields:  map[string]string{"recommended_action": string(verifyResult.RecommendedAction)},
			})
			result, err := rt.reconciler.Reconcile(ctx, verifyResult.PatchID, reconcileIn)
			if err != nil {
				return err
			}
			rt.emit(ctx, reconcileUIEvent(goal.GoalID, verifyResult.PatchID, result))
			if result.PatchAccepted {
				acceptedPatchIDs = append(acceptedPatchIDs, verifyResult.PatchID)
			}
			if result.MergeReady {
				readyPatchID = verifyResult.PatchID
				readyResult = result
				if !result.HumanGateRequired {
					reconcilerMergeEmitted[verifyResult.PatchID] = true
				}
			}
			if len(result.FollowUpObligationIDs) > 0 {
				followUpIDs = append(followUpIDs, result.FollowUpObligationIDs...)
			}
			if result.BlockingReason != "" {
				blockingReason = result.BlockingReason
			}
		}
		rt.emit(ctx, UIEvent{Kind: EventKindGoalPlanning, GoalID: goal.GoalID, Summary: fmt.Sprintf("goal %s: computing ROI", goal.GoalID)})
		if _, err := rt.budget.ComputeROI(ctx, goal.GoalID); err != nil {
			return err
		}

		if readyResult.MergeReady {
			rt.emit(ctx, UIEvent{
				Kind:    EventKindMergeReady,
				GoalID:  goal.GoalID,
				PatchID: readyPatchID,
				Summary: fmt.Sprintf("patch %s: merge ready", readyPatchID),
				Status:  "ready",
			})
			if readyResult.HumanGateRequired {
				// Gate on the last merge-ready patch; merge all accepted patches on approval.
				decision, err := rt.gatekeeper.ReviewMerge(ctx, readyPatchID)
				if err != nil {
					return err
				}
				if !decision.Approved {
					return fmt.Errorf("orca: merge gate rejected patch %s: %s", readyPatchID, decision.Notes)
				}
				for _, pid := range acceptedPatchIDs {
					if err := rt.applyPatchToWorkDir(ctx, pid); err != nil {
						return fmt.Errorf("orca: apply patch %s: %w", pid, err)
					}
				}
				// PR creation only runs after explicit human gate approval.
				// There is no auto-PR path in Phase 5.
				if rt.cfg.PR.Enabled {
					if err := rt.createAndSavePR(ctx, goal.GoalID, readyPatchID); err != nil {
						return fmt.Errorf("orca: create pr for patch %s: %w", readyPatchID, err)
					}
				}
				for _, pid := range acceptedPatchIDs {
					if err := rt.appendMergeApplied(ctx, goal.GoalID, pid); err != nil {
						return err
					}
				}
			} else {
				// Apply all accepted patches to the working directory.
				for _, pid := range acceptedPatchIDs {
					if err := rt.applyPatchToWorkDir(ctx, pid); err != nil {
						return fmt.Errorf("orca: apply patch %s: %w", pid, err)
					}
				}
				// The reconciler already emitted merge_applied for every patch where
				// MergeReady=true && !HumanGateRequired. Emit only for earlier accepted
				// patches that did not receive a reconciler merge_applied.
				for _, pid := range acceptedPatchIDs {
					if reconcilerMergeEmitted[pid] {
						continue
					}
					if err := rt.appendMergeApplied(ctx, goal.GoalID, pid); err != nil {
						return err
					}
				}
			}
			return rt.updateGoalStatus(ctx, goal.GoalID, schema.GoalStatusComplete)
		}
		if len(followUpIDs) > 0 {
			continue
		}
		return fmt.Errorf("orca: reconciliation stopped: %s", blockingReason)
	}
}

// ── PR creation helpers ──────────────────────────────────────────────────────

// createAndSavePR resolves branches, builds the PR body, calls prwriter.Create,
// and persists the returned PRRecord via store.SavePRRecord.
// Called only when pr.enabled is true and the human gate has approved the merge.
func (rt *runtime) createAndSavePR(ctx context.Context, goalID, patchID string) error {
	baseBranch, err := rt.resolveBaseBranch(ctx)
	if err != nil {
		return fmt.Errorf("resolve base branch: %w", err)
	}
	headBranch, err := rt.resolveHeadBranch(ctx, patchID)
	if err != nil {
		return fmt.Errorf("resolve head branch: %w", err)
	}
	title, body, err := rt.buildPRContent(ctx, goalID, patchID)
	if err != nil {
		return fmt.Errorf("build pr content: %w", err)
	}

	token := rt.cfg.Intake.GitHubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	prCfg := prwriter.Config{
		Repo:        rt.cfg.Intake.Repo,
		GitHubToken: token,
		Draft:       rt.cfg.PR.Draft,
		Label:       rt.cfg.PR.Label,
		BaseURL:     rt.prWriterBaseURL,
	}
	in := prwriter.CreateInput{
		GoalID:     goalID,
		PatchID:    patchID,
		BaseBranch: baseBranch,
		HeadBranch: headBranch,
		Title:      title,
		Body:       body,
		Draft:      rt.cfg.PR.Draft,
	}
	record, err := prwriter.Create(ctx, prCfg, in)
	if err != nil {
		return fmt.Errorf("create github pr: %w", err)
	}
	if err := rt.store.SavePRRecord(ctx, goalID, record); err != nil {
		return fmt.Errorf("save pr record: %w", err)
	}
	rt.emit(ctx, UIEvent{Kind: EventKindPRCreated, GoalID: goalID, PatchID: patchID, Summary: "pr created: " + record.PRURL})
	return nil
}

// resolveBaseBranch resolves the PR base branch using the following priority:
//  1. pr.base_branch config value
//  2. git symbolic-ref refs/remotes/origin/HEAD
//  3. GitHub API GET /repos/{owner}/{repo} default_branch
//  4. error
func (rt *runtime) resolveBaseBranch(ctx context.Context) (string, error) {
	if rt.cfg.PR.BaseBranch != "" {
		return rt.cfg.PR.BaseBranch, nil
	}

	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = filepath.Dir(rt.orcaDir)
	if out, gitErr := cmd.Output(); gitErr == nil {
		ref := strings.TrimSpace(string(out))
		if branch, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok && branch != "" {
			return branch, nil
		}
	}

	token := rt.cfg.Intake.GitHubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if rt.cfg.Intake.Repo != "" && token != "" {
		prCfg := prwriter.Config{
			Repo:        rt.cfg.Intake.Repo,
			GitHubToken: token,
			BaseURL:     rt.prWriterBaseURL,
		}
		if branch, apiErr := prwriter.FetchDefaultBranch(ctx, prCfg); apiErr == nil && branch != "" {
			return branch, nil
		}
	}

	return "", fmt.Errorf("cannot resolve base branch: set pr.base_branch or configure intake.repo with a github token")
}

// resolveHeadBranch finds the current branch in the capsule's worktree.
// Returns an error when the worktree is missing, the path is not a git
// repository, or the HEAD is detached (git branch --show-current returns "").
func (rt *runtime) resolveHeadBranch(ctx context.Context, patchID string) (string, error) {
	patch, err := rt.store.LoadPatch(ctx, patchID)
	if err != nil {
		return "", fmt.Errorf("load patch %s: %w", patchID, err)
	}
	capsule, err := rt.store.LoadCapsule(ctx, patch.CapsuleID)
	if err != nil {
		return "", fmt.Errorf("load capsule %s: %w", patch.CapsuleID, err)
	}
	if capsule.Sandbox.WorktreePath == "" {
		return "", fmt.Errorf("capsule %s has no worktree path", capsule.CapsuleID)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", capsule.Sandbox.WorktreePath, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get current branch in capsule %s worktree: %w", capsule.CapsuleID, err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("capsule %s worktree is in detached HEAD state or has no current branch", capsule.CapsuleID)
	}
	return branch, nil
}

// buildPRContent derives the PR title and body from the goal, patch, and
// verifier result artifacts. No transcript content is used.
func (rt *runtime) buildPRContent(ctx context.Context, goalID, patchID string) (title, body string, err error) {
	goal, loadErr := rt.store.LoadGoal(ctx, goalID)
	if loadErr != nil {
		return "", "", fmt.Errorf("load goal %s: %w", goalID, loadErr)
	}
	patch, loadErr := rt.store.LoadPatch(ctx, patchID)
	if loadErr != nil {
		return "", "", fmt.Errorf("load patch %s: %w", patchID, loadErr)
	}

	t := goal.OriginalIntent
	if len(t) > 72 {
		t = t[:69] + "..."
	}

	var sb strings.Builder
	sb.WriteString("## Goal\n\n")
	sb.WriteString(goal.OriginalIntent)
	sb.WriteString("\n\n")

	if len(patch.ObligationIDsClaimed) > 0 {
		sb.WriteString("## Obligations Addressed\n\n")
		for _, oblID := range patch.ObligationIDsClaimed {
			obl, oblErr := rt.store.LoadObligation(ctx, oblID)
			if oblErr != nil {
				fmt.Fprintf(&sb, "- %s\n", oblID)
				continue
			}
			fmt.Fprintf(&sb, "- %s\n", obl.Description)
		}
		sb.WriteString("\n")
	}

	vr, vrErr := rt.store.LoadVerifierResultForPatch(ctx, patchID)
	if vrErr != nil && !errors.Is(vrErr, store.ErrNotFound) {
		return "", "", fmt.Errorf("load verifier result for patch %s: %w", patchID, vrErr)
	}
	if vrErr == nil && len(vr.ObligationResults) > 0 {
		sb.WriteString("## Evidence\n\n")
		for _, verdict := range vr.ObligationResults {
			if len(verdict.EvidenceIDs) > 0 {
				fmt.Fprintf(&sb, "- %s (%s): %s\n", verdict.ObligationID, verdict.Verdict, strings.Join(verdict.EvidenceIDs, ", "))
			} else {
				fmt.Fprintf(&sb, "- %s (%s)\n", verdict.ObligationID, verdict.Verdict)
			}
		}
		sb.WriteString("\n")
		if vr.RecommendationRationale != "" {
			sb.WriteString("## Merge Rationale\n\n")
			sb.WriteString(vr.RecommendationRationale)
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("---\n*Generated by Orca*\n")
	return t, sb.String(), nil
}

func (rt *runtime) emitCycleStartSnapshot(ctx context.Context, goalID string, lastEvent schema.Event) error {
	now := time.Now().UTC()
	snapshot := &schema.StateSnapshot{
		SnapshotID:  idgen.New("SNAP-CYCLE"),
		GoalID:      goalID,
		EventID:     lastEvent.EventID,
		SequenceNum: lastEvent.SequenceNum,
		CreatedAt:   now,
	}
	if err := rt.store.SaveSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("orca: save cycle snapshot for goal %s: %w", goalID, err)
	}
	return nil
}

func (rt *runtime) verifyPatch(ctx context.Context, patchID string, supplementalEvidenceIDs, supplementalClaimIDs []string) (*schema.VerifierResult, error) {
	return rt.verifierEngine.Verify(ctx, patchID, verifier.VerifyInput{
		SupplementalEvidenceIDs: supplementalEvidenceIDs,
		SupplementalClaimIDs:    supplementalClaimIDs,
	})
}

func verifierUIEvent(goalID string, result *schema.VerifierResult) UIEvent {
	if result == nil {
		return UIEvent{
			Kind:     EventKindVerifierFailed,
			GoalID:   goalID,
			Summary:  "verification failed",
			Status:   "failed",
			Severity: "error",
		}
	}
	if verifierResultPassed(result) {
		return UIEvent{
			Kind:    EventKindVerifierPassed,
			GoalID:  goalID,
			PatchID: result.PatchID,
			Summary: "patch " + result.PatchID + ": verification passed",
			Status:  "passed",
			Fields:  map[string]string{"recommended_action": string(result.RecommendedAction)},
		}
	}
	return UIEvent{
		Kind:     EventKindVerifierFailed,
		GoalID:   goalID,
		PatchID:  result.PatchID,
		Summary:  "patch " + result.PatchID + ": verification failed",
		Detail:   strings.Join(result.BlockingFailures, "; "),
		Status:   "failed",
		Severity: "error",
		Fields:   map[string]string{"recommended_action": string(result.RecommendedAction)},
	}
}

func verifierResultPassed(result *schema.VerifierResult) bool {
	if result == nil {
		return false
	}
	switch result.RecommendedAction {
	case schema.ActionReject, schema.ActionRetry, schema.ActionSplit:
		return false
	}
	if len(result.BlockingFailures) > 0 {
		return false
	}
	for _, verdict := range result.ObligationResults {
		if verdict.Verdict != schema.VerdictSatisfied && verdict.Verdict != schema.VerdictWaived {
			return false
		}
	}
	return true
}

func reconcileUIEvent(goalID, patchID string, result reconciler.ReconcileResult) UIEvent {
	fields := map[string]string{
		"patch_accepted": fmt.Sprintf("%t", result.PatchAccepted),
		"merge_ready":    fmt.Sprintf("%t", result.MergeReady),
	}
	if result.DecisionID != "" {
		fields["decision_id"] = result.DecisionID
	}
	if len(result.FollowUpObligationIDs) > 0 {
		return UIEvent{
			Kind:    EventKindReconcileFollowUp,
			GoalID:  goalID,
			PatchID: patchID,
			Summary: fmt.Sprintf("patch %s: follow-up required (%d obligation(s))", patchID, len(result.FollowUpObligationIDs)),
			Detail:  strings.Join(result.FollowUpObligationIDs, ", "),
			Status:  "follow_up",
			Fields:  fields,
		}
	}
	if result.PatchAccepted {
		return UIEvent{
			Kind:    EventKindReconcileAccepted,
			GoalID:  goalID,
			PatchID: patchID,
			Summary: "patch " + patchID + ": accepted",
			Status:  "accepted",
			Fields:  fields,
		}
	}
	return UIEvent{
		Kind:     EventKindReconcileBlocked,
		GoalID:   goalID,
		PatchID:  patchID,
		Summary:  "patch " + patchID + ": blocked",
		Detail:   result.BlockingReason,
		Status:   "blocked",
		Severity: "error",
		Fields:   fields,
	}
}

// collectWaivers presents a ReviewWaiver gate for each blocking obligation in
// vr whose verifier verdict is VerdictFailed. Gate-level failures (static/test
// gate summaries) that appear in BlockingFailures but not in ObligationResults
// are not waivable and are skipped. If any obligation waiver is rejected it
// returns an error immediately. Otherwise it returns a map from obligation ID
// to the approved gate decision ID.
func (rt *runtime) collectWaivers(ctx context.Context, vr *schema.VerifierResult) (map[string]string, error) {
	waivers := make(map[string]string)
	for _, verdict := range vr.ObligationResults {
		if verdict.Verdict != schema.VerdictFailed {
			continue
		}
		obl, err := rt.store.LoadObligation(ctx, verdict.ObligationID)
		if err != nil {
			return nil, fmt.Errorf("orca: load obligation %s for waiver: %w", verdict.ObligationID, err)
		}
		if !obl.Blocking {
			continue
		}
		decision, err := rt.gatekeeper.ReviewWaiver(ctx, verdict.ObligationID, obl.Description)
		if err != nil {
			return nil, fmt.Errorf("orca: waiver review for obligation %s: %w", verdict.ObligationID, err)
		}
		if !decision.Approved {
			return nil, fmt.Errorf("orca: waiver rejected for obligation %s: %s", verdict.ObligationID, decision.Notes)
		}
		waivers[verdict.ObligationID] = decision.DecisionID
	}
	return waivers, nil
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

func isatty(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// findProjectRoot walks up from dir until it finds a directory containing
// .git/, then returns that directory as the project root. Falls back to dir
// if no git repository is found (e.g., not in a git repo).
func findProjectRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break // reached filesystem root
		}
		abs = parent
	}
	return dir // fallback: no .git found
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func printHelp() {
	fmt.Println("Commands:")
	fmt.Println("  <goal text>   start a new goal (runs in background)")
	fmt.Println("  /status       show active goal status (concise)")
	fmt.Println("  /details      show active goal status (full/raw)")
	fmt.Println("  /logs         show agent or verifier logs (Phase 3)")
	fmt.Println("  /approve      approve the current waiting gate")
	fmt.Println("  /reject       reject the current waiting gate")
	fmt.Println("  /cancel       cancel the active goal")
	fmt.Println("  /resume       resume a cancelled goal (Phase 5)")
	fmt.Println("  /config       show config path")
	fmt.Println("  /help         show this help")
	fmt.Println("  exit / quit   exit orca")
}

func autoInit(orcaDir string) (err error) {
	configPath := filepath.Join(orcaDir, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return nil
	}
	if err := ensureInitTarget(orcaDir); err != nil {
		return err
	}
	log, err := eventlog.Open(filepath.Join(orcaDir, "events.log"))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if _, err := store.New(orcaDir, log); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(orcaDir, "capsules"), 0o755); err != nil {
		return fmt.Errorf("autoInit: create capsules dir: %w", err)
	}
	projectType := config.DetectProjectType(".")
	yaml := config.DefaultConfigYAML(projectType)
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		return fmt.Errorf("autoInit: write config.yaml: %w", err)
	}
	if projectType == "" {
		fmt.Fprintf(os.Stderr, "Initialized %s/ (no project type detected; add gates to config.yaml).\n", orcaDir)
	} else {
		fmt.Fprintf(os.Stderr, "Initialized %s/ with %s defaults.\n", orcaDir, projectType)
	}
	return nil
}

func autoInitWithConfirmation(orcaDir string, in io.Reader, out io.Writer) error {
	interactive := false
	if f, ok := in.(*os.File); ok && isatty(f) {
		interactive = true
	}
	return autoInitConfirm(orcaDir, in, out, interactive)
}

func autoInitConfirm(orcaDir string, in io.Reader, out io.Writer, interactive bool) error {
	configPath := filepath.Join(orcaDir, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return nil
	}
	absOrcaDir, err := filepath.Abs(orcaDir)
	if err != nil {
		absOrcaDir = orcaDir
	}
	if !interactive {
		fmt.Fprintf(out, "Initializing .orca/ at %s\n", absOrcaDir)
		return autoInit(orcaDir)
	}
	fmt.Fprintf(out, "Initializing .orca/ at %s — proceed? [y/N] ", absOrcaDir)
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return fmt.Errorf("orca: init aborted")
	}
	line := strings.TrimSpace(scanner.Text())
	if line != "y" && line != "Y" {
		return fmt.Errorf("orca: init aborted")
	}
	return autoInit(orcaDir)
}

func runInteractive(orcaDir string) error {
	if err := autoInitWithConfirmation(orcaDir, os.Stdin, os.Stderr); err != nil {
		return err
	}
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		return err
	}
	defer closeFn()
	rt.notifier = newPlainNotifier(os.Stderr, false)
	if err := rt.cfg.Verifier.ValidateGates(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Orca  local proof runtime\nWorking directory: %s\n\n", mustAbs("."))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := newSupervisor(orcaDir, rt, os.Stdin, os.Stdout, os.Stderr)
	return sup.Run(ctx)
}

func runUI(args []string) error {
	fs := flag.NewFlagSet("orca ui", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}

	desktop, err := findDesktopBinary()
	if err != nil {
		return err
	}

	absOrcaDir, err := filepath.Abs(*orcaDir)
	if err != nil {
		return fmt.Errorf("orca ui: resolve orca-dir: %w", err)
	}
	cmd := exec.Command(desktop, "--orca-dir", absOrcaDir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// findDesktopBinary checks candidate locations for the orca-desktop binary
// in priority order and returns the first path that exists and is executable.
func findDesktopBinary() (string, error) {
	for _, p := range desktopBinaryCandidates() {
		if info, err := os.Stat(p); err == nil && isExecutableDesktopBinary(info) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("orca-desktop"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf(
		"orca ui: orca-desktop not found.\n" +
			"Install it with:\n" +
			"  go install github.com/micronwave/orca/desktop/cmd/orca-desktop@latest\n" +
			"Or download from: https://github.com/micronwave/orca/releases",
	)
}

func isExecutableDesktopBinary(info os.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	if goos.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// desktopBinaryCandidates returns well-known paths to check for orca-desktop,
// in priority order. It branches on goos.GOOS for Windows vs. Unix paths.
func desktopBinaryCandidates() []string {
	var candidates []string

	// 1. Phase B install-script location.
	if goos.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			candidates = append(candidates, filepath.Join(localAppData, "Programs", "orca", "orca-desktop.exe"))
		}
	} else {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidates = append(candidates, filepath.Join(home, ".orca", "bin", "orca-desktop"))
		}
	}

	// 2. Side-by-side with the running orca binary.
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if goos.GOOS == "windows" {
			candidates = append(candidates, filepath.Join(dir, "orca-desktop.exe"))
			// 3. Windows only: developer repo layout (desktop/build/bin).
			candidates = append(candidates, filepath.Join(dir, "desktop", "build", "bin", "orca-desktop.exe"))
		} else {
			candidates = append(candidates, filepath.Join(dir, "orca-desktop"))
		}
	}

	return candidates
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
	writeAdvancedStatus(out, rt.cfg.Advanced, latestVerifier)
	falsePositives, totalFindings, err := rt.computeAdvancedFalsePositiveRate(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	if totalFindings > 0 {
		fmt.Fprintf(out, "Advanced false positives: %d/%d findings\n", falsePositives, totalFindings)
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

	// Phase 5 indicators
	mcpAddr := rt.cfg.MCP.Listen
	if mcpAddr == "" {
		mcpAddr = "127.0.0.1:7070"
	}
	if rt.cfg.MCP.Enabled {
		fmt.Fprintf(out, "MCP server: running on %s\n", mcpAddr)
	} else {
		fmt.Fprintln(out, "MCP server: disabled")
	}
	if rt.cfg.Remote.Enabled {
		fmt.Fprintf(out, "Remote execution: enabled (host=%s)\n", rt.cfg.Remote.Host)
	} else {
		fmt.Fprintln(out, "Remote execution: disabled")
	}
	prURL, err := rt.latestPRURLForGoal(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	if prURL != "" {
		fmt.Fprintf(out, "Latest PR: %s\n", prURL)
	} else {
		fmt.Fprintln(out, "Latest PR: none")
	}
	ciRecord, err := rt.latestCIStatusForGoal(ctx, goal.GoalID)
	if err != nil {
		return err
	}
	if ciRecord != nil {
		fmt.Fprintf(out, "CI: provider=%s branch=%s status=%s\n", ciRecord.Provider, ciRecord.Branch, ciRecord.Status)
	} else {
		fmt.Fprintln(out, "CI: no runs recorded")
	}
	return nil
}

// printStatusConcise writes a human-friendly status summary that hides raw
// artifact IDs, budget numbers, and infrastructure configuration. For the full
// operational dump use printStatus (exposed via orca status --raw).
func (rt *runtime) printStatusConcise(ctx context.Context, out io.Writer) error {
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

	fmt.Fprintf(out, "Goal:    %s\n", goal.OriginalIntent)
	fmt.Fprintf(out, "Status:  %s\n", goal.Status)
	fmt.Fprintf(out, "Merge:   %s\n", readiness)
	if len(obligations) > 0 {
		fmt.Fprintf(out, "Open:    %d obligation(s)\n", len(obligations))
	}
	if len(capsules) > 0 {
		fmt.Fprintf(out, "Active:  %d capsule(s) running\n", len(capsules))
	}
	if len(humanDecisions) > 0 {
		fmt.Fprintln(out, "Waiting:")
		for _, d := range humanDecisions {
			fmt.Fprintf(out, "  %s\n", d)
		}
	}
	return nil
}

func writeAdvancedStatus(out io.Writer, adv config.AdvancedConfig, latest *schema.VerifierResult) {
	status := "disabled"
	if adv.Enabled {
		status = "enabled"
	}
	fmt.Fprintf(out, "Advanced checks: %s\n", status)
	fmt.Fprintf(out, "  MAVEN: %s  Mutation: %s  Adversarial: %s  Reviewer diversity: %s\n",
		onOff(adv.Enabled && adv.Maven),
		onOff(adv.Enabled && adv.Mutation),
		onOff(adv.Enabled && adv.AdversarialTests),
		onOff(adv.Enabled && adv.ReviewerDiversity),
	)
	findings := advancedWarnings(latest)
	if len(findings) == 0 {
		return
	}
	fmt.Fprintln(out, "Advanced findings:")
	for _, warning := range findings {
		fmt.Fprintf(out, "  %s\n", warning)
	}
}

func advancedWarnings(result *schema.VerifierResult) []string {
	if result == nil {
		return nil
	}
	var findings []string
	for _, warning := range result.Warnings {
		if hasAdvancedPrefix(warning) {
			findings = append(findings, warning)
		}
	}
	return findings
}

func hasAdvancedPrefix(s string) bool {
	return strings.HasPrefix(s, "[maven]") ||
		strings.HasPrefix(s, "[mutation]") ||
		strings.HasPrefix(s, "[adversarial]")
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (rt *runtime) computeAdvancedFalsePositiveRate(ctx context.Context, goalID string) (int, int, error) {
	findingsByPatch := make(map[string]bool)
	approvedByPatch := make(map[string]bool)
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return 0, 0, fmt.Errorf("orca status: read events for advanced false positives: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			switch event.Type {
			case schema.EventVerifierResultCreated:
				var result schema.VerifierResult
				if err := json.Unmarshal(event.Payload, &result); err != nil {
					return 0, 0, fmt.Errorf("orca status: unmarshal verifier result %s: %w", event.ArtifactID, err)
				}
				if result.RecommendedAction == schema.ActionHumanReview &&
					strings.TrimSpace(result.PatchID) != "" &&
					hasAdvancedFinding(result.RecommendationRationale) {
					findingsByPatch[result.PatchID] = true
				}
			case schema.EventDecisionRecordCreated:
				var decision schema.DecisionRecord
				if err := json.Unmarshal(event.Payload, &decision); err != nil {
					return 0, 0, fmt.Errorf("orca status: unmarshal decision %s: %w", event.ArtifactID, err)
				}
				if decision.Context != "merge_review" ||
					(decision.Decision != "approved" && decision.Decision != "auto_proceeded") {
					continue
				}
				for _, relatedID := range decision.RelatedIDs {
					if relatedID != "" {
						approvedByPatch[relatedID] = true
					}
				}
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	total := len(findingsByPatch)
	var falsePositives int
	for patchID := range findingsByPatch {
		if approvedByPatch[patchID] {
			falsePositives++
		}
	}
	return falsePositives, total, nil
}

func hasAdvancedFinding(s string) bool {
	return strings.Contains(s, "[maven]") ||
		strings.Contains(s, "[mutation]") ||
		strings.Contains(s, "[adversarial]")
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
		scanner := bufio.NewScanner(in)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("orca cancel: read confirmation: %w", err)
			}
			fmt.Fprintln(out, "Cancel aborted.")
			return nil
		}
		if !strings.EqualFold(strings.TrimSpace(scanner.Text()), "cancel") {
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
	ev, err := rt.eventLog.Append(ctx, schema.Event{
		Type:       schema.EventGoalStatusUpdated,
		GoalID:     goalID,
		ArtifactID: goalID,
		Payload:    payload,
	})
	if err != nil {
		return fmt.Errorf("orca: append goal_status_updated: %w", err)
	}
	if err := rt.store.UpdateGoalStatus(ctx, goalID, status); err != nil {
		return &store.MaterializationError{Event: ev, Err: fmt.Errorf("orca: update goal status: %w", err)}
	}
	return nil
}

// applyPatchToWorkDir applies the diff from patchID to the project working
// directory using git apply. It is called after the human gate (or
// auto-accept) approves a merge so that the agent's changes land on disk.
// A zero-byte or missing diff is treated as a no-op (the agent may have
// produced no file changes beyond evidence artifacts).
func (rt *runtime) applyPatchToWorkDir(ctx context.Context, patchID string) error {
	patch, err := rt.store.LoadPatch(ctx, patchID)
	if err != nil {
		return fmt.Errorf("load patch: %w", err)
	}
	if patch.DiffPath == "" || patch.DiffPath == "inline" {
		return nil
	}
	info, err := os.Stat(patch.DiffPath)
	if err != nil {
		return fmt.Errorf("stat diff %s: %w", patch.DiffPath, err)
	}
	if info.Size() == 0 {
		return nil
	}
	projectRoot := filepath.Dir(rt.orcaDir)
	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", patch.DiffPath)
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply in %s: %w\n%s", projectRoot, err, strings.TrimSpace(string(out)))
	}
	rt.emit(ctx, UIEvent{Kind: EventKindMergeApplied, PatchID: patchID, Summary: "patch " + patchID + ": applied to working directory"})
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

func (rt *runtime) compileAgentProjection(ctx context.Context, capsule *schema.ExecutionCapsule) (*schema.ContextProjection, error) {
	switch capsule.Role {
	case schema.RoleReviewer:
		return rt.projector.CompileReviewer(ctx, capsule.CapsuleID)
	case schema.RoleTester:
		return rt.projector.CompileTester(ctx, capsule.CapsuleID)
	default:
		return rt.projector.CompileExecutor(ctx, capsule.CapsuleID)
	}
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
		if capsule.Role != schema.RoleExecutor {
			continue
		}
		decision, err := rt.store.LoadDecision(ctx, capsule.TopologyDecisionID)
		if err != nil {
			return nil, fmt.Errorf("orca status: load topology decision %s: %w", capsule.TopologyDecisionID, err)
		}
		if gate.ShouldReviewProjection(schema.Topology(decision.Decision), maxRisk) {
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

// latestPRURLForGoal scans the event log for the most recent pr_created event
// and returns its PRURL, or "" when no PR has been created for the goal.
func (rt *runtime) latestPRURLForGoal(ctx context.Context, goalID string) (string, error) {
	var url string
	var latestSeq int64
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return "", fmt.Errorf("orca status: read events for pr: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Type == schema.EventPRCreated && ev.SequenceNum > latestSeq {
				var pr schema.PRRecord
				if err := json.Unmarshal(ev.Payload, &pr); err != nil {
					return "", fmt.Errorf("orca status: decode pr_created payload for event %s: %w", ev.EventID, err)
				}
				if pr.PRURL != "" {
					latestSeq = ev.SequenceNum
					url = pr.PRURL
				}
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return url, nil
}

// latestCIStatusForGoal scans the event log for the most recent ci_status_received
// event and returns the record, or nil when no CI run has been recorded.
func (rt *runtime) latestCIStatusForGoal(ctx context.Context, goalID string) (*schema.CIStatusRecord, error) {
	var latest *schema.CIStatusRecord
	var latestSeq int64
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return nil, fmt.Errorf("orca status: read events for ci status: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Type == schema.EventCIStatusReceived && ev.SequenceNum > latestSeq {
				var r schema.CIStatusRecord
				if err := json.Unmarshal(ev.Payload, &r); err != nil {
					return nil, fmt.Errorf("orca status: decode ci_status_received payload for event %s: %w", ev.EventID, err)
				}
				latestSeq = ev.SequenceNum
				rc := r
				latest = &rc
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return latest, nil
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
		fmt.Fprintf(out, "- %s tokens=%d wall_time_seconds=%.2f tool_calls=%d retries=%d coordination_cost=%d discharged=%d accepted=%d rejected=%d\n",
			id,
			record.TokensSpent,
			record.WallTimeSeconds,
			record.ToolCalls,
			record.Retries,
			budget.CoordinationCost(record),
			record.ObligationsDischarged,
			record.PatchesAccepted,
			record.PatchesRejected,
		)
	}
}

func (rt *runtime) emitTopologySelected(ctx context.Context, goalID string, plan planner.PlanResult) (schema.Event, error) {
	payload, err := json.Marshal(schema.TopologySelectedPayload{
		Topology:   plan.Topology,
		DecisionID: plan.DecisionID,
	})
	if err != nil {
		return schema.Event{}, fmt.Errorf("orca: marshal topology_selected payload: %w", err)
	}
	return rt.eventLog.Append(ctx, schema.Event{
		Type:       schema.EventTopologySelected,
		GoalID:     goalID,
		ArtifactID: plan.DecisionID,
		Payload:    payload,
	})
}

func newIntentCompiler(st *store.FileStore) *intent.Compiler {
	return intent.New(st)
}

func newVerifierEngine(st *store.FileStore, cfg config.VerifierConfig, adv config.AdvancedConfig, noLearning bool) *verifier.Engine {
	return verifier.NewWithConfig(st, verifier.Config{
		Gates:      cfg.Gates,
		WorkingDir: cfg.WorkingDir,
		NoLearning: noLearning,
		Advanced:   adv,
	}, nil)
}

func newPlanner(
	st *store.FileStore,
	cfg config.BudgetConfig,
	adapters config.AdapterConfig,
	adv config.AdvancedConfig,
	orcaDir string,
	noLearning bool,
) *planner.Planner {
	var outcomes planner.OutcomeReader
	if !noLearning {
		outcomes = st
	}
	preferredReviewer := ""
	if adv.Enabled && adv.ReviewerDiversity && adapters.ClaudePath != "" && adapters.CodexPath != "" {
		preferredReviewer = string(schema.AgentClaude)
	}
	return planner.New(st, planner.Config{
		OrcaDir:                  orcaDir,
		ApprovalPolicy:           "auto",
		DefaultMaxTokens:         cfg.DefaultMaxTokens,
		DefaultMaxWallTime:       cfg.DefaultMaxWallTimeSeconds,
		DefaultMaxRetries:        cfg.DefaultMaxRetries,
		NoLearning:               noLearning,
		ReviewerDiversityEnabled: adv.Enabled && adv.ReviewerDiversity,
		PreferredReviewerAdapter: preferredReviewer,
	}, outcomes)
}

func newProjector(st *store.FileStore, cfg config.VerifierConfig, adv config.AdvancedConfig) *projector.Compiler {
	return projector.NewWithConfig(st, cfg, adv)
}

func newGatekeeper(st *store.FileStore, _ config.GateConfig) gateService {
	return gate.New(st)
}

func newBudgetController(log *eventlog.FileLog, _ config.BudgetConfig) *budget.Controller {
	return budget.New(log)
}

func newCapsuleRunner(st *store.FileStore, log *eventlog.FileLog, orcaDir string, cfg config.AdapterConfig, remoteCfg config.RemoteConfig, noLearning bool) *runner.Runner {
	adapters := []runner.Adapter{
		codex.New(orcaDir, cfg.CodexPath),
		claude.New(orcaDir, cfg.ClaudePath),
	}
	if remoteCfg.Enabled {
		// Remote adapters are appended last; the runner's registry takes the last
		// registration per AgentType, so remote overrides local when enabled.
		adapters = append(adapters,
			remote.New(remoteCfg, schema.AgentCodex, orcaDir, nil),
			remote.New(remoteCfg, schema.AgentClaude, orcaDir, nil),
		)
	}
	return runner.NewWithConfig(
		st,
		log,
		orcaDir,
		runner.Config{NoLearning: noLearning},
		adapters...,
	)
}

func newReconciler(st *store.FileStore, log *eventlog.FileLog, noLearning bool) *reconciler.Reconciler {
	return reconciler.New(st, log, reconciler.Config{NoLearning: noLearning})
}
