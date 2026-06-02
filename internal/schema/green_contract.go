package schema

// GreenLevel is the highest verification tier achieved by a capsule run.
// Tier progression: targeted_tests < package < workspace < merge_ready.
// merge_ready is computed by the reconciler; it is not annotated on gates.
// orca.md Phase B §4.
type GreenLevel string

const (
	GreenLevelNone          GreenLevel = ""
	GreenLevelTargetedTests GreenLevel = "targeted_tests"
	GreenLevelPackage       GreenLevel = "package"
	GreenLevelWorkspace     GreenLevel = "workspace"
	GreenLevelMergeReady    GreenLevel = "merge_ready"
)

// GreenEvidence links one passing annotated gate to its evidence artifact.
type GreenEvidence struct {
	GateName   string `json:"gate_name"`
	Tier       string `json:"tier"`
	EvidenceID string `json:"evidence_id"`
}

// GreenContract captures the highest verification tier achieved and the
// evidence supporting it. Computed by the verifier from annotated gates.
// merge_ready is elevated by the reconciler when all obligations are satisfied
// and no stale-base or CI-failure blockers exist. orca.md Phase B §4.
type GreenContract struct {
	ObservedGreenLevel GreenLevel      `json:"observed_green_level"`
	Evidence           []GreenEvidence `json:"evidence"`
	// MergeReadyBlocker describes why merge_ready was not achieved despite
	// workspace green level. Populated by the reconciler.
	MergeReadyBlocker string `json:"merge_ready_blocker,omitempty"`
}
