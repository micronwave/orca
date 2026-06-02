// Package schema defines the core data types for the Orca proof runtime.
//
// Every type in this package is a plain data struct with JSON tags matching
// the artifact schemas defined in orca.md. No business logic lives here.
// Constructors, validation, and persistence are in separate packages.
//
// Sole exception: UnmarshalJSON methods on string-typed enum types (see
// enum_decode.go). Go requires methods to be defined in the same package as
// their receiver, so these cannot be moved. They implement the json.Unmarshaler
// interface to reject unknown values at decode time; they carry no domain logic.
//
// Naming follows orca.md section numbering:
//
//	goal.go       §5.1  GoalIR
//	obligation.go §5.2  Obligation
//	capsule.go    §5.3  ExecutionCapsule
//	context.go    §5.4  ContextProjection, HumanSummaryProjection
//	patch.go      §5.5  PatchArtifact, ProofCarryingPatch
//	evidence.go   §5.6  EvidenceArtifact
//	claim.go      §5.8  ClaimArtifact
//	failure.go    §5.9  FailureFingerprint
//	verifier.go   §10   VerifierResult
//	sidecar.go    §8    AgentSidecarOutput
//	decision.go   §5.10 DecisionRecord
//	event.go      §9    Event
//	budget.go     §12   BudgetRecord
//	snapshot.go         StateSnapshot
//	common.go           Shared enumerations
package schema
