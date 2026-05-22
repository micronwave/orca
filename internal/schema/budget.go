package schema

import "time"

// BudgetRecord tracks token, time, and coordination cost against verified value
// for a capsule or goal. orca.md §12.
//
// Primary metric: verified value per 1K tokens, where verified value is based
// on satisfied obligations, accepted patches, avoided retries, and reused evidence.
type BudgetRecord struct {
	BudgetID     string `json:"budget_id"`
	GoalID       string `json:"goal_id"`
	CapsuleID    string `json:"capsule_id,omitempty"`
	ObligationID string `json:"obligation_id,omitempty"`

	// Consumption. CLI-backed capsules may report zero tokens when the underlying
	// CLI does not expose usage, but wall time should be populated from measured
	// execution duration.
	TokensSpent     int     `json:"tokens_spent"`
	WallTimeSeconds float64 `json:"wall_time_seconds"`
	ToolCalls       int     `json:"tool_calls"`
	Retries         int     `json:"retries"`

	// Waste signals
	DuplicatedFileReads int `json:"duplicated_file_reads"`
	OverlappingEdits    int `json:"overlapping_edits"`

	// Value delivered
	ObligationsDischarged   int `json:"obligations_discharged"`
	PatchesAccepted         int `json:"patches_accepted"`
	PatchesRejected         int `json:"patches_rejected"`
	EvidenceArtifactsReused int `json:"evidence_artifacts_reused"`
	AvoidedRetries          int `json:"avoided_retries"`
	HumanInterventions      int `json:"human_interventions"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
