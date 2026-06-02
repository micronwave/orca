// Package permission implements the centralised policy decision point for
// capsule execution. The runner builds an Enforcer from a capsule's policy
// fields and calls Check before invoking any adapter. Adapters do not decide
// permission semantics. orca.md Phase A §1.
package permission

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Mode is the effective permission level of a capsule run. Higher modes grant
// progressively more capability. ModePrompt is not a capability level; it means
// the run requires human approval before proceeding.
type Mode string

const (
	ModeReadOnly         Mode = "read_only"
	ModeWorkspaceWrite   Mode = "workspace_write"
	ModeDangerFullAccess Mode = "danger_full_access"
	ModePrompt           Mode = "prompt"
)

// modeLevel maps Mode to a numeric capability level for comparison.
// ModePrompt is not in this map; it is handled separately.
var modeLevel = map[Mode]int{
	ModeReadOnly:         0,
	ModeWorkspaceWrite:   1,
	ModeDangerFullAccess: 2,
}

// RuleEffect is the outcome of a matching policy rule.
type RuleEffect string

const (
	EffectAllow RuleEffect = "allow"
	EffectDeny  RuleEffect = "deny"
	EffectAsk   RuleEffect = "ask"
)

// Rule is a single permission rule. It matches when Tool equals the requested
// tool name (empty means all tools) and Pattern is a substring of the request's
// InputSummary (empty means all inputs). Effect is applied on match.
type Rule struct {
	Tool    string
	Pattern string
	Effect  RuleEffect
	Reason  string
}

// Request is a policy decision request from the runner.
type Request struct {
	CapsuleID    string
	ToolName     string
	InputSummary string
	// RequiredMode is the minimum capability level the action needs.
	RequiredMode Mode
	// ActiveMode is the capsule's configured mode (same as the Enforcer's mode
	// when built from a capsule, but included for traceability).
	ActiveMode Mode
	// PathScope is the absolute path of the resource being accessed. Empty
	// means no path-specific check is needed.
	PathScope string
	Reason    string
}

// Decision is the outcome of a policy evaluation.
type Decision struct {
	Effect    RuleEffect
	Reason    string
	AskPrompt string // non-empty only when Effect == EffectAsk
}

// Enforcer evaluates the permission policy for one capsule. Build one from a
// capsule's policy fields via NewEnforcer; reuse it for all checks within a run.
type Enforcer struct {
	mode             Mode
	allowedTools     map[string]bool // nil means no restriction
	forbiddenActions map[string]bool
	allowedPaths     []string // empty means any path is allowed
	forbiddenPaths   []string
	worktreePath     string // canonical absolute worktree path for write-scope checks
	rules            []Rule
}

// NewEnforcer constructs an Enforcer from explicit capsule policy fields.
// allowedTools, forbiddenActions, allowedPaths, and forbiddenPaths come from
// ExecutionCapsule. worktreePath is capsule.Sandbox.WorktreePath (used for
// write-scope checks). rules come from global config defaults.
//
// If mode is empty, it defaults to ModeDangerFullAccess to preserve Phase 1
// behaviour for capsules that predate the PermissionMode field.
func NewEnforcer(
	mode Mode,
	allowedTools, forbiddenActions []string,
	allowedPaths, forbiddenPaths []string,
	worktreePath string,
	rules []Rule,
) *Enforcer {
	if mode == "" {
		mode = ModeDangerFullAccess
	}
	var allowed map[string]bool
	if len(allowedTools) > 0 {
		allowed = make(map[string]bool, len(allowedTools))
		for _, t := range allowedTools {
			allowed[strings.TrimSpace(t)] = true
		}
	}
	var forbidden map[string]bool
	if len(forbiddenActions) > 0 {
		forbidden = make(map[string]bool, len(forbiddenActions))
		for _, a := range forbiddenActions {
			forbidden[strings.TrimSpace(a)] = true
		}
	}
	clean := make([]string, 0, len(allowedPaths))
	for _, p := range allowedPaths {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, filepath.Clean(p))
		}
	}
	cleanForbid := make([]string, 0, len(forbiddenPaths))
	for _, p := range forbiddenPaths {
		if p = strings.TrimSpace(p); p != "" {
			cleanForbid = append(cleanForbid, filepath.Clean(p))
		}
	}
	return &Enforcer{
		mode:             mode,
		allowedTools:     allowed,
		forbiddenActions: forbidden,
		allowedPaths:     clean,
		forbiddenPaths:   cleanForbid,
		worktreePath:     worktreePath,
		rules:            append([]Rule(nil), rules...),
	}
}

// Check evaluates the policy and returns a Decision. Evaluation order:
//
//  1. ModePrompt: always returns Ask (requires human approval).
//  2. Mode capability: if ActiveMode cannot satisfy RequiredMode, deny.
//  3. Tool allowlist: if AllowedTools is non-empty and ToolName is absent, deny.
//  4. Forbidden actions: if ToolName is in ForbiddenActions, deny.
//  5. Path scope: if PathScope is set and RequiredMode ≥ workspace_write,
//     verify PathScope is within the worktree and not in ForbiddenPaths.
//  6. ForbiddenPaths: checked even when RequiredMode < workspace_write.
//  7. Config rules: deny wins over allow when both match the same request.
//  8. Default: allow.
func (e *Enforcer) Check(req Request) Decision {
	// 1. Prompt mode: always block for human approval.
	if e.mode == ModePrompt {
		return Decision{
			Effect:    EffectAsk,
			Reason:    "capsule is in prompt mode — human approval required before execution",
			AskPrompt: fmt.Sprintf("capsule %s requires approval: %s", req.CapsuleID, req.Reason),
		}
	}

	// 2. Mode capability check.
	if req.RequiredMode != "" && req.RequiredMode != ModePrompt {
		if !e.modePermits(req.RequiredMode) {
			return Decision{
				Effect: EffectDeny,
				Reason: fmt.Sprintf("mode %q does not permit operation requiring %q", e.mode, req.RequiredMode),
			}
		}
	}

	// 3. Tool allowlist.
	if e.allowedTools != nil {
		tool := strings.TrimSpace(req.ToolName)
		if tool != "" && !e.allowedTools[tool] {
			return Decision{
				Effect: EffectDeny,
				Reason: fmt.Sprintf("tool %q is not in AllowedTools", req.ToolName),
			}
		}
	}

	// 4. Forbidden actions.
	if e.forbiddenActions != nil {
		tool := strings.TrimSpace(req.ToolName)
		if tool != "" && e.forbiddenActions[tool] {
			return Decision{
				Effect: EffectDeny,
				Reason: fmt.Sprintf("tool %q is in ForbiddenActions", req.ToolName),
			}
		}
	}

	// 5 & 6. Path checks.
	if req.PathScope != "" {
		if d := e.checkPath(req.PathScope, req.RequiredMode); d != nil {
			return *d
		}
	}

	// 7. Config rules.
	var firstDeny, firstAllow *Rule
	for i := range e.rules {
		r := &e.rules[i]
		if !ruleMatches(r, req) {
			continue
		}
		switch r.Effect {
		case EffectDeny:
			if firstDeny == nil {
				firstDeny = r
			}
		case EffectAllow:
			if firstAllow == nil {
				firstAllow = r
			}
		case EffectAsk:
			return Decision{
				Effect:    EffectAsk,
				Reason:    r.Reason,
				AskPrompt: fmt.Sprintf("rule requires approval for tool %q: %s", req.ToolName, r.Reason),
			}
		}
	}
	if firstDeny != nil {
		return Decision{Effect: EffectDeny, Reason: firstDeny.Reason}
	}
	if firstAllow != nil {
		return Decision{Effect: EffectAllow, Reason: firstAllow.Reason}
	}

	// 8. Default allow.
	return Decision{Effect: EffectAllow, Reason: "default allow"}
}

// modePermits reports whether the enforcer's active mode satisfies the required mode.
func (e *Enforcer) modePermits(required Mode) bool {
	myLevel, myOK := modeLevel[e.mode]
	reqLevel, reqOK := modeLevel[required]
	if !myOK || !reqOK {
		return false
	}
	return myLevel >= reqLevel
}

// checkPath validates path against write-scope and forbidden-path rules.
// Returns a non-nil *Decision only when the path fails a check.
func (e *Enforcer) checkPath(path string, required Mode) *Decision {
	// Forbidden paths always win, regardless of required mode.
	for _, fp := range e.forbiddenPaths {
		if pathMatchesPrefix(path, fp) {
			return &Decision{
				Effect: EffectDeny,
				Reason: fmt.Sprintf("path %q is in ForbiddenPaths (matches %q)", path, fp),
			}
		}
	}

	// Write-scope check: if the operation requires workspace_write or higher,
	// verify the path is within the capsule worktree.
	reqLevel, reqOK := modeLevel[required]
	writeLevel := modeLevel[ModeWorkspaceWrite]
	if reqOK && reqLevel >= writeLevel && e.worktreePath != "" {
		if !IsWithinWorktree(path, e.worktreePath) {
			return &Decision{
				Effect: EffectDeny,
				Reason: fmt.Sprintf("path %q is outside the capsule worktree %q", path, e.worktreePath),
			}
		}
	}

	return nil
}

// ruleMatches returns true when the rule applies to req.
func ruleMatches(r *Rule, req Request) bool {
	if r.Tool != "" && r.Tool != req.ToolName {
		return false
	}
	if r.Pattern != "" && !strings.Contains(req.InputSummary, r.Pattern) {
		return false
	}
	return true
}

// IsWithinWorktree reports whether path is within (or equal to) worktree.
// Both paths are canonicalized before comparison to handle symlinks and
// Windows drive-letter case differences.
func IsWithinWorktree(path, worktree string) bool {
	cp := CanonicalPath(path)
	cw := CanonicalPath(worktree)
	if cw == "" {
		return false
	}
	return cp == cw || strings.HasPrefix(cp, cw+string(filepath.Separator))
}

// CanonicalPath returns the canonical absolute path with symlinks resolved
// and Windows drive letter uppercased.
func CanonicalPath(path string) string {
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Path may not exist yet; use cleaned path.
		resolved = clean
	}
	return normalizeDriveLetter(resolved)
}

// normalizeDriveLetter uppercases the drive letter in a Windows-style absolute
// path (e.g. c:\foo → C:\foo). On non-Windows paths this is a no-op.
func normalizeDriveLetter(path string) string {
	if len(path) >= 2 && path[1] == ':' && path[0] >= 'a' && path[0] <= 'z' {
		return string(path[0]-32) + path[1:]
	}
	return path
}

// pathMatchesPrefix returns true when path equals prefix or has prefix as a
// directory prefix (i.e. path is under the prefix directory tree).
func pathMatchesPrefix(path, prefix string) bool {
	cp := CanonicalPath(path)
	cf := CanonicalPath(prefix)
	return cp == cf || strings.HasPrefix(cp, cf+string(filepath.Separator))
}
