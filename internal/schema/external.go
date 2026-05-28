package schema

import "time"

// PRRecord tracks a pull request created from an accepted patch.
// orca.md §22 Phase 5.
type PRRecord struct {
	PRID       string    `json:"pr_id"`
	GoalID     string    `json:"goal_id"`
	PatchID    string    `json:"patch_id"`
	PRURL      string    `json:"pr_url"`
	BaseBranch string    `json:"base_branch"`
	HeadBranch string    `json:"head_branch"`
	Draft      bool      `json:"draft"`
	CreatedAt  time.Time `json:"created_at"`
}

// CIStatusRecord captures a single CI pipeline observation for a capsule branch.
// orca.md §22 Phase 5.
type CIStatusRecord struct {
	RecordID   string    `json:"record_id"`
	GoalID     string    `json:"goal_id"`
	CapsuleID  string    `json:"capsule_id"`
	Provider   string    `json:"provider"`
	Branch     string    `json:"branch"`
	Status     string    `json:"status"`
	RunURL     string    `json:"run_url"`
	Summary    string    `json:"summary,omitempty"`
	RawLogPath string    `json:"raw_log_path,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// IntakeRecord tracks an external issue ingested as a candidate goal.
// orca.md §22 Phase 5.
type IntakeRecord struct {
	RecordID    string    `json:"record_id"`
	GoalID      string    `json:"goal_id"`
	Source      string    `json:"source"`
	ExternalID  string    `json:"external_id"`
	ExternalURL string    `json:"external_url"`
	IngestedAt  time.Time `json:"ingested_at"`
}
