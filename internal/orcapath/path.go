// Package orcapath owns deterministic local filesystem path policy that is not
// part of artifact persistence.
package orcapath

import "path/filepath"

// CapsuleWorktreePath returns the deterministic worktree path for a capsule.
func CapsuleWorktreePath(orcaDir, capsuleID string) string {
	return filepath.Join(orcaDir, "capsules", capsuleID, "worktree")
}
