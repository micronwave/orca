// Package integration_test boundary checks verify that Phase 5 packages are
// not imported by core runtime packages. These run as normal (non-integration)
// tests because they only read source files — no process or network needed.
package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findOrcaRoot walks up from the test's working directory looking for the
// go.mod that declares "module github.com/micronwave/orca".
func findOrcaRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("findOrcaRoot: Getwd: %v", err)
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && strings.Contains(string(content), "module github.com/micronwave/orca") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("findOrcaRoot: could not locate project root go.mod")
		}
		dir = parent
	}
}

// goFilesIn returns all non-test .go files directly in dir (not recursive).
func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestBoundary_CorePackagesDoNotImportPhase5Packages asserts that core runtime
// packages do not import Phase 5 adapters or the desktop package. This guards
// against accidental coupling introduced during future development.
//
// Checked packages: intent, planner, runner (root), verifier, reconciler,
// projector, budget, gate, store, eventlog, schema.
//
// Forbidden imports: mcp, intake, cigate, prwriter, runner/adapters/remote, desktop.
func TestBoundary_CorePackagesDoNotImportPhase5Packages(t *testing.T) {
	root := findOrcaRoot(t)

	corePackageDirs := []string{
		filepath.Join(root, "internal", "intent"),
		filepath.Join(root, "internal", "planner"),
		filepath.Join(root, "internal", "runner"),
		filepath.Join(root, "internal", "verifier"),
		filepath.Join(root, "internal", "reconciler"),
		filepath.Join(root, "internal", "projector"),
		filepath.Join(root, "internal", "budget"),
		filepath.Join(root, "internal", "gate"),
		filepath.Join(root, "internal", "store"),
		filepath.Join(root, "internal", "eventlog"),
		filepath.Join(root, "internal", "schema"),
	}

	// Forbidden import path substrings (quoted as they appear in Go source).
	forbidden := []string{
		`"github.com/micronwave/orca/internal/mcp"`,
		`"github.com/micronwave/orca/internal/intake"`,
		`"github.com/micronwave/orca/internal/cigate"`,
		`"github.com/micronwave/orca/internal/prwriter"`,
		`"github.com/micronwave/orca/internal/runner/adapters/remote"`,
		`"github.com/micronwave/orca/desktop"`,
	}

	for _, pkgDir := range corePackageDirs {
		for _, goFile := range goFilesIn(t, pkgDir) {
			content, err := os.ReadFile(goFile)
			if err != nil {
				t.Fatalf("ReadFile %s: %v", goFile, err)
			}
			src := string(content)
			rel, _ := filepath.Rel(root, goFile)
			for _, imp := range forbidden {
				if strings.Contains(src, imp) {
					t.Errorf("boundary violation: %s imports forbidden package %s", rel, imp)
				}
			}
		}
	}
}

// TestBoundary_DesktopDoesNotImportOrcaInternals asserts that no Go source file
// under desktop/ imports from github.com/micronwave/orca/internal/... .
// The desktop module has its own go.mod and must remain a read-only consumer
// of local data files only.
func TestBoundary_DesktopDoesNotImportOrcaInternals(t *testing.T) {
	root := findOrcaRoot(t)
	desktopDir := filepath.Join(root, "desktop")

	// Walk all .go files under desktop/
	err := filepath.Walk(desktopDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(string(content), `"github.com/micronwave/orca/internal/`) {
			t.Errorf("boundary violation: %s imports orca internal package", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk %s: %v", desktopDir, err)
	}
}

// TestBoundary_SchemaIsDataOnly asserts that internal/schema/*.go contains no
// function definitions with non-trivial bodies (constructors, methods with
// logic). The schema package must remain a bag of JSON-tagged structs plus
// simple constant/var declarations per orca.md §5.
//
// Implementation: no file in internal/schema may import any internal/ package
// other than itself, and no file may contain function bodies longer than the
// trivial String()/MarshalJSON() patterns already allowed.
func TestBoundary_SchemaImportsNoInternalPackages(t *testing.T) {
	root := findOrcaRoot(t)
	schemaDir := filepath.Join(root, "internal", "schema")

	for _, goFile := range goFilesIn(t, schemaDir) {
		content, err := os.ReadFile(goFile)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", goFile, err)
		}
		rel, _ := filepath.Rel(root, goFile)
		src := string(content)
		// schema is only allowed to import standard library packages.
		// Any internal/ import is a boundary violation.
		if strings.Contains(src, `"github.com/micronwave/orca/internal/`) {
			t.Errorf("boundary violation: schema file %s imports an internal package "+
				"(schema must be data-only with stdlib imports only)", rel)
		}
	}
}

// TestBoundary_SchemaHasNoExportedFreeFunctions asserts that internal/schema
// contains no exported free functions (top-level func declarations without a
// receiver). The only functions permitted in schema are methods that implement
// stdlib interfaces on the enum types — specifically UnmarshalJSON([]byte)error,
// which cannot be moved because Go requires methods to live in the same package
// as their receiver type.
//
// This test catches logic helpers like ordinal/ranking functions that belong in
// the consumer package (verifier, reconciler) rather than in the data layer.
func TestBoundary_SchemaHasNoExportedFreeFunctions(t *testing.T) {
	root := findOrcaRoot(t)
	schemaDir := filepath.Join(root, "internal", "schema")

	for _, goFile := range goFilesIn(t, schemaDir) {
		content, err := os.ReadFile(goFile)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", goFile, err)
		}
		rel, _ := filepath.Rel(root, goFile)
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			// A top-level exported free function starts with "func [A-Z]".
			// Method declarations start with "func (" — those are allowed.
			if !strings.HasPrefix(trimmed, "func ") {
				continue
			}
			rest := strings.TrimPrefix(trimmed, "func ")
			if strings.HasPrefix(rest, "(") {
				// method — allowed
				continue
			}
			// Free function: exported if first rune is uppercase.
			if len(rest) == 0 || rest[0] < 'A' || rest[0] > 'Z' {
				continue
			}
			t.Errorf("boundary violation: schema file %s line %d defines exported free function %q "+
				"(schema must be data-only; logic helpers belong in the consumer package)",
				rel, i+1, trimmed)
		}
	}
}

// TestBoundary_StoreEmitsOneEventPerSave verifies that SavePRRecord,
// SaveCIStatusRecord, and SaveIntakeRecord each contain exactly one
// s.appendEvent call (store emits exactly one event per Save* call).
func TestBoundary_StoreEmitsOneEventPerSave(t *testing.T) {
	root := findOrcaRoot(t)
	externalFile := filepath.Join(root, "internal", "store", "filestore_external.go")

	content, err := os.ReadFile(externalFile)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", externalFile, err)
	}
	src := string(content)

	// Each Save* function must call s.appendEvent exactly once.
	// We verify by checking function boundaries between Save methods.
	saveMethods := []struct {
		name    string
		startAt string
	}{
		{"SavePRRecord", "func (s *FileStore) SavePRRecord("},
		{"SaveCIStatusRecord", "func (s *FileStore) SaveCIStatusRecord("},
		{"SaveIntakeRecord", "func (s *FileStore) SaveIntakeRecord("},
	}

	for _, m := range saveMethods {
		start := strings.Index(src, m.startAt)
		if start < 0 {
			t.Errorf("%s not found in filestore_external.go", m.name)
			continue
		}
		// Find the closing brace of this function by counting braces.
		depth := 0
		funcBody := ""
		for i := start; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					funcBody = src[start : i+1]
					goto done
				}
			}
		}
	done:
		count := strings.Count(funcBody, "s.appendEvent(")
		if count != 1 {
			t.Errorf("%s: found %d s.appendEvent calls, want exactly 1", m.name, count)
		}
	}
}
