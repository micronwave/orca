package store

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/micronwave/orca/internal/schema"
)

// ── Goal IR ──────────────────────────────────────────────────────────────────

func (s *FileStore) SaveGoal(ctx context.Context, g *schema.GoalIR) error {
	if g == nil {
		return fmt.Errorf("store: goal is required")
	}
	if err := validateArtifactID("goal", g.GoalID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Enforce one-active-goal invariant atomically under the write lock,
	// closing the TOCTOU window between LoadActiveGoal and SaveGoal.
	if g.Status == schema.GoalStatusActive {
		goals, err := scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
		if err != nil {
			return fmt.Errorf("store: check active goal: %w", err)
		}
		for _, ex := range goals {
			if ex.Status == schema.GoalStatusActive {
				return fmt.Errorf("store: active goal %s already exists", ex.GoalID)
			}
		}
	}
	if err := ensureArtifactAbsent("goal", s.artifactPath(dirGoals, g.GoalID), g.GoalID); err != nil {
		return err
	}
	ev, err := s.appendEvent(ctx, schema.EventGoalCreated, g.GoalID, g.GoalID, g)
	if err != nil {
		return err
	}
	if err := s.writeFile(ctx, s.artifactPath(dirGoals, g.GoalID), g); err != nil {
		return &MaterializationError{Event: ev, Err: err}
	}
	s.knownGoals[g.GoalID] = true
	for _, c := range g.GoalConditions {
		s.condToGoal[c.ID] = g.GoalID
	}
	return nil
}

func (s *FileStore) LoadGoal(ctx context.Context, goalID string) (*schema.GoalIR, error) {
	if err := validateArtifactID("goal", goalID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.GoalIR](ctx, s.artifactPath(dirGoals, goalID))
}

// LoadActiveGoal scans all goal files and returns the one with status "active".
// Returns (nil, nil) when no active goal exists. The MVP enforces one active goal
// per repo; the IntentCompiler calls this before creating a new goal.
func (s *FileStore) LoadActiveGoal(ctx context.Context) (*schema.GoalIR, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	goals, err := scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
	if err != nil {
		return nil, err
	}
	for _, g := range goals {
		if g.Status == schema.GoalStatusActive {
			return g, nil
		}
	}
	return nil, nil
}

func (s *FileStore) UpdateGoalStatus(ctx context.Context, goalID string, status schema.GoalStatus) error {
	if err := validateArtifactID("goal", goalID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := readFile[schema.GoalIR](ctx, s.artifactPath(dirGoals, goalID))
	if err != nil {
		return err
	}
	ev, err := s.appendEvent(ctx, schema.EventGoalStatusUpdated, goalID, goalID,
		schema.GoalStatusPayload{GoalID: goalID, Status: status})
	if err != nil {
		return fmt.Errorf("store: append goal_status_updated: %w", err)
	}
	g.Status = status
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirGoals, goalID), g))
}

func (s *FileStore) LoadGoalCondition(ctx context.Context, conditionID string) (*schema.GoalCondition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	goals, err := scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
	if err != nil {
		return nil, err
	}
	for _, g := range goals {
		for i := range g.GoalConditions {
			if g.GoalConditions[i].ID == conditionID {
				c := g.GoalConditions[i]
				return &c, nil
			}
		}
	}
	return nil, ErrNotFound
}

// ── Obligations ──────────────────────────────────────────────────────────────

func (s *FileStore) SaveObligation(ctx context.Context, o *schema.Obligation) error {
	if o == nil {
		return fmt.Errorf("store: obligation is required")
	}
	if err := validateArtifactID("obligation", o.ObligationID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("obligation", s.artifactPath(dirObligations, o.ObligationID), o.ObligationID); err != nil {
		return err
	}
	goalID, err := s.findGoalIDForConditionLocked(ctx, o.GoalConditionID)
	if err != nil {
		return fmt.Errorf("store: SaveObligation: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventObligationCreated, goalID, o.ObligationID, o)
	if err != nil {
		return err
	}
	if err := s.writeFile(ctx, s.artifactPath(dirObligations, o.ObligationID), o); err != nil {
		return materializationError(ev, err)
	}
	if o.Status == schema.ObligationOpen {
		s.addToOpenIdx(goalID, o.ObligationID)
	}
	return nil
}

func (s *FileStore) LoadObligation(ctx context.Context, obligationID string) (*schema.Obligation, error) {
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.Obligation](ctx, s.artifactPath(dirObligations, obligationID))
}

func (s *FileStore) LoadOpenObligations(ctx context.Context, goalID string) ([]*schema.Obligation, error) {
	if err := validateArtifactID("goal", goalID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := readFile[schema.GoalIR](ctx, s.artifactPath(dirGoals, goalID)); err != nil {
		return nil, fmt.Errorf("store: LoadOpenObligations: %w", err)
	}
	openIDs := s.openOblIdx[goalID]
	if len(openIDs) == 0 {
		return nil, nil
	}
	out := make([]*schema.Obligation, 0, len(openIDs))
	for oblID := range openIDs {
		o, err := readFile[schema.Obligation](ctx, s.artifactPath(dirObligations, oblID))
		if err != nil {
			return nil, fmt.Errorf("store: LoadOpenObligations: %w", err)
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *FileStore) LoadObligationsForCondition(ctx context.Context, conditionID string) ([]*schema.Obligation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.Obligation](ctx, filepath.Join(s.root, dirObligations))
	if err != nil {
		return nil, err
	}
	var out []*schema.Obligation
	for _, o := range all {
		if o.GoalConditionID == conditionID {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *FileStore) UpdateObligationStatus(ctx context.Context, obligationID string, status schema.ObligationStatus, satisfiedBy *[]string) error {
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := readFile[schema.Obligation](ctx, s.artifactPath(dirObligations, obligationID))
	if err != nil {
		return err
	}
	goalID, err := s.findGoalIDForConditionLocked(ctx, o.GoalConditionID)
	if err != nil {
		return fmt.Errorf("store: UpdateObligationStatus: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventObligationStatusUpdated, goalID, obligationID,
		schema.ObligationStatusPayload{ObligationID: obligationID, Status: status, SatisfiedBy: satisfiedBy})
	if err != nil {
		return fmt.Errorf("store: append obligation_status_updated: %w", err)
	}
	o.Status = status
	if satisfiedBy != nil {
		o.SatisfiedBy = *satisfiedBy
	}
	if err := s.writeFile(ctx, s.artifactPath(dirObligations, obligationID), o); err != nil {
		return materializationError(ev, err)
	}
	s.updateOpenIdx(goalID, obligationID, status)
	return nil
}

// ── Execution Capsules ───────────────────────────────────────────────────────

func (s *FileStore) SaveCapsule(ctx context.Context, c *schema.ExecutionCapsule) error {
	if c == nil {
		return fmt.Errorf("store: capsule is required")
	}
	if err := validateArtifactID("capsule", c.CapsuleID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("capsule", s.artifactPath(dirCapsules, c.CapsuleID), c.CapsuleID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsuleFromObligation(ctx, c)
	if err != nil {
		return fmt.Errorf("store: SaveCapsule: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventCapsuleCreated, goalID, c.CapsuleID, c)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirCapsules, c.CapsuleID), c))
}

// goalIDForCapsuleFromObligation resolves the goalID from the capsule's
// first ObligationID without loading the capsule from disk (it's not saved yet).
func (s *FileStore) goalIDForCapsuleFromObligation(ctx context.Context, c *schema.ExecutionCapsule) (string, error) {
	if len(c.ObligationIDs) == 0 {
		return "", fmt.Errorf("store: capsule %s has no obligation IDs", c.CapsuleID)
	}
	return s.goalIDForObligationLocked(ctx, c.ObligationIDs[0])
}

func (s *FileStore) LoadCapsule(ctx context.Context, capsuleID string) (*schema.ExecutionCapsule, error) {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.ExecutionCapsule](ctx, s.artifactPath(dirCapsules, capsuleID))
}

func (s *FileStore) UpdateCapsuleState(ctx context.Context, capsuleID string, state schema.CapsuleState) error {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.artifactPath(dirCapsules, capsuleID)
	c, err := readFile[schema.ExecutionCapsule](ctx, path)
	if err != nil {
		return err
	}
	allowed, known := validCapsuleTransitions[c.State]
	if !known {
		return fmt.Errorf("%w: capsule %s has unrecognised state %q", ErrInvalidCapsuleTransition, capsuleID, c.State)
	}
	if !allowed[state] {
		return fmt.Errorf("%w: capsule %s: %q → %q", ErrInvalidCapsuleTransition, capsuleID, c.State, state)
	}
	c.State = state
	return s.writeFile(ctx, path, c)
}

func (s *FileStore) UpdateCapsuleProjectionID(ctx context.Context, capsuleID, projectionID string) error {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return err
	}
	if err := validateArtifactID("projection", projectionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ExecutionCapsule](ctx, s.artifactPath(dirCapsules, capsuleID))
	if err != nil {
		return err
	}
	if len(c.ObligationIDs) == 0 {
		return fmt.Errorf("store: UpdateCapsuleProjectionID: capsule %s has no obligation IDs", capsuleID)
	}
	goalID, err := s.goalIDForObligationLocked(ctx, c.ObligationIDs[0])
	if err != nil {
		return fmt.Errorf("store: UpdateCapsuleProjectionID: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventCapsuleProjectionLinked, goalID, capsuleID,
		&schema.CapsuleProjectionPayload{CapsuleID: capsuleID, ProjectionID: projectionID})
	if err != nil {
		return err
	}
	c.ContextProjectionID = projectionID
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirCapsules, capsuleID), c))
}
