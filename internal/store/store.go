// Package store defines the ArtifactStore interface — the sole read/write layer
// for all durable structured artifacts. Every component that persists or retrieves
// a structured artifact must go through this interface.
//
// Exception: raw debug log files written by the CapsuleRunner/Adapter to
// .orca/artifacts/logs/CAP-*/ are the one allowed direct filesystem write.
// They are unstructured debug artifacts, not consumed by runtime components,
// and are not mediated by this interface. orca.md §8, §9.
//
// The concrete implementation (file-backed JSON/JSONL for MVP) is responsible for
// appending artifact-creation events to the EventLog on every Save call. This is
// the mechanism by which obligation_planner, verifier_engine, and context_compiler
// get their artifacts logged without holding a direct EventLog dependency.
package store

import (
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// ArtifactStore is the read/write layer for all durable artifacts.
// It is the only persistence surface available to runtime components.
// The concrete implementation appends corresponding events to the EventLog
// on every Save call so that callers without a direct EventLog dependency
// still produce a complete event history.
type ArtifactStore interface {
	// --- Goal IR ---

	SaveGoal(ctx context.Context, g *schema.GoalIR) error
	LoadGoal(ctx context.Context, goalID string) (*schema.GoalIR, error)
	// Callers must append goal_status_updated before calling this method.
	UpdateGoalStatus(ctx context.Context, goalID string, status schema.GoalStatus) error

	// LoadActiveGoal returns the GoalIR with status "active", or (nil, nil) when
	// no active goal exists. The MVP constraint is one active goal per repo;
	// IntentCompiler calls this before creating a new goal to enforce that constraint.
	LoadActiveGoal(ctx context.Context) (*schema.GoalIR, error)

	// LoadGoalCondition finds a single GoalCondition by its ID within the stored GoalIR.
	// Returns an error if the goal or condition is not found.
	LoadGoalCondition(ctx context.Context, conditionID string) (*schema.GoalCondition, error)

	// --- Obligations ---

	SaveObligation(ctx context.Context, o *schema.Obligation) error
	LoadObligation(ctx context.Context, obligationID string) (*schema.Obligation, error)
	// LoadOpenObligations returns all Obligations for goalID with status "open".
	LoadOpenObligations(ctx context.Context, goalID string) ([]*schema.Obligation, error)
	// LoadObligationsForCondition returns all Obligations assigned to a GoalCondition.
	LoadObligationsForCondition(ctx context.Context, conditionID string) ([]*schema.Obligation, error)
	// UpdateObligationStatus transitions an obligation and records the evidence IDs
	// that satisfy it (satisfiedBy may be nil for failed/waived transitions).
	// Callers must append obligation_status_updated before calling this method.
	UpdateObligationStatus(ctx context.Context, obligationID string, status schema.ObligationStatus, satisfiedBy []string) error

	// --- Execution Capsules ---

	SaveCapsule(ctx context.Context, c *schema.ExecutionCapsule) error
	LoadCapsule(ctx context.Context, capsuleID string) (*schema.ExecutionCapsule, error)
	// Callers must append capsule_started or capsule_completed before calling this method.
	UpdateCapsuleState(ctx context.Context, capsuleID string, state schema.CapsuleState) error

	// --- Context Projections ---

	// SaveProjection stores a base ContextProjection (used for executor_projection).
	SaveProjection(ctx context.Context, p *schema.ContextProjection) error
	// SaveHumanSummaryProjection stores a HumanSummaryProjection.
	SaveHumanSummaryProjection(ctx context.Context, p *schema.HumanSummaryProjection) error
	LoadProjection(ctx context.Context, projectionID string) (*schema.ContextProjection, error)
	LoadHumanSummaryProjection(ctx context.Context, projectionID string) (*schema.HumanSummaryProjection, error)

	// --- Patch Artifacts ---

	SavePatch(ctx context.Context, p *schema.PatchArtifact) error
	LoadPatch(ctx context.Context, patchID string) (*schema.PatchArtifact, error)
	// Callers must append patch_accepted or patch_rejected before calling this method.
	UpdatePatchStatus(ctx context.Context, patchID string, status schema.PatchStatus) error
	// LoadPatchesForCapsule returns all PatchArtifacts produced by capsuleID.
	LoadPatchesForCapsule(ctx context.Context, capsuleID string) ([]*schema.PatchArtifact, error)

	// --- Evidence Artifacts ---

	SaveEvidence(ctx context.Context, e *schema.EvidenceArtifact) error
	LoadEvidence(ctx context.Context, evidenceID string) (*schema.EvidenceArtifact, error)
	// LoadEvidenceForObligation returns all EvidenceArtifacts that support or weaken obligationID.
	LoadEvidenceForObligation(ctx context.Context, obligationID string) ([]*schema.EvidenceArtifact, error)

	// --- Claim Artifacts ---

	SaveClaim(ctx context.Context, c *schema.ClaimArtifact) error
	LoadClaim(ctx context.Context, claimID string) (*schema.ClaimArtifact, error)
	// LoadVerifiedClaimsForFiles returns ClaimVerified claims whose AffectedFiles
	// intersect the given file list. Proposed and stale claims are excluded; the
	// context_compiler injects only verified claims as facts.
	LoadVerifiedClaimsForFiles(ctx context.Context, files []string) ([]*schema.ClaimArtifact, error)
	// LoadClaimsForCapsule returns all ClaimArtifacts whose SourceCapsuleID matches
	// capsuleID, regardless of status. Used by the Reconciler to verify claims on
	// patch acceptance: for each claim whose evidence_ids all resolve to artifacts
	// in the store, call UpdateClaimStatus(verified). orca.md §16.
	LoadClaimsForCapsule(ctx context.Context, capsuleID string) ([]*schema.ClaimArtifact, error)
	// Callers must append claim_status_updated before calling this method.
	UpdateClaimStatus(ctx context.Context, claimID string, status schema.ClaimStatus) error

	// --- Failure Fingerprints ---

	SaveFailure(ctx context.Context, f *schema.FailureFingerprint) error
	// LoadFailure returns a single FailureFingerprint by its ID.
	LoadFailure(ctx context.Context, failureID string) (*schema.FailureFingerprint, error)
	// LoadFailuresForFiles returns FailureFingerprints whose AffectedFiles intersect files.
	LoadFailuresForFiles(ctx context.Context, files []string) ([]*schema.FailureFingerprint, error)
	// LoadFailuresForCapsule returns FailureFingerprints produced during capsuleID's execution.
	// Used by the Reconciler to build follow-up obligations from runner failures.
	LoadFailuresForCapsule(ctx context.Context, capsuleID string) ([]*schema.FailureFingerprint, error)
	// LoadAllFailures returns every FailureFingerprint recorded under goalID.
	LoadAllFailures(ctx context.Context, goalID string) ([]*schema.FailureFingerprint, error)

	// --- Verifier Results ---

	SaveVerifierResult(ctx context.Context, r *schema.VerifierResult) error
	LoadVerifierResult(ctx context.Context, resultID string) (*schema.VerifierResult, error)
	// LoadVerifierResultForPatch returns the VerifierResult produced for patchID,
	// or an error if none exists yet.
	LoadVerifierResultForPatch(ctx context.Context, patchID string) (*schema.VerifierResult, error)

	// --- Decision Records ---

	SaveDecision(ctx context.Context, d *schema.DecisionRecord) error
	LoadDecision(ctx context.Context, decisionID string) (*schema.DecisionRecord, error)

	// --- Budget Records ---

	SaveBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error
	LoadBudgetRecord(ctx context.Context, budgetID string) (*schema.BudgetRecord, error)
	// LoadBudgetForGoal returns all BudgetRecords associated with goalID.
	LoadBudgetForGoal(ctx context.Context, goalID string) ([]*schema.BudgetRecord, error)
	UpdateBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error

	// --- State Snapshots ---

	SaveSnapshot(ctx context.Context, s *schema.StateSnapshot) error
	// LoadLatestSnapshot returns the most recent StateSnapshot for goalID.
	LoadLatestSnapshot(ctx context.Context, goalID string) (*schema.StateSnapshot, error)
}
