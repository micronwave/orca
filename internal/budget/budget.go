// Package budget defines the BudgetController interface, which tracks token,
// time, and coordination cost against verified value. It is a projection over
// the event log: budget state is derived from events, not from the artifact store.
//
// BudgetRecords in the artifact store are written by the Reconciler after
// reconciliation completes. The BudgetController computes live metrics directly
// from the event stream and enforces per-capsule limits before execution begins.
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
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// BudgetController enforces capsule budget limits and computes ROI metrics.
// Budget limits come from the capsule_created event payload; accumulated spend
// comes from budget_record_saved/updated event payloads in the event log.
type BudgetController interface {
	// CheckCapsuleBudget reads accumulated spend and budget limits from the
	// event log for capsuleID and returns whether execution is permitted.
	// Called by the orchestrator before handing capsuleID to the CapsuleRunner.
	CheckCapsuleBudget(ctx context.Context, capsuleID string) (BudgetCheck, error)

	// ComputeROI reads the full event log for goalID and returns verified
	// value metrics. The primary metric is verified value per 1K tokens.
	// orca.md §12.
	ComputeROI(ctx context.Context, goalID string) (ROI, error)
}

// BudgetCheck is the result of a pre-run budget evaluation.
type BudgetCheck struct {
	// Allowed is false when accumulated spend plus estimated capsule cost
	// would exceed the configured limit.
	Allowed bool

	// Reason explains why Allowed is false; empty when Allowed is true.
	Reason string

	// CurrentSpend is the spend accumulated so far for this capsule's goal,
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
}

// ROI holds the verified-value metrics for a completed or in-progress goal.
// All counts are derived from events in the log. orca.md §12.
type ROI struct {
	// VerifiedValuePer1KTokens is the primary metric: satisfied obligations and
	// accepted patches normalized to token cost. orca.md §12.
	VerifiedValuePer1KTokens float64

	TotalTokensSpent        int
	TotalWallTimeSeconds    float64
	ObligationsDischarged   int
	PatchesAccepted         int
	PatchesRejected         int
	EvidenceArtifactsReused int
	HumanInterventions      int
}
