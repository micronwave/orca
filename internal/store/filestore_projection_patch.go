package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/micronwave/orca/internal/schema"
)

// ── Context Projections ──────────────────────────────────────────────────────

func (s *FileStore) SaveProjection(ctx context.Context, p *schema.ContextProjection) error {
	if p == nil {
		return fmt.Errorf("store: context projection is required")
	}
	if err := validateArtifactID("context projection", p.ContextProjectionID); err != nil {
		return err
	}
	dir, err := projectionDir(p.Role)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProjectionAbsent(p.ContextProjectionID); err != nil {
		return err
	}
	saved := *p
	goalID, err := s.goalIDForProjectionSources(ctx, saved.SourceArtifactIDs)
	if err != nil {
		return fmt.Errorf("store: SaveProjection: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventContextProjectionCreated, goalID, saved.ContextProjectionID, &saved)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dir, saved.ContextProjectionID), &saved))
}

func (s *FileStore) SaveHumanSummaryProjection(ctx context.Context, p *schema.HumanSummaryProjection) error {
	if p == nil {
		return fmt.Errorf("store: human summary projection is required")
	}
	if err := validateArtifactID("context projection", p.ContextProjectionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProjectionAbsent(p.ContextProjectionID); err != nil {
		return err
	}
	saved := *p
	saved.Role = schema.ProjectionRoleHumanSummary
	goalID, err := s.goalIDForProjectionSources(ctx, saved.SourceArtifactIDs)
	if err != nil {
		return fmt.Errorf("store: SaveHumanSummaryProjection: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventContextProjectionCreated, goalID, saved.ContextProjectionID, &saved)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirProjHuman, saved.ContextProjectionID), &saved))
}

func (s *FileStore) LoadProjection(ctx context.Context, projectionID string) (*schema.ContextProjection, error) {
	if err := validateArtifactID("projection", projectionID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, dir := range []string{dirProjExecutor, dirProjReviewer, dirProjTester} {
		projection, err := readFile[schema.ContextProjection](s.artifactPath(dir, projectionID))
		if err == nil {
			return projection, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) LoadHumanSummaryProjection(ctx context.Context, projectionID string) (*schema.HumanSummaryProjection, error) {
	if err := validateArtifactID("projection", projectionID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.HumanSummaryProjection](s.artifactPath(dirProjHuman, projectionID))
}

func (s *FileStore) LoadHumanSummaryProjectionForCapsule(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error) {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.HumanSummaryProjection](ctx, filepath.Join(s.root, dirProjHuman))
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		for _, sourceID := range p.SourceArtifactIDs {
			if sourceID == capsuleID {
				return p, nil
			}
		}
	}
	return nil, ErrNotFound
}

// ── Patch Artifacts ──────────────────────────────────────────────────────────

func (s *FileStore) SavePatch(ctx context.Context, p *schema.PatchArtifact) error {
	if p == nil {
		return fmt.Errorf("store: patch is required")
	}
	if err := validateArtifactID("patch", p.PatchID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("patch", s.artifactPath(dirPatches, p.PatchID), p.PatchID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(ctx, p.CapsuleID)
	if err != nil {
		return fmt.Errorf("store: SavePatch: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventPatchArtifactCreated, goalID, p.PatchID, p)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirPatches, p.PatchID), p))
}

func (s *FileStore) LoadPatch(ctx context.Context, patchID string) (*schema.PatchArtifact, error) {
	if err := validateArtifactID("patch", patchID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.PatchArtifact](s.artifactPath(dirPatches, patchID))
}

func (s *FileStore) UpdatePatchStatus(ctx context.Context, patchID string, status schema.PatchStatus) error {
	if err := validateArtifactID("patch", patchID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := readFile[schema.PatchArtifact](s.artifactPath(dirPatches, patchID))
	if err != nil {
		return err
	}
	p.Status = status
	return s.writeFile(s.artifactPath(dirPatches, patchID), p)
}

func (s *FileStore) LoadPatchesForCapsule(ctx context.Context, capsuleID string) ([]*schema.PatchArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.PatchArtifact](ctx, filepath.Join(s.root, dirPatches))
	if err != nil {
		return nil, err
	}
	var out []*schema.PatchArtifact
	for _, p := range all {
		if p.CapsuleID == capsuleID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *FileStore) LoadPatchesForObligation(ctx context.Context, obligationID string) ([]*schema.PatchArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return nil, err
	}
	all, err := scanDir[schema.PatchArtifact](ctx, filepath.Join(s.root, dirPatches))
	if err != nil {
		return nil, err
	}
	var out []*schema.PatchArtifact
	for _, p := range all {
		if containsString(p.ObligationIDsClaimed, obligationID) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ── Evidence Artifacts ───────────────────────────────────────────────────────

func (s *FileStore) SaveEvidence(ctx context.Context, e *schema.EvidenceArtifact) error {
	if e == nil {
		return fmt.Errorf("store: evidence is required")
	}
	if err := validateArtifactID("evidence", e.EvidenceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("evidence", s.artifactPath(dirEvidence, e.EvidenceID), e.EvidenceID); err != nil {
		return err
	}
	goalID, err := s.goalIDForEvidence(ctx, e)
	if err != nil {
		return fmt.Errorf("store: SaveEvidence: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventEvidenceArtifactCreated, goalID, e.EvidenceID, e)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirEvidence, e.EvidenceID), e))
}

func (s *FileStore) LoadEvidence(ctx context.Context, evidenceID string) (*schema.EvidenceArtifact, error) {
	if err := validateArtifactID("evidence", evidenceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.EvidenceArtifact](s.artifactPath(dirEvidence, evidenceID))
}

func (s *FileStore) LoadEvidenceForObligation(ctx context.Context, obligationID string) ([]*schema.EvidenceArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.EvidenceArtifact](ctx, filepath.Join(s.root, dirEvidence))
	if err != nil {
		return nil, err
	}
	var out []*schema.EvidenceArtifact
	for _, ev := range all {
		if containsString(ev.Supports, obligationID) || containsString(ev.Weakens, obligationID) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *FileStore) LoadReusableEvidenceForObligation(ctx context.Context, obligationID string, evidenceType schema.EvidenceType, reuseKey string, snapshotID string) (*schema.EvidenceArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.EvidenceArtifact](ctx, filepath.Join(s.root, dirEvidence))
	if err != nil {
		return nil, err
	}
	var best *schema.EvidenceArtifact
	for _, ev := range all {
		if ev.Type != evidenceType ||
			ev.ExitCode != 0 ||
			ev.ReuseKey != reuseKey ||
			ev.ValidatedAgainst != snapshotID ||
			!containsString(ev.Supports, obligationID) {
			continue
		}
		if best == nil || ev.CreatedAt.After(best.CreatedAt) || (ev.CreatedAt.Equal(best.CreatedAt) && ev.EvidenceID < best.EvidenceID) {
			best = ev
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}
