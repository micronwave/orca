// Package runner defines the CapsuleRunner and Adapter interfaces.
//
// CapsuleRunner executes an agent inside a bounded execution capsule by
// delegating to an Adapter. Adapters translate specific coding-agent CLIs
// (Codex, Claude Code, Copilot) into Orca's artifact schema. Adapter
// implementations live in sub-packages; the runner selects an adapter by
// capsule.Agent at runtime via a registry (wired by the orchestrator).
//
// Dependency contract (CapsuleRunner):
//
//	Reads  (store):   ExecutionCapsule via LoadCapsule,
//	                  ContextProjection via LoadProjection
//	Writes (store):   PatchArtifact via SavePatch,
//	                  EvidenceArtifacts via SaveEvidence,
//	                  ClaimArtifacts via SaveClaim,
//	                  FailureFingerprints via SaveFailure,
//	                  CapsuleState transitions via UpdateCapsuleState
//	Writes (log):     EventCapsuleStarted, EventCapsuleCompleted
//	                  before the matching UpdateCapsuleState call,
//	                  EventPatchArtifactCreated, EventEvidenceArtifactCreated,
//	                  EventClaimCreated, EventFailureFingerprintCreated
//
//	Must NOT import:  internal/planner, internal/verifier, internal/reconciler,
//	                  internal/projector, internal/budget, internal/gate
//	Must NOT call:    store.SaveGoal, store.SaveObligation, store.SaveCapsule,
//	                  store.SaveVerifierResult, store.SaveBudgetRecord
//	Must NOT advance: Obligation status — that belongs to the Reconciler
package runner

import (
	"context"
	"errors"

	"github.com/micronwave/orca/internal/schema"
)

// ErrNoSidecar is returned by Adapter.Execute when the agent does not produce
// structured sidecar output. The CapsuleRunner falls back to ExtractFromTranscript.
var ErrNoSidecar = errors.New("agent produced no structured sidecar output")

// ErrInvalidSidecar is returned by Adapter.Execute when the agent produced
// sidecar output that fails schema validation. The CapsuleRunner falls back
// to ExtractFromTranscript on both ErrNoSidecar and ErrInvalidSidecar. orca.md §8.
var ErrInvalidSidecar = errors.New("agent sidecar output failed schema validation")

// CapsuleRunner executes an agent inside a bounded execution capsule.
// It selects an Adapter by capsule.Agent, runs the agent under contract,
// collects outputs via sidecar or transcript extraction (transparently),
// normalizes the output into schema artifacts, persists them, and returns
// the IDs of all produced artifacts.
//
// State transitions driven by CapsuleRunner (in order):
//
//	CapsuleStatePending → CapsuleStateWorktreeCreated → CapsuleStateWorkspaceAttached
//	→ CapsuleStateSetupRun → CapsuleStateAgentRunning
//	→ CapsuleStateCompleted | CapsuleStateFailed
//
// Partial failures must leave no ambiguous intermediate state. On any error,
// the runner transitions the capsule to CapsuleStateFailed and ensures
// a FailureFingerprint is persisted before returning.
type CapsuleRunner interface {
	// Run executes capsuleID end-to-end and returns the IDs of all artifacts
	// produced. The PatchID field is empty only when the capsule fails before
	// producing any patch. EvidenceIDs, ClaimIDs, and FailureIDs may all be
	// non-empty on a partial run.
	Run(ctx context.Context, capsuleID string) (RunResult, error)
}

// RunResult holds the artifact IDs produced by a capsule run.
// Consumers load the actual artifacts from the store using these IDs.
type RunResult struct {
	CapsuleID string

	// PatchID is the ID of the candidate PatchArtifact. Empty on total failure.
	PatchID string

	EvidenceIDs []string
	ClaimIDs    []string

	// FailureIDs is non-empty when the capsule failed or partially failed.
	FailureIDs []string

	// SidecarUsed is true when the sidecar collection path was used,
	// false when transcript extraction was the primary path. orca.md §8.
	SidecarUsed bool
}

// Adapter translates a specific coding-agent CLI into Orca's artifact schema.
// Each AgentType has exactly one registered Adapter. Adapters are called only
// by CapsuleRunner; nothing else imports or calls an Adapter directly.
//
// The two collection paths (Execute and ExtractFromTranscript) are alternate
// paths that must produce structurally equivalent AgentSidecarOutput values.
// Downstream consumers (CapsuleRunner and beyond) must not be able to
// distinguish which path was used. orca.md §8.
type Adapter interface {
	// AgentType returns the schema.AgentType this adapter handles.
	AgentType() schema.AgentType

	// Preflight verifies that the CLI is available, authenticated, and the
	// capsule worktree is in a clean state before execution begins.
	// Returns a descriptive error if any check fails; the runner records a
	// FailureInfra fingerprint and aborts without launching the agent.
	Preflight(ctx context.Context, capsule *schema.ExecutionCapsule) error

	// Execute launches the agent under the capsule contract, injecting projection
	// as the agent's working briefing. It returns sidecar output when the agent
	// supports it. Returns ErrNoSidecar when the agent produces no structured
	// output, or ErrInvalidSidecar when output exists but fails schema validation.
	// The CapsuleRunner falls back to ExtractFromTranscript on either error. orca.md §8.
	Execute(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error)

	// ExtractFromTranscript parses a raw transcript file and returns the same
	// AgentSidecarOutput schema as Execute. The output must be structurally
	// identical to sidecar output; it must not be treated as a degraded path
	// that produces lesser or differently-shaped artifacts. orca.md §8.
	ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error)
}
