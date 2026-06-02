package schema

import "time"

// RecoveryScenario identifies the class of failure that triggered automatic recovery.
// orca.md Phase B §5.
type RecoveryScenario string

const (
	RecoveryPermissionGate         RecoveryScenario = "permission_gate"
	RecoveryPromptDelivery         RecoveryScenario = "prompt_delivery"
	RecoveryStaleBranch            RecoveryScenario = "stale_branch"
	RecoveryCompileFailure         RecoveryScenario = "compile_failure"
	RecoveryAdapterProtocolFailure RecoveryScenario = "adapter_protocol_failure"
	RecoveryProviderFailure        RecoveryScenario = "provider_failure"
	RecoveryCITimeout              RecoveryScenario = "ci_timeout"
)

// RecoveryOutcome is the result of one recovery attempt.
type RecoveryOutcome string

const (
	RecoveryOutcomeSucceeded RecoveryOutcome = "succeeded"
	RecoveryOutcomeFailed    RecoveryOutcome = "failed"
	RecoveryOutcomeEscalated RecoveryOutcome = "escalated"
)

// RecoveryLedgerEntry records one automatic recovery attempt.
// The ledger is keyed by (GoalID, CapsuleID, Scenario). AttemptNum starts at 1;
// escalation is triggered when AttemptNum > MaxAttempts. orca.md Phase B §5.
type RecoveryLedgerEntry struct {
	EntryID          string           `json:"entry_id"`
	GoalID           string           `json:"goal_id"`
	CapsuleID        string           `json:"capsule_id"`
	Scenario         RecoveryScenario `json:"scenario"`
	FailureClass     string           `json:"failure_class"`
	AttemptNum       int              `json:"attempt_num"`
	MaxAttempts      int              `json:"max_attempts"`
	CommandRun       string           `json:"command_run,omitempty"`
	CommandResult    string           `json:"command_result,omitempty"`
	Outcome          RecoveryOutcome  `json:"outcome"`
	EscalationReason string           `json:"escalation_reason,omitempty"`
	AttemptedAt      time.Time        `json:"attempted_at"`
}
