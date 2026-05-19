// Package orcapath owns deterministic local filesystem path policy that is not
// part of artifact persistence.
package orcapath

import "path/filepath"

// CapsuleWorktreePath returns the deterministic worktree path for a capsule.
func CapsuleWorktreePath(orcaDir, capsuleID string) string {
	return filepath.Join(orcaDir, "capsules", capsuleID, "worktree")
}

// TranscriptPath returns the deterministic transcript log path for a capsule.
func TranscriptPath(orcaDir, capsuleID string) string {
	return filepath.Join(orcaDir, "capsules", capsuleID, "transcript.log")
}
