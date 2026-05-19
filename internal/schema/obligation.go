package schema

// ObligationStatus is the lifecycle state of a single obligation. orca.md §5.2.
type ObligationStatus string

const (
	ObligationOpen      ObligationStatus = "open"
	ObligationSatisfied ObligationStatus = "satisfied"
	ObligationFailed    ObligationStatus = "failed"
	ObligationWaived    ObligationStatus = "waived"
)

// Obligation is a checkable requirement that must be discharged before Orca
// can say progress was made. Capsules exist only to satisfy obligations.
// orca.md §5.2.
type Obligation struct {
	ObligationID    string `json:"obligation_id"`
	GoalConditionID string `json:"goal_condition_id"`
	Description     string `json:"description"`
	// EvidenceRequired lists the evidence types expected to satisfy this obligation.
	EvidenceRequired []string         `json:"evidence_required"`
	Blocking         bool             `json:"blocking"`
	RiskLevel        RiskLevel        `json:"risk_level"`
	Status           ObligationStatus `json:"status"`
	// ExpectedFiles is the planner's file-level scheduling hint for this obligation.
	// It enables Phase 2 protected-path and overlap checks without making symbol-level
	// conflict detection an MVP dependency.
	ExpectedFiles []string `json:"expected_files,omitempty"`
	// SatisfiedBy holds the evidence artifact IDs that discharge this obligation.
	SatisfiedBy []string `json:"satisfied_by"`
}
