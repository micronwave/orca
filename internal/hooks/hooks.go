// Package hooks implements lifecycle hooks that fire at capsule execution
// boundaries. Hook executables are user-configured shell commands. They receive
// execution context as JSON on stdin and must write a JSON Result to stdout.
// Hooks may not mutate Orca artifacts directly — they return a Result that the
// runtime stores as evidence or a blocked decision. orca.md Phase D §9.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Point identifies when in the capsule lifecycle a hook fires.
type Point string

const (
	PointPreCapsule Point = "pre_capsule"
	PointPostVerify Point = "post_verify"
	// Defined but not yet wired — reserved for future hook points:
	PointPostCapsule  Point = "post_capsule"
	PointPreVerify    Point = "pre_verify"
	PointPreMergeGate Point = "pre_merge_gate"
)

// ResultKind is the structured outcome a hook executable returns.
type ResultKind string

const (
	ResultAllow          ResultKind = "allow"
	ResultDeny           ResultKind = "deny"
	ResultAsk            ResultKind = "ask"
	ResultAttachEvidence ResultKind = "attach_evidence"
	ResultAttachWarning  ResultKind = "attach_warning"
)

// Result is the parsed output of a hook executable. Hooks write this as JSON
// to stdout. Unknown fields are ignored.
type Result struct {
	Kind ResultKind `json:"kind"`
	// Reason explains a deny or ask result.
	Reason string `json:"reason,omitempty"`
	// Prompt is the human-readable prompt for an ask result.
	Prompt string `json:"prompt,omitempty"`
	// EvidenceSummary and EvidenceSource describe attached evidence.
	EvidenceSummary string `json:"evidence_summary,omitempty"`
	EvidenceSource  string `json:"evidence_source,omitempty"`
	// Warning is the message for an attach_warning result.
	Warning string `json:"warning,omitempty"`
}

// Config configures one lifecycle hook. A nil pointer means no hook is
// configured for that point.
type Config struct {
	// Command is the executable path and optional fixed arguments, space-separated.
	// The executable must not have spaces in its path; use a wrapper script if needed.
	Command        string
	TimeoutSeconds int
}

// Input is the execution context passed to the hook executable via stdin.
type Input struct {
	HookPoint     Point    `json:"hook_point"`
	CapsuleID     string   `json:"capsule_id"`
	GoalID        string   `json:"goal_id"`
	ObligationIDs []string `json:"obligation_ids"`
	WorktreePath  string   `json:"worktree_path,omitempty"`
}

// execFn is the subprocess execution contract used internally by Runner.
// The default implementation runs a real OS subprocess. Tests inject a mock.
type execFn func(ctx context.Context, command string, inputJSON []byte) ([]byte, error)

// Runner executes lifecycle hooks. Use New to construct one.
type Runner struct {
	exec  execFn
	nowFn func() time.Time
}

// New returns a hook Runner that executes real OS subprocesses.
func New() *Runner {
	return &Runner{exec: defaultExec, nowFn: time.Now}
}

// Run executes the hook command and returns the parsed Result.
//
// The hook executable receives a JSON-encoded Input on stdin. It must write a
// JSON-encoded Result to stdout. Return behaviour:
//
//   - Empty command: returns ResultAllow immediately (no-op hook).
//   - Context/timeout exceeded: returns an error; caller records an infra failure.
//   - Non-zero exit with no parseable JSON: returns ResultDeny.
//   - Non-zero exit with parseable JSON: returns the parsed Result.
//   - Zero exit with parseable JSON: returns the parsed Result.
//   - Zero exit with no parseable JSON: returns an error.
func (r *Runner) Run(ctx context.Context, cfg Config, input Input) (Result, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return Result{Kind: ResultAllow}, nil
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return Result{}, fmt.Errorf("hooks: marshal input for %s: %w", cfg.Command, err)
	}

	out, runErr := r.exec(hookCtx, cfg.Command, inputJSON)
	if hookCtx.Err() != nil {
		return Result{}, fmt.Errorf("hooks: %s timed out after %s: %w", cfg.Command, timeout, hookCtx.Err())
	}

	// Try to parse output regardless of exit code — a hook may return a deny
	// result via JSON even when it exits non-zero.
	if len(out) > 0 {
		var res Result
		if jsonErr := json.Unmarshal(out, &res); jsonErr == nil && res.Kind != "" {
			return res, nil
		}
	}

	if runErr != nil {
		reason := runErr.Error()
		if len(out) > 0 {
			reason = strings.TrimSpace(string(out))
		}
		return Result{Kind: ResultDeny, Reason: fmt.Sprintf("hook %s exited non-zero: %s", cfg.Command, reason)}, nil
	}

	return Result{}, fmt.Errorf("hooks: %s produced no parseable JSON output", cfg.Command)
}

// defaultExec runs command as a subprocess, passing inputJSON via stdin.
func defaultExec(ctx context.Context, command string, inputJSON []byte) ([]byte, error) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return nil, fmt.Errorf("hooks: empty command")
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Stdin = bytes.NewReader(inputJSON)
	return cmd.Output()
}

// withExec returns a shallow copy of r with the exec function replaced.
// Used exclusively by tests to inject deterministic behaviour without
// spawning OS subprocesses.
func (r *Runner) withExec(fn execFn) *Runner {
	return &Runner{exec: fn, nowFn: r.nowFn}
}
