package schema

import "time"

// GoalStatus is the lifecycle state of a GoalIR. orca.md §5.1.
type GoalStatus string

const (
	GoalStatusActive    GoalStatus = "active"
	GoalStatusBlocked   GoalStatus = "blocked"
	GoalStatusComplete  GoalStatus = "complete"
	GoalStatusCancelled GoalStatus = "cancelled"
)

// GoalConditionStatus tracks how far a single goal condition has been met.
type GoalConditionStatus string

const (
	GoalConditionUnmet        GoalConditionStatus = "unmet"
	GoalConditionPartiallyMet GoalConditionStatus = "partially_met"
	GoalConditionMet          GoalConditionStatus = "met"
	GoalConditionBlocked      GoalConditionStatus = "blocked"
)

// GoalConditionRefinement records an approved clarification or narrowing of a
// goal condition. Refinements may never silently broaden scope; user approval
// is required for any material change. orca.md §5.1.
type GoalConditionRefinement struct {
	RefinementID string    `json:"refinement_id"`
	Description  string    `json:"description"`
	ApprovedAt   time.Time `json:"approved_at"`
	// ApprovedBy is "human" or the capsule ID that triggered the refinement.
	ApprovedBy string `json:"approved_by"`
}

// GoalCondition is one checkable condition within a GoalIR. orca.md §5.1.
type GoalCondition struct {
	ID                   string                    `json:"id"`
	Description          string                    `json:"description"`
	EffectiveDescription string                    `json:"effective_description"`
	Status               GoalConditionStatus       `json:"status"`
	Refinements          []GoalConditionRefinement `json:"refinements"`
}

// ScopeConstraints bounds what an agent is allowed to touch. orca.md §5.1.
type ScopeConstraints struct {
	AllowedFiles     []string `json:"allowed_files"`
	ForbiddenFiles   []string `json:"forbidden_files"`
	ForbiddenActions []string `json:"forbidden_actions"`
}

// GoalIR is the durable representation of the user's objective. It tracks
// the desired end state, not a fixed task list. orca.md §5.1.
type GoalIR struct {
	GoalID           string           `json:"goal_id"`
	OriginalIntent   string           `json:"original_intent"`
	GoalConditions   []GoalCondition  `json:"goal_conditions"`
	ScopeConstraints ScopeConstraints `json:"scope_constraints"`
	RiskLevel        RiskLevel        `json:"risk_level"`
	CreatedAt        time.Time        `json:"created_at"`
	Status           GoalStatus       `json:"status"`
}
