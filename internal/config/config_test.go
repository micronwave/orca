package config

import (
	"os"
	"path/filepath"
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
