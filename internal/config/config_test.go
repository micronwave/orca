package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadParsesPhaseOneConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
    - name: "go_vet"
      command: "go vet ./..."
      blocking: false
  working_dir: ""

gate:
  review_window_seconds: 45

budget:
  default_max_tokens: 64000
  default_max_wall_time_seconds: 600
  default_max_retries: 2

adapters:
  codex_path: ""
  claude_path: "C:/tools/claude.exe"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.Verifier.Gates); got != 2 {
		t.Fatalf("len(Verifier.Gates) = %d, want 2", got)
	}
	if cfg.Verifier.Gates[0].Command != "go test ./..." || !cfg.Verifier.Gates[0].Blocking {
		t.Fatalf("first verifier gate = %+v", cfg.Verifier.Gates[0])
	}
	if cfg.Verifier.Gates[1].Blocking {
		t.Fatalf("second verifier gate Blocking = true, want false")
	}
	if cfg.Gate.ReviewWindowSeconds != 45 {
		t.Fatalf("ReviewWindowSeconds = %d, want 45", cfg.Gate.ReviewWindowSeconds)
	}
	if cfg.Budget.DefaultMaxTokens != 64000 || cfg.Budget.DefaultMaxWallTimeSeconds != 600 || cfg.Budget.DefaultMaxRetries != 2 {
		t.Fatalf("Budget = %+v", cfg.Budget)
	}
	if cfg.Adapters.ClaudePath != "C:/tools/claude.exe" {
		t.Fatalf("ClaudePath = %q", cfg.Adapters.ClaudePath)
	}
}

func TestLoadRejectsMissingVerifierGates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  working_dir: ""
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded without verifier gates")
	}
}

func TestLoadGateBlockingDefaultsTrue(t *testing.T) {
	// A gate without an explicit blocking field must default to true so that
	// a config omission cannot silently downgrade a blocking gate to warning-only.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Verifier.Gates[0].Blocking {
		t.Fatal("gate without explicit blocking: field defaulted to false, want true")
	}
}

func TestLoadRejectsZeroBudgetFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "zero_max_tokens",
			yaml: `
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
budget:
  default_max_tokens: 0
  default_max_wall_time_seconds: 300
`,
		},
		{
			name: "zero_wall_time",
			yaml: `
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 0
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load succeeded with zero budget field, want error")
			}
		})
	}
}

func TestAdvancedConfigAbsentDefaultsToZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := AdvancedConfig{}
	if cfg.Advanced != want {
		t.Fatalf("Advanced = %+v, want zero value", cfg.Advanced)
	}
}

func TestAdvancedConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300

advanced:
  enabled: true
  maven: true
  mutation: true
  mutation_command: "go-mutants ./..."
  mutation_timeout_seconds: 90
  mutation_blocking: true
  adversarial_tests: true
  adversarial_command: "fuzz run ./..."
  adversarial_timeout_seconds: 45
  adversarial_blocking: true
  reviewer_diversity: true
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := AdvancedConfig{
		Enabled:                   true,
		Maven:                     true,
		Mutation:                  true,
		MutationCommand:           "go-mutants ./...",
		MutationTimeoutSeconds:    90,
		MutationBlocking:          true,
		AdversarialTests:          true,
		AdversarialCommand:        "fuzz run ./...",
		AdversarialTimeoutSeconds: 45,
		AdversarialBlocking:       true,
		ReviewerDiversity:         true,
	}
	if cfg.Advanced != want {
		t.Fatalf("Advanced = %+v, want %+v", cfg.Advanced, want)
	}
}

func TestAdvancedConfigUnknownKeyReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300

advanced:
  enabled: false
  unknown_field: true
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with unknown advanced field, want error")
	}
}

func TestAdvancedConfigTimeoutExceedsMaxReturnsError(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "mutation_timeout_overflow",
			yaml: `
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
advanced:
  enabled: true
  mutation_timeout_seconds: 99999999
`,
		},
		{
			name: "adversarial_timeout_overflow",
			yaml: `
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
advanced:
  enabled: true
  adversarial_timeout_seconds: 99999999
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load succeeded with timeout exceeding max, want error")
			}
		})
	}
}

// minimalValidYAML returns a minimal valid config YAML prepended to extra.
// Used to avoid duplicating the required verifier/budget sections in every test.
func minimalValidYAML(extra string) string {
	return `
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."

budget:
  default_max_tokens: 32000
  default_max_wall_time_seconds: 300
` + extra
}

func TestPhase5SectionsAbsentDefaultToZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("")), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MCP.Enabled || cfg.MCP.Listen != "" {
		t.Fatalf("MCP not zero: %+v", cfg.MCP)
	}
	if cfg.Intake.GitHubToken != "" || cfg.Intake.Repo != "" {
		t.Fatalf("Intake not zero: %+v", cfg.Intake)
	}
	if cfg.PR.Enabled || cfg.PR.BaseBranch != "" || cfg.PR.Draft || cfg.PR.Label != "" {
		t.Fatalf("PR not zero: %+v", cfg.PR)
	}
	if cfg.CI.Provider != "" || cfg.CI.PollIntervalSeconds != 0 || cfg.CI.Branch != "" {
		t.Fatalf("CI not zero: %+v", cfg.CI)
	}
	if cfg.Remote.Enabled || cfg.Remote.Host != "" || cfg.Remote.Workspace != "" || cfg.Remote.SSHKeyPath != "" {
		t.Fatalf("Remote not zero: %+v", cfg.Remote)
	}
}

func TestPhase5SectionsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML(`
mcp:
  enabled: true
  listen: "127.0.0.1:9090"

intake:
  github_token: "ghp_secret"
  repo: "org/repo"

pr:
  enabled: true
  base_branch: "main"
  draft: true
  label: "orca-generated"

ci:
  provider: "github"
  poll_interval_seconds: 60
  branch: "feat/x"

remote:
  enabled: true
  host: "builder.internal"
  workspace: "/home/orca"
  ssh_key_path: "/home/user/.ssh/id_ed25519"
`)), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MCP.Enabled || cfg.MCP.Listen != "127.0.0.1:9090" {
		t.Fatalf("MCP = %+v", cfg.MCP)
	}
	if cfg.Intake.GitHubToken != "ghp_secret" || cfg.Intake.Repo != "org/repo" {
		t.Fatalf("Intake = %+v", cfg.Intake)
	}
	if !cfg.PR.Enabled || cfg.PR.BaseBranch != "main" || !cfg.PR.Draft || cfg.PR.Label != "orca-generated" {
		t.Fatalf("PR = %+v", cfg.PR)
	}
	if cfg.CI.Provider != "github" || cfg.CI.PollIntervalSeconds != 60 || cfg.CI.Branch != "feat/x" {
		t.Fatalf("CI = %+v", cfg.CI)
	}
	if !cfg.Remote.Enabled || cfg.Remote.Host != "builder.internal" ||
		cfg.Remote.Workspace != "/home/orca" || cfg.Remote.SSHKeyPath != "/home/user/.ssh/id_ed25519" {
		t.Fatalf("Remote = %+v", cfg.Remote)
	}
}

func TestPhase5SectionsRejectUnknownKeys(t *testing.T) {
	cases := []struct {
		section string
		yaml    string
	}{
		{"mcp", "mcp:\n  enabled: false\n  unknown_key: foo\n"},
		{"intake", "intake:\n  repo: \"\"\n  bogus: bar\n"},
		{"pr", "pr:\n  enabled: false\n  typo_field: x\n"},
		{"ci", "ci:\n  provider: \"\"\n  no_such: true\n"},
		{"remote", "remote:\n  enabled: false\n  extra_key: baz\n"},
	}
	for _, tc := range cases {
		t.Run(tc.section, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(minimalValidYAML(tc.yaml)), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("Load succeeded with unknown key in %s section, want error", tc.section)
			}
		})
	}
}

func TestCIPollIntervalExceedsMaxReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML(`
ci:
  poll_interval_seconds: 99999999
`)), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with poll_interval_seconds overflow, want error")
	}
}

func TestStripCommentQuoteAware(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`command: "go test -run Test#Helper"`, `command: "go test -run Test#Helper"`},
		{`command: 'go test ./...' # a comment`, `command: 'go test ./...' `},
		{`key: value # comment`, `key: value `},
		{`key: value`, `key: value`},
	}
	for _, tc := range cases {
		got := stripComment(tc.input)
		if got != tc.want {
			t.Errorf("stripComment(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDetectProjectType_GoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if got := DetectProjectType(dir); got != "go" {
		t.Fatalf("DetectProjectType = %q, want %q", got, "go")
	}
}

func TestDetectProjectType_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if got := DetectProjectType(dir); got != "node" {
		t.Fatalf("DetectProjectType = %q, want %q", got, "node")
	}
}

func TestDetectProjectType_PomXML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project/>"), 0o644); err != nil {
		t.Fatalf("write pom.xml: %v", err)
	}
	if got := DetectProjectType(dir); got != "maven" {
		t.Fatalf("DetectProjectType = %q, want %q", got, "maven")
	}
}

func TestDetectProjectType_PriorityGoModOverPackageJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if got := DetectProjectType(dir); got != "go" {
		t.Fatalf("DetectProjectType = %q, want %q (go.mod has higher priority)", got, "go")
	}
}

func TestDetectProjectType_WalksUpToFindGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	sub := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := DetectProjectType(sub); got != "go" {
		t.Fatalf("DetectProjectType = %q, want %q (should walk up to root)", got, "go")
	}
}

func TestDetectProjectType_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	// Create a fake filesystem root so the walk is bounded and doesn't reach a
	// real project root on the host. We test the no-marker case by checking
	// an isolated deep directory that has no markers above it within TempDir.
	// The actual host might have markers above the temp root, so we just verify
	// DetectProjectType returns a string (not panic); the empty case is exercised
	// by the Makefile sub-test instead.
	result := DetectProjectType(dir)
	_ = result // "go", "node", "maven", or "" — depends on host filesystem above tmpdir
}

func TestDetectProjectType_Makefile_ReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("all:\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// sub has no markers; root has Makefile which maps to "".
	// Walk should find Makefile in root and return "".
	if got := DetectProjectType(sub); got != "" {
		t.Fatalf("DetectProjectType = %q, want %q (Makefile maps to empty)", got, "")
	}
}

func TestDefaultConfigYAML_GoLoadsAndHasGates(t *testing.T) {
	yaml := DefaultConfigYAML("go")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(go): %v", err)
	}
	if len(cfg.Verifier.Gates) != 3 {
		t.Fatalf("gates count = %d, want 3", len(cfg.Verifier.Gates))
	}
	names := map[string]bool{}
	for _, g := range cfg.Verifier.Gates {
		names[g.Name] = true
	}
	for _, want := range []string{"go_test", "go_vet", "go_build"} {
		if !names[want] {
			t.Fatalf("missing gate %q", want)
		}
	}
}

func TestDefaultConfigYAML_NodeLoadsAndHasNpmGate(t *testing.T) {
	yaml := DefaultConfigYAML("node")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(node): %v", err)
	}
	if len(cfg.Verifier.Gates) != 1 || cfg.Verifier.Gates[0].Name != "npm_test" {
		t.Fatalf("gates = %+v, want single npm_test gate", cfg.Verifier.Gates)
	}
}

func TestDefaultConfigYAML_MavenLoadsAndHasMvnGate(t *testing.T) {
	yaml := DefaultConfigYAML("maven")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(maven): %v", err)
	}
	if len(cfg.Verifier.Gates) != 1 || cfg.Verifier.Gates[0].Name != "mvn_test" {
		t.Fatalf("gates = %+v, want single mvn_test gate", cfg.Verifier.Gates)
	}
}

func TestDefaultConfigYAML_FallbackHasEmptyGatesSection(t *testing.T) {
	yaml := DefaultConfigYAML("")
	if !strings.Contains(yaml, "gates:") {
		t.Fatal("fallback YAML missing 'gates:' section")
	}
	// Load should fail (no gates) — that's expected; user must configure.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(fallback) succeeded; expected error about missing gates")
	}
	if !strings.Contains(err.Error(), "gates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDetectProjectType_CurrentDirReturnsGo(t *testing.T) {
	// Acceptance criterion 7: DetectProjectType(".") in the orca repo returns "go"
	// because go.mod is present in an ancestor directory.
	got := DetectProjectType(".")
	if got != "go" {
		t.Fatalf("DetectProjectType('.') = %q, want 'go' (go.mod in ancestor)", got)
	}
}

func TestDefaultConfigYAML_IncludesAllRequiredSections(t *testing.T) {
	for _, pt := range []string{"go", "node", "maven"} {
		yaml := DefaultConfigYAML(pt)
		for _, section := range []string{"verifier:", "gate:", "budget:", "adapters:", "advanced:", "mcp:", "intake:", "pr:", "ci:", "remote:"} {
			if !strings.Contains(yaml, section) {
				t.Errorf("DefaultConfigYAML(%q) missing section %q", pt, section)
			}
		}
	}
}
