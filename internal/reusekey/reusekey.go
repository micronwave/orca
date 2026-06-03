package reusekey

import (
	"path/filepath"
	"sort"
	"strings"
)

// NormalizeWorkingDir canonicalizes a working directory path for use in reuse
// keys: resolves to absolute, cleans, normalizes separators to "/", and
// lowercases Windows drive letters.
func NormalizeWorkingDir(workingDir string) string {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		workingDir = "."
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = abs
	}
	workingDir = filepath.Clean(workingDir)
	workingDir = strings.ReplaceAll(workingDir, "\\", "/")
	if len(workingDir) >= 2 && workingDir[1] == ':' {
		workingDir = strings.ToLower(workingDir[:1]) + workingDir[1:]
	}
	return workingDir
}

// ForVerifierGate builds the canonical reuse key for a verifier gate run.
// All parameters must exactly match the values used when the evidence was
// originally stored, or reuse will silently fail.
func ForVerifierGate(evidenceType, command, workingDir string, obligationRefs []string, snapshotID string) string {
	normalized := append([]string(nil), obligationRefs...)
	sort.Strings(normalized)
	parts := []string{
		"type=" + evidenceType,
		"command=" + normalizeCommand(command),
		"scope=" + NormalizeWorkingDir(workingDir),
		"obligations=" + strings.Join(normalized, ","),
		"snapshot=" + strings.TrimSpace(snapshotID),
	}
	return strings.Join(parts, "|")
}

func normalizeCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}
