package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micronwave/orca/internal/schema"
)

// ── Claim Artifacts ──────────────────────────────────────────────────────────

func (s *FileStore) SaveClaim(ctx context.Context, c *schema.ClaimArtifact) error {
	if c == nil {
		return fmt.Errorf("store: claim is required")
	}
	if err := validateArtifactID("claim", c.ClaimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("claim", s.artifactPath(dirClaims, c.ClaimID), c.ClaimID); err != nil {
		return err
	}
	// Resolve goalID for the event log. Repo-scoped claims (GoalID == "" and
	// SourceCapsuleID == "") are not bound to any goal; their event goalID is "".
	var goalID string
	if strings.TrimSpace(c.GoalID) != "" {
		if err := s.requireExistingGoal(ctx, c.GoalID); err != nil {
			return fmt.Errorf("store: SaveClaim: %w", err)
		}
		goalID = c.GoalID
	} else if strings.TrimSpace(c.SourceCapsuleID) != "" {
		resolved, err := s.goalIDForCapsuleLocked(ctx, c.SourceCapsuleID)
		if err != nil {
			return fmt.Errorf("store: SaveClaim: %w", err)
		}
		goalID = resolved
	}
	// goalID == "" for repo-scoped claims; event is appended without a goal binding.
	ev, err := s.appendEvent(ctx, schema.EventClaimCreated, goalID, c.ClaimID, c)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirClaims, c.ClaimID), c))
}

func (s *FileStore) LoadClaim(ctx context.Context, claimID string) (*schema.ClaimArtifact, error) {
	if err := validateArtifactID("claim", claimID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, claimID))
}

func (s *FileStore) LoadVerifiedClaimsForFiles(ctx context.Context, files []string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(files) == 0 {
		return nil, nil
	}
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		if normalized := normalizeArtifactPath(f); normalized != "" {
			fileSet[normalized] = true
		}
	}
	all, err := scanDir[schema.ClaimArtifact](ctx, filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	var out []*schema.ClaimArtifact
	for _, c := range all {
		if c.Status != schema.ClaimVerified {
			continue
		}
		for _, af := range c.AffectedFiles {
			if fileSet[normalizeArtifactPath(af)] {
				out = append(out, c)
				break
			}
		}
	}
	return out, nil
}

func (s *FileStore) LoadClaimsForCapsule(ctx context.Context, capsuleID string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ClaimArtifact](ctx, filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	var out []*schema.ClaimArtifact
	for _, c := range all {
		if c.SourceCapsuleID == capsuleID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *FileStore) LoadClaimsForGoal(ctx context.Context, goalID string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExistsLocked(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	all, err := scanDir[schema.ClaimArtifact](ctx, filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.ClaimArtifact, 0, len(all))
	for _, claim := range all {
		// Repo-scoped claims (both GoalID and SourceCapsuleID empty) are excluded
		// from goal-scoped queries; use LoadRepoScopedClaims for those.
		if strings.TrimSpace(claim.GoalID) == "" && strings.TrimSpace(claim.SourceCapsuleID) == "" {
			continue
		}
		// Short-circuit: if GoalID is set directly, compare without capsule chain.
		if strings.TrimSpace(claim.GoalID) != "" {
			if claim.GoalID == goalID {
				out = append(out, claim)
			}
			continue
		}
		claimGoalID, err := s.goalIDForCapsuleLocked(ctx, claim.SourceCapsuleID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if claimGoalID == goalID {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s *FileStore) LoadClaimsByStatus(ctx context.Context, goalID string, status schema.ClaimStatus) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExistsLocked(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	all, err := scanDir[schema.ClaimArtifact](ctx, filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.ClaimArtifact, 0, len(all))
	for _, claim := range all {
		if claim.Status != status {
			continue
		}
		// Exclude repo-scoped claims from goal-scoped status queries.
		if strings.TrimSpace(claim.GoalID) == "" && strings.TrimSpace(claim.SourceCapsuleID) == "" {
			continue
		}
		if strings.TrimSpace(claim.GoalID) != "" {
			if claim.GoalID == goalID {
				out = append(out, claim)
			}
			continue
		}
		claimGoalID, err := s.goalIDForCapsuleLocked(ctx, claim.SourceCapsuleID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if claimGoalID == goalID {
			out = append(out, claim)
		}
	}
	return out, nil
}

// LoadRepoScopedClaims returns all repo-scoped claims — those with both GoalID
// and SourceCapsuleID empty. These claims persist across all goals.
func (s *FileStore) LoadRepoScopedClaims(ctx context.Context) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ClaimArtifact](ctx, filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.ClaimArtifact, 0)
	for _, claim := range all {
		if strings.TrimSpace(claim.GoalID) == "" && strings.TrimSpace(claim.SourceCapsuleID) == "" {
			out = append(out, claim)
		}
	}
	return out, nil
}

// claimGoalIDNoLock resolves the goalID for a loaded claim. Returns "" for
// repo-scoped claims (both GoalID and SourceCapsuleID empty). Caller must hold s.mu.
func (s *FileStore) claimGoalIDNoLock(ctx context.Context, c *schema.ClaimArtifact) (string, error) {
	if strings.TrimSpace(c.GoalID) != "" {
		return c.GoalID, nil
	}
	if strings.TrimSpace(c.SourceCapsuleID) != "" {
		goalID, err := s.goalIDForCapsuleLocked(ctx, c.SourceCapsuleID)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", nil
}

// UpdateClaimSupersession sets the SupersededBy field on a claim artifact and
// emits a claim_superseded event before writing, keeping the event log and
// materialized state in sync.
func (s *FileStore) UpdateClaimSupersession(ctx context.Context, claimID, supersededBy string) error {
	if err := validateArtifactID("claim", claimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, claimID))
	if err != nil {
		return err
	}
	goalID, err := s.claimGoalIDNoLock(ctx, c)
	if err != nil {
		return fmt.Errorf("store: UpdateClaimSupersession: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventClaimSuperseded, goalID, claimID,
		schema.ClaimSupersededPayload{ClaimID: claimID, SupersededBy: supersededBy})
	if err != nil {
		return fmt.Errorf("store: append claim_superseded: %w", err)
	}
	c.SupersededBy = supersededBy
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirClaims, claimID), c))
}

// UpdateClaimStatus updates the status field on a claim artifact and emits a
// claim_status_updated event before writing.
func (s *FileStore) UpdateClaimStatus(ctx context.Context, claimID string, status schema.ClaimStatus) error {
	if err := validateArtifactID("claim", claimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, claimID))
	if err != nil {
		return err
	}
	goalID, err := s.claimGoalIDNoLock(ctx, c)
	if err != nil {
		return fmt.Errorf("store: UpdateClaimStatus: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventClaimStatusUpdated, goalID, claimID,
		schema.ClaimStatusPayload{
			ClaimID:              claimID,
			Status:               status,
			LastValidatedAgainst: c.LastValidatedAgainst,
			ContradictedBy:       c.ContradictedBy,
			InvalidatedBy:        c.InvalidatedBy,
		})
	if err != nil {
		return fmt.Errorf("store: append claim_status_updated: %w", err)
	}
	c.Status = status
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirClaims, claimID), c))
}

// UpdateClaimDispute updates status, ContradictedBy, and InvalidatedBy on a
// claim artifact and emits a claim_status_updated event before writing.
func (s *FileStore) UpdateClaimDispute(ctx context.Context, claimID string, status schema.ClaimStatus, contradictedBy, invalidatedBy []string) error {
	if err := validateArtifactID("claim", claimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, claimID))
	if err != nil {
		return err
	}
	goalID, err := s.claimGoalIDNoLock(ctx, c)
	if err != nil {
		return fmt.Errorf("store: UpdateClaimDispute: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventClaimStatusUpdated, goalID, claimID,
		schema.ClaimStatusPayload{
			ClaimID:              claimID,
			Status:               status,
			LastValidatedAgainst: c.LastValidatedAgainst,
			ContradictedBy:       contradictedBy,
			InvalidatedBy:        invalidatedBy,
		})
	if err != nil {
		return fmt.Errorf("store: append claim_status_updated: %w", err)
	}
	return materializationError(ev, s.updateClaimStatusNoLock(ctx, claimID, status, "", contradictedBy, invalidatedBy))
}

// UpdateClaimValidation updates status and LastValidatedAgainst on a claim
// artifact and emits a claim_status_updated event before writing.
func (s *FileStore) UpdateClaimValidation(ctx context.Context, claimID string, status schema.ClaimStatus, snapshotID string) error {
	if err := validateArtifactID("claim", claimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, claimID))
	if err != nil {
		return err
	}
	goalID, err := s.claimGoalIDNoLock(ctx, c)
	if err != nil {
		return fmt.Errorf("store: UpdateClaimValidation: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventClaimStatusUpdated, goalID, claimID,
		schema.ClaimStatusPayload{
			ClaimID:              claimID,
			Status:               status,
			LastValidatedAgainst: snapshotID,
			ContradictedBy:       c.ContradictedBy,
			InvalidatedBy:        c.InvalidatedBy,
		})
	if err != nil {
		return fmt.Errorf("store: append claim_status_updated: %w", err)
	}
	return materializationError(ev, s.updateClaimStatusNoLock(ctx, claimID, status, snapshotID, nil, nil))
}

// ── Failure Fingerprints ─────────────────────────────────────────────────────

func (s *FileStore) SaveFailure(ctx context.Context, f *schema.FailureFingerprint) error {
	if f == nil {
		return fmt.Errorf("store: failure fingerprint is required")
	}
	if err := validateArtifactID("failure", f.FailureID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("failure", s.artifactPath(dirFailures, f.FailureID), f.FailureID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsuleLocked(ctx, f.SourceCapsuleID)
	if err != nil {
		return fmt.Errorf("store: SaveFailure: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventFailureFingerprintCreated, goalID, f.FailureID, f)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(ctx, s.artifactPath(dirFailures, f.FailureID), f))
}

func (s *FileStore) LoadFailure(ctx context.Context, failureID string) (*schema.FailureFingerprint, error) {
	if err := validateArtifactID("failure", failureID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.FailureFingerprint](ctx, s.artifactPath(dirFailures, failureID))
}

func (s *FileStore) LoadFailuresForFiles(ctx context.Context, files []string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(files) == 0 {
		return nil, nil
	}
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		if normalized := normalizeArtifactPath(f); normalized != "" {
			fileSet[normalized] = true
		}
	}
	all, err := scanDir[schema.FailureFingerprint](ctx, filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		for _, af := range f.AffectedFiles {
			if fileSet[normalizeArtifactPath(af)] {
				out = append(out, f)
				break
			}
		}
	}
	return out, nil
}

func (s *FileStore) LoadFailuresForCapsule(ctx context.Context, capsuleID string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.FailureFingerprint](ctx, filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		if f.SourceCapsuleID == capsuleID {
			out = append(out, f)
		}
	}
	return out, nil
}

// LoadAllFailures returns FailureFingerprints associated with goalID.
// FailureFingerprint has no GoalID field, so the MVP implementation resolves
// SourceCapsuleID → ObligationID → GoalConditionID → GoalID.
func (s *FileStore) LoadAllFailures(ctx context.Context, goalID string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExistsLocked(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	all, err := scanDir[schema.FailureFingerprint](ctx, filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		resolvedGoalID, err := s.goalIDForCapsuleLocked(ctx, f.SourceCapsuleID)
		if err != nil {
			// Skip orphaned failures (capsule or obligation files missing) but
			// propagate genuine I/O or corruption errors so callers are not
			// silently given a truncated result.
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("store: LoadAllFailures: resolve goal for failure %s: %w", f.FailureID, err)
		}
		if resolvedGoalID != goalID {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *FileStore) LoadFailuresBySignature(ctx context.Context, goalID string, errorSignature string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExistsLocked(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	events, err := s.log.ReadByType(ctx, schema.EventFailureFingerprintCreated, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("store: LoadFailuresBySignature: read failure events: %w", err)
	}
	out := make([]*schema.FailureFingerprint, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		if ev.GoalID != goalID {
			continue
		}
		var failure schema.FailureFingerprint
		if err := json.Unmarshal(ev.Payload, &failure); err != nil {
			return nil, fmt.Errorf("store: LoadFailuresBySignature: unmarshal failure event %s: %w", ev.EventID, err)
		}
		if failure.ErrorSignature != errorSignature {
			continue
		}
		current, err := readFile[schema.FailureFingerprint](ctx, s.artifactPath(dirFailures, failure.FailureID))
		if err != nil {
			return nil, err
		}
		out = append(out, current)
		seen[failure.FailureID] = true
	}
	all, err := scanDir[schema.FailureFingerprint](ctx, filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].FailureID < all[j].FailureID
	})
	for _, failure := range all {
		if seen[failure.FailureID] || failure.ErrorSignature != errorSignature {
			continue
		}
		resolvedGoalID, err := s.goalIDForCapsuleLocked(ctx, failure.SourceCapsuleID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if resolvedGoalID == goalID {
			out = append(out, failure)
		}
	}
	return out, nil
}
