// Package verifier defines the VerifierEngine interface, which has two jobs:
// (1) propose initial obligations from a GoalIR, and (2) check whether evidence
// satisfies obligations for a given patch. orca.md §6 step 3, §10.
//
// Dependency contract:
//
//	Reads  (store):   GoalIR via LoadGoal,
//	                  GoalConditions via LoadGoalCondition,
//	                  PatchArtifact via LoadPatch,
//	                  ExecutionCapsule via LoadCapsule (for scope contract),
//	                  Obligations via LoadObligation (for each claimed obligation),
//	                  EvidenceArtifacts via LoadEvidenceForObligation
//	Writes (store):   Obligations via SaveObligation (ProposeObligations only),
//	                  VerifierResult via SaveVerifierResult (Verify only)
//	Writes (log):     none directly — the ArtifactStore implementation emits
//	                  obligation_created on SaveObligation,
//	                  verifier_result_created on SaveVerifierResult
//
//	Must NOT import:  internal/planner, internal/runner, internal/reconciler,
//	                  internal/projector, internal/budget, internal/gate
//	Must NOT call:    store.SaveGoal, store.SaveCapsule, store.SaveBudgetRecord,
//	                  store.UpdateObligationStatus
//	Must NOT update:  Obligation status — advancing obligation state belongs
//	                  exclusively to the Reconciler
//	Must NOT run:     agent commands or model calls directly; verifier gates
//	                  invoke pre-configured user commands via a subprocess interface
package verifier

import (
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// VerifierEngine has two jobs (orca.md §6, §10):
//  1. ProposeObligations: derive the initial Obligation set from a GoalIR.
//  2. Verify: check whether collected evidence satisfies obligations for a patch.
//
// Default verification stages for Verify run in order (orca.md §10):
//  1. Preflight: repo status, auth, configured gates, clean base
//  2. Scope check: changed files and LOC within capsule contract
//  3. Static checks: lint, typecheck, formatting as configured
//  4. Targeted tests: tests relevant to changed files or obligations
//  5. Regression checks: reproduction or regression evidence for bugfix/security obligations
//  6. Patch review: model or human review for risk, assumptions, obligation fit
//  7. Merge readiness: all blocking obligations satisfied or waived
//
// Stages 1–4 run in order; the first blocking failure within that group stops
// the remaining stages in 1–4. Stages 5–7 always run for their applicable
// obligation types regardless of any stage 1–4 blocking failure.
type VerifierEngine interface {
	// ProposeObligations derives the initial Obligation set from the GoalIR
	// for goalID, persists each obligation via SaveObligation, and returns the
	// IDs of the created obligations. Called once by the orchestrator after
	// IntentCompiler.Compile and before ObligationPlanner.Plan. orca.md §6 step 3.
	ProposeObligations(ctx context.Context, goalID string) ([]string, error)

	// Verify runs all applicable verifier stages for the patch identified by
	// patchID and returns a VerifierResult mapping each obligation to its
	// verdict. The result is persisted via SaveVerifierResult before returning.
	//
	// The RecommendedAction field is the authoritative signal consumed by the
	// Reconciler: accept, retry, split, reject, or human_review.
	Verify(ctx context.Context, patchID string) (*schema.VerifierResult, error)
}
