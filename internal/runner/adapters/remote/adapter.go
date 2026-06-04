// Package remote implements runner.Adapter for remote SSH execution. The agent
// CLI (codex or claude) must be installed on the remote host. File transfer uses
// SSH standard I/O (stdin/stdout of shell commands) to avoid an SFTP dependency.
//
// Boundary rules (per docs/module_boundaries.md):
//
//	May import:    internal/runner, internal/schema, internal/config, internal/orcapath
//	Must NOT import: internal/planner, internal/verifier, internal/reconciler,
//	                 internal/projector, internal/budget, internal/gate
//	No remote-specific core event types.
//	RunResult.SidecarUsed stays observability-only.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

// Session abstracts remote operations so tests inject a fake without a real
// SSH server.
type Session interface {
	// RunCommand executes cmd on the remote host and returns combined output.
	RunCommand(ctx context.Context, cmd string) (string, error)
	// Upload writes data to remotePath, creating parent dirs as needed.
	Upload(ctx context.Context, remotePath string, data []byte) error
	// Download reads the file at remotePath and returns its contents.
	Download(ctx context.Context, remotePath string) ([]byte, error)
	// MkdirAll creates remotePath and any missing parents.
	MkdirAll(ctx context.Context, remotePath string) error
	// RemoveAll deletes remotePath and all descendants.
	RemoveAll(ctx context.Context, remotePath string) error
	// Close terminates the session.
	Close() error
}

// Dialer opens a remote session.
type Dialer interface {
	Dial(ctx context.Context, cfg config.RemoteConfig) (Session, error)
}

// Adapter implements runner.Adapter for SSH-based remote execution.
type Adapter struct {
	cfg     config.RemoteConfig
	agent   schema.AgentType
	orcaDir string
	dialer  Dialer
}

// New returns an Adapter for agent. Pass nil dialer to use the default SSH
// implementation; tests inject a Dialer backed by a fakeSession.
func New(cfg config.RemoteConfig, agent schema.AgentType, orcaDir string, dialer Dialer) *Adapter {
	if dialer == nil {
		dialer = sshDialer{}
	}
	return &Adapter{
		cfg:     cfg,
		agent:   agent,
		orcaDir: strings.TrimSpace(orcaDir),
		dialer:  dialer,
	}
}

func (a *Adapter) AgentType() schema.AgentType { return a.agent }

func (a *Adapter) Preflight(_ context.Context, capsule *schema.ExecutionCapsule) error {
	if capsule == nil {
		return fmt.Errorf("remote adapter: capsule is required")
	}
	if !a.cfg.Enabled {
		return fmt.Errorf("remote adapter: remote.enabled must be true")
	}
	if strings.TrimSpace(a.cfg.Host) == "" {
		return fmt.Errorf("remote adapter: remote.host is required")
	}
	if strings.TrimSpace(a.cfg.Workspace) == "" {
		return fmt.Errorf("remote adapter: remote.workspace is required")
	}
	return nil
}

func (a *Adapter) Execute(ctx context.Context, capsule *schema.ExecutionCapsule, projection *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	briefing, err := runner.SerializeExecutorProjection(projection)
	if err != nil {
		return nil, fmt.Errorf("remote adapter: serialize projection: %w", err)
	}

	sess, err := a.dialer.Dial(ctx, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("remote adapter: dial %s: %w", a.cfg.Host, err)
	}
	defer sess.Close()

	remoteDir := path.Join(a.cfg.Workspace, "CAP-"+capsule.CapsuleID)
	if err := sess.MkdirAll(ctx, remoteDir); err != nil {
		return nil, fmt.Errorf("remote adapter: mkdir %s: %w", remoteDir, err)
	}

	briefingPath := remoteDir + "/executor_briefing.md"
	if err := sess.Upload(ctx, briefingPath, []byte(briefing)); err != nil {
		return nil, fmt.Errorf("remote adapter: upload briefing: %w", err)
	}

	capsuleJSON, err := json.Marshal(capsule)
	if err != nil {
		return nil, fmt.Errorf("remote adapter: marshal capsule: %w", err)
	}
	if err := sess.Upload(ctx, remoteDir+"/capsule.json", capsuleJSON); err != nil {
		return nil, fmt.Errorf("remote adapter: upload capsule: %w", err)
	}

	// Upload sidecar schema so Codex can enforce structured output via --output-schema.
	remoteSchemaPath := remoteDir + "/sidecar_schema.json"
	if a.agent == schema.AgentCodex {
		if err := sess.Upload(ctx, remoteSchemaPath, []byte(remoteSidecarJSONSchema())); err != nil {
			return nil, fmt.Errorf("remote adapter: upload sidecar schema: %w", err)
		}
	}

	start := time.Now()
	cmd := buildAgentCommand(a.agent, remoteDir, briefingPath, remoteSchemaPath, capsule.PermissionMode)
	_, runErr := sess.RunCommand(ctx, cmd)

	// Always try to download transcript for the ExtractFromTranscript fallback.
	localTranscriptPath := orcapath.TranscriptPath(a.orcaDir, capsule.CapsuleID)
	if mkErr := os.MkdirAll(filepath.Dir(localTranscriptPath), 0o755); mkErr == nil {
		if transcriptData, dlErr := sess.Download(ctx, remoteDir+"/transcript.log"); dlErr == nil && len(transcriptData) > 0 {
			_ = os.WriteFile(localTranscriptPath, transcriptData, 0o644)
		}
	}

	// Use Background to clean up even when ctx was cancelled before we return.
	defer func() { _ = sess.RemoveAll(context.Background(), remoteDir) }()

	if runErr != nil {
		return nil, fmt.Errorf("remote adapter: run %s on %s: %w", a.agent, a.cfg.Host, runErr)
	}

	outputData, dlErr := sess.Download(ctx, remoteDir+"/output.json")
	if dlErr != nil {
		return nil, runner.ErrNoSidecar
	}

	out, parseErr := runner.ParseOutputJSON(outputData)
	if parseErr != nil {
		return nil, parseErr
	}
	if out.WallTimeSeconds <= 0 {
		out.WallTimeSeconds = time.Since(start).Seconds()
	}
	return out, nil
}

func (a *Adapter) ExtractFromTranscript(ctx context.Context, capsule *schema.ExecutionCapsule, transcriptPath string) (*schema.AgentSidecarOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("remote adapter: read transcript %s: %w", transcriptPath, err)
	}
	return extractSidecarFromTranscript(string(data), transcriptPath, capsule.ObligationIDs)
}

// buildAgentCommand returns a shell command that runs the agent on the remote
// host, writes output to output.json and stderr to transcript.log.
// remoteSchemaPath is the remote path to sidecar_schema.json (used by Codex via
// --output-schema); Claude uses the schema inline via --json-schema.
func buildAgentCommand(agent schema.AgentType, remoteDir, briefingPath, remoteSchemaPath string, mode schema.PermissionMode) string {
	switch agent {
	case schema.AgentCodex:
		// Codex writes sidecar JSON to -o; we redirect stdout+stderr to transcript.
		return fmt.Sprintf(
			"codex exec -s %s --ephemeral --output-schema %s -o %s/output.json - < %s > %s/transcript.log 2>&1",
			remoteCodexSandboxFlag(mode), shellQuote(remoteSchemaPath), remoteDir, briefingPath, remoteDir,
		)
	case schema.AgentClaude:
		// Claude writes JSON envelope to stdout; stderr goes to transcript.
		return fmt.Sprintf(
			"claude -p --output-format json --json-schema %s --no-session-persistence --permission-mode %s < %s > %s/output.json 2>%s/transcript.log",
			shellQuote(remoteSidecarJSONSchemaInline()), remoteClaudePermissionMode(mode), briefingPath, remoteDir, remoteDir,
		)
	default:
		return fmt.Sprintf("%s < %s > %s/output.json 2>%s/transcript.log", string(agent), briefingPath, remoteDir, remoteDir)
	}
}

// remoteSidecarJSONSchema returns the minified Codex sidecar schema for upload
// to the remote host. Matches the content used by the local Codex adapter.
func remoteSidecarJSONSchema() string {
	return `{
  "type": "object",
  "additionalProperties": false,
  "required": ["obligations_addressed", "files_changed", "commands_run", "assumptions", "claims", "risks", "follow_up_needed", "evidence_paths", "summary"],
  "properties": {
    "obligations_addressed": {
      "type": "array",
      "items": {"type": "string"}
    },
    "files_changed": {
      "type": "array",
      "items": {"type": "string"}
    },
    "commands_run": {
      "type": "array",
      "items": {"type": "string"}
    },
    "assumptions": {
      "type": "array",
      "items": {"type": "string"}
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
          "evidence": {"type": "string"},
          "contradicts": {"type": "array", "items": {"type": "string"}},
          "invalidates": {"type": "array", "items": {"type": "string"}}
        }
      }
    },
    "risks": {
      "type": "array",
      "items": {"type": "string"}
    },
    "follow_up_needed": {
      "type": "array",
      "items": {"type": "string"}
    },
    "evidence_paths": {
      "type": "array",
      "items": {"type": "string"}
    },
    "summary": {
      "type": "string"
    }
  }
}`
}

// remoteSidecarJSONSchemaInline returns the minified Claude sidecar schema as a
// compact single-line JSON string for inline --json-schema injection.
func remoteSidecarJSONSchemaInline() string {
	return `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","required":["obligations_addressed","files_changed","commands_run","assumptions","claims","risks","follow_up_needed","evidence_paths"],"properties":{"obligations_addressed":{"type":"array","items":{"type":"string"}},"files_changed":{"type":"array","items":{"type":"string"}},"commands_run":{"type":"array","items":{"type":"string"}},"assumptions":{"type":"array","items":{"type":"string"}},"claims":{"type":"array","items":{"type":"object","required":["claim","type"],"properties":{"claim":{"type":"string"},"type":{"type":"string","enum":["verified","proposed"]},"evidence":{"type":"string"},"contradicts":{"type":"array","items":{"type":"string"}},"invalidates":{"type":"array","items":{"type":"string"}}}}},"risks":{"type":"array","items":{"type":"string"}},"follow_up_needed":{"type":"array","items":{"type":"string"}},"evidence_paths":{"type":"array","items":{"type":"string"}},"summary":{"type":"string"}}}`
}

func remoteCodexSandboxFlag(mode schema.PermissionMode) string {
	switch mode {
	case schema.PermissionReadOnly:
		return "read-only"
	case schema.PermissionWorkspaceWrite:
		return "workspace-write"
	case schema.PermissionDangerFullAccess, "":
		return "danger-full-access"
	default:
		return "danger-full-access"
	}
}

func remoteClaudePermissionMode(mode schema.PermissionMode) string {
	switch mode {
	case schema.PermissionReadOnly:
		return "prompt"
	case schema.PermissionWorkspaceWrite:
		return "acceptEdits"
	case schema.PermissionDangerFullAccess, "":
		return "bypassPermissions"
	default:
		return "bypassPermissions"
	}
}

// ── transcript extraction (same heuristics as local adapters) ────────────────

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
		return nil, fmt.Errorf("remote adapter: scan transcript commands: %w", err)
	}
	return uniqueStrings(out), nil
}

func collectByPrefix(text, prefix string) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(text))
	out := make([]string, 0)
	lowerPrefix := strings.ToLower(prefix)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(strings.ToLower(line), lowerPrefix) {
			out = append(out, strings.TrimSpace(line[len(prefix):]))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remote adapter: scan transcript prefix %q: %w", prefix, err)
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
			out = append(out, schema.SidecarClaim{Claim: claim, Type: schema.SidecarClaimVerified, Contradicts: contradicts, Invalidates: invalidates})
		case strings.HasPrefix(lower, "claim:"):
			claim, contradicts, invalidates := splitClaimMetadata(line[len("claim:"):])
			out = append(out, schema.SidecarClaim{Claim: claim, Type: schema.SidecarClaimProposed, Contradicts: contradicts, Invalidates: invalidates})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remote adapter: scan transcript claims: %w", err)
	}
	return out, nil
}

func splitClaimMetadata(raw string) (string, []string, []string) {
	parts := strings.Split(raw, "|")
	claim := strings.TrimSpace(parts[0])
	var contradicts, invalidates []string
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
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func uniqueMatches(text string, re *regexp.Regexp) []string {
	matches := re.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return uniqueStrings(out)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		t := strings.TrimSpace(v)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ── SSH production dialer ────────────────────────────────────────────────────

type sshDialer struct{}

func (sshDialer) Dial(ctx context.Context, cfg config.RemoteConfig) (Session, error) {
	var authMethods []ssh.AuthMethod
	if cfg.SSHKeyPath != "" {
		key, err := os.ReadFile(cfg.SSHKeyPath)
		if err != nil {
			return nil, fmt.Errorf("remote adapter: read ssh key %s: %w", cfg.SSHKeyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("remote adapter: parse ssh key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	clientCfg := &ssh.ClientConfig{
		User:            sshUser(cfg.Host),
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Phase 5 MVP
		Timeout:         30 * time.Second,
	}

	host := sshHost(cfg.Host)
	client, err := ssh.Dial("tcp", host, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("remote adapter: ssh dial %s: %w", host, err)
	}
	return &sshSession{client: client}, nil
}

// sshUser extracts "user" from "user@host:port" or returns a default.
func sshUser(hostSpec string) string {
	if at := strings.Index(hostSpec, "@"); at >= 0 {
		return hostSpec[:at]
	}
	return "orca"
}

// sshHost normalises "user@host:port" or "host" to "host:port".
func sshHost(hostSpec string) string {
	h := hostSpec
	if at := strings.Index(h, "@"); at >= 0 {
		h = h[at+1:]
	}
	if !strings.Contains(h, ":") {
		h += ":22"
	}
	return h
}

type sshSession struct {
	client *ssh.Client
}

// RunCommand ignores ctx: golang.org/x/crypto/ssh does not support context-based
// cancellation for CombinedOutput. A hung remote command blocks until exit.
// MVP limitation — command-level timeout is out of scope for Phase 5.
func (s *sshSession) RunCommand(_ context.Context, cmd string) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("remote: new ssh session: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return string(out), fmt.Errorf("remote: run %q: %w: %s", cmd, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s *sshSession) Upload(_ context.Context, remotePath string, data []byte) error {
	dir := path.Dir(remotePath)
	mkCmd := fmt.Sprintf("mkdir -p %s", shellQuote(dir))
	if _, err := s.RunCommand(context.Background(), mkCmd); err != nil {
		return fmt.Errorf("remote: mkdir %s: %w", dir, err)
	}
	// Pipe data through stdin: `cat > <path>`
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("remote: new ssh session for upload: %w", err)
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	out, err := sess.CombinedOutput(fmt.Sprintf("cat > %s", shellQuote(remotePath)))
	if err != nil {
		return fmt.Errorf("remote: upload %s: %w: %s", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *sshSession) Download(_ context.Context, remotePath string) ([]byte, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("remote: new ssh session for download: %w", err)
	}
	defer sess.Close()
	var buf bytes.Buffer
	sess.Stdout = &buf
	if err := sess.Run(fmt.Sprintf("cat %s", shellQuote(remotePath))); err != nil {
		return nil, fmt.Errorf("remote: download %s: %w", remotePath, err)
	}
	return buf.Bytes(), nil
}

func (s *sshSession) MkdirAll(_ context.Context, remotePath string) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("remote: new ssh session for mkdir: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(fmt.Sprintf("mkdir -p %s", shellQuote(remotePath)))
	if err != nil {
		return fmt.Errorf("remote: mkdir %s: %w: %s", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *sshSession) RemoveAll(_ context.Context, remotePath string) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("remote: new ssh session for remove: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(fmt.Sprintf("rm -rf %s", shellQuote(remotePath)))
	if err != nil {
		return fmt.Errorf("remote: rm -rf %s: %w: %s", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *sshSession) Close() error { return s.client.Close() }

// shellQuote wraps a path in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

var _ runner.Adapter = (*Adapter)(nil)
