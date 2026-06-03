package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// runnerWith returns a Runner backed by a fake exec function that returns
// (output, nil). Used by unit tests that do not need a real subprocess.
func runnerWith(output []byte) *Runner {
	return New().withExec(func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		return output, nil
	})
}

// runnerWithErr returns a Runner whose exec function returns (output, err).
func runnerWithErr(output []byte, execErr error) *Runner {
	return New().withExec(func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		return output, execErr
	})
}

// runnerSleeping returns a Runner whose exec function blocks until ctx is
// cancelled, simulating a hook that never finishes.
func runnerSleeping() *Runner {
	return New().withExec(func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
}

func TestRunner_EmptyCommandAllows(t *testing.T) {
	r := New()
	res, err := r.Run(context.Background(), Config{}, Input{HookPoint: PointPreCapsule})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultAllow {
		t.Fatalf("kind = %s, want allow", res.Kind)
	}
}

func TestRunner_AllowResult(t *testing.T) {
	payload, _ := json.Marshal(Result{Kind: ResultAllow})
	r := runnerWith(payload)
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPreCapsule, CapsuleID: "CAP-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultAllow {
		t.Fatalf("kind = %s, want allow", res.Kind)
	}
}

func TestRunner_DenyResult(t *testing.T) {
	payload, _ := json.Marshal(Result{Kind: ResultDeny, Reason: "not allowed"})
	r := runnerWith(payload)
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPreCapsule})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultDeny {
		t.Fatalf("kind = %s, want deny", res.Kind)
	}
	if res.Reason == "" {
		t.Fatal("deny result must have a reason")
	}
}

func TestRunner_AttachWarningResult(t *testing.T) {
	payload, _ := json.Marshal(Result{Kind: ResultAttachWarning, Warning: "test coverage low"})
	r := runnerWith(payload)
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPostVerify})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultAttachWarning {
		t.Fatalf("kind = %s, want attach_warning", res.Kind)
	}
	if res.Warning != "test coverage low" {
		t.Fatalf("warning = %q, want %q", res.Warning, "test coverage low")
	}
}

func TestRunner_AttachEvidenceResult(t *testing.T) {
	payload, _ := json.Marshal(Result{
		Kind:            ResultAttachEvidence,
		EvidenceSummary: "static analysis passed",
		EvidenceSource:  "hook-lint",
	})
	r := runnerWith(payload)
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPreCapsule})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultAttachEvidence {
		t.Fatalf("kind = %s, want attach_evidence", res.Kind)
	}
	if res.EvidenceSummary != "static analysis passed" {
		t.Fatalf("evidence_summary = %q", res.EvidenceSummary)
	}
}

func TestRunner_NonZeroExitWithJSONReturnsParsedResult(t *testing.T) {
	payload, _ := json.Marshal(Result{Kind: ResultDeny, Reason: "hook rejected"})
	r := runnerWithErr(payload, errors.New("exit status 1"))
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPreCapsule})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultDeny {
		t.Fatalf("kind = %s, want deny", res.Kind)
	}
}

func TestRunner_NonZeroExitWithoutJSONReturnsDeny(t *testing.T) {
	r := runnerWithErr([]byte("something went wrong"), errors.New("exit status 1"))
	res, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5},
		Input{HookPoint: PointPreCapsule})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Kind != ResultDeny {
		t.Fatalf("kind = %s, want deny", res.Kind)
	}
}

func TestRunner_TimeoutReturnsError(t *testing.T) {
	r := runnerSleeping()
	cfg := Config{Command: "mock", TimeoutSeconds: 1}
	start := time.Now()
	_, err := r.Run(context.Background(), cfg, Input{HookPoint: PointPreCapsule})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run should return error on timeout")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Run took too long: %s (timeout should have fired at ~1s)", elapsed)
	}
}

func TestRunner_RealSubprocessTimeoutKillsProcessTree(t *testing.T) {
	command := writeSleepingHookCommand(t)
	start := time.Now()
	_, err := New().Run(context.Background(), Config{Command: command, TimeoutSeconds: 1}, Input{HookPoint: PointPreCapsule})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run should return error on real subprocess timeout")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("Run took too long: %s (timeout should have terminated the subprocess tree promptly)", elapsed)
	}
}

func TestRunner_InputIsPassedToExec(t *testing.T) {
	var capturedInput Input
	r := New().withExec(func(ctx context.Context, _ string, inputJSON []byte) ([]byte, error) {
		if err := json.Unmarshal(inputJSON, &capturedInput); err != nil {
			return nil, err
		}
		out, _ := json.Marshal(Result{Kind: ResultAllow})
		return out, nil
	})
	input := Input{
		HookPoint:     PointPreCapsule,
		CapsuleID:     "CAP-X",
		GoalID:        "G-Y",
		ObligationIDs: []string{"OB-1", "OB-2"},
		WorktreePath:  "/tmp/worktree",
	}
	_, err := r.Run(context.Background(), Config{Command: "mock", TimeoutSeconds: 5}, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if capturedInput.CapsuleID != "CAP-X" {
		t.Errorf("input.CapsuleID = %q, want %q", capturedInput.CapsuleID, "CAP-X")
	}
	if len(capturedInput.ObligationIDs) != 2 {
		t.Errorf("input.ObligationIDs = %v, want 2 elements", capturedInput.ObligationIDs)
	}
}

func writeSleepingHookCommand(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		scriptPath := filepath.Join(dir, "sleep-hook.ps1")
		script := "Start-Sleep -Seconds 5\nWrite-Output '{\"kind\":\"allow\"}'\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			t.Fatalf("write hook script: %v", err)
		}
		return "powershell -NoProfile -File " + scriptPath
	}
	scriptPath := filepath.Join(dir, "sleep-hook.sh")
	script := "#!/bin/sh\nsleep 5\nprintf '{\"kind\":\"allow\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	return scriptPath
}
