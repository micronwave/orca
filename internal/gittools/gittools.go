// Package gittools provides read-only git inspection helpers for use by Orca
// components. All operations run git with explicit argument construction — never
// shell strings — so they are safe to call from the verifier, projector, and MCP
// layer. Write operations (commit, push, branch creation, reset) are intentionally
// absent. orca.md Phase B §3.
package gittools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// reUnsafeRef matches characters that are not allowed in a safe git ref.
// Permits alphanumeric, hyphen, underscore, dot, slash, colon, and @.
var reUnsafeRef = regexp.MustCompile(`[^a-zA-Z0-9_./:@^~-]`)

// validateRef rejects refs with unsafe characters or path traversal.
func validateRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("gittools: ref is required")
	}
	if reUnsafeRef.MatchString(ref) {
		return fmt.Errorf("gittools: ref %q contains unsafe characters", ref)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("gittools: ref %q contains path traversal sequence", ref)
	}
	return nil
}

// validateRelPath rejects absolute paths and path traversal sequences.
func validateRelPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("gittools: path is required")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("gittools: path %q must be relative", path)
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("gittools: path %q contains path traversal sequence", path)
	}
	return nil
}

// FileStatus is the status of one tracked or untracked file in the working tree.
type FileStatus struct {
	Path       string
	Staged     bool
	Unstaged   bool
	Untracked  bool
	StatusCode string // two-char git status code, e.g. " M", "A ", "??"
}

// StatusResult is the structured result of a git status call.
type StatusResult struct {
	Branch         string
	TrackingBranch string
	Files          []FileStatus
	Clean          bool
	Raw            string
}

// DiffResult is the structured result of a git diff call.
type DiffResult struct {
	ChangedFiles []string
	DiffText     string
}

// LogEntry is one commit in a log result.
type LogEntry struct {
	Hash    string
	Author  string
	Date    string
	Subject string
}

// LogResult is the structured result of a git log call.
type LogResult struct {
	Entries []LogEntry
	Raw     string
}

// ShowResult is the structured result of a git show call.
type ShowResult struct {
	Hash    string
	Author  string
	Date    string
	Subject string
	Body    string
	Raw     string
}

// BlameLine is one annotated source line from git blame.
type BlameLine struct {
	LineNum int
	Hash    string
	Author  string
	Content string
}

// BlameResult is the structured result of a git blame call.
type BlameResult struct {
	FilePath string
	Lines    []BlameLine
	Raw      string
}

// Status runs git status --porcelain=v1 -b in workDir and returns structured
// output. Works in both primary repos and linked worktrees.
func Status(ctx context.Context, workDir string) (StatusResult, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-b")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	raw := string(out)
	if err != nil {
		return StatusResult{Raw: raw}, fmt.Errorf("gittools: git status in %s: %w: %s", workDir, err, strings.TrimSpace(raw))
	}
	return parseStatus(raw), nil
}

func parseStatus(raw string) StatusResult {
	result := StatusResult{Raw: raw, Clean: true}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 2 {
			continue
		}
		if branch, ok := strings.CutPrefix(line, "## "); ok {
			// strip trailing annotations like "[ahead 1]"
			if idx := strings.Index(branch, " ["); idx >= 0 {
				branch = branch[:idx]
			}
			parts := strings.SplitN(branch, "...", 2)
			result.Branch = strings.TrimSpace(parts[0])
			if len(parts) > 1 {
				// tracking branch may have " [ahead N]" suffix
				tracking := strings.TrimSpace(parts[1])
				if sp := strings.Index(tracking, " "); sp >= 0 {
					tracking = tracking[:sp]
				}
				result.TrackingBranch = tracking
			}
			// Handle "No commits yet on main"
			result.Branch = strings.TrimPrefix(result.Branch, "No commits yet on ")
			continue
		}
		if len(line) < 3 {
			continue
		}
		code := line[:2]
		path := strings.TrimSpace(line[3:])
		// Renamed files show as "old -> new"; use the new path.
		if strings.Contains(path, " -> ") {
			parts := strings.SplitN(path, " -> ", 2)
			path = strings.TrimSpace(parts[1])
		}
		fs := FileStatus{Path: path, StatusCode: code}
		if code == "??" {
			fs.Untracked = true
		} else {
			if code[0] != ' ' && code[0] != '!' {
				fs.Staged = true
			}
			if code[1] != ' ' && code[1] != '!' {
				fs.Unstaged = true
			}
		}
		result.Files = append(result.Files, fs)
		result.Clean = false
	}
	return result
}

// Diff runs git diff in workDir and returns structured output.
// When staged is true, uses --cached (staged changes only).
// paths filters output to specific files; empty means all.
// Path elements must be relative and contain no ".." sequences.
func Diff(ctx context.Context, workDir string, staged bool, paths []string) (DiffResult, error) {
	for _, p := range paths {
		if err := validateRelPath(p); err != nil {
			return DiffResult{}, err
		}
	}
	args := []string{"diff", "--no-color"}
	if staged {
		args = append(args, "--cached")
	}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	raw := string(out)
	if err != nil {
		return DiffResult{DiffText: raw}, fmt.Errorf("gittools: git diff in %s: %w: %s", workDir, err, strings.TrimSpace(raw))
	}
	return DiffResult{
		ChangedFiles: parseDiffFiles(raw),
		DiffText:     raw,
	}, nil
}

func parseDiffFiles(diff string) []string {
	seen := make(map[string]bool)
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimRight(line, "\r")
		if f, ok := strings.CutPrefix(line, "+++ b/"); ok {
			f = strings.TrimSpace(f)
			if f != "" && !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	return files
}

// Log runs git log in workDir and returns up to maxCount entries for ref.
// ref must pass validateRef; empty string uses HEAD.
// maxCount <= 0 defaults to 20. Works in linked worktrees.
func Log(ctx context.Context, workDir string, maxCount int, ref string) (LogResult, error) {
	if ref != "" {
		if err := validateRef(ref); err != nil {
			return LogResult{}, err
		}
	}
	if maxCount <= 0 {
		maxCount = 20
	}
	// Use a multi-line format: each commit prints 4 lines + blank separator.
	// This avoids NUL bytes in format strings which are invalid on Windows.
	args := []string{
		"log",
		fmt.Sprintf("--max-count=%d", maxCount),
		"--format=%H%n%an%n%ai%n%s%n",
	}
	if ref != "" {
		args = append(args, ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	raw := string(out)
	if err != nil {
		return LogResult{Raw: raw}, fmt.Errorf("gittools: git log in %s: %w: %s", workDir, err, strings.TrimSpace(raw))
	}
	return parseLog(raw), nil
}

func parseLog(raw string) LogResult {
	result := LogResult{Raw: raw}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	// Each commit: hash, author, date, subject, blank line.
	for i := 0; i+3 < len(lines); i += 5 {
		hash := strings.TrimSpace(lines[i])
		if hash == "" {
			continue
		}
		author := strings.TrimSpace(lines[i+1])
		date := strings.TrimSpace(lines[i+2])
		subject := strings.TrimSpace(lines[i+3])
		result.Entries = append(result.Entries, LogEntry{
			Hash:    hash,
			Author:  author,
			Date:    date,
			Subject: subject,
		})
	}
	return result
}

// Show runs git show for a specific ref/commit in workDir.
// ref must pass validateRef (no unsafe characters, no "..").
func Show(ctx context.Context, workDir string, ref string) (ShowResult, error) {
	if err := validateRef(ref); err != nil {
		return ShowResult{}, err
	}
	// Multi-line format avoids NUL bytes which are invalid on Windows.
	// Format: hash, author, date, subject (each on own line), then blank + diff.
	cmd := exec.CommandContext(ctx, "git", "show", "--no-color",
		"--format=%H%n%an%n%ai%n%s%n", ref)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	raw := string(out)
	if err != nil {
		return ShowResult{Raw: raw}, fmt.Errorf("gittools: git show %s in %s: %w: %s", ref, workDir, err, strings.TrimSpace(raw))
	}
	return parseShow(raw), nil
}

func parseShow(raw string) ShowResult {
	result := ShowResult{Raw: raw}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	// The first 5 lines are: hash, author, date, subject, blank line.
	// Everything after is the diff / body.
	lines := strings.SplitN(normalized, "\n", 6)
	if len(lines) < 4 {
		return result
	}
	result.Hash = strings.TrimSpace(lines[0])
	result.Author = strings.TrimSpace(lines[1])
	result.Date = strings.TrimSpace(lines[2])
	result.Subject = strings.TrimSpace(lines[3])
	if len(lines) >= 6 {
		result.Body = strings.TrimSpace(lines[5])
	}
	return result
}

// Blame runs git blame --porcelain on filePath in workDir. fromLine and toLine
// are 1-based inclusive; pass 0 for both to get the whole file.
// filePath must be a relative path with no ".." sequences.
func Blame(ctx context.Context, workDir string, filePath string, fromLine, toLine int) (BlameResult, error) {
	if err := validateRelPath(filePath); err != nil {
		return BlameResult{}, err
	}
	args := []string{"blame", "--porcelain"}
	if fromLine > 0 || toLine > 0 {
		if fromLine <= 0 {
			fromLine = 1
		}
		if toLine <= 0 || toLine < fromLine {
			toLine = fromLine
		}
		args = append(args, fmt.Sprintf("-L%d,%d", fromLine, toLine))
	}
	args = append(args, "--", filePath)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	raw := string(out)
	if err != nil {
		return BlameResult{FilePath: filePath, Raw: raw}, fmt.Errorf("gittools: git blame %s in %s: %w: %s", filePath, workDir, err, strings.TrimSpace(raw))
	}
	return parseBlame(filePath, raw), nil
}

// parseBlame parses git blame --porcelain output into BlameLine entries.
// The porcelain format groups lines by commit header; each content line is
// preceded by a tab character.
func parseBlame(filePath, raw string) BlameResult {
	result := BlameResult{FilePath: filePath, Raw: raw}
	var currentHash, currentAuthor string
	lineNum := 0
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		// Commit header: 40-hex-char hash followed by SP and line numbers.
		if len(line) >= 40 && !strings.HasPrefix(line, "\t") && isHexString(line[:40]) {
			currentHash = line[:40]
		} else if a, ok := strings.CutPrefix(line, "author "); ok {
			currentAuthor = a
		} else if strings.HasPrefix(line, "\t") {
			lineNum++
			result.Lines = append(result.Lines, BlameLine{
				LineNum: lineNum,
				Hash:    currentHash,
				Author:  currentAuthor,
				Content: line[1:],
			})
		}
	}
	return result
}

func isHexString(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
