// Package store defines the ArtifactStore interface — the sole read/write layer
// for all durable structured artifacts. Every component that persists or retrieves
// a structured artifact must go through this interface.
//
// Exception: raw debug log files written by the CapsuleRunner/Adapter are the
// one allowed direct filesystem write. For capsule execution transcripts this
// follows <orcaDir>/capsules/<capsuleID>/transcript.log.
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
	// For the initial transition to CapsuleStateWorktreeCreated, callers must append
	// capsule_started before calling this method. For the final transition to
	// CapsuleStateCompleted or CapsuleStateFailed, callers must append capsule_completed
	// first. Intermediate transitions (workspace_attached, setup_run, agent_running)
	// do not require a preceding log event.
	UpdateCapsuleState(ctx context.Context, capsuleID string, state schema.CapsuleState) error
	// UpdateCapsuleProjectionID links an executor projection to its capsule.
	// Called by the orchestrator after CompileExecutor so the runner can load
	// the projection via capsule.ContextProjectionID. The implementation appends
	// capsule_projection_linked to the event log before mutating the capsule file.
	UpdateCapsuleProjectionID(ctx context.Context, capsuleID, projectionID string) error

	// --- Context Projections ---

	// SaveProjection stores a base agent ContextProjection (executor, reviewer, tester).
	SaveProjection(ctx context.Context, p *schema.ContextProjection) error
	// SaveHumanSummaryProjection stores a HumanSummaryProjection.
	SaveHumanSummaryProjection(ctx context.Context, p *schema.HumanSummaryProjection) error
	LoadProjection(ctx context.Context, projectionID string) (*schema.ContextProjection, error)
	LoadHumanSummaryProjection(ctx context.Context, projectionID string) (*schema.HumanSummaryProjection, error)
	LoadHumanSummaryProjectionForCapsule(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error)

	// --- Patch Artifacts ---

	SavePatch(ctx context.Context, p *schema.PatchArtifact) error
	LoadPatch(ctx context.Context, patchID string) (*schema.PatchArtifact, error)
	// Callers must append patch_accepted or patch_rejected before calling this method.
	UpdatePatchStatus(ctx context.Context, patchID string, status schema.PatchStatus) error
	// LoadPatchesForCapsule returns all PatchArtifacts produced by capsuleID.
	LoadPatchesForCapsule(ctx context.Context, capsuleID string) ([]*schema.PatchArtifact, error)
	// LoadPatchesForObligation returns candidate patches claiming obligationID.
	LoadPatchesForObligation(ctx context.Context, obligationID string) ([]*schema.PatchArtifact, error)

	// --- Evidence Artifacts ---

	SaveEvidence(ctx context.Context, e *schema.EvidenceArtifact) error
	LoadEvidence(ctx context.Context, evidenceID string) (*schema.EvidenceArtifact, error)
	// LoadEvidenceForObligation returns all EvidenceArtifacts that support or weaken obligationID.
	LoadEvidenceForObligation(ctx context.Context, obligationID string) ([]*schema.EvidenceArtifact, error)
	LoadReusableEvidenceForObligation(ctx context.Context, obligationID string, evidenceType schema.EvidenceType, reuseKey string, snapshotID string) (*schema.EvidenceArtifact, error)

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
	// LoadClaimsForGoal returns all claims whose source capsule resolves to goalID.
	// Reconciler uses this to stale-out verified claims when accepted patches
	// overlap previously validated files/symbols.
	LoadClaimsForGoal(ctx context.Context, goalID string) ([]*schema.ClaimArtifact, error)
	LoadClaimsByStatus(ctx context.Context, goalID string, status schema.ClaimStatus) ([]*schema.ClaimArtifact, error)
	// Callers must append claim_status_updated before calling this method.
	UpdateClaimStatus(ctx context.Context, claimID string, status schema.ClaimStatus) error
	// Callers must append claim_status_updated before calling these methods.
	UpdateClaimDispute(ctx context.Context, claimID string, status schema.ClaimStatus, contradictedBy, invalidatedBy []string) error
	UpdateClaimValidation(ctx context.Context, claimID string, status schema.ClaimStatus, snapshotID string) error

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
	LoadFailuresBySignature(ctx context.Context, goalID string, errorSignature string) ([]*schema.FailureFingerprint, error)

	// --- Verifier Results ---

	SaveVerifierResult(ctx context.Context, r *schema.VerifierResult) error
	LoadVerifierResult(ctx context.Context, resultID string) (*schema.VerifierResult, error)
	// LoadVerifierResultForPatch returns the VerifierResult produced for patchID,
	// or an error if none exists yet.
	LoadVerifierResultForPatch(ctx context.Context, patchID string) (*schema.VerifierResult, error)

	// --- Decision Records ---

	SaveDecision(ctx context.Context, d *schema.DecisionRecord) error
	LoadDecision(ctx context.Context, decisionID string) (*schema.DecisionRecord, error)

	// --- Topology Outcomes ---

	SaveTopologyOutcome(ctx context.Context, r *schema.TopologyOutcomeRecord) error
	LoadTopologyOutcomesForGoal(ctx context.Context, goalID string) ([]*schema.TopologyOutcomeRecord, error)
	LoadTopologyOutcomes(ctx context.Context, topology schema.Topology, maxRisk schema.RiskLevel) ([]*schema.TopologyOutcomeRecord, error)

	// --- Budget Records ---

	SaveBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error
	LoadBudgetRecord(ctx context.Context, budgetID string) (*schema.BudgetRecord, error)
	// LoadBudgetForGoal returns all BudgetRecords associated with goalID.
	LoadBudgetForGoal(ctx context.Context, goalID string) ([]*schema.BudgetRecord, error)
	UpdateBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error

	// --- State Snapshots ---

	SaveSnapshot(ctx context.Context, s *schema.StateSnapshot) error
	// LoadLatestSnapshot returns the most recent StateSnapshot for goalID,
	// or (nil, ErrNotFound) when no snapshot has been taken yet (e.g. before the
	// first reconciliation completes). Callers must check errors.Is(err, ErrNotFound)
	// before accessing the returned snapshot; this is a normal condition, not a failure.
	LoadLatestSnapshot(ctx context.Context, goalID string) (*schema.StateSnapshot, error)
	LoadSnapshot(ctx context.Context, snapshotID string) (*schema.StateSnapshot, error)
}
