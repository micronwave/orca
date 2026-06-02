// Package mock provides a deterministic test adapter for Orca integration
// tests. It exercises every adapter collection path — sidecar accepted,
// invalid-sidecar → transcript fallback, no-sidecar → transcript fallback,
// and dirty-worktree preflight failure — without spawning real agent CLIs.
//
// All scenarios that write file changes require the capsule's
// Sandbox.WorktreePath to be an initialised git repository so the runner's
// subsequent git-diff and git-rev-parse operations succeed.
package mock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

// AgentType is the agent type string registered by this adapter. Capsules that
// should be handled by the mock adapter must set Agent to this value.
// This aliases schema.AgentMock for convenience.
const AgentType = schema.AgentMock

// Scenario selects which collection path and outcome the adapter simulates.
type Scenario string

const (
	// ScenarioSidecarAccepted — Execute returns valid sidecar output directly.
	ScenarioSidecarAccepted Scenario = "sidecar_accepted"

	// ScenarioStreamingOutput — same as SidecarAccepted; signals tests to assert
	// incremental event ordering.
	ScenarioStreamingOutput Scenario = "streaming_output"

	// ScenarioInvalidSidecar — Execute returns ErrInvalidSidecar; then
	// ExtractFromTranscript returns valid output (transcript fallback).
	ScenarioInvalidSidecar Scenario = "invalid_sidecar_fallback"

	// ScenarioNoSidecar — Execute returns ErrNoSidecar; then
	// ExtractFromTranscript returns valid output (transcript fallback).
	ScenarioNoSidecar Scenario = "no_sidecar_fallback"

	// ScenarioDirtyWorktree — Preflight returns a dirty-worktree error; Execute
	// is never called.
	ScenarioDirtyWorktree Scenario = "dirty_worktree_preflight"

	// ScenarioVerifierPass — Execute returns sidecar output; subsequent verifier
	// runs with a pass gate runner should produce VerdictSatisfied.
	ScenarioVerifierPass Scenario = "verifier_pass"

	// ScenarioVerifierFailure — Execute returns sidecar output; subsequent
	// verifier runs with a fail gate runner should produce VerdictFailed, leading
	// the reconciler to create a follow-up obligation.
	ScenarioVerifierFailure Scenario = "verifier_failure"
)

// Adapter is a test-only implementation of runner.Adapter that produces
// configurable, deterministic outputs for each collection path.
type Adapter struct {
	scenario Scenario
	orcaDir  string
}

// New returns an Adapter configured for the given scenario. orcaDir is used
// to build transcript paths (matching the convention in orcapath.TranscriptPath).
func New(scenario Scenario, orcaDir string) *Adapter {
	return &Adapter{scenario: scenario, orcaDir: orcaDir}
}

// AgentType returns the mock agent type string.
func (a *Adapter) AgentType() schema.AgentType { return AgentType }

// Preflight returns a clean-worktree error for ScenarioDirtyWorktree; otherwise
// it is a no-op and returns nil.
func (a *Adapter) Preflight(_ context.Context, capsule *schema.ExecutionCapsule) error {
	if a.scenario == ScenarioDirtyWorktree {
		return fmt.Errorf("clean worktree check failed: uncommitted changes in worktree %s",
			capsule.Sandbox.WorktreePath)
	}
	return nil
}

// Execute produces output according to the configured scenario.
//
//   - Sidecar scenarios: writes one file to the worktree, stages it, and returns
//     a valid AgentSidecarOutput.
//   - ScenarioInvalidSidecar: writes a transcript and returns ErrInvalidSidecar.
//   - ScenarioNoSidecar: writes a transcript and returns ErrNoSidecar.
//
// The runner falls back to ExtractFromTranscript for the two error cases.
func (a *Adapter) Execute(ctx context.Context, capsule *schema.ExecutionCapsule, _ *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	switch a.scenario {
	case ScenarioInvalidSidecar:
		if err := writeTranscript(a.orcaDir, capsule.CapsuleID, "mock agent output (sidecar invalid)"); err != nil {
			return nil, err
		}
		return nil, runner.ErrInvalidSidecar

	case ScenarioNoSidecar:
		if err := writeTranscript(a.orcaDir, capsule.CapsuleID, "mock agent output (no sidecar)"); err != nil {
			return nil, err
		}
		return nil, runner.ErrNoSidecar

	default:
		// ScenarioSidecarAccepted, ScenarioStreamingOutput,
		// ScenarioVerifierPass, ScenarioVerifierFailure — all return sidecar output.
		changedFile := "mock_output_" + capsule.CapsuleID + ".txt"
		if err := stageFile(ctx, capsule.Sandbox.WorktreePath, changedFile, "mock agent output"); err != nil {
			return nil, err
		}
		transcriptPath := orcapath.TranscriptPath(a.orcaDir, capsule.CapsuleID)
		if err := writeTranscript(a.orcaDir, capsule.CapsuleID, "mock agent sidecar output"); err != nil {
			return nil, err
		}
		return &schema.AgentSidecarOutput{
			ObligationsAddressed: append([]string(nil), capsule.ObligationIDs...),
			FilesChanged:         []string{changedFile},
			CommandsRun:          []string{"mock --scenario " + string(a.scenario)},
			Claims: []schema.SidecarClaim{{
				Claim: "mock implementation complete for " + string(a.scenario),
				Type:  schema.SidecarClaimVerified,
			}},
			Risks:           []string{},
			FollowUpNeeded:  []string{},
			EvidencePaths:   []string{transcriptPath},
			Summary:         "mock adapter: " + string(a.scenario),
			TokensUsed:      42,
			WallTimeSeconds: 1.0,
		}, nil
	}
}

// ExtractFromTranscript is called by the runner when Execute returns
// ErrNoSidecar or ErrInvalidSidecar. It writes a fallback file to the
// worktree, stages it, and returns structured output.
func (a *Adapter) ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
	changedFile := "mock_fallback_" + capsule.CapsuleID + ".txt"
	if err := stageFile(ctx, capsule.Sandbox.WorktreePath, changedFile, "mock fallback output"); err != nil {
		return nil, err
	}
	return &schema.AgentSidecarOutput{
		ObligationsAddressed: append([]string(nil), capsule.ObligationIDs...),
		FilesChanged:         []string{changedFile},
		CommandsRun:          []string{"mock --extract"},
		Claims: []schema.SidecarClaim{{
			Claim: "mock transcript extraction complete",
			Type:  schema.SidecarClaimProposed,
		}},
		EvidencePaths:   []string{transcriptPath},
		Summary:         "mock adapter: transcript fallback for " + capsule.CapsuleID,
		TokensUsed:      21,
		WallTimeSeconds: 0.5,
	}, nil
}

// stageFile writes content to worktreePath/name and stages it with git add.
func stageFile(ctx context.Context, worktreePath, name, content string) error {
	if err := os.WriteFile(filepath.Join(worktreePath, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("mock adapter: write %s: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, "git", "add", name)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mock adapter: git add %s: %w: %s", name, err, string(out))
	}
	return nil
}

// writeTranscript writes content to the capsule transcript path.
func writeTranscript(orcaDir, capsuleID, content string) error {
	p := orcapath.TranscriptPath(orcaDir, capsuleID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("mock adapter: create transcript dir: %w", err)
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// compile-time interface check.
var _ runner.Adapter = (*Adapter)(nil)
