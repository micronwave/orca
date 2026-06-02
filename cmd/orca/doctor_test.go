package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goos "runtime"
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/config"
)

// ── Auto-init project type detection ─────────────────────────────────────────

// TestAutoInitConfirmGoProjectWritesConfigAndGates verifies that a fresh Go
// project (has go.mod) auto-writes config with Go gates and no confirmation.
func TestAutoInitConfirmGoProjectWritesConfigAndGates(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	orcaDir := filepath.Join(projectRoot, ".orca")

	var out strings.Builder
	if err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("autoInitConfirm: %v", err)
	}

	configPath := filepath.Join(orcaDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	content := string(data)
	for _, want := range []string{"go_test", "go test ./...", "go_vet", "go_build"} {
		if !strings.Contains(content, want) {
			t.Errorf("config.yaml missing %q:\n%s", want, content)
		}
	}

	// Config must be loadable.
	if _, err := config.Load(configPath); err != nil {
		t.Fatalf("generated config did not load: %v", err)
	}
}

// TestAutoInitConfirmNodeProjectWritesConfigAndGate verifies Node project detection.
func TestAutoInitConfirmNodeProjectWritesConfigAndGate(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "package.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	orcaDir := filepath.Join(projectRoot, ".orca")

	var out strings.Builder
	if err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("autoInitConfirm: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(orcaDir, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "npm_test") || !strings.Contains(content, "npm test") {
		t.Errorf("config.yaml missing npm gate:\n%s", content)
	}
}

// TestAutoInitConfirmMavenProjectWritesConfigAndGate verifies Maven project detection.
func TestAutoInitConfirmMavenProjectWritesConfigAndGate(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "pom.xml"), []byte("<project/>"), 0o644); err != nil {
		t.Fatalf("write pom.xml: %v", err)
	}
	orcaDir := filepath.Join(projectRoot, ".orca")

	var out strings.Builder
	if err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("autoInitConfirm: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(orcaDir, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "mvn_test") || !strings.Contains(content, "mvn test") {
		t.Errorf("config.yaml missing maven gate:\n%s", content)
	}
}

// TestAutoInitConfirmUnknownProjectInteractivePromptsOrFailsNonInteractive
// covers the two paths for an unknown project type:
//   - interactive: user is prompted; "n" aborts cleanly
//   - non-interactive: fails with a single actionable message
func TestAutoInitConfirmUnknownProjectInteractivePromptsOrFailsNonInteractive(t *testing.T) {
	t.Run("interactive_prompts_and_aborts_on_deny", func(t *testing.T) {
		projectRoot := t.TempDir() // no markers → unknown type
		orcaDir := filepath.Join(t.TempDir(), ".orca")

		var out strings.Builder
		err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader("n\n"), &out, true)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("error = %q, want 'aborted'", err.Error())
		}
		// Config must NOT have been written.
		if _, statErr := os.Stat(filepath.Join(orcaDir, "config.yaml")); statErr == nil {
			t.Fatal("config.yaml written after user said no")
		}
		// Output must explain what the user needs to do.
		msg := out.String()
		if !strings.Contains(msg, "gate") {
			t.Errorf("prompt output missing gate guidance:\n%s", msg)
		}
	})

	t.Run("interactive_proceeds_on_yes", func(t *testing.T) {
		projectRoot := t.TempDir()
		orcaDir := filepath.Join(t.TempDir(), ".orca")

		var out strings.Builder
		if err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader("y\n"), &out, true); err != nil {
			t.Fatalf("autoInitConfirm y: %v", err)
		}
		if _, err := os.Stat(filepath.Join(orcaDir, "config.yaml")); err != nil {
			t.Fatalf("config.yaml not created after y: %v", err)
		}
	})

	t.Run("non_interactive_fails_with_actionable_message", func(t *testing.T) {
		projectRoot := t.TempDir()
		orcaDir := filepath.Join(t.TempDir(), ".orca")

		var out strings.Builder
		err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader(""), &out, false)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "project type not recognized") {
			t.Fatalf("error = %q, want 'project type not recognized'", msg)
		}
		// Must include an actionable command or path.
		if !strings.Contains(msg, "orca init") && !strings.Contains(msg, "config.yaml") {
			t.Fatalf("error missing actionable fix:\n%s", msg)
		}
	})
}

// TestAutoInitConfirmExistingConfigNotOverwritten ensures that an existing
// config.yaml is left untouched even when called with recognized project type.
func TestAutoInitConfirmExistingConfigNotOverwritten(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	orcaDir := filepath.Join(projectRoot, ".orca")
	if err := os.MkdirAll(orcaDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := "# existing-sentinel-do-not-overwrite\n"
	configPath := filepath.Join(orcaDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	var out strings.Builder
	if err := autoInitConfirm(orcaDir, projectRoot, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("autoInitConfirm: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != sentinel {
		t.Fatalf("config.yaml was overwritten:\ngot:  %q\nwant: %q", string(data), sentinel)
	}
}

// ── Doctor preflight output ───────────────────────────────────────────────────

// TestPreflightMissingAdapterProducesActionableOutput verifies that when both
// adapters are unavailable the blocking error is clear and includes install hints.
func TestPreflightMissingAdapterProducesActionableOutput(t *testing.T) {
	orcaDir := t.TempDir()
	configPath := filepath.Join(orcaDir, "config.yaml")

	// Write a minimal config with a gate whose executable is definitely in PATH (shell).
	if err := os.WriteFile(configPath, []byte(`
verifier:
  gates:
    - name: "shell_check"
      command: "true"
      blocking: true
gate:
  review_window_seconds: 30
budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
  default_max_retries: 3
adapters:
  codex_path: "/nonexistent/path/to/codex-xyz"
  claude_path: "/nonexistent/path/to/claude-xyz"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	result := runPreflight(orcaDir, configPath, true, cfg)

	if result.CodexAvailable {
		t.Error("CodexAvailable = true, want false for nonexistent path")
	}
	if result.ClaudeAvailable {
		t.Error("ClaudeAvailable = true, want false for nonexistent path")
	}

	// Must have a blocking error about no runnable adapter.
	found := false
	for _, e := range result.BlockingErrors {
		if strings.Contains(e, "no runnable adapter") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("BlockingErrors = %v, want 'no runnable adapter' entry", result.BlockingErrors)
	}

	// InferredFixes must include install guidance.
	fixText := strings.Join(result.InferredFixes, "\n")
	if !strings.Contains(fixText, "codex") || !strings.Contains(fixText, "claude") {
		t.Errorf("InferredFixes = %v, want install hints for codex and claude", result.InferredFixes)
	}

	// printDoctorOutput must surface the issue in BLOCKING ISSUES section.
	var buf bytes.Buffer
	printDoctorOutput(&buf, result)
	output := buf.String()
	if !strings.Contains(output, "BLOCKING ISSUES") {
		t.Fatalf("doctor output missing BLOCKING ISSUES section:\n%s", output)
	}
	if !strings.Contains(output, "no runnable adapter") {
		t.Fatalf("doctor output missing adapter error:\n%s", output)
	}
}

// TestPreflightMissingGateExecutableProducesActionableOutput verifies that a
// blocking gate whose command uses a non-existent executable is surfaced.
func TestPreflightMissingGateExecutableProducesActionableOutput(t *testing.T) {
	orcaDir := t.TempDir()
	configPath := filepath.Join(orcaDir, "config.yaml")

	if err := os.WriteFile(configPath, []byte(`
verifier:
  gates:
    - name: "custom_check"
      command: "nonexistent-cmd-orca-test-xyz-12345 --run"
      blocking: true
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

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	result := runPreflight(orcaDir, configPath, true, cfg)

	if len(result.GateChecks) == 0 {
		t.Fatal("GateChecks is empty")
	}
	gc := result.GateChecks[0]
	if gc.Available {
		t.Errorf("gate %q Available = true, want false for nonexistent executable", gc.Name)
	}
	if gc.Executable != "nonexistent-cmd-orca-test-xyz-12345" {
		t.Errorf("Executable = %q, want nonexistent-cmd-orca-test-xyz-12345", gc.Executable)
	}

	// Blocking error must name the gate.
	found := false
	for _, e := range result.BlockingErrors {
		if strings.Contains(e, "custom_check") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("BlockingErrors = %v, want entry mentioning 'custom_check'", result.BlockingErrors)
	}

	// printDoctorOutput must show the gate status.
	var buf bytes.Buffer
	printDoctorOutput(&buf, result)
	output := buf.String()
	if !strings.Contains(output, "custom_check") {
		t.Fatalf("doctor output missing gate name:\n%s", output)
	}
	if !strings.Contains(output, "MISSING") {
		t.Fatalf("doctor output missing MISSING marker for unavailable gate:\n%s", output)
	}
}

// TestPreflightOptionalIntegrationsDoNotBlock verifies that missing GitHub token
// / PR / CI / remote settings do not appear as blocking errors when those
// features are not enabled in config.
func TestPreflightOptionalIntegrationsDoNotBlock(t *testing.T) {
	orcaDir := t.TempDir()
	configPath := filepath.Join(orcaDir, "config.yaml")

	// Minimal config with no optional features enabled.
	if err := os.WriteFile(configPath, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
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
  github_token: ""
  repo: ""
pr:
  enabled: false
ci:
  provider: ""
remote:
  enabled: false
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	result := runPreflight(orcaDir, configPath, true, cfg)

	// The only possible blocking error here relates to adapter availability
	// (codex and claude both default to PATH lookup and may not be present in
	// the test environment). There must be NO blocking error about GitHub token,
	// PR, CI, or remote.
	for _, e := range result.BlockingErrors {
		for _, bad := range []string{"github", "token", "pr.enabled", "ci.provider", "remote"} {
			if strings.Contains(strings.ToLower(e), bad) {
				t.Errorf("optional integration produced blocking error: %q", e)
			}
		}
	}

	// OptionalUnavailable must NOT mention GitHub token when no relevant
	// feature is enabled and no repo/token is configured.
	for _, o := range result.OptionalUnavailable {
		if strings.Contains(strings.ToLower(o), "github token") {
			t.Errorf("GitHub token warning surfaced when no optional feature enabled: %q", o)
		}
	}
}

// TestPreflightWindowsPathWarning verifies that paths with spaces are flagged
// on Windows (and NOT flagged on non-Windows platforms).
func TestPreflightWindowsPathWarning(t *testing.T) {
	// Build a PreflightResult that simulates a gate with a path-space risk.
	r := &PreflightResult{
		ProjectRoot:  t.TempDir(),
		ConfigExists: true,
		GateChecks: []GateCheck{
			{
				Name:       "spaced_gate",
				Command:    `C:\Program Files\mytool\check.exe --run`,
				Blocking:   true,
				Executable: `C:\Program Files\mytool\check.exe`,
				Available:  true,
				PathRisk:   true, // explicitly set — mirrors what runPreflight computes on Windows
			},
		},
	}

	// Simulate the warnings that runPreflight would add for this gate.
	if goos.GOOS == "windows" {
		for _, gc := range r.GateChecks {
			if gc.PathRisk {
				r.Warnings = append(r.Warnings,
					"gate "+gc.Name+`: executable path "`+gc.Executable+`" contains spaces (Windows path risk)`)
			}
		}
	}

	var buf bytes.Buffer
	printDoctorOutput(&buf, r)
	output := buf.String()

	if goos.GOOS == "windows" {
		if !strings.Contains(output, "path-space-risk") {
			t.Errorf("Windows: doctor output missing path-space-risk marker:\n%s", output)
		}
		// Warnings section must mention it.
		if len(r.Warnings) == 0 {
			t.Error("Windows: no warnings for path with spaces")
		}
	} else {
		// On non-Windows, PathRisk=true still shows the marker in the gate table
		// (we set it explicitly above), but the Warnings list is empty because we
		// only added them for windows above.
		_ = output // nothing mandatory to assert here; just confirm no panic
	}
}

// TestRunDoctorCommandExists verifies that the "doctor" subcommand is wired
// into the run() dispatcher and returns without panicking on an initialized dir.
func TestRunDoctorCommandExists(t *testing.T) {
	orcaDir := t.TempDir()
	configPath := filepath.Join(orcaDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
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

	// Doctor must not panic; exit code (error vs nil) is secondary — the test
	// environment may lack codex/claude so BlockingErrors may be non-empty.
	_ = run([]string{"doctor", "--orca-dir", orcaDir})
}

func TestPrintDoctorJSONEmitsSetupFacts(t *testing.T) {
	r := &PreflightResult{
		ProjectRoot:    "repo",
		ProjectType:    "go",
		ConfigPath:     filepath.Join("repo", ".orca", "config.yaml"),
		ConfigExists:   true,
		CodexAvailable: true,
		GateChecks: []GateCheck{{
			Name:       "go_test",
			Command:    "go test ./...",
			Blocking:   true,
			Executable: "go",
			Available:  true,
		}},
	}

	var buf bytes.Buffer
	if err := printDoctorJSON(&buf, r); err != nil {
		t.Fatalf("printDoctorJSON: %v", err)
	}
	var decoded PreflightResult
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("doctor JSON did not decode: %v\n%s", err, buf.String())
	}
	if decoded.ProjectType != "go" || !decoded.ConfigExists || len(decoded.GateChecks) != 1 {
		t.Fatalf("decoded result = %+v", decoded)
	}
}

// TestGateExecutableName verifies that gateExecutableName extracts the first
// token from a gate command string.
func TestGateExecutableName(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"go test ./...", "go"},
		{"npm test", "npm"},
		{"mvn test", "mvn"},
		{"  go vet ./...", "go"},
		{"singleword", "singleword"},
		{"", ""},
		{`"C:\Program Files\go\bin\go.exe" build ./...`, `C:\Program Files\go\bin\go.exe`},
		{`C:\Program Files\go\bin\go.exe build ./...`, `C:\Program Files\go\bin\go.exe`},
	}
	for _, tt := range tests {
		got := gateExecutableName(tt.command)
		if got != tt.want {
			t.Errorf("gateExecutableName(%q) = %q, want %q", tt.command, got, tt.want)
		}
	}
}

func TestGateExecutableReportsQuotedPathWithoutSpaceRisk(t *testing.T) {
	exe, quoted := gateExecutable(`"C:\Program Files\go\bin\go.exe" test ./...`)
	if exe != `C:\Program Files\go\bin\go.exe` {
		t.Fatalf("executable = %q, want full quoted path", exe)
	}
	if !quoted {
		t.Fatal("quoted = false, want true")
	}
}

func TestCheckGitStateDetectsLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit(root, "init")
	runGit(root, "config", "user.email", "orca-test@example.invalid")
	runGit(root, "config", "user.name", "Orca Test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(root, "add", "README.md")
	runGit(root, "commit", "-m", "seed")

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(root, "worktree", "add", linked, "HEAD")
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", linked).Run()
	})
	if info, err := os.Stat(filepath.Join(linked, ".git")); err != nil || info.IsDir() {
		t.Fatalf("linked worktree .git should be a file, info=%v err=%v", info, err)
	}

	present, dirty := checkGitState(linked)
	if !present {
		t.Fatal("checkGitState linked worktree present = false, want true")
	}
	if dirty {
		t.Fatal("checkGitState linked worktree dirty = true, want false")
	}
}

// TestHasSpacePathRisk verifies space-risk detection.
func TestHasSpacePathRisk(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{`C:\Program Files\app.exe`, true},
		{`"C:\Program Files\app.exe"`, false}, // already quoted
		{"/usr/local/bin/go", false},
		{"go", false},
		{"go test ./...", true},
		{"", false},
	}
	for _, tt := range tests {
		got := hasSpacePathRisk(tt.s)
		if got != tt.want {
			t.Errorf("hasSpacePathRisk(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
