package schema

import "time"

// EvidenceType classifies what kind of verification an evidence artifact records.
// Additional types (static_analysis_result, manual_review, agent_review,
// runtime_trace, reproduction_log, mutation_survivor, security_scan) are
// deferred; add a type when a verification gate requires it. orca.md §5.6.
type EvidenceType string

const (
	EvidenceTestResult      EvidenceType = "test_result"
	EvidenceLintResult      EvidenceType = "lint_result"
	EvidenceTypecheckResult EvidenceType = "typecheck_result"
	EvidenceDiffRiskReport  EvidenceType = "diff_risk_report"
	// EvidenceAgentOutput is a raw agent transcript or log file. It does not
	// assert any command exit code and must not satisfy gate checks that require
	// test_result, lint_result, or typecheck_result evidence.
	EvidenceAgentOutput EvidenceType = "agent_output"
	// Phase 4 types — add only these three; others remain deferred.
	EvidenceStaticAnalysis EvidenceType = "static_analysis_result"
	EvidenceMutationResult EvidenceType = "mutation_survivor"
	EvidenceAgentReview    EvidenceType = "agent_review"
)

// EvidenceArtifact proves, weakens, or contextualizes a claim or patch.
// orca.md §5.6.
type EvidenceArtifact struct {
	EvidenceID string       `json:"evidence_id"`
	Type       EvidenceType `json:"type"`
	Source     string       `json:"source"`
	Command    string       `json:"command"`
	ExitCode   int          `json:"exit_code"`
	Summary    string       `json:"summary"`
	// RawLogPath is a path to the full command output, or empty when inline_output is used.
	RawLogPath string `json:"raw_log_path"`
	// InlineOutput holds short output directly when RawLogPath is empty.
	InlineOutput string `json:"inline_output,omitempty"`
	// Supports lists obligation IDs this evidence helps satisfy.
	Supports []string `json:"supports"`
	// Weakens lists obligation IDs this evidence undermines.
	Weakens          []string  `json:"weakens"`
	ContentHash      string    `json:"content_hash"`
	ReuseKey         string    `json:"reuse_key"`
	ValidatedAgainst string    `json:"validated_against"`
	ReusedFromID     string    `json:"reused_from_id"`
	CreatedAt        time.Time `json:"created_at"`
}
