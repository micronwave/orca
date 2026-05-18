package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// Replay reconstructs the FileStore's artifact files from the event log,
// reading events in order starting from afterSeq (0 = from the beginning).
// When afterSeq is non-zero, the caller must first restore the materialized
// snapshot state through that sequence; Replay only applies the later delta.
//
// It does not emit new events to the log; it writes artifact files directly
// using the store's internal write helpers. The store's mutex is held per
// event to prevent concurrent access during replay.
//
// Use Replay for crash recovery: delete all artifact JSON files under root,
// then call Replay(ctx, log, store, 0) to reconstruct the full materialized
// state from the authoritative event history.
//
// Events that require updating existing files (patch_accepted, capsule_started,
// etc.) fail if the target artifact is missing. That catches out-of-order or
// malformed histories instead of silently materializing stale state.
func Replay(ctx context.Context, log eventlog.EventLog, s *FileStore, afterSeq int64) error {
	const batchSize = 200
	seq := afterSeq
	for {
		events, err := log.ReadAfter(ctx, seq, batchSize)
		if err != nil {
			return fmt.Errorf("replay: read events after seq=%d: %w", seq, err)
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			if err := applyEvent(ctx, s, e); err != nil {
				return fmt.Errorf("replay: apply event seq=%d type=%s: %w",
					e.SequenceNum, e.Type, err)
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return nil
}

// applyEvent applies a single event to the file store without emitting a
// new event. Callers must not hold s.mu (this function acquires it internally
// via the internal write helpers to keep locking symmetric with normal saves).
func applyEvent(_ context.Context, s *FileStore, e schema.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch e.Type {

	// ── artifact creation: unmarshal payload and write file ──────────────────

	case schema.EventGoalCreated:
		var v schema.GoalIR
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal GoalIR: %w", err)
		}
		if err := validateArtifactID("goal", v.GoalID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirGoals, v.GoalID), &v)

	case schema.EventObligationCreated:
		var v schema.Obligation
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal Obligation: %w", err)
		}
		if err := validateArtifactID("obligation", v.ObligationID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirObligations, v.ObligationID), &v)

	case schema.EventCapsuleCreated:
		var v schema.ExecutionCapsule
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal ExecutionCapsule: %w", err)
		}
		if err := validateArtifactID("capsule", v.CapsuleID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirCapsules, v.CapsuleID), &v)

	case schema.EventContextProjectionCreated:
		// Distinguish executor vs human_summary by the Role field.
		var base schema.ContextProjection
		if err := json.Unmarshal(e.Payload, &base); err != nil {
			return fmt.Errorf("unmarshal ContextProjection base: %w", err)
		}
		if err := validateArtifactID("context projection", base.ContextProjectionID); err != nil {
			return err
		}
		if base.Role == schema.ProjectionRoleHumanSummary {
			var v schema.HumanSummaryProjection
			if err := json.Unmarshal(e.Payload, &v); err != nil {
				return fmt.Errorf("unmarshal HumanSummaryProjection: %w", err)
			}
			return s.writeFile(s.artifactPath(dirProjHuman, v.ContextProjectionID), &v)
		}
		if base.Role != schema.ProjectionRoleExecutor {
			return fmt.Errorf("invalid context_projection_created payload: role %q is not supported", base.Role)
		}
		return s.writeFile(s.artifactPath(dirProjExecutor, base.ContextProjectionID), &base)

	case schema.EventPatchArtifactCreated:
		var v schema.PatchArtifact
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal PatchArtifact: %w", err)
		}
		if err := validateArtifactID("patch", v.PatchID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirPatches, v.PatchID), &v)

	case schema.EventEvidenceArtifactCreated:
		var v schema.EvidenceArtifact
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal EvidenceArtifact: %w", err)
		}
		if err := validateArtifactID("evidence", v.EvidenceID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirEvidence, v.EvidenceID), &v)

	case schema.EventClaimCreated:
		var v schema.ClaimArtifact
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal ClaimArtifact: %w", err)
		}
		if err := validateArtifactID("claim", v.ClaimID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirClaims, v.ClaimID), &v)

	case schema.EventFailureFingerprintCreated:
		var v schema.FailureFingerprint
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal FailureFingerprint: %w", err)
		}
		if err := validateArtifactID("failure", v.FailureID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirFailures, v.FailureID), &v)

	case schema.EventVerifierResultCreated:
		var v schema.VerifierResult
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal VerifierResult: %w", err)
		}
		if err := validateArtifactID("verifier result", v.VerifierResultID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirVerifierResults, v.VerifierResultID), &v)

	case schema.EventDecisionRecordCreated:
		var v schema.DecisionRecord
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal DecisionRecord: %w", err)
		}
		if err := validateArtifactID("decision", v.DecisionID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirDecisions, v.DecisionID), &v)

	case schema.EventBudgetRecordSaved, schema.EventBudgetRecordUpdated:
		var v schema.BudgetRecord
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal BudgetRecord: %w", err)
		}
		if err := validateArtifactID("budget", v.BudgetID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirBudgets, v.BudgetID), &v)

	case schema.EventStateSnapshotSaved:
		var v schema.StateSnapshot
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("unmarshal StateSnapshot: %w", err)
		}
		if err := validateArtifactID("snapshot", v.SnapshotID); err != nil {
			return err
		}
		return s.writeFile(s.artifactPath(dirSnapshots, v.SnapshotID), &v)

	// ── state transitions: update existing files ─────────────────────────────

	case schema.EventGoalStatusUpdated:
		var p schema.GoalStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal goal_status_updated payload: %w", err)
		}
		if p.GoalID == "" || p.Status == "" {
			return fmt.Errorf("invalid goal_status_updated payload: goal_id and status are required")
		}
		return s.updateGoalStatusNoLock(p.GoalID, p.Status)

	case schema.EventObligationStatusUpdated:
		var p schema.ObligationStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal obligation_status_updated payload: %w", err)
		}
		if p.ObligationID == "" || p.Status == "" {
			return fmt.Errorf("invalid obligation_status_updated payload: obligation_id and status are required")
		}
		return s.updateObligationStatusNoLock(p.ObligationID, p.Status, p.SatisfiedBy)

	case schema.EventClaimStatusUpdated:
		var p schema.ClaimStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal claim_status_updated payload: %w", err)
		}
		if p.ClaimID == "" || p.Status == "" {
			return fmt.Errorf("invalid claim_status_updated payload: claim_id and status are required")
		}
		return s.updateClaimStatusNoLock(p.ClaimID, p.Status)

	case schema.EventCapsuleStarted, schema.EventCapsuleCompleted:
		var p schema.CapsuleTransitionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal capsule transition payload: %w", err)
		}
		if p.CapsuleID == "" || p.State == "" {
			return fmt.Errorf("invalid capsule transition payload: capsule_id and state are required")
		}
		return s.updateCapsuleStateNoLock(p.CapsuleID, p.State)

	case schema.EventPatchAccepted:
		var p schema.PatchStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal patch_accepted payload: %w", err)
		}
		if p.PatchID == "" {
			return fmt.Errorf("invalid patch_accepted payload: patch_id is required")
		}
		return s.updatePatchStatusNoLock(p.PatchID, schema.PatchAccepted)

	case schema.EventPatchRejected:
		var p schema.PatchStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal patch_rejected payload: %w", err)
		}
		if p.PatchID == "" {
			return fmt.Errorf("invalid patch_rejected payload: patch_id is required")
		}
		return s.updatePatchStatusNoLock(p.PatchID, schema.PatchRejected)

	// ── events that require no artifact file change ──────────────────────────

	case schema.EventTopologySelected,
		schema.EventMergeApplied,
		schema.EventArtifactInvalidated:
		return nil

	default:
		// Unknown event types are tolerated so that a newer log can be
		// replayed by an older binary without crashing.
		return nil
	}
}

// updateGoalStatusNoLock reads the goal file, updates Status, writes back.
// Caller must hold s.mu.Lock().
func (s *FileStore) updateGoalStatusNoLock(goalID string, status schema.GoalStatus) error {
	path := s.artifactPath(dirGoals, goalID)
	g, err := readFile[schema.GoalIR](path)
	if err != nil {
		return err
	}
	g.Status = status
	return s.writeFile(path, g)
}

// updateObligationStatusNoLock reads the obligation file, updates Status and
// SatisfiedBy, then writes back. Caller must hold s.mu.Lock().
func (s *FileStore) updateObligationStatusNoLock(obligationID string, status schema.ObligationStatus, satisfiedBy []string) error {
	path := s.artifactPath(dirObligations, obligationID)
	o, err := readFile[schema.Obligation](path)
	if err != nil {
		return err
	}
	o.Status = status
	if satisfiedBy != nil {
		o.SatisfiedBy = satisfiedBy
	}
	return s.writeFile(path, o)
}

// updateClaimStatusNoLock reads the claim file, updates Status, writes back.
// Caller must hold s.mu.Lock().
func (s *FileStore) updateClaimStatusNoLock(claimID string, status schema.ClaimStatus) error {
	path := s.artifactPath(dirClaims, claimID)
	c, err := readFile[schema.ClaimArtifact](path)
	if err != nil {
		return err
	}
	c.Status = status
	return s.writeFile(path, c)
}

// updateCapsuleStateNoLock reads the capsule file, updates State, writes back.
// Caller must hold s.mu.Lock().
func (s *FileStore) updateCapsuleStateNoLock(capsuleID string, state schema.CapsuleState) error {
	path := s.artifactPath(dirCapsules, capsuleID)
	c, err := readFile[schema.ExecutionCapsule](path)
	if err != nil {
		return err
	}
	c.State = state
	return s.writeFile(path, c)
}

// updatePatchStatusNoLock reads the patch file, updates Status, writes back.
// Caller must hold s.mu.Lock().
func (s *FileStore) updatePatchStatusNoLock(patchID string, status schema.PatchStatus) error {
	path := s.artifactPath(dirPatches, patchID)
	p, err := readFile[schema.PatchArtifact](path)
	if err != nil {
		return err
	}
	p.Status = status
	return s.writeFile(path, p)
}

// ReplayDir returns the subdirectory paths that Replay populates, useful for
// callers that want to wipe artifact files before a full replay.
func ReplayDir(root string) []string {
	dirs := []string{
		dirGoals, dirObligations, dirCapsules, dirSnapshots,
		dirProjExecutor, dirProjHuman,
		dirPatches, dirEvidence, dirClaims, dirBudgets,
		dirFailures, dirDecisions, dirVerifierResults,
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = filepath.Join(root, d)
	}
	return out
}
