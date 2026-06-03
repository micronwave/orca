package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/micronwave/orca/internal/schema"
)

// ── Recovery Ledger ───────────────────────────────────────────────────────────

// SaveRecoveryEntry persists a RecoveryLedgerEntry and emits a
// recovery_ledger_saved event. orca.md Phase B §5.
func (s *FileStore) SaveRecoveryEntry(ctx context.Context, entry *schema.RecoveryLedgerEntry) error {
	if entry == nil {
		return fmt.Errorf("store: recovery entry is required")
	}
	if err := validateArtifactID("recovery entry", entry.EntryID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("recovery entry", s.artifactPath(dirRecoveryLedger, entry.EntryID), entry.EntryID); err != nil {
		return err
	}
	ev, err := s.appendEvent(ctx, schema.EventRecoveryLedgerSaved, entry.GoalID, entry.EntryID, entry)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirRecoveryLedger, entry.EntryID), entry))
}

// LoadRecoveryEntry loads a single RecoveryLedgerEntry by ID.
func (s *FileStore) LoadRecoveryEntry(ctx context.Context, entryID string) (*schema.RecoveryLedgerEntry, error) {
	if err := validateArtifactID("recovery entry", entryID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.RecoveryLedgerEntry](ctx, s.artifactPath(dirRecoveryLedger, entryID))
}

// LoadRecoveryEntriesForGoal returns all RecoveryLedgerEntry records for goalID,
// sorted by AttemptedAt ascending.
func (s *FileStore) LoadRecoveryEntriesForGoal(ctx context.Context, goalID string) ([]*schema.RecoveryLedgerEntry, error) {
	s.mu.RLock()
	dir := filepath.Join(s.root, dirRecoveryLedger)
	s.mu.RUnlock()

	all, err := scanDir[schema.RecoveryLedgerEntry](ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("store: load recovery entries for goal %s: %w", goalID, err)
	}
	var out []*schema.RecoveryLedgerEntry
	for _, e := range all {
		if e.GoalID == goalID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AttemptedAt.Before(out[j].AttemptedAt)
	})
	return out, nil
}

// LoadRecoveryEntriesForCapsule returns all RecoveryLedgerEntry records for
// capsuleID, sorted by AttemptedAt ascending.
func (s *FileStore) LoadRecoveryEntriesForCapsule(ctx context.Context, capsuleID string) ([]*schema.RecoveryLedgerEntry, error) {
	s.mu.RLock()
	dir := filepath.Join(s.root, dirRecoveryLedger)
	s.mu.RUnlock()

	all, err := scanDir[schema.RecoveryLedgerEntry](ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("store: load recovery entries for capsule %s: %w", capsuleID, err)
	}
	var out []*schema.RecoveryLedgerEntry
	for _, e := range all {
		if e.CapsuleID == capsuleID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AttemptedAt.Before(out[j].AttemptedAt)
	})
	return out, nil
}
