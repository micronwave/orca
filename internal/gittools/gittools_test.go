package gittools_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/micronwave/orca/internal/gittools"
)

// initRepo creates a temp git repo with an initial commit and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v: %s", args, err, string(out))
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")
	run("git", "config", "commit.gpgsign", "false")
	// Create an initial commit so HEAD is valid.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "README.md")
	run("git", "commit", "-m", "init")
	return dir
}

func TestStatus_CleanRepo(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	result, err := gittools.Status(ctx, dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !result.Clean {
		t.Errorf("expected clean=true, got false (files: %v)", result.Files)
	}
	if result.Branch == "" {
		t.Error("expected non-empty branch name")
	}
}

func TestStatus_DirtyRepo(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()

	// Add an untracked file.
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := gittools.Status(ctx, dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if result.Clean {
		t.Error("expected clean=false with untracked file")
	}
	var foundUntracked bool
	for _, f := range result.Files {
		if f.Untracked && f.Path == "untracked.txt" {
			foundUntracked = true
		}
	}
	if !foundUntracked {
		t.Errorf("expected untracked file in status, got: %v", result.Files)
	}
}

func TestStatus_StagedFile(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()

	// Stage a new file.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "new.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, string(out))
	}
	result, err := gittools.Status(ctx, dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if result.Clean {
		t.Error("expected clean=false with staged file")
	}
	var foundStaged bool
	for _, f := range result.Files {
		if f.Staged && f.Path == "new.go" {
			foundStaged = true
		}
	}
	if !foundStaged {
		t.Errorf("expected staged file in status, got: %v", result.Files)
	}
}

func TestDiff_StagedAndUnstaged(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()

	// Modify the file and stage.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "README.md")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, string(out))
	}

	// Staged diff.
	stagedResult, err := gittools.Diff(ctx, dir, true, nil)
	if err != nil {
		t.Fatalf("Diff staged: %v", err)
	}
	if len(stagedResult.ChangedFiles) == 0 {
		t.Error("expected changed files in staged diff")
	}

	// Unstaged diff (file committed above, no unstaged changes remain).
	unstaged, err := gittools.Diff(ctx, dir, false, nil)
	if err != nil {
		t.Fatalf("Diff unstaged: %v", err)
	}
	_ = unstaged // may be empty after staging
}

func TestDiff_PathFiltered(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	// No changes to README.md; diff should be empty.
	result, err := gittools.Diff(ctx, dir, false, []string{"README.md"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(result.ChangedFiles) != 0 {
		t.Errorf("expected no changed files, got: %v", result.ChangedFiles)
	}
}

func TestDiff_PathTraversalRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Diff(ctx, dir, false, []string{"../secret"})
	if err == nil {
		t.Error("expected error for path traversal, got nil")
	}
}

func TestLog_SafeRef(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	result, err := gittools.Log(ctx, dir, 5, "HEAD")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Error("expected at least one log entry")
	}
	if result.Entries[0].Hash == "" {
		t.Error("expected non-empty hash in first log entry")
	}
}

func TestLog_UnsafeRefRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Log(ctx, dir, 5, "HEAD; rm -rf /")
	if err == nil {
		t.Error("expected error for unsafe ref, got nil")
	}
}

func TestLog_PathTraversalRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Log(ctx, dir, 5, "../../etc/passwd")
	if err == nil {
		t.Error("expected error for path traversal ref, got nil")
	}
}

func TestShow_ValidRef(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	result, err := gittools.Show(ctx, dir, "HEAD")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if result.Hash == "" {
		t.Error("expected non-empty hash from Show")
	}
}

func TestShow_UnsafeRefRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Show(ctx, dir, "HEAD && cat /etc/passwd")
	if err == nil {
		t.Error("expected error for unsafe ref")
	}
}

func TestBlame_BoundedLineRange(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	result, err := gittools.Blame(ctx, dir, "README.md", 1, 1)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(result.Lines) == 0 {
		t.Error("expected at least one blame line")
	}
	if result.Lines[0].Hash == "" {
		t.Error("expected non-empty hash from Blame")
	}
}

func TestBlame_PathTraversalRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Blame(ctx, dir, "../../etc/passwd", 0, 0)
	if err == nil {
		t.Error("expected error for path traversal in blame")
	}
}

func TestBlame_AbsolutePathRejected(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	_, err := gittools.Blame(ctx, dir, "/etc/passwd", 0, 0)
	if err == nil {
		t.Error("expected error for absolute path in blame")
	}
}

func TestStatus_WorksInLinkedWorktree(t *testing.T) {
	// Create primary repo.
	primaryDir := initRepo(t)
	// Create a linked worktree.
	wtDir := t.TempDir()
	linkDir := filepath.Join(wtDir, "wt")
	cmd := exec.Command("git", "worktree", "add", "--detach", linkDir, "HEAD")
	cmd.Dir = primaryDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create linked worktree: %v: %s", err, string(out))
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", primaryDir, "worktree", "remove", "--force", linkDir).Run()
	})

	ctx := context.Background()
	result, err := gittools.Status(ctx, linkDir)
	if err != nil {
		t.Fatalf("Status in linked worktree: %v", err)
	}
	if !result.Clean {
		t.Errorf("expected clean linked worktree, got files: %v", result.Files)
	}
}
