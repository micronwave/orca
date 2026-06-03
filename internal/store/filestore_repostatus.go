package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/micronwave/orca/internal/schema"
)

// ── Repo Status Snapshots ─────────────────────────────────────────────────────

// SaveRepoStatusSnapshot persists a RepoStatusSnapshot and emits a
// repo_status_snapshot_saved event. orca.md Phase B §3.
func (s *FileStore) SaveRepoStatusSnapshot(ctx context.Context, snap *schema.RepoStatusSnapshot) error {
	if snap == nil {
		return fmt.Errorf("store: repo status snapshot is required")
	}
	if err := validateArtifactID("repo status snapshot", snap.SnapshotID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("repo status snapshot",
		s.artifactPath(dirRepoStatus, snap.SnapshotID), snap.SnapshotID); err != nil {
		return err
	}
	ev, err := s.appendEvent(ctx, schema.EventRepoStatusSnapshotSaved, snap.GoalID, snap.SnapshotID, snap)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirRepoStatus, snap.SnapshotID), snap))
}

// LoadLatestRepoStatusSnapshot loads the most recently created RepoStatusSnapshot
// for goalID. Returns ErrNotFound when no snapshot exists.
func (s *FileStore) LoadLatestRepoStatusSnapshot(ctx context.Context, goalID string) (*schema.RepoStatusSnapshot, error) {
	s.mu.RLock()
	dir := filepath.Join(s.root, dirRepoStatus)
	s.mu.RUnlock()

	all, err := scanDir[schema.RepoStatusSnapshot](ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("store: load repo status snapshot for goal %s: %w", goalID, err)
	}
	var candidates []*schema.RepoStatusSnapshot
	for _, snap := range all {
		if snap.GoalID == goalID {
			candidates = append(candidates, snap)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNotFound
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	return candidates[0], nil
}
