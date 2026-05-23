// Package config defines the Phase 1 MVP runtime configuration loaded by the
// orchestrator from .orca/config.yaml.
//
// The package intentionally has no dependencies on other internal packages.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the minimum viable Phase 1 configuration.
type Config struct {
	Verifier VerifierConfig
	Gate     GateConfig
	Budget   BudgetConfig
	Adapters AdapterConfig
}

type VerifierConfig struct {
	Gates      []VerifierGate
	WorkingDir string
}

type VerifierGate struct {
	Name     string
	Command  string
	Blocking bool
}

type GateConfig struct {
	// ReviewWindowSeconds is the auto-proceed window for single/low-risk projection
	// gates. 0 disables auto-proceed (gate blocks until explicit human input).
	// Default: 30. See gate.ReviewWindowFor for how this is applied per topology.
	ReviewWindowSeconds int
}

type BudgetConfig struct {
	DefaultMaxTokens          int
	DefaultMaxWallTimeSeconds int
	DefaultMaxRetries         int
}

type AdapterConfig struct {
	CodexPath  string
	ClaudePath string
}

// Default returns the documented Phase 1 config values.
func Default() *Config {
	return &Config{
		Verifier: VerifierConfig{
			Gates: []VerifierGate{
				{Name: "go_test", Command: "go test ./...", Blocking: true},
				{Name: "go_vet", Command: "go vet ./...", Blocking: true},
				{Name: "go_build", Command: "go build ./...", Blocking: true},
			},
		},
		Gate: GateConfig{
			ReviewWindowSeconds: 30,
		},
		Budget: BudgetConfig{
			DefaultMaxTokens:          32000,
			DefaultMaxWallTimeSeconds: 300,
			DefaultMaxRetries:         3,
		},
	}
}

// Load reads the MVP .orca/config.yaml file.
//
// Phase 1 only needs the simple YAML shape documented in plans/prebuild_1.md:
// nested sections, scalar keys, and a verifier.gates list. Keeping the parser
// local avoids adding a dependency before the runtime actually needs broader
// YAML support.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("config: path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	cfg := Default()
	cfg.Verifier.Gates = nil

	var section string
	var inVerifierGates bool
	var currentGate *VerifierGate

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := stripComment(scanner.Text())
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := leadingSpaces(raw)
		line := strings.TrimSpace(raw)

		if indent == 0 && strings.HasSuffix(line, ":") {
			section = strings.TrimSuffix(line, ":")
			inVerifierGates = false
			currentGate = nil
			continue
		}

		if section == "" {
			return nil, fmt.Errorf("config: line %d outside a section", lineNum)
		}

		switch section {
		case "verifier":
			if line == "gates:" {
				inVerifierGates = true
				currentGate = nil
				continue
			}
			if inVerifierGates && indent > 2 {
				if strings.HasPrefix(line, "- ") {
					cfg.Verifier.Gates = append(cfg.Verifier.Gates, VerifierGate{Blocking: true})
					currentGate = &cfg.Verifier.Gates[len(cfg.Verifier.Gates)-1]
					line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
					if line == "" {
						continue
					}
				}
				if currentGate == nil {
					return nil, fmt.Errorf("config: verifier gate field before list item on line %d", lineNum)
				}
				key, value, err := parseKeyValue(line, lineNum)
				if err != nil {
					return nil, err
				}
				if err := setVerifierGateField(currentGate, key, value, lineNum); err != nil {
					return nil, err
				}
				continue
			}
			inVerifierGates = false
			currentGate = nil
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			if key != "working_dir" {
				return nil, fmt.Errorf("config: unknown verifier field %q on line %d", key, lineNum)
			}
			cfg.Verifier.WorkingDir = value
		case "gate":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			if key != "review_window_seconds" {
				return nil, fmt.Errorf("config: unknown gate field %q on line %d", key, lineNum)
			}
			cfg.Gate.ReviewWindowSeconds, err = parseNonNegativeInt(value, lineNum)
			if err != nil {
				return nil, err
			}
		case "budget":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			n, err := parseNonNegativeInt(value, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "default_max_tokens":
				cfg.Budget.DefaultMaxTokens = n
			case "default_max_wall_time_seconds":
				cfg.Budget.DefaultMaxWallTimeSeconds = n
			case "default_max_retries":
				cfg.Budget.DefaultMaxRetries = n
			default:
				return nil, fmt.Errorf("config: unknown budget field %q on line %d", key, lineNum)
			}
		case "adapters":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "codex_path":
				cfg.Adapters.CodexPath = value
			case "claude_path":
				cfg.Adapters.ClaudePath = value
			default:
				return nil, fmt.Errorf("config: unknown adapters field %q on line %d", key, lineNum)
			}
		default:
			return nil, fmt.Errorf("config: unknown section %q on line %d", section, lineNum)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("config: scan %s: %w", path, err)
	}
	if len(cfg.Verifier.Gates) == 0 {
		return nil, fmt.Errorf("config: verifier.gates must contain at least one gate")
	}
	for i, gate := range cfg.Verifier.Gates {
		if gate.Name == "" {
			return nil, fmt.Errorf("config: verifier.gates[%d].name is required", i)
		}
		if gate.Command == "" {
			return nil, fmt.Errorf("config: verifier.gates[%d].command is required", i)
		}
	}
	if cfg.Budget.DefaultMaxTokens == 0 {
		return nil, fmt.Errorf("config: budget.default_max_tokens must be greater than 0")
	}
	if cfg.Budget.DefaultMaxWallTimeSeconds == 0 {
		return nil, fmt.Errorf("config: budget.default_max_wall_time_seconds must be greater than 0")
	}
	return cfg, nil
}

func stripComment(line string) string {
	inSingle, inDouble := false, false
	for i, r := range line {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '#' && !inSingle && !inDouble:
			return line[:i]
		}
	}
	return line
}

func leadingSpaces(line string) int {
	for i, r := range line {
		if r != ' ' {
			return i
		}
	}
	return len(line)
}

func parseKeyValue(line string, lineNum int) (string, string, error) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", fmt.Errorf("config: expected key/value on line %d", lineNum)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", fmt.Errorf("config: empty key on line %d", lineNum)
	}
	return key, parseScalar(value), nil
}

func parseScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func setVerifierGateField(gate *VerifierGate, key, value string, lineNum int) error {
	switch key {
	case "name":
		gate.Name = value
	case "command":
		gate.Command = value
	case "blocking":
		blocking, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
		}
		gate.Blocking = blocking
	default:
		return fmt.Errorf("config: unknown verifier gate field %q on line %d", key, lineNum)
	}
	return nil
}

func parseNonNegativeInt(value string, lineNum int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("config: invalid integer on line %d: %w", lineNum, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("config: negative integer on line %d", lineNum)
	}
	return n, nil
}
