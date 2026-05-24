// Package projector provides the Compiler, which builds minimal role-specific
// context projections from the artifact graph.
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
