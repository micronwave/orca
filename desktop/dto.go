package main

import "time"

// ── On-disk types — match the JSON stored in .orca/ exactly ──────────────────

type goalConditionDisk struct {
	ID                   string `json:"id"`
	Description          string `json:"description"`
	EffectiveDescription string `json:"effective_description"`
	Status               string `json:"status"`
}

type goalDisk struct {
	GoalID         string              `json:"goal_id"`
	OriginalIntent string              `json:"original_intent"`
	GoalConditions []goalConditionDisk `json:"goal_conditions"`
	RiskLevel      string              `json:"risk_level"`
	CreatedAt      time.Time           `json:"created_at"`
	Status         string              `json:"status"`
}

type obligationDisk struct {
	ObligationID     string   `json:"obligation_id"`
	GoalConditionID  string   `json:"goal_condition_id"`
	Description      string   `json:"description"`
	EvidenceRequired []string `json:"evidence_required"`
	Blocking         bool     `json:"blocking"`
	RiskLevel        string   `json:"risk_level"`
	Status           string   `json:"status"`
	SatisfiedBy      []string `json:"satisfied_by"`
}

type capsuleBudgetDisk struct {
	MaxTokens          int `json:"max_tokens"`
	MaxWallTimeSeconds int `json:"max_wall_time_seconds"`
	MaxRetries         int `json:"max_retries"`
}

type capsuleSandboxDisk struct {
	WorktreePath string `json:"worktree_path"`
	Network      string `json:"network"`
	WriteScope   string `json:"write_scope"`
}

type capsuleDisk struct {
	CapsuleID           string             `json:"capsule_id"`
	ObligationIDs       []string           `json:"obligation_ids"`
	Agent               string             `json:"agent"`
	Role                string             `json:"role"`
	ContextProjectionID string             `json:"context_projection_id"`
	Budget              capsuleBudgetDisk  `json:"budget"`
	Sandbox             capsuleSandboxDisk `json:"sandbox"`
	State               string             `json:"state"`
	TopologyDecisionID  string             `json:"topology_decision_id"`
}

type capsuleRuntimeDisk struct {
	Seq        int64     `json:"seq"`
	CapsuleID  string    `json:"capsule_id"`
	GoalID     string    `json:"goal_id"`
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	FailClass  string    `json:"failure_class,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

type patchDisk struct {
	PatchID              string   `json:"patch_id"`
	CapsuleID            string   `json:"capsule_id"`
	BaseCommit           string   `json:"base_commit"`
	ChangedFiles         []string `json:"changed_files"`
	DiffPath             string   `json:"diff_path"`
	Summary              string   `json:"summary"`
	ObligationIDsClaimed []string `json:"obligation_ids_claimed"`
	Status               string   `json:"status"`
	TokensUsed           int      `json:"tokens_used"`
	WallTimeSeconds      float64  `json:"wall_time_seconds"`
}

type evidenceDisk struct {
	EvidenceID   string    `json:"evidence_id"`
	Type         string    `json:"type"`
	Source       string    `json:"source"`
	Command      string    `json:"command"`
	ExitCode     int       `json:"exit_code"`
	Summary      string    `json:"summary"`
	RawLogPath   string    `json:"raw_log_path"`
	InlineOutput string    `json:"inline_output"`
	Supports     []string  `json:"supports"`
	Weakens      []string  `json:"weakens"`
	ReusedFromID string    `json:"reused_from_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type failureDisk struct {
	FailureID             string   `json:"failure_id"`
	SourceCapsuleID       string   `json:"source_capsule_id"`
	FailureType           string   `json:"failure_type"`
	Summary               string   `json:"summary"`
	AffectedFiles         []string `json:"affected_files"`
	AffectedSymbols       []string `json:"affected_symbols"`
	ErrorSignature        string   `json:"error_signature"`
	PriorAttemptCount     int      `json:"prior_attempt_count"`
	PriorCapsuleIDs       []string `json:"prior_capsule_ids"`
	RecommendedNextAction string   `json:"recommended_next_action"`
}

type decisionDisk struct {
	DecisionID string    `json:"decision_id"`
	Context    string    `json:"context"`
	Decision   string    `json:"decision"`
	Rationale  string    `json:"rationale"`
	MadeBy     string    `json:"made_by"`
	RelatedIDs []string  `json:"related_ids"`
	CreatedAt  time.Time `json:"created_at"`
}

type budgetDisk struct {
	BudgetID                string    `json:"budget_id"`
	GoalID                  string    `json:"goal_id"`
	CapsuleID               string    `json:"capsule_id"`
	ObligationID            string    `json:"obligation_id"`
	TokensSpent             int       `json:"tokens_spent"`
	WallTimeSeconds         float64   `json:"wall_time_seconds"`
	ToolCalls               int       `json:"tool_calls"`
	Retries                 int       `json:"retries"`
	DuplicatedFileReads     int       `json:"duplicated_file_reads"`
	OverlappingEdits        int       `json:"overlapping_edits"`
	ObligationsDischarged   int       `json:"obligations_discharged"`
	PatchesAccepted         int       `json:"patches_accepted"`
	PatchesRejected         int       `json:"patches_rejected"`
	EvidenceArtifactsReused int       `json:"evidence_artifacts_reused"`
	HumanInterventions      int       `json:"human_interventions"`
	CreatedAt               time.Time `json:"created_at"`
}

type obligationVerdictDisk struct {
	ObligationID string   `json:"obligation_id"`
	Verdict      string   `json:"verdict"`
	EvidenceIDs  []string `json:"evidence_ids"`
}

type verifierResultDisk struct {
	VerifierResultID        string                  `json:"verifier_result_id"`
	PatchID                 string                  `json:"patch_id"`
	CapsuleID               string                  `json:"capsule_id"`
	ObligationResults       []obligationVerdictDisk `json:"obligation_results"`
	BlockingFailures        []string                `json:"blocking_failures"`
	Warnings                []string                `json:"warnings"`
	RecommendedAction       string                  `json:"recommended_action"`
	RecommendationRationale string                  `json:"recommendation_rationale"`
	CreatedAt               time.Time               `json:"created_at"`
}

// ── View types returned to the frontend ──────────────────────────────────────

// GoalView is the frontend representation of a GoalIR.
type GoalView struct {
	GoalID         string          `json:"goal_id"`
	OriginalIntent string          `json:"original_intent"`
	Status         string          `json:"status"`
	RiskLevel      string          `json:"risk_level"`
	Conditions     []ConditionView `json:"conditions"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ConditionView is one goal condition for the frontend.
type ConditionView struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// ObligationView is the frontend representation of an Obligation.
type ObligationView struct {
	ObligationID    string   `json:"obligation_id"`
	GoalConditionID string   `json:"goal_condition_id"`
	Description     string   `json:"description"`
	Status          string   `json:"status"`
	Blocking        bool     `json:"blocking"`
	RiskLevel       string   `json:"risk_level"`
	SatisfiedBy     []string `json:"satisfied_by"`
}

// CapsuleView is the frontend representation of an ExecutionCapsule.
type CapsuleView struct {
	CapsuleID          string `json:"capsule_id"`
	Agent              string `json:"agent"`
	Role               string `json:"role"`
	State              string `json:"state"`
	RuntimeStatus      string `json:"runtime_status"`
	RuntimeFailure     string `json:"runtime_failure_class"`
	RuntimeDetail      string `json:"runtime_detail"`
	WorktreePath       string `json:"worktree_path"`
	MaxTokens          int    `json:"max_tokens"`
	MaxWallTimeSec     int    `json:"max_wall_time_seconds"`
	TopologyDecisionID string `json:"topology_decision_id"`
}

// PatchView is the frontend representation of a PatchArtifact.
type PatchView struct {
	PatchID              string   `json:"patch_id"`
	CapsuleID            string   `json:"capsule_id"`
	Status               string   `json:"status"`
	Summary              string   `json:"summary"`
	ChangedFiles         []string `json:"changed_files"`
	ObligationIDsClaimed []string `json:"obligation_ids_claimed"`
	TokensUsed           int      `json:"tokens_used"`
	WallTimeSeconds      float64  `json:"wall_time_seconds"`
	BaseCommit           string   `json:"base_commit"`
	DiffPath             string   `json:"diff_path"`
}

// EvidenceView is the frontend representation of an EvidenceArtifact.
type EvidenceView struct {
	EvidenceID   string    `json:"evidence_id"`
	Type         string    `json:"type"`
	Source       string    `json:"source"`
	Command      string    `json:"command"`
	ExitCode     int       `json:"exit_code"`
	Summary      string    `json:"summary"`
	RawLogPath   string    `json:"raw_log_path"`
	InlineOutput string    `json:"inline_output"`
	Supports     []string  `json:"supports"`
	ReusedFromID string    `json:"reused_from_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// FailureView is the frontend representation of a FailureFingerprint.
type FailureView struct {
	FailureID             string   `json:"failure_id"`
	SourceCapsuleID       string   `json:"source_capsule_id"`
	FailureType           string   `json:"failure_type"`
	Summary               string   `json:"summary"`
	AffectedFiles         []string `json:"affected_files"`
	ErrorSignature        string   `json:"error_signature"`
	PriorAttemptCount     int      `json:"prior_attempt_count"`
	RecommendedNextAction string   `json:"recommended_next_action"`
}

// BudgetView is per-capsule budget information for the frontend.
type BudgetView struct {
	BudgetID              string  `json:"budget_id"`
	GoalID                string  `json:"goal_id"`
	CapsuleID             string  `json:"capsule_id"`
	ObligationID          string  `json:"obligation_id"`
	TokensSpent           int     `json:"tokens_spent"`
	WallTimeSeconds       float64 `json:"wall_time_seconds"`
	ToolCalls             int     `json:"tool_calls"`
	Retries               int     `json:"retries"`
	ObligationsDischarged int     `json:"obligations_discharged"`
	PatchesAccepted       int     `json:"patches_accepted"`
	PatchesRejected       int     `json:"patches_rejected"`
}

// BudgetSummary is the aggregated budget across all records for the active goal.
type BudgetSummary struct {
	TotalTokensSpent     int     `json:"total_tokens_spent"`
	TotalWallTimeSeconds float64 `json:"total_wall_time_seconds"`
	TotalToolCalls       int     `json:"total_tool_calls"`
	TotalRetries         int     `json:"total_retries"`
	TotalDischarged      int     `json:"total_obligations_discharged"`
	TotalPatchesAccepted int     `json:"total_patches_accepted"`
	TotalPatchesRejected int     `json:"total_patches_rejected"`
}

// DecisionView is the frontend representation of a DecisionRecord.
type DecisionView struct {
	DecisionID string    `json:"decision_id"`
	Context    string    `json:"context"`
	Decision   string    `json:"decision"`
	Rationale  string    `json:"rationale"`
	MadeBy     string    `json:"made_by"`
	RelatedIDs []string  `json:"related_ids"`
	CreatedAt  time.Time `json:"created_at"`
}

// PendingGate describes a gate action that is needed but has not yet been taken.
// Returned by GetBlockedDecisions.
type PendingGate struct {
	GateType  string `json:"gate_type"`  // "projection_review" or "merge_review"
	RelatedID string `json:"related_id"` // capsule ID or patch ID
	Reason    string `json:"reason"`
}

// TimelineEntry is one step in the goal lifecycle timeline, derived from
// the events.log JSONL file.
type TimelineEntry struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Summary string    `json:"summary"`
	Status  string    `json:"status"` // "ok" | "error" | "warning" | ""
}

// SetupHealthView is a lightweight health check derived from the .orca/
// directory state. For full diagnostic output use the CLI orca doctor command.
type SetupHealthView struct {
	ConfigExists   bool   `json:"config_exists"`
	EventLogExists bool   `json:"event_log_exists"`
	Warning        string `json:"warning,omitempty"`
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toGoalView(g goalDisk) GoalView {
	v := GoalView{
		GoalID:         g.GoalID,
		OriginalIntent: g.OriginalIntent,
		Status:         g.Status,
		RiskLevel:      g.RiskLevel,
		CreatedAt:      g.CreatedAt,
		Conditions:     make([]ConditionView, 0, len(g.GoalConditions)),
	}
	for _, c := range g.GoalConditions {
		v.Conditions = append(v.Conditions, ConditionView{
			ID:          c.ID,
			Description: c.Description,
			Status:      c.Status,
		})
	}
	return v
}

func toObligationView(o obligationDisk) ObligationView {
	satisfiedBy := o.SatisfiedBy
	if satisfiedBy == nil {
		satisfiedBy = []string{}
	}
	return ObligationView{
		ObligationID:    o.ObligationID,
		GoalConditionID: o.GoalConditionID,
		Description:     o.Description,
		Status:          o.Status,
		Blocking:        o.Blocking,
		RiskLevel:       o.RiskLevel,
		SatisfiedBy:     satisfiedBy,
	}
}

func toCapsuleView(c capsuleDisk, runtime *capsuleRuntimeDisk) CapsuleView {
	var runtimeStatus, runtimeFailure, runtimeDetail string
	if runtime != nil {
		runtimeStatus = runtime.Status
		runtimeFailure = runtime.FailClass
		runtimeDetail = runtime.Detail
	}
	return CapsuleView{
		CapsuleID:          c.CapsuleID,
		Agent:              c.Agent,
		Role:               c.Role,
		State:              c.State,
		RuntimeStatus:      runtimeStatus,
		RuntimeFailure:     runtimeFailure,
		RuntimeDetail:      runtimeDetail,
		WorktreePath:       c.Sandbox.WorktreePath,
		MaxTokens:          c.Budget.MaxTokens,
		MaxWallTimeSec:     c.Budget.MaxWallTimeSeconds,
		TopologyDecisionID: c.TopologyDecisionID,
	}
}

func toPatchView(p patchDisk) PatchView {
	return PatchView{
		PatchID:              p.PatchID,
		CapsuleID:            p.CapsuleID,
		Status:               p.Status,
		Summary:              p.Summary,
		ChangedFiles:         p.ChangedFiles,
		ObligationIDsClaimed: p.ObligationIDsClaimed,
		TokensUsed:           p.TokensUsed,
		WallTimeSeconds:      p.WallTimeSeconds,
		BaseCommit:           p.BaseCommit,
		DiffPath:             p.DiffPath,
	}
}

func toEvidenceView(e evidenceDisk) EvidenceView {
	return EvidenceView{
		EvidenceID:   e.EvidenceID,
		Type:         e.Type,
		Source:       e.Source,
		Command:      e.Command,
		ExitCode:     e.ExitCode,
		Summary:      e.Summary,
		RawLogPath:   e.RawLogPath,
		InlineOutput: e.InlineOutput,
		Supports:     e.Supports,
		ReusedFromID: e.ReusedFromID,
		CreatedAt:    e.CreatedAt,
	}
}

func toFailureView(f failureDisk) FailureView {
	return FailureView{
		FailureID:             f.FailureID,
		SourceCapsuleID:       f.SourceCapsuleID,
		FailureType:           f.FailureType,
		Summary:               f.Summary,
		AffectedFiles:         f.AffectedFiles,
		ErrorSignature:        f.ErrorSignature,
		PriorAttemptCount:     f.PriorAttemptCount,
		RecommendedNextAction: f.RecommendedNextAction,
	}
}

func toDecisionView(d decisionDisk) DecisionView {
	return DecisionView{
		DecisionID: d.DecisionID,
		Context:    d.Context,
		Decision:   d.Decision,
		Rationale:  d.Rationale,
		MadeBy:     d.MadeBy,
		RelatedIDs: d.RelatedIDs,
		CreatedAt:  d.CreatedAt,
	}
}
