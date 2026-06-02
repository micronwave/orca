package schema

import "time"

// ProjectionRole distinguishes context projection types. orca.md §5.4.
type ProjectionRole string

const (
	ProjectionRoleExecutor     ProjectionRole = "executor"
	ProjectionRoleHumanSummary ProjectionRole = "human_summary"
	ProjectionRoleReviewer     ProjectionRole = "reviewer"
	ProjectionRoleTester       ProjectionRole = "tester"
)

// OmittedSection records a section removed during budget enforcement together
// with the reason it was omitted. Phase C §7.
type OmittedSection struct {
	Key    string `json:"key"`
	Reason string `json:"reason"` // "budget_exceeded" | "content_truncated" | "empty"
}

// ContextProjection is the base set of fields shared by all projection types.
// A projection is a deterministic, role-specific briefing compiled from the
// artifact graph — agents do not receive raw transcripts by default. orca.md §5.4.
type ContextProjection struct {
	ContextProjectionID string         `json:"context_projection_id"`
	Role                ProjectionRole `json:"role"`
	SourceArtifactIDs   []string       `json:"source_artifact_ids"`
	IncludedSections    []string       `json:"included_sections"`
	OmittedSections     []string       `json:"omitted_sections"`
	TokenBudget         int            `json:"token_budget"`
	// FreshnessBase is the state_snapshot_id this projection was compiled from.
	FreshnessBase string    `json:"freshness_base"`
	CreatedAt     time.Time `json:"created_at"`

	// Phase C §7: compaction metrics and stable hashes.

	// SourceHash is SHA-256 of sorted(SourceArtifactIDs) + "|" + FreshnessBase.
	// Stable across identical artifact graphs; changes when the snapshot changes.
	SourceHash string `json:"source_hash,omitempty"`
	// ContentHash is SHA-256 of the joined included section texts.
	// Two projections with the same SourceHash must have the same ContentHash.
	ContentHash string `json:"content_hash,omitempty"`
	// TokensBefore is the estimated token count before budget enforcement.
	TokensBefore int `json:"tokens_before,omitempty"`
	// TokensAfter is the estimated token count after budget enforcement.
	TokensAfter int `json:"tokens_after,omitempty"`
	// OmittedWithReasons carries the omitted sections with structured reasons,
	// complementing the legacy OmittedSections string slice.
	OmittedWithReasons []OmittedSection `json:"omitted_with_reasons,omitempty"`
	// ReuseKey is a stable cache key: "{role}|{sourceHash}".
	// Identical ReuseKey means the projection content is reproducible from cache.
	ReuseKey string `json:"reuse_key,omitempty"`
}

// ProjectionReuseRecord is saved when a previously compiled projection is
// reused instead of recomputed. Visible in status --raw. Phase C §7.
type ProjectionReuseRecord struct {
	ReuseID              string         `json:"reuse_id"`
	CapsuleID            string         `json:"capsule_id"`
	GoalID               string         `json:"goal_id"`
	Role                 ProjectionRole `json:"role"`
	SourceHash           string         `json:"source_hash"`
	OriginalProjectionID string         `json:"original_projection_id"`
	// TokensSaved is the TokensAfter of the original projection — tokens that
	// were not spent because the projection was reused.
	TokensSaved int       `json:"tokens_saved"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// ConditionRef is a lightweight reference to a goal condition used inside
// HumanSummaryProjection. Internal IDs are secondary; descriptions are primary.
type ConditionRef struct {
	ConditionID string `json:"condition_id"`
	Description string `json:"description"`
}

// ObligationRef is a lightweight reference to an obligation used inside
// HumanSummaryProjection.
type ObligationRef struct {
	ObligationID string    `json:"obligation_id"`
	Description  string    `json:"description"`
	RiskLevel    RiskLevel `json:"risk_level"`
}

// ExpectedFileScope declares the files a capsule expects to touch. orca.md §5.4.2.
type ExpectedFileScope struct {
	ToRead   []string `json:"to_read"`
	ToWrite  []string `json:"to_write"`
	ToCreate []string `json:"to_create"`
}

// TopologyDecision records which topology the classifier chose and why. orca.md §5.4.2.
type TopologyDecision struct {
	Selected  Topology `json:"selected"`
	Rationale string   `json:"rationale"`
}

// DesignDecision records an architectural or implementation choice the context
// compiler inferred from the obligation set, codebase state, and verified claims.
type DesignDecision struct {
	Decision               string   `json:"decision"`
	AlternativesConsidered []string `json:"alternatives_considered"`
	Reason                 string   `json:"reason"`
}

// PreExecutionRisk is a risk known before the agent runs, derived from obligation
// risk levels, failure fingerprints, and scope. orca.md §5.4.2.
type PreExecutionRisk struct {
	Description string `json:"description"`
	// Source is one of: obligation_risk, failure_fingerprint, scope, claim.
	Source string `json:"source"`
}

// EvidencePlan describes what the verifier will check after the capsule completes.
type EvidencePlan struct {
	VerifierGates  []string `json:"verifier_gates"`
	TestsToRun     []string `json:"tests_to_run"`
	StaticChecks   []string `json:"static_checks"`
	AdvancedChecks []string `json:"advanced_checks,omitempty"`
}

// ProjectionBudget is the token and time budget declared in a human summary projection.
type ProjectionBudget struct {
	MaxTokens          int `json:"max_tokens"`
	MaxWallTimeSeconds int `json:"max_wall_time_seconds"`
}

// HumanSummaryProjection is the developer-facing implementation briefing emitted
// before the capsule runner launches the agent. It is a pre-execution design
// document, not a post-hoc diff summary. orca.md §5.4.2.
//
// The executor_projection and human_summary_projection are always two separate
// documents — merging them would bloat agent context or leave the developer
// without go/no-go information.
type HumanSummaryProjection struct {
	ContextProjection
	GoalPlain              string             `json:"goal_plain"`
	ConditionsAddressed    []ConditionRef     `json:"conditions_addressed"`
	ObligationsAddressed   []ObligationRef    `json:"obligations_addressed"`
	ImplementationApproach string             `json:"implementation_approach"`
	ExpectedFileScope      ExpectedFileScope  `json:"expected_file_scope"`
	ExplicitExclusions     []string           `json:"explicit_exclusions"`
	Topology               TopologyDecision   `json:"topology"`
	DesignDecisions        []DesignDecision   `json:"design_decisions"`
	PreExecutionRisks      []PreExecutionRisk `json:"pre_execution_risks"`
	EvidencePlan           EvidencePlan       `json:"evidence_plan"`
	Budget                 ProjectionBudget   `json:"budget"`
	RequiredApprovals      []string           `json:"required_approvals"`
}
