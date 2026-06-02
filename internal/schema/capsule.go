package schema

// AgentType identifies the coding agent that runs inside a capsule. orca.md §5.3.
type AgentType string

const (
	AgentCodex   AgentType = "codex"
	AgentClaude  AgentType = "claude"
	AgentCopilot AgentType = "copilot"
	AgentTool    AgentType = "tool"
	// AgentMock is the agent type used by the deterministic test adapter.
	AgentMock AgentType = "mock"
)

// CapsuleRole is the functional role the agent plays inside the capsule.
type CapsuleRole string

const (
	RoleExecutor     CapsuleRole = "executor"
	RoleReviewer     CapsuleRole = "reviewer"
	RoleTester       CapsuleRole = "tester"
	RoleInvestigator CapsuleRole = "investigator"
)

// CapsuleState is the lifecycle state of an execution capsule.
// The capsule runner must track and expose these states;
// partial failures must leave no ambiguous intermediate state. orca.md §5.3.
type CapsuleState string

const (
	// CapsuleStatePending is the initial state assigned by the ObligationPlanner
	// when it creates a capsule. No worktree exists yet. The CapsuleRunner
	// transitions pending → worktree_created as its first action.
	CapsuleStatePending           CapsuleState = "pending"
	CapsuleStateWorktreeCreated   CapsuleState = "worktree_created"
	CapsuleStateWorkspaceAttached CapsuleState = "workspace_attached"
	CapsuleStateSetupRun          CapsuleState = "setup_run"
	CapsuleStateAgentRunning      CapsuleState = "agent_running"
	CapsuleStateCompleted         CapsuleState = "completed"
	CapsuleStateFailed            CapsuleState = "failed"
)

// NetworkPolicy controls outbound network access inside a capsule sandbox.
type NetworkPolicy string

const (
	NetworkDeny      NetworkPolicy = "deny"
	NetworkAllowlist NetworkPolicy = "allowlist"
	NetworkAllow     NetworkPolicy = "allow"
)

// CapsuleBudget limits the resources a capsule may consume. orca.md §5.3.
type CapsuleBudget struct {
	MaxTokens          int `json:"max_tokens"`
	MaxWallTimeSeconds int `json:"max_wall_time_seconds"`
	MaxRetries         int `json:"max_retries"`
}

// CapsuleSandbox defines the isolation policy for a capsule. orca.md §5.3.
type CapsuleSandbox struct {
	WorktreePath string        `json:"worktree_path"`
	Network      NetworkPolicy `json:"network"`
	// WriteScope describes allowed write targets, e.g. "worktree_only".
	WriteScope string `json:"write_scope"`
}

// PermissionMode is the effective permission level granted to a capsule's agent.
// It controls which operations the agent may perform and maps to adapter-specific
// sandbox or permission flags. orca.md Phase A §1.
type PermissionMode string

const (
	PermissionReadOnly         PermissionMode = "read_only"
	PermissionWorkspaceWrite   PermissionMode = "workspace_write"
	PermissionDangerFullAccess PermissionMode = "danger_full_access"
	PermissionPrompt           PermissionMode = "prompt"
)

// ExecutionCapsule is the contract for one agent or tool run.
// It is the most important primitive in Orca. orca.md §5.3.
type ExecutionCapsule struct {
	CapsuleID           string         `json:"capsule_id"`
	ObligationIDs       []string       `json:"obligation_ids"`
	Agent               AgentType      `json:"agent"`
	Role                CapsuleRole    `json:"role"`
	ContextProjectionID string         `json:"context_projection_id"`
	AllowedPaths        []string       `json:"allowed_paths"`
	ForbiddenPaths      []string       `json:"forbidden_paths"`
	AllowedTools        []string       `json:"allowed_tools"`
	ForbiddenActions    []string       `json:"forbidden_actions"`
	RequiredOutputs     []string       `json:"required_outputs"`
	VerifierGates       []string       `json:"verifier_gates"`
	Budget              CapsuleBudget  `json:"budget"`
	Sandbox             CapsuleSandbox `json:"sandbox"`
	State               CapsuleState   `json:"state"`
	// PermissionMode is the effective permission level for the agent. Defaults to
	// danger_full_access when empty (preserving Phase 1 behaviour). Set by the
	// ObligationPlanner from config. Enforced by the runner before adapter.Execute.
	PermissionMode PermissionMode `json:"permission_mode,omitempty"`
	// TopologyDecisionID is the ID of the DecisionRecord that captures the topology
	// classifier's selection and rationale for this capsule's plan cycle. Set by the
	// ObligationPlanner when it creates the capsule. The ContextCompiler loads this
	// via store.LoadDecision to populate HumanSummaryProjection.Topology.Rationale.
	TopologyDecisionID string `json:"topology_decision_id,omitempty"`
}
