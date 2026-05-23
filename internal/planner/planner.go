// Package planner provides the Planner, which creates ExecutionCapsules for open
// obligations and records the topology decision. Topology classification is an
// internal detail; nothing outside this package calls it directly.
//
// Dependency contract (ObligationPlanner):
//
//	Reads  (store):   GoalIR via LoadGoal, GoalConditions via LoadGoalCondition,
//	                  open Obligations via LoadOpenObligations,
//	                  FailureFingerprints via LoadAllFailures
//	Writes (store):   ExecutionCapsules via SaveCapsule,
//	                  DecisionRecord (topology decision) via SaveDecision
//	Writes (log):     none directly — the ArtifactStore implementation emits
//	                  capsule_created, decision_record_created events on each Save call
//
//	Must NOT import:  internal/runner, internal/verifier, internal/reconciler,
//	                  internal/projector, internal/budget, internal/gate
//	Must NOT call:    store.SaveObligation, store.SavePatch, store.SaveEvidence,
//	                  store.SaveClaim, store.SaveVerifierResult, store.SaveBudgetRecord
//	                  (initial obligations are created by VerifierEngine.ProposeObligations;
//	                  follow-up obligations are created by Reconciler — the planner
//	                  only reads open obligations and creates capsules for them)
package planner

import (
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// ObligationPlanner reads open obligations for a goal, classifies topology,
// generates ExecutionCapsules, and returns the new capsule IDs ready for
// projection and execution.
//
// The planner calls TopologyClassifier before creating capsules. The topology
// decision is persisted as a DecisionRecord so the ContextCompiler and
// HumanGatekeeper can surface the rationale without re-running classification.
//
// Capsules must be created with State = CapsuleStatePending. The CapsuleRunner
// owns the transition pending → worktree_created as its first action, ensuring
// the stored state never claims a worktree exists before the runner creates it.
//
// Capsules must have TopologyDecisionID set to the DecisionRecord.DecisionID
// returned by SaveDecision before SaveCapsule is called. The ContextCompiler
// uses this field to load the topology rationale for HumanSummaryProjection.Topology
// via store.LoadDecision(capsule.TopologyDecisionID).
//
// PlanResult is returned by ObligationPlanner.Plan. It carries the capsule IDs
// plus the topology decision so the orchestrator can emit topology_selected to
// the event log without querying state it did not observe directly.
type PlanResult struct {
	// CapsuleIDs contains the IDs of the newly created ExecutionCapsules,
	// one per obligation group after topology selection.
	CapsuleIDs []string

	// Topology is the topology the classifier selected for this plan cycle.
	Topology schema.Topology

	// DecisionID is the ID of the persisted DecisionRecord that records the
	// topology selection rationale.
	DecisionID string

	// MaxObligationRisk is the highest risk level across the obligations in this
	// plan cycle. The orchestrator uses this — not goal.RiskLevel — to determine
	// whether a pre-execution projection gate is required, since goal risk and
	// obligation risk are set independently by the intent compiler and verifier.
	MaxObligationRisk schema.RiskLevel
}

// ClassifyInput holds the inputs to the internal topology classifier.
// Unknown fields remain zero-valued and must be treated as "unknown / use
// default behavior" rather than as negative evidence.
type ClassifyInput struct {
	Obligations  []*schema.Obligation
	Fingerprints []*schema.FailureFingerprint

	// The following fields may be empty/zero in Phase 1 implementations.
	// Populate them in Phase 2+ as the data becomes available.
	ExpectedFileOverlap       bool
	ApprovalPolicy            string
	BudgetRemaining           int
	ExpectedFilesByObligation map[string][]string
	ProtectedPaths            []string
}

// OutcomeReader is the planner-owned interface for reading historical topology
// outcomes. It is defined here (consumer side) rather than in internal/store so
// that the planner package can be tested without a concrete FileStore dependency.
// The concrete implementation is typically the ArtifactStore, wired by cmd/orca.
// TopologyClassifier operates correctly when the reader is nil.
type OutcomeReader interface {
	LoadTopologyOutcomes(ctx context.Context, topology schema.Topology, maxRisk schema.RiskLevel) ([]*schema.TopologyOutcomeRecord, error)
}
