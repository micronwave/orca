package schema

// PatchStatus is the lifecycle state of a patch artifact. orca.md §5.5.
type PatchStatus string

const (
	PatchCandidate  PatchStatus = "candidate"
	PatchAccepted   PatchStatus = "accepted"
	PatchRejected   PatchStatus = "rejected"
	PatchSuperseded PatchStatus = "superseded"
)

// PatchArtifact is the actual code change plus metadata. orca.md §5.5.
// Status remains "candidate" until VerifierResult is complete.
type PatchArtifact struct {
	PatchID              string      `json:"patch_id"`
	CapsuleID            string      `json:"capsule_id"`
	BaseCommit           string      `json:"base_commit"`
	ChangedFiles         []string    `json:"changed_files"`
	// DiffPath is a path to the diff file or "inline" when diff is embedded here.
	DiffPath             string      `json:"diff_path"`
	Summary              string      `json:"summary"`
	ObligationIDsClaimed []string    `json:"obligation_ids_claimed"`
	RiskNotes            []string    `json:"risk_notes"`
	Status               PatchStatus `json:"status"`
	// ScopeViolations lists any files changed outside AllowedPaths.
	ScopeViolations []string `json:"scope_violations"`
}

// RetryContract describes why a patch cannot merge and what the next capsule
// must address. Included in ProofCarryingPatch when merge is not recommended.
type RetryContract struct {
	Reason             string   `json:"reason"`
	FailedObligations  []string `json:"failed_obligations"`
	SuggestedAction    string   `json:"suggested_action"`
}

// ProofCarryingPatch is a PatchArtifact plus the evidence bundle needed to
// satisfy its obligations. A patch is not complete until the verifier can map
// evidence to obligations. orca.md §5.7.
type ProofCarryingPatch struct {
	Patch                PatchArtifact  `json:"patch"`
	ObligationsAddressed []string       `json:"obligations_addressed"`
	CommandsRun          []string       `json:"commands_run"`
	EvidenceArtifacts    []EvidenceArtifact `json:"evidence_artifacts"`
	FilesChanged         []string       `json:"files_changed"`
	Assumptions          []string       `json:"assumptions"`
	UnresolvedRisks      []string       `json:"unresolved_risks"`
	VerifierResult       VerifierResult `json:"verifier_result"`
	MergeRecommendation  RecommendedAction `json:"merge_recommendation"`
	// RetryContract is present only when MergeRecommendation is not "accept".
	RetryContract *RetryContract `json:"retry_contract,omitempty"`
}
