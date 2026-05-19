package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/micronwave/orca/internal/schema"
)

func TestExtractFromTranscript(t *testing.T) {
	root := t.TempDir()
	transcriptPath := filepath.Join(root, "transcript.log")
	content := "" +
		"$ go test ./...\n" +
		"M internal/runner/service.go\n" +
		"claim verified: scope gate passed\n" +
		"assumption: go toolchain present\n" +
		"risk: flaky integration test\n" +
		"follow-up: add retries for flaky test\n" +
		"obligation OB-123 fulfilled\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	adapter := New(root, "codex")
	out, err := adapter.ExtractFromTranscript(context.Background(), &schema.ExecutionCapsule{CapsuleID: "CAP-1"}, transcriptPath)
	if err != nil {
		t.Fatalf("ExtractFromTranscript: %v", err)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "internal/runner/service.go" {
		t.Fatalf("FilesChanged = %v", out.FilesChanged)
	}
	if len(out.CommandsRun) != 1 || out.CommandsRun[0] != "go test ./..." {
		t.Fatalf("CommandsRun = %v", out.CommandsRun)
	}
	if len(out.ObligationsAddressed) != 1 || out.ObligationsAddressed[0] != "OB-123" {
		t.Fatalf("ObligationsAddressed = %v", out.ObligationsAddressed)
	}
	if len(out.EvidencePaths) != 1 || out.EvidencePaths[0] != transcriptPath {
		t.Fatalf("EvidencePaths = %v", out.EvidencePaths)
	}
}
