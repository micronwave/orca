// Package projector defines the ContextCompiler interface, which builds
// minimal role-specific context projections from the artifact graph.
//
// Phase 1 decision: human-summary implementation approaches are synthesized
// deterministically from goal text, obligations, capsule scope, topology
// decisions, and verifier gates. The compiler does not call a model.
//
// The package is named "projector" rather than "context" to avoid shadowing
// the stdlib "context" package in import declarations.
//
// Dependency contract:
//
//	Reads  (store):   GoalIR, GoalConditions, Obligations (for the capsule),
//	                  verified ClaimArtifacts via LoadVerifiedClaimsForFiles,
//	                  EvidenceArtifacts via LoadEvidenceForObligation,
//	                  FailureFingerprints via LoadFailuresForFiles,
//	                  ExecutionCapsule via LoadCapsule,
//	                  DecisionRecord via LoadDecision (topology decision, using
//	                    capsule.TopologyDecisionID, to populate
//	                    HumanSummaryProjection.Topology.Rationale),
//	                  StateSnapshot via LoadLatestSnapshot
//	Writes (store):   ContextProjection via SaveProjection,
//	                  HumanSummaryProjection via SaveHumanSummaryProjection
//	Writes (log):     none directly — the ArtifactStore implementation emits
//	                  context_projection_created on each Save call
//
//	Must NOT import:  internal/runner, internal/verifier, internal/reconciler,
//	                  internal/budget, internal/gate
//	Must NOT call:    store.SavePatch, store.SaveEvidence, store.SaveClaim,
//	                  store.SaveVerifierResult, store.SaveBudgetRecord,
//	                  store.SaveObligation, store.SaveCapsule
//	Must NOT inject:  proposed or stale claims as facts — include only with
//	                  a [proposed] label when relevant to a design decision
//	Must NOT merge:   executor_projection and human_summary_projection into one
//	                  document — they are always two separate artifacts (§5.4)
package projector

import (
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// ContextCompiler compiles role-specific context projections from the artifact
// graph for a given capsule. The two MVP projection types serve opposite audiences
// and must never be the same document. orca.md §5.4.
type ContextCompiler interface {
	// CompileExecutor builds the agent's working briefing for capsuleID.
	//
	// Content is minimal and constraint-focused: relevant goal conditions,
	// assigned obligations, allowed and forbidden paths, allowed tools,
	// verified claims (only when directly needed), failure fingerprints for
	// affected files, prior evidence artifacts, required outputs and schema.
	//
	// The returned ContextProjection has Role = ProjectionRoleExecutor and
	// FreshnessBase set to the latest StateSnapshot ID for the goal. When
	// LoadLatestSnapshot returns store.ErrNotFound because no reconciliation
	// snapshot exists yet, CompileExecutor must set FreshnessBase to "" and
	// return normally. FreshnessBase = "" signals that the projection was
	// compiled before any reconciliation snapshot exists; claim freshness checks
	// must treat this as no baseline and keep verified claims current.
	// CompileExecutor persists the projection via SaveProjection and returns
	// the stored record.
	CompileExecutor(ctx context.Context, capsuleID string) (*schema.ContextProjection, error)

	// CompileHumanSummary builds the developer-facing implementation briefing
	// for capsuleID. It is emitted before the capsule runner launches the agent,
	// not after. It is a pre-execution design document, not a post-hoc summary.
	//
	// Content includes: goal in plain language, conditions addressed,
	// implementation approach, expected file scope, explicit exclusions,
	// topology selection and rationale, design decisions, pre-execution risks,
	// evidence plan, budget, and required approvals. orca.md §5.4.2.
	//
	// CompileHumanSummary persists the projection via SaveHumanSummaryProjection
	// and returns the stored record. It must not include raw transcript content
	// or executor_projection content.
	CompileHumanSummary(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error)
}
