package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// processSupersededClaims marks each claim listed in patch.SupersededClaimIDs as
// superseded by the patch. Only called for accepted patches; skips claims that
// are already superseded or missing. The store emits claim_superseded internally.
func (s *Reconciler) processSupersededClaims(ctx context.Context, patch *schema.PatchArtifact) error {
	if patch == nil || len(patch.SupersededClaimIDs) == 0 {
		return nil
	}
	for _, claimID := range patch.SupersededClaimIDs {
		if strings.TrimSpace(claimID) == "" {
			continue
		}
		claim, err := s.store.LoadClaim(ctx, claimID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reconciler: load claim %s for supersession: %w", claimID, err)
		}
		if strings.TrimSpace(claim.SupersededBy) != "" {
			continue // already superseded by a prior patch
		}
		if err := s.store.UpdateClaimSupersession(ctx, claimID, patch.PatchID); err != nil {
			return fmt.Errorf("reconciler: update claim supersession %s: %w", claimID, err)
		}
	}
	return nil
}

func (s *Reconciler) verifyClaims(ctx context.Context, claims []*schema.ClaimArtifact, goalID string) error {
	snapshotID, err := s.latestSnapshotID(ctx, goalID)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Status == schema.ClaimVerified && claim.LastValidatedAgainst != "" {
			continue
		}
		if len(claim.EvidenceIDs) == 0 {
			continue
		}
		allPresent := true
		for _, evidenceID := range claim.EvidenceIDs {
			if _, err := s.store.LoadEvidence(ctx, evidenceID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					allPresent = false
					break
				}
				return fmt.Errorf("reconciler: load claim evidence %s: %w", evidenceID, err)
			}
		}
		if !allPresent {
			continue
		}
		if err := s.store.UpdateClaimValidation(ctx, claim.ClaimID, schema.ClaimVerified, snapshotID); err != nil {
			return fmt.Errorf("reconciler: update claim %s: %w", claim.ClaimID, err)
		}
	}
	return nil
}

func (s *Reconciler) detectClaimDisputes(ctx context.Context, goalID string, goalClaims []*schema.ClaimArtifact, vr *schema.VerifierResult) error {
	claimsByID := make(map[string]*schema.ClaimArtifact, len(goalClaims))
	for _, claim := range goalClaims {
		claimsByID[claim.ClaimID] = claim
	}
	for _, claim := range goalClaims {
		if claim.Status != schema.ClaimVerified {
			continue
		}
		for _, targetID := range claim.Contradicts {
			target := claimsByID[targetID]
			if target == nil || target.Status != schema.ClaimVerified {
				continue
			}
			if err := s.markClaimDisputed(ctx, claim, schema.ClaimContested, []string{target.ClaimID}, nil); err != nil {
				return err
			}
			if err := s.markClaimDisputed(ctx, target, schema.ClaimContested, []string{claim.ClaimID}, nil); err != nil {
				return err
			}
			claim.Status = schema.ClaimContested
			target.Status = schema.ClaimContested
		}
		// A claim that became contested via the Contradicts loop above is no
		// longer verified, so it must not be allowed to invalidate other claims.
		if claim.Status == schema.ClaimVerified {
			for _, targetID := range claim.Invalidates {
				target := claimsByID[targetID]
				if target == nil || target.Status != schema.ClaimVerified {
					continue
				}
				if err := s.markClaimDisputed(ctx, target, schema.ClaimInvalidated, nil, []string{claim.ClaimID}); err != nil {
					return err
				}
				target.Status = schema.ClaimInvalidated
			}
		}
	}
	for _, targetID := range vr.Invalidates {
		target := claimsByID[targetID]
		if target == nil || target.Status != schema.ClaimVerified {
			continue
		}
		if err := s.markClaimDisputed(ctx, target, schema.ClaimInvalidated, nil, []string{vr.VerifierResultID}); err != nil {
			return err
		}
		target.Status = schema.ClaimInvalidated
	}
	invalidatedByDecision, err := s.decisionInvalidations(ctx, goalID)
	if err != nil {
		return err
	}
	for claimID, invalidators := range invalidatedByDecision {
		target := claimsByID[claimID]
		if target == nil || target.Status != schema.ClaimVerified {
			continue
		}
		if err := s.markClaimDisputed(ctx, target, schema.ClaimInvalidated, nil, invalidators); err != nil {
			return err
		}
		target.Status = schema.ClaimInvalidated
	}
	return nil
}

func (s *Reconciler) decisionInvalidations(ctx context.Context, goalID string) (map[string][]string, error) {
	snap, snapErr := s.store.LoadLatestSnapshot(ctx, goalID)
	var afterSeq int64
	if errors.Is(snapErr, store.ErrNotFound) {
		afterSeq = 0
	} else if snapErr != nil {
		return nil, fmt.Errorf("decisionInvalidations: load snapshot for goal %s: %w", goalID, snapErr)
	} else if snap != nil {
		afterSeq = snap.SequenceNum
	}
	events, err := s.log.ReadByType(ctx, schema.EventDecisionRecordCreated, afterSeq, 0)
	if err != nil {
		return nil, fmt.Errorf("reconciler: read decision events for goal %s: %w", goalID, err)
	}
	out := make(map[string][]string)
	for _, event := range events {
		if event.GoalID != goalID {
			continue
		}
		var decision schema.DecisionRecord
		if err := json.Unmarshal(event.Payload, &decision); err != nil {
			return nil, fmt.Errorf("reconciler: unmarshal decision %s: %w", event.EventID, err)
		}
		for _, claimID := range decision.Invalidates {
			out[claimID] = append(out[claimID], decision.DecisionID)
		}
	}
	return out, nil
}

func (s *Reconciler) markClaimDisputed(ctx context.Context, claim *schema.ClaimArtifact, status schema.ClaimStatus, contradictedBy, invalidatedBy []string) error {
	if claim == nil {
		return nil
	}
	contradicted := mergeStrings(claim.ContradictedBy, contradictedBy)
	invalidated := mergeStrings(claim.InvalidatedBy, invalidatedBy)
	if claim.Status == status && sameStrings(claim.ContradictedBy, contradicted) && sameStrings(claim.InvalidatedBy, invalidated) {
		return nil
	}
	if err := s.store.UpdateClaimDispute(ctx, claim.ClaimID, status, contradicted, invalidated); err != nil {
		return fmt.Errorf("reconciler: update claim dispute %s: %w", claim.ClaimID, err)
	}
	claim.Status = status
	claim.ContradictedBy = contradicted
	claim.InvalidatedBy = invalidated
	return nil
}

func (s *Reconciler) FreshnessCheck(ctx context.Context, goalID string) error {
	if s.store == nil {
		return errors.New("reconciler: store is required")
	}
	if s.log == nil {
		return errors.New("reconciler: event log is required")
	}
	current, err := s.store.LoadLatestSnapshot(ctx, goalID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reconciler: load latest snapshot for goal %s: %w", goalID, err)
	}
	claims, err := s.store.LoadClaimsByStatus(ctx, goalID, schema.ClaimVerified)
	if err != nil {
		return fmt.Errorf("reconciler: load verified claims for goal %s: %w", goalID, err)
	}
	repoClaims, err := s.store.LoadRepoScopedClaims(ctx)
	if err != nil {
		return fmt.Errorf("reconciler: load repo-scoped claims: %w", err)
	}
	repoVerified := make([]*schema.ClaimArtifact, 0, len(repoClaims))
	for _, c := range repoClaims {
		if c.Status == schema.ClaimVerified {
			repoVerified = append(repoVerified, c)
		}
	}
	checkFreshness := func(claimsToCheck []*schema.ClaimArtifact) error {
		for _, claim := range claimsToCheck {
			if claim.LastValidatedAgainst == "" || claim.LastValidatedAgainst == current.SnapshotID {
				continue
			}
			validation, err := s.store.LoadSnapshot(ctx, claim.LastValidatedAgainst)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("reconciler: load validation snapshot %s: %w", claim.LastValidatedAgainst, err)
			}
			stale, err := s.claimTouchedSince(ctx, goalID, claim, validation.SequenceNum, current.SequenceNum)
			if err != nil {
				return err
			}
			if !stale {
				continue
			}
			if err := s.markClaimStatus(ctx, claim, schema.ClaimStale); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkFreshness(claims); err != nil {
		return err
	}
	return checkFreshness(repoVerified)
}

func (s *Reconciler) claimTouchedSince(ctx context.Context, goalID string, claim *schema.ClaimArtifact, afterSeq, throughSeq int64) (bool, error) {
	claimFiles := normalizedSet(claim.AffectedFiles)
	if len(claimFiles) == 0 {
		return false, nil
	}
	events, err := s.log.ReadForGoal(ctx, goalID, afterSeq, 0)
	if err != nil {
		return false, fmt.Errorf("reconciler: read events for goal %s: %w", goalID, err)
	}
	for _, event := range events {
		if event.SequenceNum > throughSeq {
			continue
		}
		if event.Type != schema.EventPatchAccepted && event.Type != schema.EventMergeApplied {
			continue
		}
		var payload schema.PatchStatusPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return false, fmt.Errorf("reconciler: unmarshal patch event %s: %w", event.EventID, err)
		}
		patchID := strings.TrimSpace(payload.PatchID)
		if patchID == "" {
			patchID = event.ArtifactID
		}
		patch, err := s.store.LoadPatch(ctx, patchID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("reconciler: load patch %s for freshness: %w", patchID, err)
		}
		if hasOverlap(claimFiles, normalizedSet(patch.ChangedFiles)) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Reconciler) markClaimStatus(ctx context.Context, claim *schema.ClaimArtifact, status schema.ClaimStatus) error {
	if claim.Status == status {
		return nil
	}
	if err := s.store.UpdateClaimStatus(ctx, claim.ClaimID, status); err != nil {
		return fmt.Errorf("reconciler: update claim %s: %w", claim.ClaimID, err)
	}
	claim.Status = status
	return nil
}

func (s *Reconciler) latestSnapshotID(ctx context.Context, goalID string) (string, error) {
	snapshot, err := s.store.LoadLatestSnapshot(ctx, goalID)
	if err == nil {
		return snapshot.SnapshotID, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("reconciler: load latest snapshot for goal %s: %w", goalID, err)
}

func (s *Reconciler) invalidateStaleClaims(ctx context.Context, patch *schema.PatchArtifact, goalClaims []*schema.ClaimArtifact, capsuleClaims []*schema.ClaimArtifact) error {
	if patch == nil {
		return nil
	}
	repoClaims, err := s.store.LoadRepoScopedClaims(ctx)
	if err != nil {
		return fmt.Errorf("reconciler: load repo-scoped claims: %w", err)
	}
	if len(goalClaims) == 0 && len(repoClaims) == 0 {
		return nil
	}
	changedFiles := normalizedSet(patch.ChangedFiles)
	changedSymbols := make(map[string]bool)
	for _, claim := range capsuleClaims {
		for _, symbol := range claim.AffectedSymbols {
			if normalized := normalizeSymbol(symbol); normalized != "" {
				changedSymbols[normalized] = true
			}
		}
	}
	checkClaims := func(claims []*schema.ClaimArtifact) error {
		for _, claim := range claims {
			if claim.Status != schema.ClaimVerified || claim.SourceCapsuleID == patch.CapsuleID {
				continue
			}
			fileOverlap := hasOverlap(normalizedSet(claim.AffectedFiles), changedFiles)
			symbolOverlap := hasOverlap(normalizedSymbols(claim.AffectedSymbols), changedSymbols)
			if !fileOverlap && !symbolOverlap {
				continue
			}
			if err := s.markClaimStatus(ctx, claim, schema.ClaimStale); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkClaims(goalClaims); err != nil {
		return err
	}
	return checkClaims(repoClaims)
}
