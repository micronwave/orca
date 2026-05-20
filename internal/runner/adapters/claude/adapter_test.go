package claude

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
		"$ go vet ./...\n" +
		"M internal/runner/adapters/claude/adapter.go\n" +
		"claim: verify unsafe path normalization | contradicts: CL-old | invalidates: CL-bad\n" +
		"assumption: git is available\n" +
		"risk: prompt injection in transcript parser\n" +
		"follow-up: add stricter parser tests\n" +
		"obligation OB-77 addressed\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	adapter := New(root, "claude")
	out, err := adapter.ExtractFromTranscript(context.Background(), &schema.ExecutionCapsule{CapsuleID: "CAP-1"}, transcriptPath)
	if err != nil {
		t.Fatalf("ExtractFromTranscript: %v", err)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "internal/runner/adapters/claude/adapter.go" {
		t.Fatalf("FilesChanged = %v", out.FilesChanged)
	}
	if len(out.CommandsRun) != 1 || out.CommandsRun[0] != "go vet ./..." {
		t.Fatalf("CommandsRun = %v", out.CommandsRun)
	}
	if len(out.ObligationsAddressed) != 1 || out.ObligationsAddressed[0] != "OB-77" {
		t.Fatalf("ObligationsAddressed = %v", out.ObligationsAddressed)
	}
	if len(out.EvidencePaths) != 1 || out.EvidencePaths[0] != transcriptPath {
		t.Fatalf("EvidencePaths = %v", out.EvidencePaths)
	}
	if len(out.Claims) != 1 || len(out.Claims[0].Contradicts) != 1 || out.Claims[0].Contradicts[0] != "CL-old" ||
		len(out.Claims[0].Invalidates) != 1 || out.Claims[0].Invalidates[0] != "CL-bad" {
		t.Fatalf("Claims = %+v, want preserved dispute edges", out.Claims)
	}
}
