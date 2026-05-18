package schema

import (
	"encoding/json"
	"time"
)

// EventType identifies what happened in the event log. orca.md §9.
type EventType string

const (
	EventGoalCreated               EventType = "goal_created"
	EventObligationCreated         EventType = "obligation_created"
	EventTopologySelected          EventType = "topology_selected"
	EventContextProjectionCreated  EventType = "context_projection_created"
	EventCapsuleStarted            EventType = "capsule_started"
	EventCapsuleCompleted          EventType = "capsule_completed"
	EventPatchArtifactCreated      EventType = "patch_artifact_created"
	EventEvidenceArtifactCreated   EventType = "evidence_artifact_created"
	EventClaimCreated              EventType = "claim_created"
	EventVerifierResultCreated     EventType = "verifier_result_created"
	EventFailureFingerprintCreated EventType = "failure_fingerprint_created"
	EventDecisionRecordCreated     EventType = "decision_record_created"
	EventPatchAccepted             EventType = "patch_accepted"
	EventPatchRejected             EventType = "patch_rejected"
	EventMergeApplied              EventType = "merge_applied"
	EventArtifactInvalidated       EventType = "artifact_invalidated"
)

// Event is one entry in the append-only event log.
// The event log is the authoritative history; the artifact graph is the
// materialized state derived from it. orca.md §9.
type Event struct {
	EventID     string          `json:"event_id"`
	Type        EventType       `json:"type"`
	GoalID      string          `json:"goal_id"`
	// ArtifactID is the ID of the primary artifact this event concerns, if any.
	ArtifactID  string          `json:"artifact_id,omitempty"`
	// Payload holds the artifact or metadata specific to this event type.
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
	// SequenceNum is the monotonically increasing event number within a goal.
	SequenceNum int64           `json:"sequence_num"`
}
