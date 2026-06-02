package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Directory paths relative to the .orca/ root, mirroring the filestore layout.
const (
	relGoals           = "state/goals"
	relObligations     = "state/obligations"
	relCapsules        = "state/capsules"
	relPatches         = "artifacts/patches"
	relEvidence        = "artifacts/evidence"
	relFailures        = "artifacts/failures"
	relDecisions       = "artifacts/decisions"
	relBudgets         = "artifacts/budgets"
	relVerifierResults = "artifacts/verifier_results"
	relCapsuleRuntime  = "state/capsule_runtime"
)

// readFile JSON-decodes the file at path into T.
// Returns nil, nil when the file does not exist.
func readFile[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// scanDir reads every *.json file in dir and JSON-decodes it into T.
// Returns an empty slice when the directory does not exist.
// Files that fail to decode are silently skipped.
func scanDir[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []T
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		v, err := readFile[T](filepath.Join(dir, entry.Name()))
		if err != nil || v == nil {
			continue
		}
		out = append(out, *v)
	}
	return out, nil
}

// orcaPath builds a path relative to the .orca directory.
func orcaPath(orcaDir, rel string) string {
	// Replace forward slashes with OS separator for Windows compatibility.
	parts := strings.Split(rel, "/")
	return filepath.Join(append([]string{orcaDir}, parts...)...)
}
