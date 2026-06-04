package store

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/micronwave/orca/internal/schema"
)

// SaveProjectionReuseRecord persists a reuse record and appends a
// projection_reuse_recorded event. Phase C §7.
func (s *FileStore) SaveProjectionReuseRecord(ctx context.Context, r *schema.ProjectionReuseRecord) error {
	if r == nil {
		return fmt.Errorf("store: projection reuse record is required")
	}
	if err := validateArtifactID("projection reuse record", r.ReuseID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, err := s.appendEvent(ctx, schema.EventProjectionReuseRecorded, r.GoalID, r.ReuseID, r)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirProjReuse, r.ReuseID), r))
}

// LoadProjectionReuseRecordsForGoal returns all reuse records whose GoalID
// matches the given goal. Returns an empty slice when none exist.
func (s *FileStore) LoadProjectionReuseRecordsForGoal(ctx context.Context, goalID string) ([]*schema.ProjectionReuseRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ProjectionReuseRecord](ctx, filepath.Join(s.root, dirProjReuse))
	if err != nil {
		return nil, fmt.Errorf("store: load projection reuse records for goal %s: %w", goalID, err)
	}
	var out []*schema.ProjectionReuseRecord
	for _, r := range all {
		if r.GoalID == goalID {
			out = append(out, r)
		}
	}
	return out, nil
}

// LoadProjectionByEvidenceHashAndRole returns the most recently created
// projection of the given role whose EvidenceHash matches the given value,
// or ErrNotFound when none exist. Human summary projections are excluded.
// Used by T10 to reuse the evidence section text when the evidence set is unchanged.
func (s *FileStore) LoadProjectionByEvidenceHashAndRole(ctx context.Context, role schema.ProjectionRole, evidenceHash string) (*schema.ContextProjection, error) {
	dir, err := projectionDir(role)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ContextProjection](ctx, filepath.Join(s.root, dir))
	if err != nil {
		return nil, fmt.Errorf("store: load projection by evidence hash: %w", err)
	}
	var best *schema.ContextProjection
	for _, p := range all {
		if p.EvidenceHash == "" || p.EvidenceHash != evidenceHash {
			continue
		}
		if best == nil || p.CreatedAt.After(best.CreatedAt) ||
			(p.CreatedAt.Equal(best.CreatedAt) && p.ContextProjectionID < best.ContextProjectionID) {
			best = p
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// LoadProjectionBySourceHashAndRole returns the most recently created
// executor/reviewer/tester projection whose SourceHash matches the given value,
// or ErrNotFound when none exist. Human summary projections are excluded.
func (s *FileStore) LoadProjectionBySourceHashAndRole(ctx context.Context, role schema.ProjectionRole, sourceHash string) (*schema.ContextProjection, error) {
	dir, err := projectionDir(role)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ContextProjection](ctx, filepath.Join(s.root, dir))
	if err != nil {
		return nil, fmt.Errorf("store: load projection by source hash: %w", err)
	}
	var best *schema.ContextProjection
	for _, p := range all {
		if p.SourceHash != sourceHash {
			continue
		}
		if best == nil || p.CreatedAt.After(best.CreatedAt) ||
			(p.CreatedAt.Equal(best.CreatedAt) && p.ContextProjectionID < best.ContextProjectionID) {
			best = p
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}
