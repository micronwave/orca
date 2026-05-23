// Package gate provides the Gatekeeper, which surfaces approvals, blocked
// decisions, risks, and merge readiness to the developer.
//
// The Gatekeeper is called by the orchestrator at defined gate points; it is
// not called by any other component. It is the only component allowed to block
// the control loop pending human input.
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
