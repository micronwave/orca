package schema

import (
	"encoding/json"
	"time"
)

// EventType identifies what happened in the event log. orca.md §9.
type EventType string

const (
	EventGoalCreated              EventType = "goal_created"
	EventObligationCreated        EventType = "obligation_created"
	EventTopologySelected         EventType = "topology_selected"
	EventContextProjectionCreated EventType = "context_projection_created"
	// EventCapsuleCreated is emitted by the ArtifactStore on SaveCapsule.
	// It captures the full capsule contract at creation time (state=pending),
	// which is needed for deterministic replay and for the BudgetController to
	// read capsule budget limits from the event stream. orca.md §9, §12.
	EventCapsuleCreated EventType = "capsule_created"
	EventCapsuleStarted EventType = "capsule_started"
	// EventCapsuleStateUpdated is emitted by the runner before every intermediate
	// lifecycle state mutation (workspace_attached, setup_run, agent_running) so
	// crash+replay reconstructs the exact state the capsule had reached.
	EventCapsuleStateUpdated EventType = "capsule_state_updated"
	EventCapsuleCompleted    EventType = "capsule_completed"
	// EventCapsuleProjectionLinked is emitted by FileStore.UpdateCapsuleProjectionID
	// before mutating the capsule file, making the projection link replayable.
	EventCapsuleProjectionLinked   EventType = "capsule_projection_linked"
	EventPatchArtifactCreated      EventType = "patch_artifact_created"
	EventEvidenceArtifactCreated   EventType = "evidence_artifact_created"
	EventClaimCreated              EventType = "claim_created"
	EventVerifierResultCreated     EventType = "verifier_result_created"
	EventFailureFingerprintCreated EventType = "failure_fingerprint_created"
	EventDecisionRecordCreated     EventType = "decision_record_created"
	EventBudgetRecordSaved         EventType = "budget_record_saved"
	EventBudgetRecordUpdated       EventType = "budget_record_updated"
	EventStateSnapshotSaved        EventType = "state_snapshot_saved"
	EventTopologyOutcomeRecorded   EventType = "topology_outcome_recorded"
	EventGoalStatusUpdated         EventType = "goal_status_updated"
	EventObligationStatusUpdated   EventType = "obligation_status_updated"
	EventClaimStatusUpdated        EventType = "claim_status_updated"
	EventPatchAccepted             EventType = "patch_accepted"
	EventPatchRejected             EventType = "patch_rejected"
	EventMergeApplied              EventType = "merge_applied"
	EventArtifactInvalidated       EventType = "artifact_invalidated"
)

// Event is one entry in the append-only event log.
// The event log is the authoritative history; the artifact graph is the
// materialized state derived from it. orca.md §9.
type Event struct {
	EventID string    `json:"event_id"`
	Type    EventType `json:"type"`
	GoalID  string    `json:"goal_id"`
	// ArtifactID is the ID of the primary artifact this event concerns, if any.
	ArtifactID string `json:"artifact_id,omitempty"`
	// Payload holds the artifact or metadata specific to this event type.
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
	// SequenceNum is the monotonically increasing sequence number within the log.
	SequenceNum int64 `json:"sequence_num"`
}

// GoalStatusPayload is the event payload for goal_status_updated.
// Callers that update GoalIR status directly must append this event before
// calling ArtifactStore.UpdateGoalStatus so replay can reconstruct the change.
type GoalStatusPayload struct {
	GoalID string     `json:"goal_id"`
	Status GoalStatus `json:"status"`
}

// ObligationStatusPayload is the event payload for obligation_status_updated.
// SatisfiedBy is a pointer so nil (field absent) means "no change to evidence IDs"
// while non-nil (including &[]string{}) means "set SatisfiedBy to exactly this slice".
// Using *[]string instead of []string with omitempty prevents the ambiguity where
// an intentional clear to empty would be indistinguishable from "no change" after
// JSON round-trip (omitempty strips both nil and empty slices).
type ObligationStatusPayload struct {
	ObligationID string           `json:"obligation_id"`
	Status       ObligationStatus `json:"status"`
	SatisfiedBy  *[]string        `json:"satisfied_by,omitempty"`
}

// ClaimStatusPayload is the event payload for claim_status_updated.
type ClaimStatusPayload struct {
	ClaimID              string      `json:"claim_id"`
	Status               ClaimStatus `json:"status"`
	LastValidatedAgainst string      `json:"last_validated_against"`
	ContradictedBy       []string    `json:"contradicted_by"`
	InvalidatedBy        []string    `json:"invalidated_by"`
}

// CapsuleTransitionPayload is the required payload for capsule_started and
// capsule_completed. The runner must append this event before updating the
// capsule file.
type CapsuleTransitionPayload struct {
	CapsuleID string       `json:"capsule_id"`
	State     CapsuleState `json:"state"`
}

// PatchStatusPayload is the required payload for patch_accepted and
// patch_rejected. The event type determines the target status.
type PatchStatusPayload struct {
	PatchID string `json:"patch_id"`
}

// CapsuleProjectionPayload is the event payload for capsule_projection_linked.
// FileStore.UpdateCapsuleProjectionID appends this event before mutating the
// capsule file so the projection link survives crash+replay.
type CapsuleProjectionPayload struct {
	CapsuleID    string `json:"capsule_id"`
	ProjectionID string `json:"projection_id"`
}

// TopologySelectedPayload is the event payload for topology_selected.
// Emitted by the orchestrator after the planner commits the topology decision.
type TopologySelectedPayload struct {
	Topology   Topology `json:"topology"`
	DecisionID string   `json:"decision_id"`
}
