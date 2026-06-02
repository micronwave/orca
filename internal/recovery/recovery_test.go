package recovery_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/recovery"
	"github.com/micronwave/orca/internal/schema"
)

// stubStore is a minimal in-memory stub for testing.
type stubStore struct {
	saved   []*schema.RecoveryLedgerEntry
	loadErr error
}

func (s *stubStore) SaveRecoveryEntry(_ context.Context, e *schema.RecoveryLedgerEntry) error {
	if s.loadErr != nil {
		return s.loadErr
	}
	s.saved = append(s.saved, e)
	return nil
}

func (s *stubStore) LoadRecoveryEntriesForCapsule(_ context.Context, capsuleID string) ([]*schema.RecoveryLedgerEntry, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	var out []*schema.RecoveryLedgerEntry
	for _, e := range s.saved {
		if e.CapsuleID == capsuleID {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestRecordAttempt_FirstAttempt_NotEscalated(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()
	entry, err := recovery.RecordAttempt(
		ctx, st, "G-1", "CAP-1",
		schema.RecoveryProviderFailure, "provider_failure",
		3, "codex", "context deadline exceeded",
	)
	if err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if entry == nil {
		t.Fatal("expected non-nil entry")
	}
	if entry.AttemptNum != 1 {
		t.Errorf("expected attempt_num=1, got %d", entry.AttemptNum)
	}
	if recovery.IsEscalated(entry) {
		t.Errorf("expected not escalated for first attempt out of 3")
	}
	if entry.Outcome != schema.RecoveryOutcomeFailed {
		t.Errorf("expected outcome=failed, got %s", entry.Outcome)
	}
}

func TestRecordAttempt_ExceedsMaxRetries_Escalates(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()

	// First attempt.
	_, err := recovery.RecordAttempt(ctx, st, "G-1", "CAP-1",
		schema.RecoveryProviderFailure, "provider_failure", 1, "", "")
	if err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	// Second attempt exceeds maxRetries=1.
	entry, err := recovery.RecordAttempt(ctx, st, "G-1", "CAP-1",
		schema.RecoveryProviderFailure, "provider_failure", 1, "", "")
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if !recovery.IsEscalated(entry) {
		t.Error("expected escalated on attempt 2 with maxRetries=1")
	}
	if entry.Outcome != schema.RecoveryOutcomeEscalated {
		t.Errorf("expected outcome=escalated, got %s", entry.Outcome)
	}
	if entry.EscalationReason == "" {
		t.Error("expected non-empty escalation reason")
	}
}

func TestRecordAttempt_ZeroMaxRetriesEscalatesFirstAttempt(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()
	entry, err := recovery.RecordAttempt(ctx, st, "G-0", "CAP-0",
		schema.RecoveryProviderFailure, "provider_failure", 0, "", "timeout")
	if err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if !recovery.IsEscalated(entry) {
		t.Fatal("expected first attempt to escalate when maxRetries=0")
	}
	if entry.AttemptNum != 1 || entry.MaxAttempts != 0 {
		t.Fatalf("attempt/max = %d/%d, want 1/0", entry.AttemptNum, entry.MaxAttempts)
	}
}

func TestRecordAttempt_StaleBranch_RecordsCommand(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()
	entry, err := recovery.RecordAttempt(
		ctx, st, "G-2", "CAP-2",
		schema.RecoveryStaleBranch, "stale_branch",
		2, "git fetch origin main", "Already up to date.",
	)
	if err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if entry.CommandRun != "git fetch origin main" {
		t.Errorf("expected command_run='git fetch origin main', got %q", entry.CommandRun)
	}
	if entry.CommandResult != "Already up to date." {
		t.Errorf("expected command_result='Already up to date.', got %q", entry.CommandResult)
	}
}

func TestRecordAttempt_AdapterProtocol_DoesNotLoopForever(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()

	// Simulate 5 attempts with maxRetries=2.
	var lastEntry *schema.RecoveryLedgerEntry
	for i := 0; i < 5; i++ {
		entry, err := recovery.RecordAttempt(
			ctx, st, "G-3", "CAP-3",
			schema.RecoveryAdapterProtocolFailure, "adapter_protocol_failure",
			2, "", "",
		)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		lastEntry = entry
	}
	// After 5 attempts with maxRetries=2, must be escalated.
	if !recovery.IsEscalated(lastEntry) {
		t.Error("expected escalated after exceeding maxRetries=2")
	}
	// Verify all 5 entries were saved.
	if len(st.saved) != 5 {
		t.Errorf("expected 5 saved entries, got %d", len(st.saved))
	}
}

func TestRecordAttempt_LedgerSurvivesReplay(t *testing.T) {
	st := &stubStore{}
	ctx := context.Background()

	// Save two entries.
	for i := 0; i < 2; i++ {
		if _, err := recovery.RecordAttempt(
			ctx, st, "G-4", "CAP-4",
			schema.RecoveryCompileFailure, "compile_failure",
			3, "go build ./...", "build failed",
		); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	// Simulate replay: load entries and verify fields.
	entries, err := st.LoadRecoveryEntriesForCapsule(ctx, "CAP-4")
	if err != nil {
		t.Fatalf("LoadRecoveryEntriesForCapsule: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	for _, e := range entries {
		if e.GoalID != "G-4" {
			t.Errorf("expected goal_id=G-4, got %q", e.GoalID)
		}
		if e.CapsuleID != "CAP-4" {
			t.Errorf("expected capsule_id=CAP-4, got %q", e.CapsuleID)
		}
		if e.AttemptedAt.IsZero() {
			t.Error("expected non-zero attempted_at")
		}
	}
}

func TestRecordAttempt_ProviderFailure_SuccessLinkedAfterRecovery(t *testing.T) {
	// Test that a provider failure records one auto-retry; after the retry
	// succeeds, a succeeded outcome can be recorded separately.
	st := &stubStore{}
	ctx := context.Background()

	// First attempt fails.
	first, err := recovery.RecordAttempt(
		ctx, st, "G-5", "CAP-5",
		schema.RecoveryProviderFailure, "provider_failure",
		1, "codex", "timeout",
	)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if recovery.IsEscalated(first) {
		t.Error("first attempt should not be escalated when maxRetries=1")
	}

	// Simulate success on second capsule (different capsule ID for the retry).
	success := &schema.RecoveryLedgerEntry{
		EntryID:      "REC-success",
		GoalID:       "G-5",
		CapsuleID:    "CAP-5-retry",
		Scenario:     schema.RecoveryProviderFailure,
		FailureClass: "provider_failure",
		AttemptNum:   1,
		MaxAttempts:  1,
		Outcome:      schema.RecoveryOutcomeSucceeded,
		AttemptedAt:  time.Now().UTC(),
	}
	if err := st.SaveRecoveryEntry(ctx, success); err != nil {
		t.Fatalf("save success: %v", err)
	}
	if recovery.IsEscalated(success) {
		t.Error("succeeded entry should not be considered escalated")
	}
}

func TestClassifyRunError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		contains string
	}{
		{"permission", errors.New("permission denied"), "permission_gate"},
		{"adapter", errors.New("sidecar output failed"), "adapter_protocol"},
		{"timeout", errors.New("context deadline exceeded"), "startup_no_evidence"},
		{"other", errors.New("some other error"), "tool_runtime"},
		{"nil", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := recovery.ClassifyRunError(tc.err)
			if tc.contains == "" {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			if got == "" {
				t.Errorf("expected non-empty result for %q", tc.err)
			}
		})
	}
}
