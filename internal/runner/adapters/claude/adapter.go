package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

type Adapter struct {
	orcaDir    string
	executable string
}

func New(orcaDir, executable string) *Adapter {
	return &Adapter{
		orcaDir:    strings.TrimSpace(orcaDir),
		executable: strings.TrimSpace(executable),
	}
}

func (a *Adapter) AgentType() schema.AgentType {
	return schema.AgentClaude
}

func (a *Adapter) Preflight(ctx context.Context, capsule *schema.ExecutionCapsule) error {
	if capsule == nil {
		return fmt.Errorf("claude adapter: capsule is required")
	}
	cmdPath, err := a.lookupCommand()
	if err != nil {
		return err
	}
	if strings.TrimSpace(capsule.Sandbox.WorktreePath) == "" {
		return fmt.Errorf("claude adapter: capsule %s sandbox worktree path is required", capsule.CapsuleID)
	}
	if err := ensureCleanWorktree(ctx, capsule.Sandbox.WorktreePath); err != nil {
		return fmt.Errorf("claude adapter: preflight clean worktree check failed: %w", err)
	}
	cmd := exec.CommandContext(ctx, cmdPath, "--version")
	if _, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("claude adapter: version check failed: %w", err)
	}
	return nil
}

func (a *Adapter) Execute(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	cmdPath, err := a.lookupCommand()
	if err != nil {
		return nil, err
	}
	briefing, err := runner.SerializeExecutorProjection(projection)
	if err != nil {
		return nil, fmt.Errorf("claude adapter: serialize projection: %w", err)
	}

	capsuleDir := filepath.Join(a.orcaDir, "capsules", capsule.CapsuleID)
	briefingPath := filepath.Join(capsuleDir, "executor_briefing.md")
	if err := os.MkdirAll(capsuleDir, 0o755); err != nil {
		return nil, fmt.Errorf("claude adapter: create capsule dir: %w", err)
	}
	if err := os.WriteFile(briefingPath, []byte(briefing), 0o644); err != nil {
		return nil, fmt.Errorf("claude adapter: write briefing file %s: %w", briefingPath, err)
	}

	transcriptPath := orcapath.TranscriptPath(a.orcaDir, capsule.CapsuleID)
	args, jsonMode, err := resolveInvocation(ctx, cmdPath, briefingPath)
	if err != nil {
		return nil, err
	}
	out, stderr, runErr := runCommandWithTranscript(ctx, commandSpec{
		executable: cmdPath,
		args:       args,
		worktree:   capsule.Sandbox.WorktreePath,
		transcript: transcriptPath,
	})
	if runErr != nil {
		return nil, fmt.Errorf("claude adapter: execute failed: %w", runErr)
	}

	if !jsonMode {
		return nil, runner.ErrNoSidecar
	}
	sidecar := &schema.AgentSidecarOutput{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), sidecar); err != nil {
		combined := strings.TrimSpace(out + "\n" + stderr)
		if strings.TrimSpace(combined) == "" {
			return nil, runner.ErrNoSidecar
		}
		return nil, runner.ErrInvalidSidecar
	}
	if len(sidecar.FilesChanged) == 0 && len(sidecar.CommandsRun) == 0 && len(sidecar.ObligationsAddressed) == 0 {
		return nil, runner.ErrInvalidSidecar
	}
	return sidecar, nil
}

func (a *Adapter) ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("claude adapter: read transcript %s: %w", transcriptPath, err)
	}
	text := string(data)
	_ = ctx
	_ = capsule
	return extractSidecarFromTranscript(text, transcriptPath), nil
}

func (a *Adapter) lookupCommand() (string, error) {
	command := a.executable
	if command == "" {
		command = "claude"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("claude adapter: command %q not found: %w", command, err)
	}
	return path, nil
}

type commandSpec struct {
	executable string
	args       []string
	worktree   string
	transcript string
}

func runCommandWithTranscript(ctx context.Context, spec commandSpec) (string, string, error) {
	file, err := os.Create(spec.transcript)
	if err != nil {
		return "", "", fmt.Errorf("create transcript %s: %w", spec.transcript, err)
	}
	defer file.Close()

	cmd := exec.CommandContext(ctx, spec.executable, spec.args...)
	cmd.Dir = spec.worktree
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, file)
	cmd.Stderr = io.MultiWriter(&stderr, file)
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

func ensureCleanWorktree(ctx context.Context, worktreePath string) error {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git status --porcelain: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("worktree %s has uncommitted changes", worktreePath)
	}
	return nil
}

func resolveInvocation(ctx context.Context, cmdPath, briefingPath string) ([]string, bool, error) {
	help, _ := exec.CommandContext(ctx, cmdPath, "--help").CombinedOutput()
	helpText := strings.ToLower(string(help))

	fileFlag := ""
	for _, candidate := range []string{"--prompt-file", "--input-file", "--file"} {
		if strings.Contains(helpText, candidate) {
			fileFlag = candidate
			break
		}
	}
	if fileFlag == "" {
		return nil, false, fmt.Errorf("claude adapter: unable to resolve prompt file flag from %q --help output", cmdPath)
	}

	switch {
	case strings.Contains(helpText, "--json"):
		return []string{"--json", fileFlag, briefingPath}, true, nil
	case strings.Contains(helpText, "--output-format"):
		return []string{"--output-format", "json", fileFlag, briefingPath}, true, nil
	case strings.Contains(helpText, "--format"):
		return []string{"--format", "json", fileFlag, briefingPath}, true, nil
	default:
		return []string{fileFlag, briefingPath}, false, nil
	}
}

func extractSidecarFromTranscript(text, transcriptPath string) *schema.AgentSidecarOutput {
	filesChanged := uniqueMatches(text, regexp.MustCompile(`(?m)^\s*(?:M|A|D|R)\s+([^\s]+)`))
	if len(filesChanged) == 0 {
		filesChanged = uniqueMatches(text, regexp.MustCompile(`(?m)^\+\+\+\s+b\/([^\s]+)`))
	}
	commands := collectCommandLines(text)
	obligationIDs := uniqueMatches(text, regexp.MustCompile(`\b(OB-[A-Za-z0-9\-]+)\b`))
	assumptions := collectByPrefix(text, "assumption:")
	risks := collectByPrefix(text, "risk:")
	followUp := collectByPrefix(text, "follow-up:")
	claims := collectClaims(text)
	if len(claims) == 0 {
		for _, assumption := range assumptions {
			claims = append(claims, schema.SidecarClaim{Claim: assumption, Type: schema.SidecarClaimProposed})
		}
	}
	return &schema.AgentSidecarOutput{
		ObligationsAddressed: obligationIDs,
		FilesChanged:         filesChanged,
		CommandsRun:          commands,
		Assumptions:          assumptions,
		Claims:               claims,
		Risks:                risks,
		FollowUpNeeded:       followUp,
		EvidencePaths:        []string{transcriptPath},
	}
}

func collectCommandLines(text string) []string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	out := make([]string, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "$ ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "$ ")))
		}
	}
	return uniqueStrings(out)
}

func collectByPrefix(text, prefix string) []string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	out := make([]string, 0)
	lowerPrefix := strings.ToLower(prefix)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, lowerPrefix) {
			out = append(out, strings.TrimSpace(line[len(prefix):]))
		}
	}
	return uniqueStrings(out)
}

func collectClaims(text string) []schema.SidecarClaim {
	scanner := bufio.NewScanner(strings.NewReader(text))
	out := make([]schema.SidecarClaim, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "claim verified:"):
			out = append(out, schema.SidecarClaim{
				Claim: strings.TrimSpace(line[len("claim verified:"):]),
				Type:  schema.SidecarClaimVerified,
			})
		case strings.HasPrefix(lower, "claim:"):
			out = append(out, schema.SidecarClaim{
				Claim: strings.TrimSpace(line[len("claim:"):]),
				Type:  schema.SidecarClaimProposed,
			})
		}
	}
	return out
}

func uniqueMatches(text string, re *regexp.Regexp) []string {
	matches := re.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		out = append(out, strings.TrimSpace(m[1]))
	}
	return uniqueStrings(out)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

var _ runner.Adapter = (*Adapter)(nil)
