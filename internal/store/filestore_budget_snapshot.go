package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micronwave/orca/internal/schema"
)

// ── Budget Records ───────────────────────────────────────────────────────────

func (s *FileStore) SaveBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error {
	if b == nil {
		return fmt.Errorf("store: budget record is required")
	}
	if err := validateArtifactID("budget", b.BudgetID); err != nil {
		return err
	}
	if err := validateBudgetConsumption(b); err != nil {
		return fmt.Errorf("store: budget %s: %w", b.BudgetID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("budget", s.artifactPath(dirBudgets, b.BudgetID), b.BudgetID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(ctx, b.GoalID); err != nil {
		return fmt.Errorf("store: SaveBudgetRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventBudgetRecordSaved, b.GoalID, b.BudgetID, b)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirBudgets, b.BudgetID), b))
}

func (s *FileStore) LoadBudgetRecord(ctx context.Context, budgetID string) (*schema.BudgetRecord, error) {
	if err := validateArtifactID("budget", budgetID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.BudgetRecord](ctx, s.artifactPath(dirBudgets, budgetID))
}

func (s *FileStore) LoadBudgetForGoal(ctx context.Context, goalID string) ([]*schema.BudgetRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.BudgetRecord](ctx, filepath.Join(s.root, dirBudgets))
	if err != nil {
		return nil, err
	}
	var out []*schema.BudgetRecord
	for _, b := range all {
		if b.GoalID == goalID {
			out = append(out, b)
		}
	}
	return out, nil
}

// validateBudgetConsumption rejects negative consumption counters before they
// reach the event log.
func validateBudgetConsumption(b *schema.BudgetRecord) error {
	if b.TokensSpent < 0 || b.WallTimeSeconds < 0 || b.Retries < 0 || b.ToolCalls < 0 ||
		b.DuplicatedFileReads < 0 || b.OverlappingEdits < 0 || b.HumanInterventions < 0 ||
		b.ObligationsDischarged < 0 || b.PatchesAccepted < 0 || b.PatchesRejected < 0 ||
		b.EvidenceArtifactsReused < 0 || b.AvoidedRetries < 0 {
		return fmt.Errorf("consumption fields must be non-negative")
	}
	return nil
}

// UpdateBudgetRecord overwrites the stored BudgetRecord with b after appending
// a replayable budget_record_updated event.
func (s *FileStore) UpdateBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error {
	if b == nil {
		return fmt.Errorf("store: budget record is required")
	}
	if err := validateArtifactID("budget", b.BudgetID); err != nil {
		return err
	}
	if err := validateBudgetConsumption(b); err != nil {
		return fmt.Errorf("store: budget %s: %w", b.BudgetID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.artifactPath(dirBudgets, b.BudgetID)
	if _, err := readFile[schema.BudgetRecord](ctx, path); err != nil {
		return err
	}
	if err := s.requireExistingGoal(ctx, b.GoalID); err != nil {
		return fmt.Errorf("store: UpdateBudgetRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventBudgetRecordUpdated, b.GoalID, b.BudgetID, b)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, path, b))
}

// ── State Snapshots ──────────────────────────────────────────────────────────

// SaveSnapshot persists a StateSnapshot and records it in the log so replay can
// reconstruct checkpoint metadata. The snapshot's SequenceNum remains the last
// domain event included in the snapshot, not the sequence assigned to this save.
func (s *FileStore) SaveSnapshot(ctx context.Context, snap *schema.StateSnapshot) error {
	if snap == nil {
		return fmt.Errorf("store: snapshot is required")
	}
	if err := validateArtifactID("snapshot", snap.SnapshotID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("snapshot", s.artifactPath(dirSnapshots, snap.SnapshotID), snap.SnapshotID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(ctx, snap.GoalID); err != nil {
		return fmt.Errorf("store: SaveSnapshot: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventStateSnapshotSaved, snap.GoalID, snap.SnapshotID, snap)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirSnapshots, snap.SnapshotID), snap))
}

// LoadLatestSnapshot returns the StateSnapshot for goalID with the highest
// SequenceNum, representing the most recent checkpoint.
func (s *FileStore) LoadLatestSnapshot(ctx context.Context, goalID string) (*schema.StateSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.StateSnapshot](ctx, filepath.Join(s.root, dirSnapshots))
	if err != nil {
		return nil, err
	}
	var latest *schema.StateSnapshot
	for _, snap := range all {
		if snap.GoalID != goalID {
			continue
		}
		if latest == nil || snap.SequenceNum > latest.SequenceNum {
			latest = snap
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	return latest, nil
}

func (s *FileStore) LoadSnapshot(ctx context.Context, snapshotID string) (*schema.StateSnapshot, error) {
	if err := validateArtifactID("snapshot", snapshotID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.StateSnapshot](ctx, s.artifactPath(dirSnapshots, snapshotID))
}

// ── utility ──────────────────────────────────────────────────────────────────

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func normalizeArtifactPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	path = strings.Trim(path, "/")
	return strings.ToLower(path)
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func sortTopologyOutcomes(records []*schema.TopologyOutcomeRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].RecordedAt.Equal(records[j].RecordedAt) {
			return records[i].RecordedAt.Before(records[j].RecordedAt)
		}
		return records[i].OutcomeID < records[j].OutcomeID
	})
}
