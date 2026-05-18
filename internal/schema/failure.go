package schema

// FailureType classifies what kind of failure a fingerprint records. orca.md §5.9.
type FailureType string

const (
	FailureTest      FailureType = "test"
	FailureLint      FailureType = "lint"
	FailureTypecheck FailureType = "typecheck"
	FailureRuntime   FailureType = "runtime"
	FailureMerge     FailureType = "merge"
	FailurePolicy    FailureType = "policy"
	FailureInfra     FailureType = "infra"
	FailureAgent     FailureType = "agent"
)

// FailureFingerprint is a normalized failure record used to avoid repeated bad retries.
// Prior attempt history, likely cause inference, and recommended action generation
// are deferred to Phase 3. orca.md §5.9.
type FailureFingerprint struct {
	FailureID       string      `json:"failure_id"`
	FailureType     FailureType `json:"failure_type"`
	Summary         string      `json:"summary"`
	AffectedFiles   []string    `json:"affected_files"`
	AffectedSymbols []string    `json:"affected_symbols"`
	// ErrorSignature is a normalized string that identifies the failure pattern,
	// used for deduplication and retry routing.
	ErrorSignature string `json:"error_signature"`
}
