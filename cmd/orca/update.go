package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/micronwave/orca/internal/ui"
)

const orcaModule = "github.com/micronwave/orca"

// runUpdate implements `orca update`: rebuilds the current orca executable from
// a local source checkout and replaces the installed binary in place.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("orca update", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	srcRoot, err := findOrcaSourceRoot()
	if err != nil {
		return fmt.Errorf("orca update: %w", err)
	}

	goExe, err := findGoExecutable()
	if err != nil {
		return fmt.Errorf("orca update: %w", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("orca update: locate current executable: %w", err)
	}

	tmpBinary, err := tempUpdateBinaryPath(exePath)
	if err != nil {
		return fmt.Errorf("orca update: prepare temp binary path: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpBinary)
		}
	}()

	fmt.Fprintf(os.Stdout, "%s %s\n", ui.IconOrca, ui.Colorize(os.Stdout, ui.OrcaBlue+ui.Bold, "Orca  update"))
	fmt.Fprintf(os.Stdout, "%s Rebuilding from %s\n", ui.IconHammer, ui.Colorize(os.Stdout, ui.Cyan, srcRoot))
	if err := buildUpdatedBinary(goExe, srcRoot, tmpBinary); err != nil {
		return fmt.Errorf("orca update: %w", err)
	}

	if err := installUpdatedBinary(tmpBinary, exePath); err != nil {
		return fmt.Errorf("orca update: %w", err)
	}
	cleanupTmp = false

	if goruntime.GOOS == "windows" {
		fmt.Fprintf(os.Stdout, "%s %s\n",
			ui.Colorize(os.Stdout, ui.Green, ui.IconCheck),
			ui.Colorize(os.Stdout, ui.Green, "orca update staged successfully; the binary will be replaced after this process exits"),
		)
		return nil
	}

	fmt.Fprintf(os.Stdout, "%s %s\n", ui.Colorize(os.Stdout, ui.Green, ui.IconCheck), ui.Colorize(os.Stdout, ui.Green, "orca updated successfully"))
	return nil
}

func tempUpdateBinaryPath(exePath string) (string, error) {
	dir := filepath.Dir(exePath)
	base := filepath.Base(exePath)
	pattern := strings.TrimSuffix(base, filepath.Ext(base)) + "-update-*"
	if ext := filepath.Ext(base); ext != "" {
		pattern += ext
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

func buildUpdatedBinary(goExe, srcRoot, outPath string) error {
	cmd := exec.Command(goExe, "build", "-o", outPath, "./cmd/orca")
	cmd.Dir = srcRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	return nil
}

func installUpdatedBinary(srcPath, dstPath string) error {
	if goruntime.GOOS == "windows" {
		return handoffWindowsBinaryReplace(srcPath, dstPath)
	}
	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("replace %s: %w", dstPath, err)
	}
	return nil
}

func handoffWindowsBinaryReplace(srcPath, dstPath string) error {
	psExe, err := findWindowsPowerShell()
	if err != nil {
		return err
	}
	script := windowsReplaceScript(srcPath, dstPath)
	cmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start replacement helper: %w", err)
	}
	return cmd.Process.Release()
}

func findWindowsPowerShell() (string, error) {
	candidates := []string{
		"powershell.exe",
		"pwsh.exe",
		filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if p, err := exec.LookPath(candidate); err == nil {
			return p, nil
		}
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("powershell executable not found; cannot complete Windows self-update")
}

func windowsReplaceScript(srcPath, dstPath string) string {
	quotedSrc := powershellSingleQuote(srcPath)
	quotedDst := powershellSingleQuote(dstPath)
	return fmt.Sprintf(
		"$src='%s'; $dst='%s'; for ($i = 0; $i -lt 50; $i++) { Start-Sleep -Milliseconds 100; try { Move-Item -LiteralPath $src -Destination $dst -Force; exit 0 } catch { } }; exit 1",
		quotedSrc,
		quotedDst,
	)
}

func powershellSingleQuote(path string) string {
	return strings.ReplaceAll(path, "'", "''")
}

// findOrcaSourceRoot walks up from cwd and then from the running executable's
// directory, looking for a go.mod that declares module github.com/micronwave/orca.
func findOrcaSourceRoot() (string, error) {
	cwd, err := os.Getwd()
	if err == nil {
		if root := walkForOrcaMod(cwd); root != "" {
			return root, nil
		}
	}

	if exe, err := os.Executable(); err == nil {
		if root := walkForOrcaMod(filepath.Dir(exe)); root != "" {
			return root, nil
		}
	}

	return "", fmt.Errorf(
		"source root not found (no go.mod declaring module %s)\n"+
			"  Run from inside the orca source tree, or reinstall from the latest release assets.",
		orcaModule,
	)
}

// walkForOrcaMod walks up from dir until it finds a go.mod that declares the
// orca module, returning that directory. Returns "" if not found.
func walkForOrcaMod(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		modPath := filepath.Join(abs, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			for _, line := range bytes.Split(data, []byte("\n")) {
				trimmed := strings.TrimSpace(string(line))
				if trimmed == "module "+orcaModule {
					return abs
				}
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return ""
}

// findGoExecutable returns the path to the go binary, checking PATH first and
// then the standard Windows install location.
func findGoExecutable() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	const windowsDefault = `C:\Program Files\Go\bin\go.exe`
	if _, err := os.Stat(windowsDefault); err == nil {
		return windowsDefault, nil
	}
	return "", fmt.Errorf("go executable not found on PATH")
}
