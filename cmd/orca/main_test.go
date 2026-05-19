package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

func TestRunLoadsConfigAndInitializesEventLog(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	err := run([]string{"--goal", "test goal", "--orca-dir", orcaDir})
	if err == nil {
		t.Fatalf("run error = nil, want scaffold runtime error")
	}
	if _, statErr := os.Stat(filepath.Join(orcaDir, "events.log")); statErr != nil {
		t.Fatalf("events.log was not created: %v", statErr)
	}
}

func TestReviewWindowForGateRules(t *testing.T) {
	defaultWindow := 30 * time.Second
	tests := []struct {
		name     string
		topology schema.Topology
		risk     schema.RiskLevel
		want     time.Duration
	}{
		{name: "human gated blocks", topology: schema.TopologyHumanGated, risk: schema.RiskLow, want: 0},
		{name: "implementer reviewer medium blocks", topology: schema.TopologyImplementerReviewer, risk: schema.RiskMedium, want: 0},
		{name: "single low uses default", topology: schema.TopologySingle, risk: schema.RiskLow, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewWindowFor(tt.topology, tt.risk, defaultWindow)
			if got != tt.want {
				t.Fatalf("reviewWindowFor() = %s, want %s", got, tt.want)
			}
		})
	}
}

func writeTestConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`
verifier:
  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
  working_dir: ""

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
}
