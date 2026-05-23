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

import (
	"time"

	"github.com/micronwave/orca/internal/schema"
)

// ShouldReviewProjection reports whether the orchestrator must invoke a
// projection gate before executing a capsule, given the selected topology and
// the highest obligation risk level in the plan cycle.
func ShouldReviewProjection(topology schema.Topology, risk schema.RiskLevel) bool {
	switch topology {
	case schema.TopologyHumanGated:
		return true
	case schema.TopologyImplementerReviewer:
		return risk == schema.RiskMedium || risk == schema.RiskHigh
	case schema.TopologySingle, schema.TopologyParallel, schema.TopologyTestFirst, schema.TopologyInvestigateThenImpl:
		return true
	default:
		return false
	}
}

// ReviewWindowFor returns the auto-proceed duration for the projection gate.
// A zero duration means the gate blocks indefinitely (no auto-proceed).
// defaultWindow is the configured review_window_seconds from config.
func ReviewWindowFor(topology schema.Topology, risk schema.RiskLevel, defaultWindow time.Duration) time.Duration {
	if topology == schema.TopologyHumanGated || topology == schema.TopologyImplementerReviewer {
		return 0
	}
	if risk == schema.RiskMedium || risk == schema.RiskHigh {
		return 0
	}
	return defaultWindow
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
