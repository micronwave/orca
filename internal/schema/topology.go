package schema

import "time"

// TopologyOutcomeRecord captures the observed result of a topology decision.
// It is durable evidence for future routing, not runtime routing behavior itself.
type TopologyOutcomeRecord struct {
	OutcomeID       string    `json:"outcome_id"`
	GoalID          string    `json:"goal_id"`
	Topology        Topology  `json:"topology"`
	ObligationCount int       `json:"obligation_count"`
	MaxRiskLevel    RiskLevel `json:"max_risk_level"`
	AffectedFiles   []string  `json:"affected_files"`
	PatchAccepted   bool      `json:"patch_accepted"`
	ObligationsMet  int       `json:"obligations_met"`
	TokensSpent     int       `json:"tokens_spent"`
	FailureCount    int       `json:"failure_count"`
	RecordedAt      time.Time `json:"recorded_at"`
}
