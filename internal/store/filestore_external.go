package store

import (
	"context"
	"fmt"

	"github.com/micronwave/orca/internal/schema"
)

// ── PR Records ───────────────────────────────────────────────────────────────

// SavePRRecord persists a PRRecord and appends a pr_created event.
// goalID must match an existing goal.
func (s *FileStore) SavePRRecord(ctx context.Context, goalID string, pr *schema.PRRecord) error {
	if pr == nil {
		return fmt.Errorf("store: pr record is required")
	}
	if err := validateArtifactID("pr", pr.PRID); err != nil {
		return err
	}
	if pr.GoalID != goalID {
		return fmt.Errorf("store: pr record goal_id %q does not match goalID %q", pr.GoalID, goalID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("pr", s.artifactPath(dirPRs, pr.PRID), pr.PRID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(goalID); err != nil {
		return fmt.Errorf("store: SavePRRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventPRCreated, goalID, pr.PRID, pr)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirPRs, pr.PRID), pr))
}

// LoadPRRecord loads a PRRecord by its ID.
func (s *FileStore) LoadPRRecord(ctx context.Context, goalID string, prID string) (*schema.PRRecord, error) {
	if err := validateArtifactID("goal", goalID); err != nil {
		return nil, err
	}
	if err := validateArtifactID("pr", prID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pr, err := readFile[schema.PRRecord](s.artifactPath(dirPRs, prID))
	if err != nil {
		return nil, err
	}
	if pr.GoalID != goalID {
		return nil, ErrNotFound
	}
	return pr, nil
}

// ── CI Status Records ────────────────────────────────────────────────────────

// LoadCIStatusRecord loads a CIStatusRecord by its record ID.
func (s *FileStore) LoadCIStatusRecord(ctx context.Context, goalID string, recordID string) (*schema.CIStatusRecord, error) {
	if err := validateArtifactID("goal", goalID); err != nil {
		return nil, err
	}
	if err := validateArtifactID("ci status", recordID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, err := readFile[schema.CIStatusRecord](s.artifactPath(dirCIStatus, recordID))
	if err != nil {
		return nil, err
	}
	if r.GoalID != goalID {
		return nil, ErrNotFound
	}
	return r, nil
}

// SaveCIStatusRecord persists a CIStatusRecord and appends a ci_status_received event.
// goalID must match an existing goal.
func (s *FileStore) SaveCIStatusRecord(ctx context.Context, goalID string, r *schema.CIStatusRecord) error {
	if r == nil {
		return fmt.Errorf("store: ci status record is required")
	}
	if err := validateArtifactID("ci status", r.RecordID); err != nil {
		return err
	}
	if r.GoalID != goalID {
		return fmt.Errorf("store: ci status record goal_id %q does not match goalID %q", r.GoalID, goalID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("ci status", s.artifactPath(dirCIStatus, r.RecordID), r.RecordID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(goalID); err != nil {
		return fmt.Errorf("store: SaveCIStatusRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventCIStatusReceived, goalID, r.RecordID, r)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirCIStatus, r.RecordID), r))
}

// ── Intake Records ───────────────────────────────────────────────────────────

// LoadIntakeRecord loads an IntakeRecord by its record ID.
func (s *FileStore) LoadIntakeRecord(ctx context.Context, goalID string, recordID string) (*schema.IntakeRecord, error) {
	if err := validateArtifactID("goal", goalID); err != nil {
		return nil, err
	}
	if err := validateArtifactID("intake", recordID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, err := readFile[schema.IntakeRecord](s.artifactPath(dirIntake, recordID))
	if err != nil {
		return nil, err
	}
	if r.GoalID != goalID {
		return nil, ErrNotFound
	}
	return r, nil
}

// SaveIntakeRecord persists an IntakeRecord and appends an intake_issue_ingested event.
// goalID must match an existing goal.
func (s *FileStore) SaveIntakeRecord(ctx context.Context, goalID string, r *schema.IntakeRecord) error {
	if r == nil {
		return fmt.Errorf("store: intake record is required")
	}
	if err := validateArtifactID("intake", r.RecordID); err != nil {
		return err
	}
	if r.GoalID != goalID {
		return fmt.Errorf("store: intake record goal_id %q does not match goalID %q", r.GoalID, goalID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("intake", s.artifactPath(dirIntake, r.RecordID), r.RecordID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(goalID); err != nil {
		return fmt.Errorf("store: SaveIntakeRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventIntakeIssueIngested, goalID, r.RecordID, r)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirIntake, r.RecordID), r))
}
