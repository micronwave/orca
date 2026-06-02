package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/micronwave/orca/internal/schema"
)

// AppendRuntimeEvent appends a CapsuleRuntimeEvent to the event log and
// materialises the latest status for the capsule to state/capsule_runtime/.
// The stored file is always overwritten with the newest event so
// LoadLatestRuntimeStatus returns O(1) without scanning the full log.
// Replay reconstructs the same materialized file deterministically because
// applyEvent processes events in order and overwrites on each EventCapsuleRuntimeStatus.
func (s *FileStore) AppendRuntimeEvent(ctx context.Context, ev *schema.CapsuleRuntimeEvent) error {
	if ev == nil {
		return fmt.Errorf("store: runtime event is required")
	}
	if ev.CapsuleID == "" {
		return fmt.Errorf("store: runtime event capsule_id is required")
	}
	if err := validateArtifactID("capsule runtime", ev.CapsuleID); err != nil {
		return err
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("store: marshal runtime event for capsule %s: %w", ev.CapsuleID, err)
	}
	logEv, err := s.log.Append(ctx, schema.Event{
		Type:       schema.EventCapsuleRuntimeStatus,
		GoalID:     ev.GoalID,
		ArtifactID: ev.CapsuleID,
		Payload:    payload,
	})
	if err != nil {
		return fmt.Errorf("store: append runtime event for capsule %s: %w", ev.CapsuleID, err)
	}
	ev.Seq = logEv.SequenceNum

	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.artifactPath(dirCapsuleRuntime, ev.CapsuleID)
	if err := s.writeFile(path, ev); err != nil {
		return &MaterializationError{Event: logEv, Err: fmt.Errorf("store: write runtime status for capsule %s: %w", ev.CapsuleID, err)}
	}
	return nil
}

// LoadLatestRuntimeStatus returns the most recently persisted CapsuleRuntimeEvent
// for capsuleID, or ErrNotFound if no events have been emitted yet.
func (s *FileStore) LoadLatestRuntimeStatus(ctx context.Context, capsuleID string) (*schema.CapsuleRuntimeEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateArtifactID("capsule runtime", capsuleID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.CapsuleRuntimeEvent](s.artifactPath(dirCapsuleRuntime, capsuleID))
}

// SaveStartupBundle persists a StartupEvidenceBundle for a timed-out capsule.
// The bundle is keyed by CapsuleID; at most one bundle per capsule is stored.
func (s *FileStore) SaveStartupBundle(ctx context.Context, bundle *schema.StartupEvidenceBundle) error {
	if bundle == nil {
		return fmt.Errorf("store: startup bundle is required")
	}
	if bundle.CapsuleID == "" {
		return fmt.Errorf("store: startup bundle capsule_id is required")
	}
	if err := validateArtifactID("startup bundle", bundle.CapsuleID); err != nil {
		return err
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("store: marshal startup bundle for capsule %s: %w", bundle.CapsuleID, err)
	}
	logEv, err := s.log.Append(ctx, schema.Event{
		Type:       schema.EventStartupBundleCreated,
		GoalID:     "",
		ArtifactID: bundle.CapsuleID,
		Payload:    payload,
	})
	if err != nil {
		return fmt.Errorf("store: append startup bundle event for capsule %s: %w", bundle.CapsuleID, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.artifactPath(dirStartupBundles, bundle.CapsuleID)
	if err := s.writeFile(path, bundle); err != nil {
		return &MaterializationError{Event: logEv, Err: fmt.Errorf("store: write startup bundle for capsule %s: %w", bundle.CapsuleID, err)}
	}
	return nil
}

// LoadStartupBundle returns the StartupEvidenceBundle for capsuleID,
// or ErrNotFound if no bundle was recorded.
func (s *FileStore) LoadStartupBundle(ctx context.Context, capsuleID string) (*schema.StartupEvidenceBundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateArtifactID("startup bundle", capsuleID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.StartupEvidenceBundle](s.artifactPath(dirStartupBundles, capsuleID))
}
