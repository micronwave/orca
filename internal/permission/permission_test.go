package permission

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── basic mode capability tests ───────────────────────────────────────────────

func TestCheckReadOnlyDeniesWorkspaceWrite(t *testing.T) {
	e := NewEnforcer(ModeReadOnly, nil, nil, nil, nil, "", nil)
	req := Request{
		CapsuleID:    "CAP-1",
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		ActiveMode:   ModeReadOnly,
		Reason:       "execute agent",
	}
	d := e.Check(req)
	if d.Effect != EffectDeny {
		t.Fatalf("expected Deny, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckReadOnlyDeniesFullAccess(t *testing.T) {
	e := NewEnforcer(ModeReadOnly, nil, nil, nil, nil, "", nil)
	req := Request{
		CapsuleID:    "CAP-1",
		ToolName:     "codex",
		RequiredMode: ModeDangerFullAccess,
		ActiveMode:   ModeReadOnly,
	}
	d := e.Check(req)
	if d.Effect != EffectDeny {
		t.Fatalf("expected Deny, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckWorkspaceWriteAllowsWorkspaceWrite(t *testing.T) {
	e := NewEnforcer(ModeWorkspaceWrite, nil, nil, nil, nil, "", nil)
	req := Request{
		CapsuleID:    "CAP-1",
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		ActiveMode:   ModeWorkspaceWrite,
	}
	d := e.Check(req)
	if d.Effect != EffectAllow {
		t.Fatalf("expected Allow, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckWorkspaceWriteDeniesFullAccess(t *testing.T) {
	e := NewEnforcer(ModeWorkspaceWrite, nil, nil, nil, nil, "", nil)
	req := Request{
		CapsuleID:    "CAP-1",
		ToolName:     "codex",
		RequiredMode: ModeDangerFullAccess,
		ActiveMode:   ModeWorkspaceWrite,
	}
	d := e.Check(req)
	if d.Effect != EffectDeny {
		t.Fatalf("expected Deny, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckDangerFullAccessAllowsAll(t *testing.T) {
	e := NewEnforcer(ModeDangerFullAccess, nil, nil, nil, nil, "", nil)
	for _, req := range []Mode{ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess} {
		d := e.Check(Request{ToolName: "codex", RequiredMode: req})
		if d.Effect != EffectAllow {
			t.Fatalf("mode %s: expected Allow for RequiredMode=%s, got %s: %s", ModeDangerFullAccess, req, d.Effect, d.Reason)
		}
	}
}

// ── prompt mode ───────────────────────────────────────────────────────────────

func TestCheckPromptModeReturnsAsk(t *testing.T) {
	e := NewEnforcer(ModePrompt, nil, nil, nil, nil, "", nil)
	req := Request{
		CapsuleID:    "CAP-1",
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		Reason:       "execute agent",
	}
	d := e.Check(req)
	if d.Effect != EffectAsk {
		t.Fatalf("expected Ask in prompt mode, got %s", d.Effect)
	}
	if d.AskPrompt == "" {
		t.Fatal("AskPrompt should be non-empty in prompt mode")
	}
}

func TestCheckPromptModeBlocksNonInteractiveRun(t *testing.T) {
	// In non-interactive mode the caller treats EffectAsk as a blocked decision.
	e := NewEnforcer(ModePrompt, nil, nil, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "claude", RequiredMode: ModeReadOnly})
	if d.Effect != EffectAsk {
		t.Fatalf("prompt mode must block non-interactive runs (got %s)", d.Effect)
	}
}

// ── tool allowlist / denylist ─────────────────────────────────────────────────

func TestCheckDeniesToolNotInAllowlist(t *testing.T) {
	e := NewEnforcer(ModeDangerFullAccess, []string{"codex"}, nil, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "claude", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("expected Deny for tool not in AllowedTools, got %s", d.Effect)
	}
}

func TestCheckAllowsToolInAllowlist(t *testing.T) {
	e := NewEnforcer(ModeDangerFullAccess, []string{"codex"}, nil, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "codex", RequiredMode: ModeReadOnly})
	if d.Effect != EffectAllow {
		t.Fatalf("expected Allow for allowed tool, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckDeniesToolInForbiddenActions(t *testing.T) {
	e := NewEnforcer(ModeDangerFullAccess, nil, []string{"rm"}, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "rm", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("expected Deny for tool in ForbiddenActions, got %s", d.Effect)
	}
}

func TestCheckForbiddenActionWinsOverAllowedTool(t *testing.T) {
	// A tool that appears in both lists: forbidden wins.
	e := NewEnforcer(ModeDangerFullAccess, []string{"bash"}, []string{"bash"}, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "bash", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("forbidden should win over allowed, got %s", d.Effect)
	}
}

// ── rule evaluation ───────────────────────────────────────────────────────────

func TestCheckDenyRuleWinsOverAllowRule(t *testing.T) {
	rules := []Rule{
		{Tool: "bash", Effect: EffectAllow, Reason: "allow bash"},
		{Tool: "bash", Effect: EffectDeny, Reason: "deny bash"},
	}
	e := NewEnforcer(ModeDangerFullAccess, nil, nil, nil, nil, "", rules)
	d := e.Check(Request{ToolName: "bash", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("deny rule must win over allow rule, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckAllowRuleAppliedWhenNoDeny(t *testing.T) {
	rules := []Rule{
		{Tool: "codex", Effect: EffectAllow, Reason: "explicit allow"},
	}
	e := NewEnforcer(ModeDangerFullAccess, nil, nil, nil, nil, "", rules)
	d := e.Check(Request{ToolName: "codex", RequiredMode: ModeReadOnly})
	if d.Effect != EffectAllow || d.Reason != "explicit allow" {
		t.Fatalf("expected allow rule, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckRulePatternMatchesInputSummary(t *testing.T) {
	rules := []Rule{
		{Tool: "bash", Pattern: "rm -rf", Effect: EffectDeny, Reason: "no destructive"},
	}
	e := NewEnforcer(ModeDangerFullAccess, nil, nil, nil, nil, "", rules)
	// Pattern matches: deny.
	d := e.Check(Request{ToolName: "bash", InputSummary: "running rm -rf /tmp", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("matching pattern should deny, got %s", d.Effect)
	}
	// Pattern doesn't match: allow.
	d2 := e.Check(Request{ToolName: "bash", InputSummary: "echo hello", RequiredMode: ModeReadOnly})
	if d2.Effect != EffectAllow {
		t.Fatalf("non-matching pattern should allow, got %s", d2.Effect)
	}
}

func TestCheckEmptyToolRuleMatchesAnyTool(t *testing.T) {
	rules := []Rule{
		{Tool: "", Effect: EffectDeny, Reason: "deny all tools"},
	}
	e := NewEnforcer(ModeDangerFullAccess, nil, nil, nil, nil, "", rules)
	d := e.Check(Request{ToolName: "anything", RequiredMode: ModeReadOnly})
	if d.Effect != EffectDeny {
		t.Fatalf("empty tool rule should match any tool, got %s", d.Effect)
	}
}

// ── path scope tests ──────────────────────────────────────────────────────────

func TestCheckForbiddenPathDeniesEvenWithAllowedTool(t *testing.T) {
	worktree := t.TempDir()
	forbidden := filepath.Join(worktree, "secrets")
	e := NewEnforcer(ModeDangerFullAccess, []string{"codex"}, nil, nil, []string{forbidden}, worktree, nil)
	d := e.Check(Request{
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		PathScope:    forbidden,
	})
	if d.Effect != EffectDeny {
		t.Fatalf("forbidden path must deny even for allowed tool, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckWorkspaceWritePermitsPathInsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	inside := filepath.Join(worktree, "pkg", "main.go")
	e := NewEnforcer(ModeWorkspaceWrite, nil, nil, nil, nil, worktree, nil)
	d := e.Check(Request{
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		PathScope:    inside,
	})
	if d.Effect != EffectAllow {
		t.Fatalf("path inside worktree should be allowed, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckWorkspaceWriteDeniesPathOutsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "sensitive.go")
	e := NewEnforcer(ModeWorkspaceWrite, nil, nil, nil, nil, worktree, nil)
	d := e.Check(Request{
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		PathScope:    outside,
	})
	if d.Effect != EffectDeny {
		t.Fatalf("path outside worktree should be denied, got %s: %s", d.Effect, d.Reason)
	}
}

func TestCheckReadOnlyDoesNotEnforceWriteScopeForPathScope(t *testing.T) {
	// When RequiredMode is read_only, write-scope check is not applied.
	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	// But mode is also read_only so mode check would catch workspace_write attempt first.
	// This test specifically checks: read-only mode + read-only required + outside path = allow
	// (because read-only ops don't restrict path scope).
	e := NewEnforcer(ModeWorkspaceWrite, nil, nil, nil, nil, worktree, nil)
	d := e.Check(Request{
		ToolName:     "codex",
		RequiredMode: ModeReadOnly,
		PathScope:    outside,
	})
	// RequiredMode is read_only, so no write-scope check applies.
	if d.Effect != EffectAllow {
		t.Fatalf("read-only operation on outside path should be allowed, got %s: %s", d.Effect, d.Reason)
	}
}

// ── Windows path canonicalization ─────────────────────────────────────────────

func TestNormalizeDriveLetter(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`c:\orca\worktree`, `C:\orca\worktree`},
		{`C:\orca\worktree`, `C:\orca\worktree`},
		{`d:\data`, `D:\data`},
		{`/unix/path`, `/unix/path`},
		{`relative\path`, `relative\path`},
		{``, ``},
	}
	for _, tc := range cases {
		got := normalizeDriveLetter(tc.input)
		if got != tc.want {
			t.Errorf("normalizeDriveLetter(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestIsWithinWorktreeWindowsDriveLetterCasing(t *testing.T) {
	// Simulate Windows-style paths. On Linux these are just string comparisons
	// after normalizeDriveLetter, which is sufficient for the casing logic.
	if runtime.GOOS != "windows" {
		// On non-Windows, test the normalization logic directly.
		// CanonicalPath calls EvalSymlinks which requires real paths; test the
		// normalization function that feeds into it.
		upper := normalizeDriveLetter(`C:\orca\worktree`)
		lower := normalizeDriveLetter(`c:\orca\worktree`)
		if upper != lower {
			t.Fatalf("normalizeDriveLetter: %q != %q — drive letter case must be normalised", upper, lower)
		}
		return
	}
	// On Windows: verify the full IsWithinWorktree path with real filesystem.
	worktree := t.TempDir()
	// Produce a lowercase-drive version of the worktree path.
	lowered := strings.ToLower(string(worktree[0])) + worktree[1:]
	inside := filepath.Join(lowered, "file.go")
	if !IsWithinWorktree(inside, worktree) {
		t.Fatalf("IsWithinWorktree(%q, %q) = false, want true", inside, worktree)
	}
}

func TestIsWithinWorktreePathTraversal(t *testing.T) {
	// Ensure .. traversal cannot escape the worktree.
	worktree := t.TempDir()
	// filepath.Clean should resolve .. before EvalSymlinks.
	escape := filepath.Join(worktree, "..", "outside")
	if IsWithinWorktree(escape, worktree) {
		t.Fatalf("path traversal %q should not be within worktree %q", escape, worktree)
	}
}

func TestIsWithinWorktreeExactMatch(t *testing.T) {
	worktree := t.TempDir()
	if !IsWithinWorktree(worktree, worktree) {
		t.Fatalf("worktree path should be within itself")
	}
}

func TestIsWithinWorktreeEmptyWorktree(t *testing.T) {
	if IsWithinWorktree("/some/path", "") {
		t.Fatal("empty worktree should never match")
	}
}

// ── empty mode defaults to danger_full_access ─────────────────────────────────

func TestEmptyModeDefaultsToDangerFullAccess(t *testing.T) {
	e := NewEnforcer("", nil, nil, nil, nil, "", nil)
	d := e.Check(Request{ToolName: "codex", RequiredMode: ModeDangerFullAccess})
	if d.Effect != EffectAllow {
		t.Fatalf("empty mode should default to danger_full_access, got %s: %s", d.Effect, d.Reason)
	}
}

// ── forbidden path wins over allowed tool (combined test) ─────────────────────

func TestForbiddenPathWinsOverAllowedToolWithRule(t *testing.T) {
	worktree := t.TempDir()
	forbidden := filepath.Join(worktree, "internal", "secrets.go")
	rules := []Rule{
		{Tool: "codex", Effect: EffectAllow, Reason: "codex always allowed"},
	}
	e := NewEnforcer(ModeDangerFullAccess, []string{"codex"}, nil, nil, []string{forbidden}, worktree, rules)
	d := e.Check(Request{
		ToolName:     "codex",
		RequiredMode: ModeWorkspaceWrite,
		PathScope:    forbidden,
	})
	if d.Effect != EffectDeny {
		t.Fatalf("forbidden path must win over allowed tool and allow rule, got %s: %s", d.Effect, d.Reason)
	}
}
