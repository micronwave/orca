package schema

import "time"

// CapsuleRuntimeStatus is a pre-terminal diagnostic status emitted by the
// runner during a capsule execution. It covers only the interior of a run.
// Terminal outcomes (completed, failed) remain exclusively in CapsuleState,
// which is the durable materialized lifecycle record. The two types serve
// different purposes and must not overlap. orca.md Phase A §2.
type CapsuleRuntimeStatus string

const (
	RuntimeStatusSpawning           CapsuleRuntimeStatus = "spawning"
	RuntimeStatusPreflightWarning   CapsuleRuntimeStatus = "preflight_warning"
	RuntimeStatusPermissionRequired CapsuleRuntimeStatus = "permission_required"
	RuntimeStatusReadyForPrompt     CapsuleRuntimeStatus = "ready_for_prompt"
	RuntimeStatusPromptSent         CapsuleRuntimeStatus = "prompt_sent"
	RuntimeStatusAgentRunning       CapsuleRuntimeStatus = "agent_running"
	RuntimeStatusOutputCollecting   CapsuleRuntimeStatus = "output_collecting"
)

// CapsuleRuntimeFailureClass classifies why a capsule stopped making progress
// before reaching a terminal CapsuleState. Referenced by Item 1 (permission_gate)
// and Item 2 of Phase A.
type CapsuleRuntimeFailureClass string

const (
	RuntimeFailurePermissionGate    CapsuleRuntimeFailureClass = "permission_gate"
	RuntimeFailurePromptDelivery    CapsuleRuntimeFailureClass = "prompt_delivery"
	RuntimeFailureProvider          CapsuleRuntimeFailureClass = "provider"
	RuntimeFailureAdapterProtocol   CapsuleRuntimeFailureClass = "adapter_protocol"
	RuntimeFailureWorktreeState     CapsuleRuntimeFailureClass = "worktree_state"
	RuntimeFailureToolRuntime       CapsuleRuntimeFailureClass = "tool_runtime"
	RuntimeFailureStartupNoEvidence CapsuleRuntimeFailureClass = "startup_no_evidence"
)

// CapsuleRuntimeEvent is one entry in the diagnostic runtime status stream for
// a capsule. Events are ordered by Seq (monotonic event log sequence number).
type CapsuleRuntimeEvent struct {
	// Seq is filled by the store from the appended event log entry.
	Seq       int64                      `json:"seq"`
	CapsuleID string                     `json:"capsule_id"`
	GoalID    string                     `json:"goal_id"`
	Source    string                     `json:"source"` // e.g. "runner", "codex_adapter"
	Status    CapsuleRuntimeStatus       `json:"status"`
	FailClass CapsuleRuntimeFailureClass `json:"fail_class,omitempty"`
	Detail    string                     `json:"detail,omitempty"`
	OccurredAt time.Time                 `json:"occurred_at"`
}

// StartupEvidenceBundle is persisted when a capsule startup timeout occurs.
// It captures the last known runtime state so that diagnosis can answer
// whether the agent was ever launched and how far it progressed. orca.md Phase A §2.
type StartupEvidenceBundle struct {
	CapsuleID         string                     `json:"capsule_id"`
	LastStatus        CapsuleRuntimeStatus       `json:"last_status"`
	FailureClass      CapsuleRuntimeFailureClass `json:"failure_class"`
	ProcessCommand    string                     `json:"process_command,omitempty"`
	PromptSentAt      *time.Time                 `json:"prompt_sent_at,omitempty"`
	StdoutPreviewHash string                     `json:"stdout_preview_hash,omitempty"`
	StderrPreviewHash string                     `json:"stderr_preview_hash,omitempty"`
	HealthChecks      []string                   `json:"health_checks"`
	CreatedAt         time.Time                  `json:"created_at"`
}
