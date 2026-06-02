// Package recovery evaluates recovery recipes for failed capsule runs and
// records attempts in the recovery ledger. It is distinct from failurehistory,
// which handles failure fingerprint deduplication and stamping.
//
// Recovery is conservative: one automatic attempt is enough for most scenarios.
// After MaxAttempts, the entry is escalated to a blocked decision or follow-up
// obligation. orca.md Phase B §5.
package recovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
)

// Store is the narrow store interface consumed by the recovery package.
// It matches the methods on *store.FileStore that recovery needs.
type Store interface {
	SaveRecoveryEntry(ctx context.Context, entry *schema.RecoveryLedgerEntry) error
	LoadRecoveryEntriesForCapsule(ctx context.Context, capsuleID string) ([]*schema.RecoveryLedgerEntry, error)
}

// ClassifyScenario maps a CapsuleRuntimeFailureClass to a RecoveryScenario.
// Returns "" when the failure class has no mapped scenario.
func ClassifyScenario(failureClass schema.CapsuleRuntimeFailureClass) schema.RecoveryScenario {
	switch failureClass {
	case schema.RuntimeFailurePermissionGate:
		return schema.RecoveryPermissionGate
	case schema.RuntimeFailureAdapterProtocol:
		return schema.RecoveryAdapterProtocolFailure
	case schema.RuntimeFailureStartupNoEvidence:
		return schema.RecoveryProviderFailure
	default:
		return ""
	}
}

// ClassifyRunError derives a failure class string from a runner error message.
// Used when no runtime-status event is available.
func ClassifyRunError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "permission") || strings.Contains(msg, "denied"):
		return string(schema.RuntimeFailurePermissionGate)
	case strings.Contains(msg, "sidecar") || strings.Contains(msg, "adapter"):
		return string(schema.RuntimeFailureAdapterProtocol)
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return string(schema.RuntimeFailureStartupNoEvidence)
	default:
		return string(schema.RuntimeFailureToolRuntime)
	}
}

// MaxAttemptsForScenario returns the maximum number of automatic recovery
// attempts for a scenario. One attempt is the conservative default.
func MaxAttemptsForScenario(scenario schema.RecoveryScenario) int {
	switch scenario {
	case schema.RecoveryStaleBranch, schema.RecoveryCompileFailure:
		return 1
	default:
		return 1
	}
}

// RecordAttempt loads existing entries for capsuleID, computes the next
// attempt number, and persists a new entry. It returns the persisted entry.
// The caller is responsible for checking entry.Outcome == RecoveryOutcomeEscalated
// and treating it as a blocked decision rather than a retry.
func RecordAttempt(
	ctx context.Context,
	st Store,
	goalID string,
	capsuleID string,
	scenario schema.RecoveryScenario,
	failureClass string,
	maxRetries int,
	commandRun string,
	commandResult string,
) (*schema.RecoveryLedgerEntry, error) {
	existing, err := st.LoadRecoveryEntriesForCapsule(ctx, capsuleID)
	if err != nil {
		return nil, fmt.Errorf("recovery: load entries for capsule %s: %w", capsuleID, err)
	}

	// Count previous attempts for the same scenario.
	prevAttempts := 0
	for _, e := range existing {
		if e.Scenario == scenario {
			prevAttempts++
		}
	}

	maxAllowed := maxRetries
	if maxAllowed < 0 {
		maxAllowed = MaxAttemptsForScenario(scenario)
	}

	attemptNum := prevAttempts + 1
	outcome := schema.RecoveryOutcomeFailed
	escalationReason := ""
	if attemptNum > maxAllowed {
		outcome = schema.RecoveryOutcomeEscalated
		escalationReason = fmt.Sprintf("attempt %d exceeds max_retries=%d for scenario %s",
			attemptNum, maxAllowed, scenario)
	}

	entry := &schema.RecoveryLedgerEntry{
		EntryID:          idgen.New("REC"),
		GoalID:           goalID,
		CapsuleID:        capsuleID,
		Scenario:         scenario,
		FailureClass:     failureClass,
		AttemptNum:       attemptNum,
		MaxAttempts:      maxAllowed,
		CommandRun:       commandRun,
		CommandResult:    commandResult,
		Outcome:          outcome,
		EscalationReason: escalationReason,
		AttemptedAt:      time.Now().UTC(),
	}

	if err := st.SaveRecoveryEntry(ctx, entry); err != nil {
		return nil, fmt.Errorf("recovery: save entry for capsule %s: %w", capsuleID, err)
	}
	return entry, nil
}

// IsEscalated returns true when entry.Outcome is RecoveryOutcomeEscalated,
// meaning the automatic recovery budget is exhausted.
func IsEscalated(entry *schema.RecoveryLedgerEntry) bool {
	if entry == nil {
		return false
	}
	return entry.Outcome == schema.RecoveryOutcomeEscalated
}
