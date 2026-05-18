package schema

import "time"

// StateSnapshot is a point-in-time record of the artifact graph state.
// Context projections reference a snapshot via FreshnessBase; claim artifacts
// reference one via LastValidatedAgainst. orca.md §9.
type StateSnapshot struct {
	SnapshotID  string    `json:"snapshot_id"`
	GoalID      string    `json:"goal_id"`
	// EventID is the last event included in this snapshot.
	EventID     string    `json:"event_id"`
	SequenceNum int64     `json:"sequence_num"`
	CreatedAt   time.Time `json:"created_at"`
}
