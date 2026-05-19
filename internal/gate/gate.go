// Package gate defines the HumanGatekeeper interface, which surfaces approvals,
// blocked decisions, risks, and merge readiness to the developer.
//
// The HumanGatekeeper is called by the orchestrator at defined gate points; it
// is not called by any other component. It is the only component allowed to
// block the control loop pending human input.
//
// Dependency contract:
//
//	Reads  (store):   HumanSummaryProjection via LoadHumanSummaryProjection,
//	                  Obligations via LoadObligation,
//	                  VerifierResult via LoadVerifierResultForPatch,
//	                  FailureFingerprints via LoadFailuresForFiles,
//	                  DecisionRecords via LoadDecision
//	Writes (store):   DecisionRecord (human approval/rejection) via SaveDecision
//	Writes (log):     none directly — store emits decision_record_created
//
//	Must NOT import:  internal/planner, internal/runner, internal/verifier,
//	                  internal/reconciler, internal/projector, internal/budget
//
// Gate activation rules (orca.md §5.4.2, §6, §15):
//   - human_gated topology: developer must approve the HumanSummaryProjection
//     before execution proceeds
//   - implementer_reviewer topology with medium or high risk obligations:
//     developer must approve before the implementer capsule runs
//   - single topology with low-risk obligations: projection is displayed and
//     execution proceeds after the configured review window (default 30s)
//     unless the developer blocks
//   - high-risk patch merge: human approval always required
package gate

import (
	"context"
	"time"
)

// HumanGatekeeper presents decisions to the developer and records their response
// as a DecisionRecord. Every gate call blocks until the developer responds or
// the context is cancelled.
type HumanGatekeeper interface {
	// ReviewProjection presents the HumanSummaryProjection for capsuleID to the
	// developer.
	//
	// reviewWindow controls blocking behavior:
	//   - reviewWindow == 0: blocks indefinitely until explicit approve/reject.
	//     Required for human_gated topology and implementer_reviewer with
	//     medium or high risk obligations.
	//   - reviewWindow > 0: displays the projection and returns automatically
	//     after the window expires if no response is given.
	//     Returns GateDecision{Approved: true, Proceeded: true} on timeout.
	//     Used for single topology with low-risk obligations (default 30s). orca.md §5.4.2.
	ReviewProjection(ctx context.Context, capsuleID string, reviewWindow time.Duration) (GateDecision, error)

	// ReviewMerge presents the VerifierResult and proof-carrying patch summary
	// for patchID to the developer and blocks until they approve or reject merge.
	// Called when ReconcileResult.MergeReady && ReconcileResult.HumanGateRequired.
	// orca.md §11.
	ReviewMerge(ctx context.Context, patchID string) (GateDecision, error)

	// ReviewWaiver presents an obligation waiver request to the developer.
	// Called when the orchestrator determines that an obligation cannot be
	// satisfied and requests explicit waiver authorization. orca.md §11.
	ReviewWaiver(ctx context.Context, obligationID string, reason string) (GateDecision, error)
}

// GateDecision records the outcome of one human gate interaction.
// The orchestrator persists this as a DecisionRecord via the store.
type GateDecision struct {
	// Approved is true when the developer approved the presented action,
	// or when a timed gate expired and execution proceeded automatically.
	Approved bool

	// Proceeded is true when the gate timed out and execution proceeded
	// automatically without an explicit developer response. Only possible
	// when ReviewProjection is called with reviewWindow > 0. orca.md §5.4.2.
	Proceeded bool

	// DecisionID is the ID of the DecisionRecord persisted by the gatekeeper.
	DecisionID string

	// Notes contains optional developer-provided notes or rejection reason.
	Notes string
}
