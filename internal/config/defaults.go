package config

import (
	"os"
	"path/filepath"
)

// DetectProjectType returns "go", "node", "maven", or "" based on files present
// in dir or any ancestor directory. Checks in priority order at each level:
// go.mod > package.json > pom.xml > Makefile.
func DetectProjectType(dir string) string {
	candidates := []struct {
		file string
		typ  string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"pom.xml", "maven"},
		{"Makefile", ""},
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for {
		for _, c := range candidates {
			if _, err := os.Stat(filepath.Join(abs, c.file)); err == nil {
				return c.typ
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return ""
}

// DefaultConfigYAML returns the full default config.yaml content for the given
// project type. projectType should be the return value of DetectProjectType.
// Empty string returns a config with no verifier gates (user adds their own).
func DefaultConfigYAML(projectType string) string {
	var gatesBlock string
	switch projectType {
	case "go":
		gatesBlock = `  gates:
    - name: "go_test"
      command: "go test ./..."
      blocking: true
    - name: "go_vet"
      command: "go vet ./..."
      blocking: true
    - name: "go_build"
      command: "go build ./..."
      blocking: true`
	case "node":
		gatesBlock = `  gates:
    - name: "npm_test"
      command: "npm test"
      blocking: true`
	case "maven":
		gatesBlock = `  gates:
    - name: "mvn_test"
      command: "mvn test"
      blocking: true`
	default:
		gatesBlock = `  gates:
    # Add at least one gate before running orca goal.`
	}

	return `# Orca Phase 5 local runtime configuration.
# Keep this file in the simple shape supported by internal/config.Load:
# sections, scalar values, and verifier.gates list items only.

verifier:
  # Gates run from working_dir when set; empty means the current process directory.
  working_dir: ""
` + gatesBlock + `

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

advanced:
  # All advanced checks are off by default. Enable explicitly when needed.
  enabled: false
  maven: false
  mutation: false
  mutation_command: ""
  mutation_timeout_seconds: 120
  mutation_blocking: false
  adversarial_tests: false
  adversarial_command: ""
  adversarial_timeout_seconds: 60
  adversarial_blocking: false
  reviewer_diversity: false

mcp:
  enabled: false
  listen: "127.0.0.1:7070"

intake:
  github_token: ""
  repo: ""

pr:
  enabled: false
  base_branch: ""
  draft: true
  label: "orca-generated"

ci:
  provider: ""
  poll_interval_seconds: 30
  branch: ""

remote:
  enabled: false
  host: ""
  workspace: ""
  ssh_key_path: ""
`
}
