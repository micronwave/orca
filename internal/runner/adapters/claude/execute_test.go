package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

// TestMain doubles as a stub claude process when ORCA_FAKE_CLAUDE is set.
// The adapter invokes os.Executable() (the test binary) directly; the subprocess
// re-enters via TestMain, acts as the stub, and exits before m.Run() is called.
func TestMain(m *testing.M) {
	if mode := os.Getenv("ORCA_FAKE_CLAUDE"); mode != "" {
		os.Exit(fakeClaudeMain(mode))
	}
	os.Exit(m.Run())
}

// fakeClaudeMain writes the expected JSON envelope to stdout. Returns an exit code.
func fakeClaudeMain(mode string) int {
	switch mode {
	case "valid":
		inner, err := json.Marshal(schema.AgentSidecarOutput{
			ObligationsAddressed: []string{"OB-77"},
			FilesChanged:         []string{"pkg/bar.go"},
			CommandsRun:          []string{"go vet ./..."},
			Summary:              "review complete",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude: marshal inner:", err)
			return 2
		}
		envelope := claudeJSONResult{
			Result:  string(inner),
			IsError: false,
			Usage:   &claudeUsage{InputTokens: 100, OutputTokens: 50},
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude: marshal envelope:", err)
			return 2
		}
		fmt.Println(string(data))
	case "is_error":
		data, _ := json.Marshal(claudeJSONResult{Result: "something went wrong", IsError: true})
		fmt.Println(string(data))
	case "bad_envelope":
		fmt.Println("this is not valid json at all")
	case "bad_inner_json":
		data, _ := json.Marshal(claudeJSONResult{Result: "{{ not json }}", IsError: false})
		fmt.Println(string(data))
	case "empty_result":
		data, _ := json.Marshal(claudeJSONResult{Result: "", IsError: false})
		fmt.Println(string(data))
	case "empty_sidecar":
		inner, _ := json.Marshal(schema.AgentSidecarOutput{})
		data, _ := json.Marshal(claudeJSONResult{Result: string(inner), IsError: false})
		fmt.Println(string(data))
	case "valid_nonzero":
		// Writes a valid JSON envelope to stdout then exits 1 to simulate a
		// context timeout or cleanup error that fires after output is complete.
		inner, _ := json.Marshal(schema.AgentSidecarOutput{
			ObligationsAddressed: []string{"OB-77"},
			FilesChanged:         []string{"pkg/bar.go"},
			CommandsRun:          []string{"go vet ./..."},
			Summary:              "review complete",
		})
		data, _ := json.Marshal(claudeJSONResult{Result: string(inner), IsError: false})
		fmt.Println(string(data))
		return 1
	default:
		fmt.Fprintln(os.Stderr, "fake claude: unknown mode:", mode)
		return 2
	}
	return 0
}

func claudeTestBinaryPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

func claudeTestCapsule(worktree string) *schema.ExecutionCapsule {
	return &schema.ExecutionCapsule{
		CapsuleID:     "CAP-CLAUDE-1",
		Sandbox:       schema.CapsuleSandbox{WorktreePath: worktree},
		ObligationIDs: []string{"OB-77"},
		Budget:        schema.CapsuleBudget{MaxWallTimeSeconds: 30},
	}
}

func claudeTestProjection() *schema.ContextProjection {
	return &schema.ContextProjection{
		ContextProjectionID: "CTX-CLAUDE-1",
		Role:                schema.ProjectionRoleExecutor,
		TokenBudget:         4096,
	}
}

func TestExecute_ValidEnvelope(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "valid")
	orcaDir := t.TempDir()
	worktree := t.TempDir()
	adapter := New(orcaDir, claudeTestBinaryPath(t))

	out, err := adapter.Execute(context.Background(), claudeTestCapsule(worktree), claudeTestProjection())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.ObligationsAddressed) != 1 || out.ObligationsAddressed[0] != "OB-77" {
		t.Errorf("ObligationsAddressed = %v, want [OB-77]", out.ObligationsAddressed)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "pkg/bar.go" {
		t.Errorf("FilesChanged = %v, want [pkg/bar.go]", out.FilesChanged)
	}
	if out.TokensUsed != 150 {
		t.Errorf("TokensUsed = %d, want 150 (100+50 from usage)", out.TokensUsed)
	}
	if out.WallTimeSeconds <= 0 {
		t.Errorf("WallTimeSeconds = %v, want > 0", out.WallTimeSeconds)
	}
}

func TestExecute_IsError_ErrNoSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "is_error")
	adapter := New(t.TempDir(), claudeTestBinaryPath(t))
	_, err := adapter.Execute(context.Background(), claudeTestCapsule(t.TempDir()), claudeTestProjection())
	if !errors.Is(err, runner.ErrNoSidecar) {
		t.Errorf("err = %v, want ErrNoSidecar", err)
	}
}

func TestExecute_BadEnvelope_ErrNoSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "bad_envelope")
	adapter := New(t.TempDir(), claudeTestBinaryPath(t))
	_, err := adapter.Execute(context.Background(), claudeTestCapsule(t.TempDir()), claudeTestProjection())
	if !errors.Is(err, runner.ErrNoSidecar) {
		t.Errorf("err = %v, want ErrNoSidecar", err)
	}
}

func TestExecute_BadInnerJSON_ErrInvalidSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "bad_inner_json")
	adapter := New(t.TempDir(), claudeTestBinaryPath(t))
	_, err := adapter.Execute(context.Background(), claudeTestCapsule(t.TempDir()), claudeTestProjection())
	if !errors.Is(err, runner.ErrInvalidSidecar) {
		t.Errorf("err = %v, want ErrInvalidSidecar", err)
	}
}

func TestExecute_EmptyResult_ErrNoSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "empty_result")
	adapter := New(t.TempDir(), claudeTestBinaryPath(t))
	_, err := adapter.Execute(context.Background(), claudeTestCapsule(t.TempDir()), claudeTestProjection())
	if !errors.Is(err, runner.ErrNoSidecar) {
		t.Errorf("err = %v, want ErrNoSidecar", err)
	}
}

func TestExecute_EmptySidecar_ErrInvalidSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CLAUDE", "empty_sidecar")
	adapter := New(t.TempDir(), claudeTestBinaryPath(t))
	_, err := adapter.Execute(context.Background(), claudeTestCapsule(t.TempDir()), claudeTestProjection())
	if !errors.Is(err, runner.ErrInvalidSidecar) {
		t.Errorf("err = %v, want ErrInvalidSidecar", err)
	}
}

func TestExecute_ValidEnvelope_NonZeroExit(t *testing.T) {
	// Claude writes a valid JSON envelope then exits 1 (e.g. context timeout fires
	// after output is complete). Execute must return the sidecar, not an error.
	t.Setenv("ORCA_FAKE_CLAUDE", "valid_nonzero")
	orcaDir := t.TempDir()
	worktree := t.TempDir()
	out, err := New(orcaDir, claudeTestBinaryPath(t)).Execute(
		context.Background(), claudeTestCapsule(worktree), claudeTestProjection(),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.ObligationsAddressed) != 1 || out.ObligationsAddressed[0] != "OB-77" {
		t.Errorf("ObligationsAddressed = %v, want [OB-77]", out.ObligationsAddressed)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "pkg/bar.go" {
		t.Errorf("FilesChanged = %v, want [pkg/bar.go]", out.FilesChanged)
	}
}
