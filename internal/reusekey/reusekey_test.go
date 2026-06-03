package reusekey_test

import (
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/reusekey"
)

func TestNormalizeWorkingDir_empty(t *testing.T) {
	t.Helper()
	got := reusekey.NormalizeWorkingDir("")
	if got == "" {
		t.Fatal("NormalizeWorkingDir(\"\") returned empty string; want non-empty (resolved absolute path or \".\")")
	}
}

func TestNormalizeWorkingDir_whitespaceOnly(t *testing.T) {
	got := reusekey.NormalizeWorkingDir("   ")
	if got == "" {
		t.Fatal("NormalizeWorkingDir(whitespace) returned empty string")
	}
}

func TestNormalizeWorkingDir_windowsPath(t *testing.T) {
	// Backslashes are normalized to forward slashes; drive letter is lowercased.
	got := reusekey.NormalizeWorkingDir(`C:\foo\bar`)
	if !strings.HasPrefix(got, "c:/") {
		t.Errorf("NormalizeWorkingDir(`C:\\foo\\bar`) = %q; want prefix \"c:/\"", got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("NormalizeWorkingDir(`C:\\foo\\bar`) = %q; want no backslashes", got)
	}
}

func TestNormalizeWorkingDir_driveLetterLowercased(t *testing.T) {
	got := reusekey.NormalizeWorkingDir(`E:\orca`)
	if len(got) < 1 || got[0] != 'e' {
		t.Errorf("NormalizeWorkingDir(`E:\\orca`) = %q; want drive letter lowercased (first char 'e')", got)
	}
}

func TestForVerifierGate_deterministic(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1", "OB-2"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1", "OB-2"}, "SNAP-1")
	if k1 != k2 {
		t.Errorf("identical inputs produced different keys:\n  k1=%q\n  k2=%q", k1, k2)
	}
}

func TestForVerifierGate_obligationsSorted(t *testing.T) {
	k1 := reusekey.ForVerifierGate("lint_result", "go vet ./...", `E:\orca`, []string{"OB-A", "OB-B", "OB-C"}, "SNAP-x")
	k2 := reusekey.ForVerifierGate("lint_result", "go vet ./...", `E:\orca`, []string{"OB-C", "OB-A", "OB-B"}, "SNAP-x")
	if k1 != k2 {
		t.Errorf("different obligation orderings produced different keys:\n  sorted=%q\n  unsorted=%q", k1, k2)
	}
}

func TestForVerifierGate_commandWhitespaceNormalized(t *testing.T) {
	k1 := reusekey.ForVerifierGate("lint_result", "go  vet  ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("lint_result", "go vet ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	if k1 != k2 {
		t.Errorf("extra internal whitespace in command produced different keys:\n  k1=%q\n  k2=%q", k1, k2)
	}
}

func TestForVerifierGate_differsOnEvidenceType(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("lint_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	if k1 == k2 {
		t.Error("different evidence types produced identical keys")
	}
}

func TestForVerifierGate_differsOnCommand(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("test_result", "go test ./internal/...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	if k1 == k2 {
		t.Error("different commands produced identical keys")
	}
}

func TestForVerifierGate_differsOnWorkingDir(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\other`, []string{"OB-1"}, "SNAP-1")
	if k1 == k2 {
		t.Error("different working dirs produced identical keys")
	}
}

func TestForVerifierGate_differsOnObligations(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-2"}, "SNAP-1")
	if k1 == k2 {
		t.Error("different obligation refs produced identical keys")
	}
}

func TestForVerifierGate_differsOnSnapshot(t *testing.T) {
	k1 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-1")
	k2 := reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, []string{"OB-1"}, "SNAP-2")
	if k1 == k2 {
		t.Error("different snapshot IDs produced identical keys")
	}
}

func TestForVerifierGate_inputsNotMutated(t *testing.T) {
	obligations := []string{"OB-C", "OB-A", "OB-B"}
	original := make([]string, len(obligations))
	copy(original, obligations)

	reusekey.ForVerifierGate("test_result", "go test ./...", `E:\orca`, obligations, "SNAP-1")

	for i, v := range obligations {
		if v != original[i] {
			t.Errorf("ForVerifierGate mutated input obligations[%d]: got %q, want %q", i, v, original[i])
		}
	}
}
