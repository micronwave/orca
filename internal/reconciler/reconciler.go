// Package reconciler defines the Reconciler interface, which maps evidence to
// obligations, accepts or rejects patches, and creates follow-up obligations.
//
// Reconciliation happens after every capsule completion, verifier failure, and
// merge. It is the component that advances obligation status, updates budget
// records, and determines merge readiness. orca.md §11.
//
// Dependency contract:
//
//	Reads  (store):   VerifierResult via LoadVerifierResultForPatch,
//	                  PatchArtifact via LoadPatch,
//	                  Obligations via LoadObligation,
//	                  EvidenceArtifacts via LoadEvidence,
//	                  FailureFingerprints via LoadFailuresForCapsule
//	                    (to build follow-up obligations from runner failures),
//	                  BudgetRecords via LoadBudgetForGoal
//	Writes (store):   Obligation status via UpdateObligationStatus,
//	                  Patch status via UpdatePatchStatus,
//	                  new Obligations via SaveObligation (follow-ups from failures),
//	                  DecisionRecords via SaveDecision,
//	                  BudgetRecords via UpdateBudgetRecord,
//	                  StateSnapshot via SaveSnapshot
//	Writes (log):     EventPatchAccepted, EventPatchRejected,
//	                  EventObligationCreated (for follow-up obligations),
//	                  EventDecisionRecordCreated, EventMergeApplied
//
//	Must NOT import:  internal/runner, internal/verifier, internal/projector,
//	                  internal/budget, internal/gate
//	Must NOT call:    store.SaveCapsule, store.SaveEvidence, store.SaveClaim
//	Must NOT run:     verifier stages or agent commands
//	Must NOT accept:  a patch without mapping evidence to every blocking obligation
package reconciler

import (
	"context"
)

// Reconciler processes the verifier result for a completed patch, advances
// obligation state, decides patch acceptance, creates follow-up obligations
// from failures, and records all decisions.
//
// Merge policy (orca.md §11):
//   - No merge while any blocking obligation is open
//   - High-risk obligations require human approval before merge
//   - Scope violations require human approval or capsule retry
//   - Failed static gates block merge unless explicitly waived
//   - Unverified claims may not justify merge
type Reconciler interface {
	// Reconcile processes the VerifierResult for patchID. It:
	//   1. Reads the VerifierResult and maps evidence to obligations
	//   2. Accepts or rejects each obligation based on verdict
	//   3. Accepts or rejects the patch; updates PatchArtifact status
	//   4. Creates follow-up Obligations from blocking failures (if rejected)
	//   5. Updates BudgetRecords with token spend per obligation
	//   6. Persists a DecisionRecord explaining the outcome
	//   7. Takes a StateSnapshot
	//   8. Emits the appropriate event (patch_accepted or patch_rejected)
	//
	// Returns ReconcileResult summarizing the decision. The orchestrator reads
	// MergeReady and HumanGateRequired to decide whether to surface a merge gate,
	// merge directly, or loop back to the planner with FollowUpObligationIDs.
	Reconcile(ctx context.Context, patchID string) (ReconcileResult, error)
}

// ReconcileResult summarizes the reconciler's decision for one patch.
type ReconcileResult struct {
	// PatchAccepted is true when all blocking obligations are satisfied or waived.
	PatchAccepted bool

	// MergeReady is true when PatchAccepted is true and the reconciler's merge
	// policy is satisfied: no open blocking obligations, static gates passed,
	// scope within contract.
	MergeReady bool

	// HumanGateRequired is true when MergeReady is true but the merge policy
	// requires an additional human approval before merge — e.g. high-risk
	// obligations, scope violations, or diffs above the configured size threshold.
	// The orchestrator must call gate.ReviewMerge before proceeding. orca.md §11.
	HumanGateRequired bool

	// FollowUpObligationIDs contains IDs of new Obligations created from blocking
	// failures. Non-empty only when PatchAccepted is false.
	FollowUpObligationIDs []string

	// DecisionID is the ID of the persisted DecisionRecord for this reconciliation.
	DecisionID string

	// BlockingReason is a human-readable explanation when MergeReady is false.
	BlockingReason string
}
