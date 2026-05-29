package schema

// ClaimType classifies the nature of a claim artifact. orca.md §5.8.
type ClaimType string

const (
	ClaimAssumption   ClaimType = "assumption"
	ClaimInvariant    ClaimType = "invariant"
	ClaimExclusion    ClaimType = "exclusion"
	ClaimOpenQuestion ClaimType = "open_question"
	ClaimRisk         ClaimType = "risk"
	ClaimTestGap      ClaimType = "test_gap"
)

// ClaimStatus tracks the trust level of a claim.
type ClaimStatus string

const (
	// ClaimProposed means an agent reported it; not yet verified.
	ClaimProposed ClaimStatus = "proposed"
	// ClaimVerified means supported by a tool, independent agent, or human.
	ClaimVerified ClaimStatus = "verified"
	// ClaimStale means the affected code changed since the claim was validated.
	ClaimStale       ClaimStatus = "stale"
	ClaimContested   ClaimStatus = "contested"
	ClaimInvalidated ClaimStatus = "invalidated"
)

// ClaimArtifact replaces the old EpistemicResidual as the durable memory unit.
// Only verified claims may be injected as facts into context projections;
// proposed and stale claims must carry labels. orca.md §5.8.
type ClaimArtifact struct {
	ClaimID         string      `json:"claim_id"`
	Text            string      `json:"text"`
	ClaimType       ClaimType   `json:"claim_type"`
	SourceCapsuleID string      `json:"source_capsule_id"`
	AffectedFiles   []string    `json:"affected_files"`
	AffectedSymbols []string    `json:"affected_symbols"`
	Status          ClaimStatus `json:"status"`
	EvidenceIDs     []string    `json:"evidence_ids"`
	Contradicts     []string    `json:"contradicts,omitempty"`
	Invalidates     []string    `json:"invalidates,omitempty"`
	// LastValidatedAgainst is the state_snapshot_id current when this claim was last checked.
	LastValidatedAgainst string   `json:"last_validated_against"`
	ContradictedBy       []string `json:"contradicted_by"`
	InvalidatedBy        []string `json:"invalidated_by"`

	// GoalID is the goal this claim was created for. Empty means repo-scoped:
	// the claim persists across all goals and is available to every future run.
	// Goal-scoped claims (non-empty GoalID) are discarded when the goal closes.
	GoalID string `json:"goal_id,omitempty"`

	// InjectionKeywords, if non-empty, gates injection: the claim is included
	// only when at least one keyword appears in the current goal text or
	// obligation descriptions. Empty means no keyword gate.
	InjectionKeywords []string `json:"injection_keywords,omitempty"`

	// InjectionConditions are structured predicates evaluated against the current
	// capsule state before injection. Supported forms: "risk=high", "role=reviewer",
	// "topology=implementer_reviewer". Empty means no condition beyond keyword matching.
	InjectionConditions []string `json:"injection_conditions,omitempty"`

	// Confidence is the reliability score at creation time: 1.0 for claims sourced
	// from a verified gate result, 0.7 for agent sidecar output, 0.3 for transcript
	// extraction. Used to rank claims when the token budget is tight.
	Confidence float32 `json:"confidence,omitempty"`

	// SupersededBy, if non-empty, is the ID of the artifact (claim, patch, or capsule)
	// that replaced this claim. The projector skips claims with a non-empty SupersededBy.
	SupersededBy string `json:"superseded_by,omitempty"`
}
