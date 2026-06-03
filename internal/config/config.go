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

// maxDurationSeconds is the upper bound for any config field that will be
// multiplied by time.Second to produce a time.Duration.  Values above this
// would overflow int64 nanoseconds (math.MaxInt64 / 1e9 ≈ 292 years; we cap
// at one year for operational sanity).
const maxDurationSeconds = 365 * 24 * 3600 // 31_536_000

// Config is the runtime configuration.
type Config struct {
	Verifier   VerifierConfig
	Gate       GateConfig
	Budget     BudgetConfig
	Adapters   AdapterConfig
	Advanced   AdvancedConfig
	MCP        MCPConfig
	Intake     IntakeConfig
	PR         PRConfig
	CI         CIConfig
	Remote     RemoteConfig
	Permission PermissionConfig
	Hooks      HooksConfig
}

// HooksConfig holds the optional lifecycle hook configuration. A nil HookConfig
// pointer in a field means the hook for that point is disabled. orca.md Phase D §9.
type HooksConfig struct {
	// PreCapsule fires after the permission check passes but before the adapter
	// preflight. A deny result blocks capsule launch.
	PreCapsule *HookConfig
	// PostVerify fires after all verifier gates complete. A deny result overrides
	// the verifier's recommended action to reject.
	PostVerify *HookConfig
}

// HookConfig configures one lifecycle hook executable.
type HookConfig struct {
	// Command is the executable path and optional fixed arguments, space-separated.
	Command string
	// TimeoutSeconds is the maximum wall time the hook may run. Defaults to 30s.
	TimeoutSeconds int
}

// PermissionConfig holds the default permission policy applied to capsules that
// do not carry an explicit PermissionMode. orca.md Phase A §1.
type PermissionConfig struct {
	// DefaultMode is the default PermissionMode for new capsules.
	// Valid values: "read_only", "workspace_write", "danger_full_access", "prompt".
	// Empty means the runner defaults to "danger_full_access" for Phase 1 parity.
	DefaultMode string
	// Rules is an ordered list of allow/deny/ask overrides applied after the
	// mode and tool checks. Deny wins when both a deny and allow rule match.
	Rules []PermissionRule
}

// PermissionRule is a single allow/deny/ask override in the permission config.
type PermissionRule struct {
	Tool    string // exact tool name; empty means all tools
	Pattern string // substring of InputSummary; empty means all inputs
	Effect  string // "allow", "deny", or "ask"
	Reason  string
}

// MCPConfig holds the optional MCP server settings. Zero value disables the feature.
type MCPConfig struct {
	Enabled bool
	Listen  string
}

// IntakeConfig holds settings for ingesting external issues. Zero value disables.
type IntakeConfig struct {
	GitHubToken string
	Repo        string
}

// PRConfig holds settings for automated pull request creation. Zero value disables.
type PRConfig struct {
	Enabled    bool
	BaseBranch string
	Draft      bool
	Label      string
}

// CIConfig holds settings for CI status polling. Zero value disables.
type CIConfig struct {
	Provider            string
	PollIntervalSeconds int
	Branch              string
}

// RemoteConfig holds settings for remote execution. Zero value disables.
type RemoteConfig struct {
	Enabled    bool
	Host       string
	Workspace  string
	SSHKeyPath string
}

// AdvancedConfig holds optional Phase 4 verification features. All fields
// default to false/zero/empty, meaning every advanced check is off unless
// explicitly enabled in config.yaml.
type AdvancedConfig struct {
	Enabled                   bool
	Maven                     bool
	Mutation                  bool
	MutationCommand           string
	MutationTimeoutSeconds    int
	MutationBlocking          bool
	AdversarialTests          bool
	AdversarialCommand        string
	AdversarialTimeoutSeconds int
	AdversarialBlocking       bool
	ReviewerDiversity         bool
}

type VerifierConfig struct {
	Gates      []VerifierGate
	WorkingDir string
}

// ValidateGates returns an error if the verifier gate configuration is
// insufficient to run an execution. Read-only commands (status, cancel) skip
// this check; call it only on paths that invoke the verifier.
func (c VerifierConfig) ValidateGates() error {
	if len(c.Gates) == 0 {
		return fmt.Errorf("config: verifier.gates must contain at least one gate")
	}
	for i, gate := range c.Gates {
		if gate.Name == "" {
			return fmt.Errorf("config: verifier.gates[%d].name is required", i)
		}
		if gate.Command == "" {
			return fmt.Errorf("config: verifier.gates[%d].command is required", i)
		}
		if gate.EvidenceType != "" && !isVerifierEvidenceType(gate.EvidenceType) {
			return fmt.Errorf(
				"config: verifier.gates[%d].evidence_type %q is invalid (want test_result|lint_result|typecheck_result|diff_risk_report|agent_output|static_analysis_result|mutation_survivor|agent_review)",
				i,
				gate.EvidenceType,
			)
		}
	}
	return nil
}

// TierTargetedTests, TierPackage, TierWorkspace are valid Tier values for VerifierGate.
// Gates without a Tier do not contribute to green-level classification. orca.md Phase B §4.
const (
	TierTargetedTests = "targeted_tests"
	TierPackage       = "package"
	TierWorkspace     = "workspace"
)

type VerifierGate struct {
	Name     string
	Command  string
	Blocking bool
	// Tier annotates this gate with a verification level for green-contract classification.
	// Valid values: "targeted_tests", "package", "workspace". Empty means no tier.
	Tier string
	// EvidenceType overrides the heuristic evidence type inference when non-empty.
	// Valid values are EvidenceType constants from internal/schema/evidence.go.
	EvidenceType string
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
	var inPermissionRules bool
	var currentRule *PermissionRule
	var hookName string

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
			inPermissionRules = false
			currentRule = nil
			hookName = ""
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
		case "advanced":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "enabled":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.Enabled = b
			case "maven":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.Maven = b
			case "mutation":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.Mutation = b
			case "mutation_command":
				cfg.Advanced.MutationCommand = value
			case "mutation_timeout_seconds":
				n, err := parseNonNegativeInt(value, lineNum)
				if err != nil {
					return nil, err
				}
				if n > maxDurationSeconds {
					return nil, fmt.Errorf("config: mutation_timeout_seconds %d exceeds maximum %d", n, maxDurationSeconds)
				}
				cfg.Advanced.MutationTimeoutSeconds = n
			case "mutation_blocking":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.MutationBlocking = b
			case "adversarial_tests":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.AdversarialTests = b
			case "adversarial_command":
				cfg.Advanced.AdversarialCommand = value
			case "adversarial_timeout_seconds":
				n, err := parseNonNegativeInt(value, lineNum)
				if err != nil {
					return nil, err
				}
				if n > maxDurationSeconds {
					return nil, fmt.Errorf("config: adversarial_timeout_seconds %d exceeds maximum %d", n, maxDurationSeconds)
				}
				cfg.Advanced.AdversarialTimeoutSeconds = n
			case "adversarial_blocking":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.AdversarialBlocking = b
			case "reviewer_diversity":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Advanced.ReviewerDiversity = b
			default:
				return nil, fmt.Errorf("config: unknown advanced field %q on line %d", key, lineNum)
			}
		case "mcp":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "enabled":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.MCP.Enabled = b
			case "listen":
				cfg.MCP.Listen = value
			default:
				return nil, fmt.Errorf("config: unknown mcp field %q on line %d", key, lineNum)
			}
		case "intake":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "github_token":
				cfg.Intake.GitHubToken = value
			case "repo":
				cfg.Intake.Repo = value
			default:
				return nil, fmt.Errorf("config: unknown intake field %q on line %d", key, lineNum)
			}
		case "pr":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "enabled":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.PR.Enabled = b
			case "base_branch":
				cfg.PR.BaseBranch = value
			case "draft":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.PR.Draft = b
			case "label":
				cfg.PR.Label = value
			default:
				return nil, fmt.Errorf("config: unknown pr field %q on line %d", key, lineNum)
			}
		case "ci":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "provider":
				cfg.CI.Provider = value
			case "poll_interval_seconds":
				n, err := parseNonNegativeInt(value, lineNum)
				if err != nil {
					return nil, err
				}
				if n > maxDurationSeconds {
					return nil, fmt.Errorf("config: poll_interval_seconds %d exceeds maximum %d", n, maxDurationSeconds)
				}
				cfg.CI.PollIntervalSeconds = n
			case "branch":
				cfg.CI.Branch = value
			default:
				return nil, fmt.Errorf("config: unknown ci field %q on line %d", key, lineNum)
			}
		case "remote":
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			switch key {
			case "enabled":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config: invalid boolean on line %d: %w", lineNum, err)
				}
				cfg.Remote.Enabled = b
			case "host":
				cfg.Remote.Host = value
			case "workspace":
				cfg.Remote.Workspace = value
			case "ssh_key_path":
				cfg.Remote.SSHKeyPath = value
			default:
				return nil, fmt.Errorf("config: unknown remote field %q on line %d", key, lineNum)
			}
		case "permission":
			if line == "rules:" {
				inPermissionRules = true
				currentRule = nil
				continue
			}
			if inPermissionRules && indent > 2 {
				if strings.HasPrefix(line, "- ") {
					cfg.Permission.Rules = append(cfg.Permission.Rules, PermissionRule{})
					currentRule = &cfg.Permission.Rules[len(cfg.Permission.Rules)-1]
					line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
					if line == "" {
						continue
					}
				}
				if currentRule == nil {
					return nil, fmt.Errorf("config: permission rule field before list item on line %d", lineNum)
				}
				key, value, err := parseKeyValue(line, lineNum)
				if err != nil {
					return nil, err
				}
				if err := setPermissionRuleField(currentRule, key, value, lineNum); err != nil {
					return nil, err
				}
				continue
			}
			inPermissionRules = false
			currentRule = nil
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			if key != "default_mode" {
				return nil, fmt.Errorf("config: unknown permission field %q on line %d", key, lineNum)
			}
			cfg.Permission.DefaultMode = value
		case "hooks":
			if indent == 2 && strings.HasSuffix(line, ":") {
				hookName = strings.TrimSuffix(line, ":")
				switch hookName {
				case "pre_capsule":
					if cfg.Hooks.PreCapsule == nil {
						cfg.Hooks.PreCapsule = &HookConfig{}
					}
				case "post_verify":
					if cfg.Hooks.PostVerify == nil {
						cfg.Hooks.PostVerify = &HookConfig{}
					}
				default:
					return nil, fmt.Errorf("config: unknown hooks field %q on line %d", hookName, lineNum)
				}
				continue
			}
			if hookName == "" {
				return nil, fmt.Errorf("config: hook field before hook name on line %d", lineNum)
			}
			key, value, err := parseKeyValue(line, lineNum)
			if err != nil {
				return nil, err
			}
			var hook *HookConfig
			switch hookName {
			case "pre_capsule":
				hook = cfg.Hooks.PreCapsule
			case "post_verify":
				hook = cfg.Hooks.PostVerify
			default:
				return nil, fmt.Errorf("config: unknown hooks field %q on line %d", hookName, lineNum)
			}
			if err := setHookConfigField(hook, key, value, lineNum); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("config: unknown section %q on line %d", section, lineNum)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("config: scan %s: %w", path, err)
	}
	if cfg.Budget.DefaultMaxTokens == 0 {
		return nil, fmt.Errorf("config: budget.default_max_tokens must be greater than 0")
	}
	if cfg.Budget.DefaultMaxWallTimeSeconds == 0 {
		return nil, fmt.Errorf("config: budget.default_max_wall_time_seconds must be greater than 0")
	}
	if cfg.Budget.DefaultMaxWallTimeSeconds > maxDurationSeconds {
		return nil, fmt.Errorf("config: budget.default_max_wall_time_seconds %d exceeds maximum %d", cfg.Budget.DefaultMaxWallTimeSeconds, maxDurationSeconds)
	}
	if cfg.Gate.ReviewWindowSeconds > maxDurationSeconds {
		return nil, fmt.Errorf("config: gate.review_window_seconds %d exceeds maximum %d", cfg.Gate.ReviewWindowSeconds, maxDurationSeconds)
	}
	for name, hook := range map[string]*HookConfig{
		"hooks.pre_capsule": cfg.Hooks.PreCapsule,
		"hooks.post_verify": cfg.Hooks.PostVerify,
	} {
		if hook == nil {
			continue
		}
		if hook.TimeoutSeconds > maxDurationSeconds {
			return nil, fmt.Errorf("config: %s.timeout_seconds %d exceeds maximum %d", name, hook.TimeoutSeconds, maxDurationSeconds)
		}
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

func setPermissionRuleField(rule *PermissionRule, key, value string, lineNum int) error {
	switch key {
	case "tool":
		rule.Tool = value
	case "pattern":
		rule.Pattern = value
	case "effect":
		switch value {
		case "allow", "deny", "ask":
			rule.Effect = value
		default:
			return fmt.Errorf("config: invalid permission rule effect %q on line %d (want allow|deny|ask)", value, lineNum)
		}
	case "reason":
		rule.Reason = value
	default:
		return fmt.Errorf("config: unknown permission rule field %q on line %d", key, lineNum)
	}
	return nil
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
	case "tier":
		switch value {
		case TierTargetedTests, TierPackage, TierWorkspace, "":
			gate.Tier = value
		default:
			return fmt.Errorf("config: invalid tier %q on line %d (want targeted_tests|package|workspace)", value, lineNum)
		}
	case "evidence_type":
		if value != "" && !isVerifierEvidenceType(value) {
			return fmt.Errorf(
				"config: invalid evidence_type %q on line %d (want test_result|lint_result|typecheck_result|diff_risk_report|agent_output|static_analysis_result|mutation_survivor|agent_review)",
				value,
				lineNum,
			)
		}
		gate.EvidenceType = value
	default:
		return fmt.Errorf("config: unknown verifier gate field %q on line %d", key, lineNum)
	}
	return nil
}

func isVerifierEvidenceType(value string) bool {
	switch value {
	case "test_result",
		"lint_result",
		"typecheck_result",
		"diff_risk_report",
		"agent_output",
		"static_analysis_result",
		"mutation_survivor",
		"agent_review":
		return true
	default:
		return false
	}
}

func setHookConfigField(hook *HookConfig, key, value string, lineNum int) error {
	if hook == nil {
		return fmt.Errorf("config: hook config missing on line %d", lineNum)
	}
	switch key {
	case "command":
		hook.Command = value
	case "timeout_seconds":
		n, err := parseNonNegativeInt(value, lineNum)
		if err != nil {
			return err
		}
		hook.TimeoutSeconds = n
	default:
		return fmt.Errorf("config: unknown hook field %q on line %d", key, lineNum)
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
