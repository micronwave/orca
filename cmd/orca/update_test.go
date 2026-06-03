package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestRun_UpdateSubcommandDispatches(t *testing.T) {
	t.Chdir(t.TempDir())
	err := run([]string{"update"})
	if err == nil {
		t.Fatal("run('update') = nil, want error outside source tree")
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run('update') returned unknown-command error: %v", err)
	}
}

func TestWalkForOrcaModFindsModuleRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	goMod := "module " + orcaModule + "\n\ngo 1.25.0\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if got := walkForOrcaMod(nested); got != root {
		t.Fatalf("walkForOrcaMod(%q) = %q, want %q", nested, got, root)
	}
}

func TestTempUpdateBinaryPathUsesExecutableDirectory(t *testing.T) {
	exeName := "orca"
	if goruntime.GOOS == "windows" {
		exeName = "orca.exe"
	}
	exePath := filepath.Join(t.TempDir(), exeName)
	tmpPath, err := tempUpdateBinaryPath(exePath)
	if err != nil {
		t.Fatalf("tempUpdateBinaryPath: %v", err)
	}
	if filepath.Dir(tmpPath) != filepath.Dir(exePath) {
		t.Fatalf("temp path dir = %q, want %q", filepath.Dir(tmpPath), filepath.Dir(exePath))
	}
	if filepath.Ext(tmpPath) != filepath.Ext(exePath) {
		t.Fatalf("temp path ext = %q, want %q", filepath.Ext(tmpPath), filepath.Ext(exePath))
	}
}

func TestInstallUpdatedBinaryRenamesOnNonWindows(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("non-Windows rename semantics only")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "orca-new")
	dst := filepath.Join(dir, "orca")
	if err := os.WriteFile(src, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	if err := installUpdatedBinary(src, dst); err != nil {
		t.Fatalf("installUpdatedBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new binary" {
		t.Fatalf("dst content = %q, want new binary", string(got))
	}
}

func TestWindowsReplaceScriptEscapesQuotedPaths(t *testing.T) {
	src := `C:\Program Files\orca's\orca-new.exe`
	dst := `C:\Program Files\orca's\orca.exe`
	script := windowsReplaceScript(src, dst)
	for _, want := range []string{
		"$src='C:\\Program Files\\orca''s\\orca-new.exe'",
		"$dst='C:\\Program Files\\orca''s\\orca.exe'",
		"Move-Item -LiteralPath $src -Destination $dst -Force",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("windowsReplaceScript missing %q:\n%s", want, script)
		}
	}
}
