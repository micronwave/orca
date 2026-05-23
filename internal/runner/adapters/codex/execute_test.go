package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

// TestMain doubles as a stub codex process when ORCA_FAKE_CODEX is set.
// The adapter invokes os.Executable() (the test binary) directly; the subprocess
// re-enters via TestMain, acts as the stub, and exits before m.Run() is called.
func TestMain(m *testing.M) {
	if mode := os.Getenv("ORCA_FAKE_CODEX"); mode != "" {
		os.Exit(fakeCodexMain(mode))
	}
	os.Exit(m.Run())
}

// fakeCodexMain acts as a stub codex process. Returns an exit code.
func fakeCodexMain(mode string) int {
	switch mode {
	case "valid":
		path := argAfter(os.Args, "-o")
		if path == "" {
			fmt.Fprintln(os.Stderr, "fake codex: missing -o flag")
			return 2
		}
		out := schema.AgentSidecarOutput{
			ObligationsAddressed: []string{"OB-42"},
			FilesChanged:         []string{"pkg/foo.go"},
			CommandsRun:          []string{"go test ./..."},
			Summary:              "done",
		}
		data, err := json.Marshal(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake codex: marshal:", err)
			return 2
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake codex: write sidecar:", err)
			return 2
		}
	case "no_sidecar":
		// exit 0 without writing sidecar.json
	case "invalid_json":
		if path := argAfter(os.Args, "-o"); path != "" {
			_ = os.WriteFile(path, []byte("not valid json {{"), 0o644)
		}
	case "empty_sidecar":
		if path := argAfter(os.Args, "-o"); path != "" {
			data, _ := json.Marshal(schema.AgentSidecarOutput{})
			_ = os.WriteFile(path, data, 0o644)
		}
	default:
		fmt.Fprintln(os.Stderr, "fake codex: unknown mode:", mode)
		return 2
	}
	return 0
}

// argAfter returns the value following flag in args, or "".
func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func testBinaryPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

func codexTestCapsule(worktree string) *schema.ExecutionCapsule {
	return &schema.ExecutionCapsule{
		CapsuleID:     "CAP-EXEC-1",
		Sandbox:       schema.CapsuleSandbox{WorktreePath: worktree},
		ObligationIDs: []string{"OB-42"},
		Budget:        schema.CapsuleBudget{MaxWallTimeSeconds: 30},
	}
}

func codexTestProjection() *schema.ContextProjection {
	return &schema.ContextProjection{
		ContextProjectionID: "CTX-1",
		Role:                schema.ProjectionRoleExecutor,
		TokenBudget:         4096,
	}
}

func TestExecute_ValidSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CODEX", "valid")
	orcaDir := t.TempDir()
	worktree := t.TempDir()
	adapter := New(orcaDir, testBinaryPath(t))

	out, err := adapter.Execute(context.Background(), codexTestCapsule(worktree), codexTestProjection())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.ObligationsAddressed) != 1 || out.ObligationsAddressed[0] != "OB-42" {
		t.Errorf("ObligationsAddressed = %v, want [OB-42]", out.ObligationsAddressed)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "pkg/foo.go" {
		t.Errorf("FilesChanged = %v, want [pkg/foo.go]", out.FilesChanged)
	}
	if out.WallTimeSeconds <= 0 {
		t.Errorf("WallTimeSeconds = %v, want > 0", out.WallTimeSeconds)
	}
	// Briefing and schema files must be created before the command runs.
	capsuleDir := filepath.Join(orcaDir, "capsules", "CAP-EXEC-1")
	for _, name := range []string{"executor_briefing.md", "sidecar_schema.json"} {
		if _, err := os.Stat(filepath.Join(capsuleDir, name)); err != nil {
			t.Errorf("missing pre-execution file %s: %v", name, err)
		}
	}
}

func TestExecute_NoSidecar_ErrNoSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CODEX", "no_sidecar")
	adapter := New(t.TempDir(), testBinaryPath(t))
	_, err := adapter.Execute(context.Background(), codexTestCapsule(t.TempDir()), codexTestProjection())
	if !errors.Is(err, runner.ErrNoSidecar) {
		t.Errorf("err = %v, want ErrNoSidecar", err)
	}
}

func TestExecute_InvalidSidecarJSON_ErrInvalidSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CODEX", "invalid_json")
	adapter := New(t.TempDir(), testBinaryPath(t))
	_, err := adapter.Execute(context.Background(), codexTestCapsule(t.TempDir()), codexTestProjection())
	if !errors.Is(err, runner.ErrInvalidSidecar) {
		t.Errorf("err = %v, want ErrInvalidSidecar", err)
	}
}

func TestExecute_EmptySidecar_ErrInvalidSidecar(t *testing.T) {
	t.Setenv("ORCA_FAKE_CODEX", "empty_sidecar")
	adapter := New(t.TempDir(), testBinaryPath(t))
	_, err := adapter.Execute(context.Background(), codexTestCapsule(t.TempDir()), codexTestProjection())
	if !errors.Is(err, runner.ErrInvalidSidecar) {
		t.Errorf("err = %v, want ErrInvalidSidecar", err)
	}
}
