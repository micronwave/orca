package budget_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/budget"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type budgetEnv struct {
	ctx context.Context
	log *eventlog.FileLog
	st  *store.FileStore
	ctl *budget.Controller
}

func newBudgetEnv(t *testing.T) *budgetEnv {
	t.Helper()
	dir := t.TempDir()
	log, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(dir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &budgetEnv{ctx: context.Background(), log: log, st: st, ctl: budget.New(log)}
}

func (e *budgetEnv) seedCapsule(t *testing.T, maxTokens, maxWallSeconds, maxRetries int) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.st.SaveGoal(e.ctx, &schema.GoalIR{
		GoalID:         "G-1",
		OriginalIntent: "test",
		GoalConditions: []schema.GoalCondition{{ID: "GC-1", Description: "condition", EffectiveDescription: "condition", Status: schema.GoalConditionUnmet}},
		RiskLevel:      schema.RiskLow,
		CreatedAt:      now,
		Status:         schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := e.st.SaveObligation(e.ctx, &schema.Obligation{
		ObligationID:    "OB-1",
		GoalConditionID: "GC-1",
		Description:     "obligation",
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := e.st.SaveCapsule(e.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-1",
		ObligationIDs: []string{"OB-1"},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
		Budget: schema.CapsuleBudget{
			MaxTokens:          maxTokens,
			MaxWallTimeSeconds: maxWallSeconds,
			MaxRetries:         maxRetries,
		},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestCheckCapsuleBudget_AllowsZeroTokenSpend(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 1, 60, 1)
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:  "BUD-1",
		GoalID:    "G-1",
		CapsuleID: "CAP-1",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}

	check, err := e.ctl.CheckCapsuleBudget(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("CheckCapsuleBudget: %v", err)
	}
	if !check.Allowed {
		t.Fatalf("Allowed = false, reason = %q", check.Reason)
	}
}

func TestCheckCapsuleBudget_RejectsExhaustedWallTimeAndRetry(t *testing.T) {
	tests := []struct {
		name       string
		record     schema.BudgetRecord
		wantReason string
	}{
		{
			name: "wall time",
			record: schema.BudgetRecord{
				BudgetID: "BUD-wall", GoalID: "G-1", CapsuleID: "CAP-1", WallTimeSeconds: 60,
			},
			wantReason: "wall time budget exhausted",
		},
		{
			name: "retries",
			record: schema.BudgetRecord{
				BudgetID: "BUD-retry", GoalID: "G-1", CapsuleID: "CAP-1", Retries: 1,
			},
			wantReason: "retry budget exhausted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBudgetEnv(t)
			e.seedCapsule(t, 0, 60, 1)
			tt.record.CreatedAt = time.Now().UTC()
			tt.record.UpdatedAt = tt.record.CreatedAt
			if err := e.st.SaveBudgetRecord(e.ctx, &tt.record); err != nil {
				t.Fatalf("SaveBudgetRecord: %v", err)
			}
			check, err := e.ctl.CheckCapsuleBudget(e.ctx, "CAP-1")
			if err != nil {
				t.Fatalf("CheckCapsuleBudget: %v", err)
			}
			if check.Allowed || check.Reason != tt.wantReason {
				t.Fatalf("check = %+v, want reason %q", check, tt.wantReason)
			}
		})
	}
}

func TestComputeROI_UsesLatestBudgetRecordEvent(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 1000, 60, 1)
	record := &schema.BudgetRecord{
		BudgetID:              "BUD-1",
		GoalID:                "G-1",
		CapsuleID:             "CAP-1",
		TokensSpent:           100,
		Retries:               1,
		DuplicatedFileReads:   2,
		OverlappingEdits:      3,
		HumanInterventions:    4,
		ObligationsDischarged: 1,
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, record); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	record.TokensSpent = 200
	record.ObligationsDischarged = 2
	record.OverlappingEdits = 5
	record.UpdatedAt = time.Now().UTC()
	if err := e.st.UpdateBudgetRecord(e.ctx, record); err != nil {
		t.Fatalf("UpdateBudgetRecord: %v", err)
	}
	// PatchesAccepted is now derived from raw patch_accepted events, not BudgetRecord.
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:       schema.EventPatchAccepted,
		GoalID:     "G-1",
		ArtifactID: "PATCH-1",
		Payload:    mustMarshal(t, schema.PatchStatusPayload{PatchID: "PATCH-1"}),
	}); err != nil {
		t.Fatalf("Append patch_accepted: %v", err)
	}

	roi, err := e.ctl.ComputeROI(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("ComputeROI: %v", err)
	}
	if roi.TotalTokensSpent != 200 {
		t.Fatalf("TotalTokensSpent = %d, want latest record value 200", roi.TotalTokensSpent)
	}
	if roi.TotalCoordinationCost != 12 {
		t.Fatalf("TotalCoordinationCost = %d, want 12", roi.TotalCoordinationCost)
	}
	if roi.PatchesAccepted != 1 {
		t.Fatalf("PatchesAccepted = %d, want 1 (from patch_accepted event)", roi.PatchesAccepted)
	}
	if roi.VerifiedValuePer1KTokens <= 0 {
		t.Fatalf("VerifiedValuePer1KTokens = %f, want non-zero", roi.VerifiedValuePer1KTokens)
	}
}

func TestComputeROI_IgnoresObligationScopedRecordsForTotals(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 1000, 60, 1)
	now := time.Now().UTC()
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:              "BUD-summary",
		GoalID:                "G-1",
		CapsuleID:             "CAP-1",
		TokensSpent:           100,
		ToolCalls:             2,
		ObligationsDischarged: 1,
		PatchesAccepted:       1,
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("SaveBudgetRecord summary: %v", err)
	}
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:              "BUD-ob-1",
		GoalID:                "G-1",
		CapsuleID:             "CAP-1",
		ObligationID:          "OB-1",
		TokensSpent:           900,
		ToolCalls:             9,
		ObligationsDischarged: 1,
		PatchesAccepted:       1,
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("SaveBudgetRecord obligation: %v", err)
	}
	roi, err := e.ctl.ComputeROI(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("ComputeROI: %v", err)
	}
	if roi.TotalTokensSpent != 100 {
		t.Fatalf("TotalTokensSpent = %d, want 100 (summary record only)", roi.TotalTokensSpent)
	}
	if roi.ObligationsDischarged != 1 {
		t.Fatalf("ObligationsDischarged = %d, want 1 (summary record only)", roi.ObligationsDischarged)
	}
}

func TestCheckCapsuleBudget_RejectsExhaustedTokens(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 100, 0, 0)
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:    "BUD-token",
		GoalID:      "G-1",
		CapsuleID:   "CAP-1",
		TokensSpent: 100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	check, err := e.ctl.CheckCapsuleBudget(e.ctx, "CAP-1")
	if err != nil {
		t.Fatalf("CheckCapsuleBudget: %v", err)
	}
	if check.Allowed || check.Reason != "token budget exhausted" {
		t.Fatalf("check = %+v, want token budget exhausted", check)
	}
}

// TestComputeROI_EventDrivenValueCounts verifies that PatchesAccepted,
// PatchesRejected, and EvidenceArtifactsReused are derived from raw events
// rather than from reconciler-written BudgetRecord counters, preventing silent
// ROI drift when reconciler snapshots diverge from the event history.
func TestComputeROI_EventDrivenValueCounts(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 1000, 60, 3)
	now := time.Now().UTC()
	// BudgetRecord carries inflated counts — these must not be used for the
	// value fields that are now event-driven.
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:                "BUD-1",
		GoalID:                  "G-1",
		CapsuleID:               "CAP-1",
		TokensSpent:             500,
		ObligationsDischarged:   3,
		PatchesAccepted:         99,
		PatchesRejected:         99,
		EvidenceArtifactsReused: 99,
		CreatedAt:               now,
		UpdatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	appendEvent := func(eventType schema.EventType, payload any) {
		t.Helper()
		if _, err := e.log.Append(e.ctx, schema.Event{
			Type:    eventType,
			GoalID:  "G-1",
			Payload: mustMarshal(t, payload),
		}); err != nil {
			t.Fatalf("Append %s: %v", eventType, err)
		}
	}
	appendEvent(schema.EventPatchAccepted, schema.PatchStatusPayload{PatchID: "PATCH-1"})
	appendEvent(schema.EventPatchAccepted, schema.PatchStatusPayload{PatchID: "PATCH-2"})
	appendEvent(schema.EventPatchRejected, schema.PatchStatusPayload{PatchID: "PATCH-3"})
	// One evidence artifact with ReusedFromID set (reused), one without (fresh).
	appendEvent(schema.EventEvidenceArtifactCreated, schema.EvidenceArtifact{
		EvidenceID:   "EV-reused",
		Type:         schema.EvidenceTestResult,
		ReusedFromID: "EV-original",
	})
	appendEvent(schema.EventEvidenceArtifactCreated, schema.EvidenceArtifact{
		EvidenceID: "EV-fresh",
		Type:       schema.EvidenceTestResult,
	})

	roi, err := e.ctl.ComputeROI(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("ComputeROI: %v", err)
	}
	if roi.PatchesAccepted != 2 {
		t.Errorf("PatchesAccepted = %d, want 2 (from events, not BudgetRecord)", roi.PatchesAccepted)
	}
	if roi.PatchesRejected != 1 {
		t.Errorf("PatchesRejected = %d, want 1 (from events)", roi.PatchesRejected)
	}
	if roi.EvidenceArtifactsReused != 1 {
		t.Errorf("EvidenceArtifactsReused = %d, want 1 (from evidence events with reused_from_id)", roi.EvidenceArtifactsReused)
	}
	// ObligationsDischarged still comes from BudgetRecord.
	if roi.ObligationsDischarged != 3 {
		t.Errorf("ObligationsDischarged = %d, want 3 (from BudgetRecord)", roi.ObligationsDischarged)
	}
}

// TestComputeROI_ToolCallsIncludedInROI verifies that tool-call burn from
// BudgetRecord is exposed in the ROI summary as TotalToolCalls.
func TestComputeROI_ToolCallsIncludedInROI(t *testing.T) {
	e := newBudgetEnv(t)
	e.seedCapsule(t, 1000, 60, 1)
	now := time.Now().UTC()
	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:  "BUD-1",
		GoalID:    "G-1",
		CapsuleID: "CAP-1",
		ToolCalls: 7,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	roi, err := e.ctl.ComputeROI(e.ctx, "G-1")
	if err != nil {
		t.Fatalf("ComputeROI: %v", err)
	}
	if roi.TotalToolCalls != 7 {
		t.Errorf("TotalToolCalls = %d, want 7", roi.TotalToolCalls)
	}
}
