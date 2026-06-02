package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goos "runtime"
	"strings"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/ui"
)

// PreflightResult captures setup health for the orca doctor command and the
// pre-goal preflight check. All fields are safe to read even when no config
// exists.
type PreflightResult struct {
	// Environment facts.
	ProjectRoot  string
	ProjectType  string // "go", "node", "maven", or ""
	ConfigPath   string
	ConfigExists bool

	GitPresent    bool
	WorktreeDirty bool

	// Adapter availability.
	CodexPath       string // resolved absolute path or empty when not found
	CodexAvailable  bool
	ClaudePath      string
	ClaudeAvailable bool

	// Per-gate availability (populated when config loaded successfully).
	GateChecks []GateCheck

	// Verdict lists — populated by runPreflight.
	BlockingErrors      []string
	Warnings            []string
	OptionalUnavailable []string
	InferredFixes       []string
}

// GateCheck is the preflight result for a single configured verifier gate.
type GateCheck struct {
	Name       string
	Command    string
	Blocking   bool
	Executable string // first token of Command
	Available  bool   // whether Executable was found via exec.LookPath
	PathRisk   bool   // path contains spaces (Windows risk when embedded in commands)
}

// runDoctor implements the `orca doctor` subcommand.
func runDoctor(args []string) error {
	return runDoctorWithOutput(args, os.Stdout)
}

func runDoctorWithOutput(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("orca doctor", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	jsonOut := fs.Bool("json", false, "emit setup health as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}

	configPath := filepath.Join(*orcaDir, "config.yaml")
	configExists := false
	var cfg *config.Config
	if _, err := os.Stat(configPath); err == nil {
		configExists = true
		loaded, loadErr := config.Load(configPath)
		if loadErr != nil {
			// Report load error but continue with remaining checks.
			cfg = nil
		} else {
			cfg = loaded
		}
	}

	result := runPreflight(*orcaDir, configPath, configExists, cfg)
	if *jsonOut {
		if err := printDoctorJSON(out, result); err != nil {
			return err
		}
	} else {
		printDoctorOutput(out, result)
	}

	if len(result.BlockingErrors) > 0 {
		return fmt.Errorf("orca doctor: %d blocking issue(s) found", len(result.BlockingErrors))
	}
	return nil
}

// runPreflight performs all doctor/setup checks and returns a PreflightResult.
// Pass cfg=nil when the config file is absent or failed to load.
func runPreflight(_ string, configPath string, configExists bool, cfg *config.Config) *PreflightResult {
	r := &PreflightResult{
		ProjectRoot:  findProjectRoot("."),
		ProjectType:  config.DetectProjectType("."),
		ConfigPath:   configPath,
		ConfigExists: configExists,
	}

	// Git state.
	r.GitPresent, r.WorktreeDirty = checkGitState(r.ProjectRoot)

	// Adapter availability: prefer configured path, fall back to PATH lookup.
	var codexCfg, claudeCfg string
	if cfg != nil {
		codexCfg = cfg.Adapters.CodexPath
		claudeCfg = cfg.Adapters.ClaudePath
	}
	r.CodexPath, r.CodexAvailable = checkAdapterAvailable(codexCfg, "codex")
	r.ClaudePath, r.ClaudeAvailable = checkAdapterAvailable(claudeCfg, "claude")

	// Gate checks.
	if cfg != nil {
		for _, gate := range cfg.Verifier.Gates {
			exe, quoted := gateExecutable(gate.Command)
			available := checkCommandInPath(exe)
			pathRisk := goos.GOOS == "windows" && strings.ContainsAny(exe, " \t") && !quoted
			r.GateChecks = append(r.GateChecks, GateCheck{
				Name:       gate.Name,
				Command:    gate.Command,
				Blocking:   gate.Blocking,
				Executable: exe,
				Available:  available,
				PathRisk:   pathRisk,
			})
		}
	}

	// ── Blocking errors ──────────────────────────────────────────────────────

	if !configExists {
		r.BlockingErrors = append(r.BlockingErrors,
			fmt.Sprintf("config.yaml not found: %s", configPath))
		fix := "  Run: orca init"
		if r.ProjectType == "" {
			fix += "\n  Then add at least one verifier gate to config.yaml"
		}
		r.InferredFixes = append(r.InferredFixes, fix)
	} else if cfg == nil {
		r.BlockingErrors = append(r.BlockingErrors,
			fmt.Sprintf("config.yaml at %s failed to load", configPath))
		r.InferredFixes = append(r.InferredFixes,
			fmt.Sprintf("  Review and fix %s", configPath))
	} else {
		if len(cfg.Verifier.Gates) == 0 {
			r.BlockingErrors = append(r.BlockingErrors, "no verifier gates configured")
			r.InferredFixes = append(r.InferredFixes,
				fmt.Sprintf("  Edit %s and add at least one verifier gate", configPath))
		}
		for _, gc := range r.GateChecks {
			if gc.Blocking && !gc.Available {
				r.BlockingErrors = append(r.BlockingErrors,
					fmt.Sprintf("gate %q: executable %q not found in PATH", gc.Name, gc.Executable))
				r.InferredFixes = append(r.InferredFixes,
					fmt.Sprintf("  Install %q or update the gate command in config.yaml", gc.Executable))
			}
		}
	}

	if !r.CodexAvailable && !r.ClaudeAvailable {
		r.BlockingErrors = append(r.BlockingErrors,
			"no runnable adapter: codex and claude are both unavailable")
		r.InferredFixes = append(r.InferredFixes,
			"  Install codex:  npm install -g @openai/codex\n"+
				"  Or install claude:  https://claude.ai/code\n"+
				"  Or set adapters.codex_path / adapters.claude_path in config.yaml")
	}

	// ── Warnings ────────────────────────────────────────────────────────────

	if !r.GitPresent {
		r.Warnings = append(r.Warnings,
			"git not detected — worktree isolation requires git")
	} else if r.WorktreeDirty {
		r.Warnings = append(r.Warnings,
			"git worktree has uncommitted changes")
	}

	for _, gc := range r.GateChecks {
		if gc.PathRisk {
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("gate %q: executable path %q contains spaces (Windows path risk)", gc.Name, gc.Executable))
		}
	}

	if goos.GOOS == "windows" {
		if r.CodexAvailable && hasSpacePathRisk(r.CodexPath) {
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("codex path %q contains spaces — wrap in quotes in config.yaml if used in gate commands", r.CodexPath))
		}
		if r.ClaudeAvailable && hasSpacePathRisk(r.ClaudePath) {
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("claude path %q contains spaces — wrap in quotes in config.yaml if used in gate commands", r.ClaudePath))
		}
	}

	// ── Optional integrations ────────────────────────────────────────────────

	if cfg != nil {
		needsToken := cfg.Intake.Repo != "" || cfg.PR.Enabled || cfg.CI.Provider != ""
		if needsToken {
			hasToken := cfg.Intake.GitHubToken != "" || os.Getenv("GITHUB_TOKEN") != ""
			if !hasToken {
				r.OptionalUnavailable = append(r.OptionalUnavailable,
					"GitHub token: not set\n    Required by intake.repo / pr.enabled / ci.provider\n    Set GITHUB_TOKEN or configure intake.github_token in config.yaml")
			}
		}
		if cfg.Remote.Enabled && cfg.Remote.Host == "" {
			r.OptionalUnavailable = append(r.OptionalUnavailable,
				"remote execution: enabled but remote.host is not configured")
		}
	}

	return r
}

// printDoctorOutput writes the doctor report to w.
func printDoctorOutput(w io.Writer, r *PreflightResult) {
	fmt.Fprintf(w, "%s %s\n", ui.IconOrca, ui.Colorize(w, ui.OrcaBlue+ui.Bold, "Orca  doctor"))
	fmt.Fprintln(w, ui.Colorize(w, ui.OrcaBlue, "============"))
	fmt.Fprintln(w)

	// Environment.
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "Environment"))
	gitStatus := ui.Colorize(w, ui.Red, "not found")
	if r.GitPresent {
		if r.WorktreeDirty {
			gitStatus = ui.Colorize(w, ui.Yellow, "present  [dirty]")
		} else {
			gitStatus = ui.Colorize(w, ui.Green, "present  [clean]")
		}
	}
	projectType := r.ProjectType
	if projectType == "" {
		projectType = ui.Colorize(w, ui.Black+ui.Bold, "(unknown)")
	} else {
		projectType = ui.Colorize(w, ui.Cyan, projectType)
	}
	configStatus := ui.Colorize(w, ui.Red, "not found")
	if r.ConfigExists {
		configStatus = ui.Colorize(w, ui.Green, "present")
	}
	fmt.Fprintf(w, "  Project root:  %s\n", r.ProjectRoot)
	fmt.Fprintf(w, "  Project type:  %s\n", projectType)
	fmt.Fprintf(w, "  Config:        %s  [%s]\n", r.ConfigPath, configStatus)
	fmt.Fprintf(w, "  Git:           %s\n", gitStatus)
	fmt.Fprintln(w)

	// Adapters.
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "Adapters"))
	fmt.Fprintf(w, "  codex:  %s\n", adapterStatus(w, r.CodexPath, r.CodexAvailable))
	fmt.Fprintf(w, "  claude: %s\n", adapterStatus(w, r.ClaudePath, r.ClaudeAvailable))
	fmt.Fprintln(w)

	// Verifier gates.
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "Verifier gates"))
	if len(r.GateChecks) == 0 {
		fmt.Fprintln(w, "  (none configured)")
	}
	for _, gc := range r.GateChecks {
		avail := ui.Colorize(w, ui.Green, "ok")
		if !gc.Available {
			avail = ui.Colorize(w, ui.Red, fmt.Sprintf("MISSING executable %q", gc.Executable))
		}
		blocking := ""
		if !gc.Blocking {
			blocking = ui.Colorize(w, ui.Black+ui.Bold, " (non-blocking)")
		}
		risk := ""
		if gc.PathRisk {
			risk = ui.Colorize(w, ui.Yellow, " [path-space-risk]")
		}
		cmdDisplay := ui.Colorize(w, ui.Black+ui.Bold, fmt.Sprintf("%-36s", fmt.Sprintf("(%s)", gc.Command)))
		fmt.Fprintf(w, "  %-14s %s [%s]%s%s\n", gc.Name, cmdDisplay, avail, blocking, risk)
	}
	fmt.Fprintln(w)

	// Verdict sections.
	writeSection(w, ui.Colorize(w, ui.Red+ui.Bold, "BLOCKING ISSUES"), ui.IconCross, r.BlockingErrors, r.InferredFixes)
	writeSection(w, ui.Colorize(w, ui.Yellow+ui.Bold, "Warnings"), ui.IconWarning, r.Warnings, nil)
	writeSection(w, ui.Colorize(w, ui.Cyan+ui.Bold, "Optional unavailable"), ui.IconStep, r.OptionalUnavailable, nil)
}

func printDoctorJSON(w io.Writer, r *PreflightResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func writeSection(w io.Writer, header string, icon string, items []string, fixes []string) {
	fmt.Fprintf(w, "%s %s\n", icon, header)
	if len(items) == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		for i, item := range items {
			// Indent multi-line items.
			lines := strings.Split(item, "\n")
			fmt.Fprintf(w, "  [%d] %s\n", i+1, lines[0])
			for _, line := range lines[1:] {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
		if len(fixes) > 0 {
			fmt.Fprintln(w, "\n  Suggested fixes:")
			for _, fix := range fixes {
				lines := strings.Split(fix, "\n")
				for _, line := range lines {
					fmt.Fprintf(w, "  %s\n", ui.Colorize(w, ui.Green, line))
				}
			}
		}
	}
	fmt.Fprintln(w)
}

func adapterStatus(w io.Writer, path string, available bool) string {
	if !available {
		return ui.Colorize(w, ui.Red, "not found in PATH")
	}
	if path == "" {
		return ui.Colorize(w, ui.Green, "found")
	}
	return ui.Colorize(w, ui.Green, "found") + " " + ui.Colorize(w, ui.Black+ui.Bold, fmt.Sprintf("(%s)", path))
}

// checkGitState returns whether dir is inside a git worktree and whether that
// worktree is dirty. This supports normal repos, linked worktrees, and submodules.
// Returns (false, false) when git is not available or dir is not in a worktree.
func checkGitState(dir string) (present, dirty bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return false, false
	}
	check := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := check.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return false, false
	}
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	out, err = cmd.Output()
	if err != nil {
		return true, false
	}
	return true, strings.TrimSpace(string(out)) != ""
}

// checkAdapterAvailable resolves an adapter CLI. configPath is the explicitly
// configured binary path (from adapters.codex_path / adapters.claude_path);
// name is the fallback command name for PATH lookup.
// Returns the resolved path (or "") and whether the adapter is usable.
func checkAdapterAvailable(configPath, name string) (string, bool) {
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			return configPath, true
		}
		// Configured path does not exist.
		return configPath, false
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// gateExecutableName returns the command executable that doctor can check before
// verifier execution. It handles quoted paths and the common Windows .exe path
// with spaces so doctor does not report "C:\Program" as the executable.
func gateExecutableName(command string) string {
	exe, _ := gateExecutable(command)
	return exe
}

func gateExecutable(command string) (executable string, quoted bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", false
	}
	if command[0] == '"' {
		if end := strings.Index(command[1:], `"`); end >= 0 {
			return command[1 : end+1], true
		}
	}
	lower := strings.ToLower(command)
	if idx := strings.Index(lower, ".exe"); idx >= 0 {
		return strings.TrimSpace(command[:idx+4]), false
	}
	if idx := strings.IndexAny(command, " \t"); idx >= 0 {
		return command[:idx], false
	}
	return command, false
}

// checkCommandInPath returns true when name can be found via exec.LookPath or,
// for explicit filesystem paths, when the path exists.
func checkCommandInPath(name string) bool {
	if name == "" {
		return false
	}
	name = strings.Trim(name, `"`)
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		_, err := os.Stat(name)
		return err == nil
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// hasSpacePathRisk returns true when s contains spaces or tabs and is not
// already wrapped in double-quotes. Used to detect Windows path embedding risk.
func hasSpacePathRisk(s string) bool {
	return strings.ContainsAny(s, " \t") && !strings.HasPrefix(s, `"`)
}
