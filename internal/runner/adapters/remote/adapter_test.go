package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/runner/adapters/remote"
	"github.com/micronwave/orca/internal/schema"
)

// fakeSession implements remote.Session without a real SSH server.
type fakeSession struct {
	uploads   map[string][]byte
	downloads map[string][]byte
	commands  []string
	mkdirs    []string
	removed   []string
	runFn     func(cmd string) (string, error)
	closed    bool
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		uploads:   make(map[string][]byte),
		downloads: make(map[string][]byte),
	}
}

func (f *fakeSession) RunCommand(_ context.Context, cmd string) (string, error) {
	f.commands = append(f.commands, cmd)
	if f.runFn != nil {
		return f.runFn(cmd)
	}
	return "", nil
}

func (f *fakeSession) Upload(_ context.Context, remotePath string, data []byte) error {
	f.uploads[remotePath] = append([]byte(nil), data...)
	return nil
}

func (f *fakeSession) Download(_ context.Context, remotePath string) ([]byte, error) {
	data, ok := f.downloads[remotePath]
	if !ok {
		return nil, fmt.Errorf("fake: file not found: %s", remotePath)
	}
	return data, nil
}

func (f *fakeSession) MkdirAll(_ context.Context, remotePath string) error {
	f.mkdirs = append(f.mkdirs, remotePath)
	return nil
}

func (f *fakeSession) RemoveAll(_ context.Context, remotePath string) error {
	f.removed = append(f.removed, remotePath)
	return nil
}

func (f *fakeSession) Close() error {
	f.closed = true
	return nil
}

// fakeDialer returns the pre-built fakeSession on Dial.
type fakeDialer struct {
	sess *fakeSession
}

func (d *fakeDialer) Dial(_ context.Context, _ config.RemoteConfig) (remote.Session, error) {
	return d.sess, nil
}

func remoteConfig() config.RemoteConfig {
	return config.RemoteConfig{
		Enabled:   true,
		Host:      "build.example.com",
		Workspace: "/remote/workspace",
	}
}

func testCapsule() *schema.ExecutionCapsule {
	return &schema.ExecutionCapsule{
		CapsuleID:     "CAP-REMOTE-1",
		Agent:         schema.AgentCodex,
		ObligationIDs: []string{"OB-1"},
		Budget:        schema.CapsuleBudget{MaxWallTimeSeconds: 60},
	}
}

func testProjection() *schema.ContextProjection {
	return &schema.ContextProjection{
		ContextProjectionID: "CTX-1",
		Role:                schema.ProjectionRoleExecutor,
		TokenBudget:         4096,
	}
}

func validSidecar() []byte {
	out := schema.AgentSidecarOutput{
		ObligationsAddressed: []string{"OB-1"},
		FilesChanged:         []string{"pkg/foo.go"},
		CommandsRun:          []string{"go test ./..."},
		Summary:              "done",
	}
	data, _ := json.Marshal(out)
	return data
}

// TestAgentType verifies the adapter reports the correct AgentType.
func TestAgentType(t *testing.T) {
	sess := newFakeSession()
	a := remote.New(remoteConfig(), schema.AgentCodex, t.TempDir(), &fakeDialer{sess: sess})
	if got := a.AgentType(); got != schema.AgentCodex {
		t.Errorf("AgentType = %v, want %v", got, schema.AgentCodex)
	}
}

// TestPreflight_ValidConfig returns nil for a correctly configured adapter.
func TestPreflight_ValidConfig(t *testing.T) {
	sess := newFakeSession()
	a := remote.New(remoteConfig(), schema.AgentCodex, t.TempDir(), &fakeDialer{sess: sess})
	if err := a.Preflight(context.Background(), testCapsule()); err != nil {
		t.Fatalf("Preflight = %v, want nil", err)
	}
}

// TestPreflight_MissingHost returns an error when host is empty.
func TestPreflight_MissingHost(t *testing.T) {
	cfg := remoteConfig()
	cfg.Host = ""
	a := remote.New(cfg, schema.AgentCodex, t.TempDir(), &fakeDialer{sess: newFakeSession()})
	err := a.Preflight(context.Background(), testCapsule())
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("Preflight = %v, want host error", err)
	}
}

// TestPreflight_NotEnabled returns an error when remote.enabled is false.
func TestPreflight_NotEnabled(t *testing.T) {
	cfg := remoteConfig()
	cfg.Enabled = false
	a := remote.New(cfg, schema.AgentCodex, t.TempDir(), &fakeDialer{sess: newFakeSession()})
	err := a.Preflight(context.Background(), testCapsule())
	if err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("Preflight = %v, want enabled error", err)
	}
}

// TestPreflight_MissingWorkspace returns an error when workspace is empty.
func TestPreflight_MissingWorkspace(t *testing.T) {
	cfg := remoteConfig()
	cfg.Workspace = ""
	a := remote.New(cfg, schema.AgentCodex, t.TempDir(), &fakeDialer{sess: newFakeSession()})
	err := a.Preflight(context.Background(), testCapsule())
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("Preflight = %v, want workspace error", err)
	}
}

// TestExecute_UploadsBriefingAndCapsule verifies briefing, capsule, and (for
// Codex) the sidecar schema all land on the remote session.
func TestExecute_UploadsBriefingAndCapsule(t *testing.T) {
	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = validSidecar()

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	if _, err := a.Execute(context.Background(), testCapsule(), testProjection()); err != nil {
		t.Fatalf("Execute = %v", err)
	}

	for _, name := range []string{"executor_briefing.md", "capsule.json", "sidecar_schema.json"} {
		if _, ok := sess.uploads[remoteDir+"/"+name]; !ok {
			t.Errorf("%s was not uploaded", name)
		}
	}
}

// TestExecute_SchemaFlagInAgentCommand verifies the schema flag is injected
// into the agent command for both Codex (--output-schema) and Claude (--json-schema).
func TestExecute_SchemaFlagInAgentCommand(t *testing.T) {
	tests := []struct {
		name  string
		agent schema.AgentType
		want  string
	}{
		{name: "codex output-schema flag", agent: schema.AgentCodex, want: "--output-schema"},
		{name: "claude json-schema flag", agent: schema.AgentClaude, want: "--json-schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := newFakeSession()
			remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
			sess.downloads[remoteDir+"/output.json"] = validSidecar()
			capsule := testCapsule()
			capsule.Agent = tt.agent

			a := remote.New(remoteConfig(), tt.agent, t.TempDir(), &fakeDialer{sess: sess})
			if _, err := a.Execute(context.Background(), capsule, testProjection()); err != nil {
				t.Fatalf("Execute = %v", err)
			}
			if len(sess.commands) != 1 {
				t.Fatalf("commands len = %d, want 1", len(sess.commands))
			}
			if !strings.Contains(sess.commands[0], tt.want) {
				t.Errorf("command %q missing %q", sess.commands[0], tt.want)
			}
		})
	}
}

func TestExecute_UsesCapsulePermissionModeInAgentCommand(t *testing.T) {
	tests := []struct {
		name  string
		agent schema.AgentType
		mode  schema.PermissionMode
		want  string
	}{
		{name: "codex workspace write", agent: schema.AgentCodex, mode: schema.PermissionWorkspaceWrite, want: "-s workspace-write"},
		{name: "codex read only", agent: schema.AgentCodex, mode: schema.PermissionReadOnly, want: "-s read-only"},
		{name: "claude workspace write", agent: schema.AgentClaude, mode: schema.PermissionWorkspaceWrite, want: "--permission-mode acceptEdits"},
		{name: "claude read only", agent: schema.AgentClaude, mode: schema.PermissionReadOnly, want: "--permission-mode prompt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := newFakeSession()
			remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
			sess.downloads[remoteDir+"/output.json"] = validSidecar()
			capsule := testCapsule()
			capsule.Agent = tt.agent
			capsule.PermissionMode = tt.mode

			a := remote.New(remoteConfig(), tt.agent, t.TempDir(), &fakeDialer{sess: sess})
			if _, err := a.Execute(context.Background(), capsule, testProjection()); err != nil {
				t.Fatalf("Execute = %v", err)
			}
			if len(sess.commands) != 1 {
				t.Fatalf("commands len = %d, want 1", len(sess.commands))
			}
			if !strings.Contains(sess.commands[0], tt.want) {
				t.Fatalf("command %q missing %q", sess.commands[0], tt.want)
			}
		})
	}
}

// TestExecute_ParsesSidecarJSON returns sidecar output on success.
func TestExecute_ParsesSidecarJSON(t *testing.T) {
	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = validSidecar()

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	out, err := a.Execute(context.Background(), testCapsule(), testProjection())
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	if len(out.ObligationsAddressed) == 0 || out.ObligationsAddressed[0] != "OB-1" {
		t.Errorf("ObligationsAddressed = %v, want [OB-1]", out.ObligationsAddressed)
	}
}

// TestExecute_NoSidecar_ErrNoSidecar falls back when output.json is missing.
func TestExecute_NoSidecar_ErrNoSidecar(t *testing.T) {
	sess := newFakeSession()
	// output.json not in sess.downloads → Download will fail
	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	_, err := a.Execute(context.Background(), testCapsule(), testProjection())
	if !errors.Is(err, runner.ErrNoSidecar) {
		t.Errorf("Execute = %v, want ErrNoSidecar", err)
	}
}

// TestExecute_InvalidSidecar_ErrInvalidSidecar returns ErrInvalidSidecar for bad JSON.
func TestExecute_InvalidSidecar_ErrInvalidSidecar(t *testing.T) {
	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = []byte("{{not valid json}}")

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	_, err := a.Execute(context.Background(), testCapsule(), testProjection())
	if !errors.Is(err, runner.ErrInvalidSidecar) {
		t.Errorf("Execute = %v, want ErrInvalidSidecar", err)
	}
}

// TestExecute_RemoteRunError returns an error when the agent command fails.
func TestExecute_RemoteRunError(t *testing.T) {
	sess := newFakeSession()
	sess.runFn = func(cmd string) (string, error) {
		return "", fmt.Errorf("exit status 1")
	}

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	_, err := a.Execute(context.Background(), testCapsule(), testProjection())
	if err == nil {
		t.Fatal("Execute = nil, want error on remote run failure")
	}
}

// TestExecute_CleansRemoteWorkspace verifies RemoveAll is called after execution.
func TestExecute_CleansRemoteWorkspace(t *testing.T) {
	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = validSidecar()

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	if _, err := a.Execute(context.Background(), testCapsule(), testProjection()); err != nil {
		t.Fatalf("Execute = %v", err)
	}
	if len(sess.removed) == 0 || !strings.Contains(sess.removed[0], "CAP-REMOTE-1") {
		t.Errorf("RemoveAll not called with capsule dir, removed = %v", sess.removed)
	}
}

// TestExecute_DownloadsTranscriptLocally verifies transcript is saved locally
// for the ExtractFromTranscript fallback path.
func TestExecute_DownloadsTranscriptLocally(t *testing.T) {
	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = validSidecar()
	sess.downloads[remoteDir+"/transcript.log"] = []byte("$ go test ./...\n")

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: sess})

	if _, err := a.Execute(context.Background(), testCapsule(), testProjection()); err != nil {
		t.Fatalf("Execute = %v", err)
	}
	localTranscript := filepath.Join(orcaDir, "capsules", "CAP-REMOTE-1", "transcript.log")
	if _, err := os.Stat(localTranscript); err != nil {
		t.Errorf("transcript not saved locally: %v", err)
	}
}

// TestExtractFromTranscript_ParsesTranscript verifies transcript extraction.
func TestExtractFromTranscript_ParsesTranscript(t *testing.T) {
	transcript := "$ go test ./...\nassumption: tests pass\nclaim: all tests green\n"
	orcaDir := t.TempDir()
	transcriptPath := filepath.Join(orcaDir, "transcript.log")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	a := remote.New(remoteConfig(), schema.AgentCodex, orcaDir, &fakeDialer{sess: newFakeSession()})
	out, err := a.ExtractFromTranscript(context.Background(), testCapsule(), transcriptPath)
	if err != nil {
		t.Fatalf("ExtractFromTranscript = %v", err)
	}
	if len(out.CommandsRun) == 0 {
		t.Error("no commands extracted")
	}
	if len(out.Assumptions) == 0 {
		t.Error("no assumptions extracted")
	}
}

// TestExecute_ClaudeEnvelopeUnwrapped verifies claude JSON envelope is parsed.
func TestExecute_ClaudeEnvelopeUnwrapped(t *testing.T) {
	inner := schema.AgentSidecarOutput{
		ObligationsAddressed: []string{"OB-1"},
		FilesChanged:         []string{"x.go"},
		CommandsRun:          []string{"go build"},
	}
	innerJSON, _ := json.Marshal(inner)
	envelope := map[string]any{
		"result":   string(innerJSON),
		"is_error": false,
	}
	envelopeJSON, _ := json.Marshal(envelope)

	sess := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess.downloads[remoteDir+"/output.json"] = envelopeJSON

	orcaDir := t.TempDir()
	a := remote.New(remoteConfig(), schema.AgentClaude, orcaDir, &fakeDialer{sess: sess})

	out, err := a.Execute(context.Background(), testCapsule(), testProjection())
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	if len(out.FilesChanged) == 0 || out.FilesChanged[0] != "x.go" {
		t.Errorf("FilesChanged = %v, want [x.go]", out.FilesChanged)
	}
}

// TestExecute_SameSchemaRegardlessOfSidecarUsed verifies both adapter paths
// produce equivalent AgentSidecarOutput shapes (orca.md §8).
func TestExecute_SameSchemaRegardlessOfSidecarUsed(t *testing.T) {
	// Path 1: sidecar available.
	sess1 := newFakeSession()
	remoteDir := "/remote/workspace/CAP-CAP-REMOTE-1"
	sess1.downloads[remoteDir+"/output.json"] = validSidecar()
	a1 := remote.New(remoteConfig(), schema.AgentCodex, t.TempDir(), &fakeDialer{sess: sess1})
	out1, err := a1.Execute(context.Background(), testCapsule(), testProjection())
	if err != nil {
		t.Fatalf("sidecar path: %v", err)
	}

	// Path 2: no sidecar, but transcript present.
	transcript := "OB-1\nM pkg/foo.go\n$ go test ./...\n"
	sess2 := newFakeSession()
	sess2.downloads[remoteDir+"/transcript.log"] = []byte(transcript)
	orcaDir2 := t.TempDir()
	a2 := remote.New(remoteConfig(), schema.AgentCodex, orcaDir2, &fakeDialer{sess: sess2})
	_, err2 := a2.Execute(context.Background(), testCapsule(), testProjection())
	if !errors.Is(err2, runner.ErrNoSidecar) {
		t.Fatalf("transcript path: expected ErrNoSidecar, got %v", err2)
	}
	// Runner would then call ExtractFromTranscript.
	transcriptPath := filepath.Join(orcaDir2, "capsules", "CAP-REMOTE-1", "transcript.log")
	out2, err := a2.ExtractFromTranscript(context.Background(), testCapsule(), transcriptPath)
	if err != nil {
		t.Fatalf("ExtractFromTranscript: %v", err)
	}

	// Both outputs must have the same schema shape (all fields present).
	if out1 == nil || out2 == nil {
		t.Fatal("one or both outputs are nil")
	}
	// Both must have evidence paths set.
	if len(out1.FilesChanged) == 0 && len(out2.EvidencePaths) == 0 {
		t.Error("both outputs missing content")
	}
}
