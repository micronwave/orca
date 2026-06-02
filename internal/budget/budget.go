// Package budget provides the Controller, which tracks token, time, and
// coordination cost against verified value. Budget state is derived from events
// in the log, not from the artifact store. BudgetRecords in the store are
// written by the Reconciler; the Controller computes live metrics directly from
// the event stream and enforces per-capsule limits before execution begins.
//
// Dependency contract:
//
//	Reads  (log):     all events for a goal; specifically capsule_created (for budget
//	                  limits in the payload), budget_record_saved/updated (for spend),
//	                  patch_accepted, evidence artifact events (for reuse counts)
//	Writes (log):     none — budget enforcement decisions are recorded as
//	                  DecisionRecords by the orchestrator, not by BudgetController
//
//	Must NOT import:  internal/planner, internal/runner, internal/verifier,
//	                  internal/reconciler, internal/projector, internal/gate
//	Must NOT read:    artifact_store — live budget state is derived entirely from events.
//	                  Budget limits are read from the capsule_created event payload
//	                  (which includes the full ExecutionCapsule.Budget).
package budget

import (
	"github.com/micronwave/orca/internal/schema"
)

// BudgetCheck is the result of a pre-run budget evaluation.
type BudgetCheck struct {
	// Allowed is false when accumulated spend plus estimated capsule cost
	// would exceed the configured limit.
	Allowed bool

	// Reason explains why Allowed is false; empty when Allowed is true.
	Reason string

	// CurrentSpend is the spend accumulated so far for this capsule,
	// derived from events in the log.
	CurrentSpend Spend

	// BudgetLimit is the limit read from the capsule_created event payload.
	BudgetLimit schema.CapsuleBudget
}

// Spend is a point-in-time cost snapshot for a capsule or goal.
type Spend struct {
	TokensUsed      int
	WallTimeSeconds float64
	ToolCalls       int
	Retries         int
	// CoordinationCostUnits captures non-token coordination overhead from
	// retries, duplicated file reads, overlapping edits, and human interventions.
	CoordinationCostUnits int
}

// ROI holds the verified-value metrics for a completed or in-progress goal.
// All counts are derived from events in the log. orca.md §12.
type ROI struct {
	// VerifiedValuePer1KTokens is the primary metric: satisfied obligations and
	// accepted patches normalized to token cost. orca.md §12.
	VerifiedValuePer1KTokens float64

	TotalTokensSpent        int
	TotalWallTimeSeconds    float64
	TotalToolCalls          int
	TotalCoordinationCost   int
	ObligationsDischarged   int
	PatchesAccepted         int
	PatchesRejected         int
	EvidenceArtifactsReused int
	AvoidedRetries          int
	HumanInterventions      int
}
