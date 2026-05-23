package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	return schema.AgentCodex
}

func (a *Adapter) Preflight(ctx context.Context, capsule *schema.ExecutionCapsule) error {
	if capsule == nil {
		return fmt.Errorf("codex adapter: capsule is required")
	}
	cmdPath, err := a.lookupCommand()
	if err != nil {
		return err
	}
	if strings.TrimSpace(capsule.Sandbox.WorktreePath) == "" {
		return fmt.Errorf("codex adapter: capsule %s sandbox worktree path is required", capsule.CapsuleID)
	}
	if err := ensureCleanWorktree(ctx, capsule.Sandbox.WorktreePath); err != nil {
		return fmt.Errorf("codex adapter: preflight clean worktree check failed: %w", err)
	}
	cmd := exec.CommandContext(ctx, cmdPath, "--version")
	if _, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codex adapter: version check failed: %w", err)
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
		return nil, fmt.Errorf("codex adapter: serialize projection: %w", err)
	}

	capsuleDir := filepath.Join(a.orcaDir, "capsules", capsule.CapsuleID)
	briefingPath := filepath.Join(capsuleDir, "executor_briefing.md")
	if err := os.MkdirAll(capsuleDir, 0o755); err != nil {
		return nil, fmt.Errorf("codex adapter: create capsule dir: %w", err)
	}
	if err := os.WriteFile(briefingPath, []byte(briefing), 0o644); err != nil {
		return nil, fmt.Errorf("codex adapter: write briefing file %s: %w", briefingPath, err)
	}

	schemaPath := filepath.Join(capsuleDir, "sidecar_schema.json")
	if err := os.WriteFile(schemaPath, []byte(sidecarJSONSchema()), 0o644); err != nil {
		return nil, fmt.Errorf("codex adapter: write sidecar schema %s: %w", schemaPath, err)
	}
	sidecarPath := filepath.Join(capsuleDir, "sidecar.json")

	// Resolve to absolute paths: codex runs with cmd.Dir = worktree, so any
	// relative path would be interpreted relative to the worktree, not the
	// orca process directory.
	absSchemaPath, err := filepath.Abs(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("codex adapter: resolve schema path: %w", err)
	}
	absSidecarPath, err := filepath.Abs(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("codex adapter: resolve sidecar path: %w", err)
	}

	transcriptPath := orcapath.TranscriptPath(a.orcaDir, capsule.CapsuleID)

	briefingFile, err := os.Open(briefingPath)
	if err != nil {
		return nil, fmt.Errorf("codex adapter: open briefing for stdin: %w", err)
	}
	defer briefingFile.Close()

	// codex exec reads the prompt from stdin when "-" is passed as the prompt argument.
	// danger-full-access is appropriate here: the capsule runs in an isolated git worktree
	// (.orca/capsules/<id>/worktree) that is separate from the main working tree.
	// --output-schema constrains the model's final response to match AgentSidecarOutput.
	// -o writes that final response to absSidecarPath so we can read it back.
	args := []string{
		"exec",
		"-s", "danger-full-access",
		"--ephemeral",
		"--output-schema", absSchemaPath,
		"-o", absSidecarPath,
		"-",
	}
	_, stderr, duration, runErr := runCommandWithTranscript(ctx, commandSpec{
		executable: cmdPath,
		args:       args,
		worktree:   capsule.Sandbox.WorktreePath,
		transcript: transcriptPath,
		stdin:      briefingFile,
	})
	if runErr != nil {
		return nil, fmt.Errorf("codex adapter: execute failed: %w: %s", runErr, strings.TrimSpace(stderr))
	}

	sidecarData, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, runner.ErrNoSidecar
	}
	sidecar := &schema.AgentSidecarOutput{}
	if err := json.Unmarshal(sidecarData, sidecar); err != nil {
		return nil, runner.ErrInvalidSidecar
	}
	if len(sidecar.FilesChanged) == 0 && len(sidecar.CommandsRun) == 0 && len(sidecar.ObligationsAddressed) == 0 {
		return nil, runner.ErrInvalidSidecar
	}
	sidecar.WallTimeSeconds = duration.Seconds()
	return sidecar, nil
}

func (a *Adapter) ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("codex adapter: read transcript %s: %w", transcriptPath, err)
	}
	return extractSidecarFromTranscript(string(data), transcriptPath, capsule.ObligationIDs)
}

func (a *Adapter) lookupCommand() (string, error) {
	command := a.executable
	if command == "" {
		command = "codex"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("codex adapter: command %q not found: %w", command, err)
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

func runCommandWithTranscript(ctx context.Context, spec commandSpec) (outStr string, errStr string, d time.Duration, err error) {
	file, err := os.Create(spec.transcript)
	if err != nil {
		return "", "", 0, fmt.Errorf("create transcript %s: %w", spec.transcript, err)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()

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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git status --porcelain: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("worktree %s has uncommitted changes", worktreePath)
	}
	return nil
}

// sidecarJSONSchema returns the JSON Schema describing AgentSidecarOutput.
// Codex uses this via --output-schema to constrain its final response shape.
// OpenAI structured output requires "additionalProperties": false on every
// object in the schema, and all properties of each object must be in "required".
func sidecarJSONSchema() string {
	return `{
  "type": "object",
  "additionalProperties": false,
  "required": ["obligations_addressed", "files_changed", "commands_run", "assumptions", "claims", "risks", "follow_up_needed", "evidence_paths", "summary"],
  "properties": {
    "obligations_addressed": {
      "type": "array",
      "items": {"type": "string"},
      "description": "IDs of obligations addressed (OB-xxxx format)"
    },
    "files_changed": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Relative paths of files modified or created"
    },
    "commands_run": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Shell commands executed"
    },
    "assumptions": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Assumptions made during implementation"
    },
    "claims": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["claim", "type", "evidence", "contradicts", "invalidates"],
        "properties": {
          "claim": {"type": "string"},
          "type": {"type": "string", "enum": ["verified", "proposed"]},
          "evidence": {"type": "string", "description": "Evidence reference or empty string"},
          "contradicts": {"type": "array", "items": {"type": "string"}},
          "invalidates": {"type": "array", "items": {"type": "string"}}
        }
      }
    },
    "risks": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Risks identified"
    },
    "follow_up_needed": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Items requiring a follow-up capsule"
    },
    "evidence_paths": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Paths to evidence artifact files"
    },
    "summary": {
      "type": "string",
      "description": "Brief summary of what was accomplished"
    }
  }
}`
}

func extractSidecarFromTranscript(text, transcriptPath string, allowedObligationIDs []string) (*schema.AgentSidecarOutput, error) {
	filesChanged := uniqueMatches(text, regexp.MustCompile(`(?m)^\s*(?:M|A|D|R)\s+([^\s]+)`))
	if len(filesChanged) == 0 {
		filesChanged = uniqueMatches(text, regexp.MustCompile(`(?m)^\+\+\+\s+b\/([^\s]+)`))
	}
	commands, err := collectCommandLines(text)
	if err != nil {
		return nil, err
	}
	obligationIDs := filterToAllowed(
		uniqueMatches(text, regexp.MustCompile(`\b(OB-[A-Za-z0-9\-]+)\b`)),
		allowedObligationIDs,
	)
	assumptions, err := collectByPrefix(text, "assumption:")
	if err != nil {
		return nil, err
	}
	risks, err := collectByPrefix(text, "risk:")
	if err != nil {
		return nil, err
	}
	followUp, err := collectByPrefix(text, "follow-up:")
	if err != nil {
		return nil, err
	}
	claims, err := collectClaims(text)
	if err != nil {
		return nil, err
	}
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
	}, nil
}

func filterToAllowed(ids, allowed []string) []string {
	if len(allowed) == 0 {
		return ids
	}
	set := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
}

func collectCommandLines(text string) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(text))
	out := make([]string, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "$ ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "$ ")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex adapter: scan transcript commands: %w", err)
	}
	return uniqueStrings(out), nil
}

func collectByPrefix(text, prefix string) ([]string, error) {
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex adapter: scan transcript prefix %q: %w", prefix, err)
	}
	return uniqueStrings(out), nil
}

func collectClaims(text string) ([]schema.SidecarClaim, error) {
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex adapter: scan transcript claims: %w", err)
	}
	return out, nil
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
