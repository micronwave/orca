package schema

import "time"

// DecisionRecord captures every important orchestration decision so that
// merge/retry reasoning is auditable without reading transcripts. orca.md §5.10.
//
// Examples: topology selection, agent selection, tool permission, patch
// acceptance/rejection, obligation waiver, topology collapse.
type DecisionRecord struct {
	DecisionID string `json:"decision_id"`
	// Context describes what was being decided.
	Context string `json:"context"`
	// Decision is the choice that was made.
	Decision string `json:"decision"`
	// Rationale explains why.
	Rationale string `json:"rationale"`
	// MadeBy is "human", a capsule ID, or "system".
	MadeBy string `json:"made_by"`
	// RelatedIDs holds capsule, obligation, patch, or goal IDs relevant to this decision.
	RelatedIDs  []string  `json:"related_ids"`
	Invalidates []string  `json:"invalidates,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}
