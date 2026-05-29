package schema

// SidecarClaimStatus distinguishes a verified claim from a proposed one
// inside agent sidecar output. orca.md §8.
type SidecarClaimStatus string

const (
	SidecarClaimVerified SidecarClaimStatus = "verified"
	SidecarClaimProposed SidecarClaimStatus = "proposed"
)

// SidecarClaim is a single claim as reported by the agent in sidecar output.
// Verified claims reference an evidence artifact; proposed claims do not.
type SidecarClaim struct {
	Claim string             `json:"claim"`
	Type  SidecarClaimStatus `json:"type"`
	// Evidence is an artifact reference (e.g. "evidence_artifacts.json#ev-test-run").
	Evidence    string   `json:"evidence,omitempty"`
	Contradicts []string `json:"contradicts,omitempty"`
	Invalidates []string `json:"invalidates,omitempty"`
}

// AgentSidecarOutput is the structured output an agent produces alongside its
// diff. Transcript extraction must produce the same schema when sidecar output
// is missing or malformed. orca.md §8.
//
// Both are alternate collection paths; downstream consumers must not be able
// to distinguish them.
//
// The nine fields from orca.md §8 are the required minimum. CapsuleID,
// EvidenceReferences, and Summary are intentional extensions beyond the spec
// minimum; they are emitted by Orca-native adapters and must be tolerated by
// validators (do not treat unknown fields as errors).
type AgentSidecarOutput struct {
	// --- orca.md §8 required minimum ---
	ObligationsAddressed []string       `json:"obligations_addressed"`
	FilesChanged         []string       `json:"files_changed"`
	CommandsRun          []string       `json:"commands_run"`
	Assumptions          []string       `json:"assumptions"`
	Claims               []SidecarClaim `json:"claims"`
	Risks                []string       `json:"risks"`
	// FollowUpNeeded lists items that require a follow-up capsule; empty means none.
	FollowUpNeeded []string `json:"follow_up_needed"`
	// EvidencePaths lists paths to evidence artifact files produced alongside this output.
	EvidencePaths []string `json:"evidence_paths"`

	// --- Orca extensions beyond the §8 minimum ---
	CapsuleID string `json:"capsule_id,omitempty"`
	// EvidenceReferences holds artifact store references (e.g. "evidence.json#ev-1"),
	// complementing EvidencePaths which holds file-system paths.
	EvidenceReferences []string `json:"evidence_references,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	// TokensUsed is set by the adapter after execution from the CLI's usage report —
	// it is infrastructure metadata, not something the agent itself reports.
	// Zero means the adapter could not determine a count.
	TokensUsed int `json:"tokens_used"`
	// WallTimeSeconds is measured by the adapter/runner around CLI execution.
	// Zero means the duration could not be determined.
	WallTimeSeconds float64 `json:"wall_time_seconds"`
	// ContradictedClaimIDs lists existing claim IDs that this agent output
	// completely supersedes. The reconciler sets SupersededBy on each listed
	// claim and emits a claim_superseded event for each.
	ContradictedClaimIDs []string `json:"contradicted_claim_ids,omitempty"`
}
