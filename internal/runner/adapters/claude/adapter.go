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
	"time"

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

// claudeJSONResult is the outer envelope from `claude -p --output-format json`.
type claudeJSONResult struct {
	Result  string       `json:"result"`
	IsError bool         `json:"is_error"`
	Usage   *claudeUsage `json:"usage,omitempty"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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

	briefingFile, err := os.Open(briefingPath)
	if err != nil {
		return nil, fmt.Errorf("claude adapter: open briefing for stdin: %w", err)
	}
	defer briefingFile.Close()

	// -p runs non-interactively; stdin is the prompt.
	// --output-format json wraps the response in a JSON envelope.
	// --json-schema constrains the model's response to match AgentSidecarOutput.
	// --no-session-persistence avoids writing session history to disk.
	// --permission-mode bypassPermissions avoids interactive approval prompts.
	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", sidecarJSONSchemaInline(),
		"--no-session-persistence",
		"--permission-mode", "bypassPermissions",
	}
	stdout, _, duration, runErr := runCommandWithTranscript(ctx, commandSpec{
		executable: cmdPath,
		args:       args,
		worktree:   capsule.Sandbox.WorktreePath,
		transcript: transcriptPath,
		stdin:      briefingFile,
	})
	if runErr != nil {
		return nil, fmt.Errorf("claude adapter: execute failed: %w", runErr)
	}

	outer := &claudeJSONResult{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), outer); err != nil {
		return nil, runner.ErrNoSidecar
	}
	if outer.IsError || strings.TrimSpace(outer.Result) == "" {
		return nil, runner.ErrNoSidecar
	}
	sidecar := &schema.AgentSidecarOutput{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outer.Result)), sidecar); err != nil {
		return nil, runner.ErrInvalidSidecar
	}
	if len(sidecar.FilesChanged) == 0 && len(sidecar.CommandsRun) == 0 && len(sidecar.ObligationsAddressed) == 0 {
		return nil, runner.ErrInvalidSidecar
	}
	if outer.Usage != nil {
		sidecar.TokensUsed = outer.Usage.InputTokens + outer.Usage.OutputTokens
	}
	sidecar.WallTimeSeconds = duration.Seconds()
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
	stdin      io.Reader
}

func runCommandWithTranscript(ctx context.Context, spec commandSpec) (string, string, time.Duration, error) {
	file, err := os.Create(spec.transcript)
	if err != nil {
		return "", "", 0, fmt.Errorf("create transcript %s: %w", spec.transcript, err)
	}
	defer file.Close()

	cmd := exec.CommandContext(ctx, spec.executable, spec.args...)
	cmd.Dir = spec.worktree
	if spec.stdin != nil {
		cmd.Stdin = spec.stdin
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, file)
	cmd.Stderr = io.MultiWriter(&stderr, file)
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), time.Since(start), err
	}
	return stdout.String(), stderr.String(), time.Since(start), nil
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

// sidecarJSONSchemaInline returns the AgentSidecarOutput schema as a compact
// single-line JSON string, suitable for passing to claude via --json-schema.
func sidecarJSONSchemaInline() string {
	return `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","required":["obligations_addressed","files_changed","commands_run","assumptions","claims","risks","follow_up_needed","evidence_paths"],"properties":{"obligations_addressed":{"type":"array","items":{"type":"string"},"description":"IDs of obligations addressed (OB-xxxx format)"},"files_changed":{"type":"array","items":{"type":"string"},"description":"Relative paths of files modified or created"},"commands_run":{"type":"array","items":{"type":"string"},"description":"Shell commands executed"},"assumptions":{"type":"array","items":{"type":"string"}},"claims":{"type":"array","items":{"type":"object","required":["claim","type"],"properties":{"claim":{"type":"string"},"type":{"type":"string","enum":["verified","proposed"]},"evidence":{"type":"string"},"contradicts":{"type":"array","items":{"type":"string"}},"invalidates":{"type":"array","items":{"type":"string"}}}}},"risks":{"type":"array","items":{"type":"string"}},"follow_up_needed":{"type":"array","items":{"type":"string"}},"evidence_paths":{"type":"array","items":{"type":"string"}},"summary":{"type":"string"}}}`
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
			claim, contradicts, invalidates := splitClaimMetadata(line[len("claim verified:"):])
			out = append(out, schema.SidecarClaim{
				Claim:       claim,
				Type:        schema.SidecarClaimVerified,
				Contradicts: contradicts,
				Invalidates: invalidates,
			})
		case strings.HasPrefix(lower, "claim:"):
			claim, contradicts, invalidates := splitClaimMetadata(line[len("claim:"):])
			out = append(out, schema.SidecarClaim{
				Claim:       claim,
				Type:        schema.SidecarClaimProposed,
				Contradicts: contradicts,
				Invalidates: invalidates,
			})
		}
	}
	return out
}

func splitClaimMetadata(raw string) (string, []string, []string) {
	parts := strings.Split(raw, "|")
	claim := strings.TrimSpace(parts[0])
	var contradicts []string
	var invalidates []string
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "contradicts":
			contradicts = append(contradicts, splitIDs(value)...)
		case "invalidates":
			invalidates = append(invalidates, splitIDs(value)...)
		}
	}
	return claim, contradicts, invalidates
}

func splitIDs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed != "" {
			out = append(out, trimmed)
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
