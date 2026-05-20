package schema

import "time"

// VerifierVerdict is the outcome of checking one obligation against evidence.
// orca.md §11.
type VerifierVerdict string

const (
	VerdictSatisfied VerifierVerdict = "satisfied"
	VerdictFailed    VerifierVerdict = "failed"
	VerdictWaived    VerifierVerdict = "waived"
	VerdictBlocked   VerifierVerdict = "blocked"
)

// RecommendedAction is the verifier's merge/retry recommendation. orca.md §10.
type RecommendedAction string

const (
	ActionAccept      RecommendedAction = "accept"
	ActionRetry       RecommendedAction = "retry"
	ActionSplit       RecommendedAction = "split"
	ActionReject      RecommendedAction = "reject"
	ActionHumanReview RecommendedAction = "human_review"
)

// ObligationVerdict is the verifier's ruling on one obligation. orca.md §10.
type ObligationVerdict struct {
	ObligationID string          `json:"obligation_id"`
	Verdict      VerifierVerdict `json:"verdict"`
	// EvidenceIDs lists the evidence artifact IDs that support this verdict.
	EvidenceIDs []string `json:"evidence_ids"`
	Notes       string   `json:"notes"`
	// WaivedBy records the human approval or reason when verdict is "waived".
	WaivedBy string `json:"waived_by,omitempty"`
}

// VerifierResult maps evidence to obligations and produces a merge recommendation.
// The verifier's job is to define what evidence is needed AND decide whether
// available evidence satisfies obligations. orca.md §10.
type VerifierResult struct {
	VerifierResultID  string              `json:"verifier_result_id"`
	PatchID           string              `json:"patch_id"`
	CapsuleID         string              `json:"capsule_id"`
	ObligationResults []ObligationVerdict `json:"obligation_results"`
	// BlockingFailures lists obligation IDs or descriptions of failures that block merge.
	BlockingFailures        []string          `json:"blocking_failures"`
	FailureIDs              []string          `json:"failure_ids,omitempty"`
	Warnings                []string          `json:"warnings"`
	Invalidates             []string          `json:"invalidates,omitempty"`
	RecommendedAction       RecommendedAction `json:"recommended_action"`
	RecommendationRationale string            `json:"recommendation_rationale"`
	CreatedAt               time.Time         `json:"created_at"`
}
