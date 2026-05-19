package orcapath

import (
	"path/filepath"
	"testing"
)

func TestCapsuleWorktreePath(t *testing.T) {
	got := CapsuleWorktreePath(".orca", "CAP-1")
	want := filepath.Join(".orca", "capsules", "CAP-1", "worktree")
	if got != want {
		t.Fatalf("CapsuleWorktreePath() = %q, want %q", got, want)
	}
}

func TestTranscriptPath(t *testing.T) {
	got := TranscriptPath(".orca", "CAP-1")
	want := filepath.Join(".orca", "capsules", "CAP-1", "transcript.log")
	if got != want {
		t.Fatalf("TranscriptPath() = %q, want %q", got, want)
	}
}
