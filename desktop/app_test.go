package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── scanDir tests ─────────────────────────────────────────────────────────────

func TestScanDir_missingDirectory(t *testing.T) {
	t.Helper()
	result, err := scanDir[obligationDisk](filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("scanDir returned error for missing directory: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(result))
	}
}

func TestScanDir_readsJSONFiles(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "OB-1.json"), obligationDisk{
		ObligationID: "OB-1",
		Description:  "test obligation",
		Status:       "open",
		Blocking:     true,
		RiskLevel:    "low",
	})
	writeJSON(t, filepath.Join(dir, "OB-2.json"), obligationDisk{
		ObligationID: "OB-2",
		Description:  "second obligation",
		Status:       "satisfied",
		Blocking:     false,
		RiskLevel:    "medium",
	})
	// Non-JSON file should be ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644)

	result, err := scanDir[obligationDisk](dir)
	if err != nil {
		t.Fatalf("scanDir: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 obligations, got %d", len(result))
	}
}

func TestScanDir_skipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("not json {{{{"), 0o644)
	writeJSON(t, filepath.Join(dir, "OB-1.json"), obligationDisk{ObligationID: "OB-1"})

	result, err := scanDir[obligationDisk](dir)
	if err != nil {
		t.Fatalf("scanDir: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 valid record, got %d", len(result))
	}
}

// ── deriveMergeReadiness tests ────────────────────────────────────────────────

func TestDeriveMergeReadiness_noVerifierResults(t *testing.T) {
	result := deriveMergeReadiness(nil, nil, nil, nil)
	if result != "unknown" {
		t.Errorf("expected unknown, got %q", result)
	}
}

func TestDeriveMergeReadiness_openBlockingObligation(t *testing.T) {
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "open", Blocking: true}}
	vrs := []verifierResultDisk{{PatchID: "P-1", CreatedAt: time.Now()}}
	result := deriveMergeReadiness(obs, vrs, nil, nil)
	if result != "blocked" {
		t.Errorf("expected blocked, got %q", result)
	}
}

func TestDeriveMergeReadiness_blockingFailures(t *testing.T) {
	vrs := []verifierResultDisk{{
		PatchID:          "P-1",
		BlockingFailures: []string{"test failed"},
		CreatedAt:        time.Now(),
	}}
	result := deriveMergeReadiness(nil, vrs, nil, nil)
	if result != "blocked" {
		t.Errorf("expected blocked, got %q", result)
	}
}

func TestDeriveMergeReadiness_noMatchingPatch(t *testing.T) {
	vrs := []verifierResultDisk{{PatchID: "PATCH-1", CreatedAt: time.Now()}}
	// No patches in store → pending_reconciliation
	result := deriveMergeReadiness(nil, vrs, nil, nil)
	if result != "pending_reconciliation" {
		t.Errorf("expected pending_reconciliation, got %q", result)
	}
}

func TestDeriveMergeReadiness_candidatePatch(t *testing.T) {
	vrs := []verifierResultDisk{{PatchID: "PATCH-1", CreatedAt: time.Now()}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "candidate"}}
	result := deriveMergeReadiness(nil, vrs, patches, nil)
	if result != "pending_reconciliation" {
		t.Errorf("expected pending_reconciliation, got %q", result)
	}
}

func TestDeriveMergeReadiness_acceptedPatch_noDecisionsNeeded(t *testing.T) {
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "low"}}
	result := deriveMergeReadiness(obs, vrs, patches, nil)
	if result != "ready" {
		t.Errorf("expected ready, got %q", result)
	}
}

func TestDeriveMergeReadiness_acceptedPatch_needsMergeReview(t *testing.T) {
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	// High-risk obligation → requires merge_review decision.
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "high"}}
	result := deriveMergeReadiness(obs, vrs, patches, nil)
	if result != "needs_human_review" {
		t.Errorf("expected needs_human_review, got %q", result)
	}
}

func TestDeriveMergeReadiness_acceptedPatch_mergeReviewDone(t *testing.T) {
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "high"}}
	// The merge review decision exists.
	decisions := []decisionDisk{{
		DecisionID: "DEC-1",
		Context:    "merge_review",
		Decision:   "approved",
		RelatedIDs: []string{"PATCH-1"},
	}}
	result := deriveMergeReadiness(obs, vrs, patches, decisions)
	if result != "ready" {
		t.Errorf("expected ready, got %q", result)
	}
}

// ── shouldReviewProjection tests ──────────────────────────────────────────────

func TestShouldReviewProjection(t *testing.T) {
	tests := []struct {
		topology string
		maxRisk  string
		want     bool
	}{
		{"human_gated", "low", true},
		{"human_gated", "high", true},
		{"implementer_reviewer", "high", true},
		{"implementer_reviewer", "medium", true},
		{"implementer_reviewer", "low", false},
		{"single", "low", true},
		{"single", "medium", true},
		{"single", "high", true},
		{"", "high", false},
	}
	for _, tc := range tests {
		t.Run(tc.topology+"/"+tc.maxRisk, func(t *testing.T) {
			got := shouldReviewProjection(tc.topology, tc.maxRisk)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ── deriveBlockedDecisions projection_review tests ────────────────────────────

func TestDeriveBlockedDecisions_projectionReview_pending(t *testing.T) {
	// Active executor capsule with implementer_reviewer topology + medium risk
	// and no existing projection_review decision → should appear as blocked.
	caps := []capsuleDisk{{
		CapsuleID:          "CAP-1",
		Role:               "executor",
		State:              "pending",
		TopologyDecisionID: "DEC-TOPO-1",
	}}
	decisions := []decisionDisk{{
		DecisionID: "DEC-TOPO-1",
		Context:    "topology_selection",
		Decision:   "implementer_reviewer",
	}}
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "open", Blocking: true, RiskLevel: "medium"}}

	result := deriveBlockedDecisions(obs, caps, nil, nil, decisions)
	if len(result) != 1 {
		t.Fatalf("expected 1 pending gate, got %d", len(result))
	}
	if result[0].GateType != "projection_review" {
		t.Errorf("gate_type: got %q, want projection_review", result[0].GateType)
	}
	if result[0].RelatedID != "CAP-1" {
		t.Errorf("related_id: got %q, want CAP-1", result[0].RelatedID)
	}
}

func TestDeriveBlockedDecisions_projectionReview_alreadyDecided(t *testing.T) {
	caps := []capsuleDisk{{
		CapsuleID:          "CAP-1",
		Role:               "executor",
		State:              "pending",
		TopologyDecisionID: "DEC-TOPO-1",
	}}
	decisions := []decisionDisk{
		{DecisionID: "DEC-TOPO-1", Context: "topology_selection", Decision: "implementer_reviewer"},
		{DecisionID: "DEC-GATE-1", Context: "projection_review", Decision: "approved", RelatedIDs: []string{"CAP-1"}},
	}
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "open", Blocking: true, RiskLevel: "medium"}}

	result := deriveBlockedDecisions(obs, caps, nil, nil, decisions)
	if len(result) != 0 {
		t.Errorf("expected no pending gates (decision exists), got %d", len(result))
	}
}

func TestDeriveBlockedDecisions_singleTopology_pendingTimedProjectionReview(t *testing.T) {
	// single topology still invokes projection_review; low risk uses a timed
	// auto-proceed window in the CLI, but the pending gate is visible until a
	// decision record exists.
	caps := []capsuleDisk{{
		CapsuleID:          "CAP-1",
		Role:               "executor",
		State:              "agent_running",
		TopologyDecisionID: "DEC-TOPO-1",
	}}
	decisions := []decisionDisk{{DecisionID: "DEC-TOPO-1", Context: "topology_selection", Decision: "single"}}
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "open", Blocking: true, RiskLevel: "low"}}

	result := deriveBlockedDecisions(obs, caps, nil, nil, decisions)
	if len(result) != 1 {
		t.Fatalf("expected 1 pending projection gate for single/low-risk, got %d", len(result))
	}
	if result[0].GateType != "projection_review" || result[0].RelatedID != "CAP-1" {
		t.Fatalf("pending gate = %+v, want projection_review for CAP-1", result[0])
	}
}

// ── deriveBlockedDecisions merge_review tests ─────────────────────────────────

func TestDeriveBlockedDecisions_mergeReview_pending(t *testing.T) {
	// Accepted patch with a high-risk obligation and no open blocking obligations
	// → merge_review gate should be pending.
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "high"}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CapsuleID: "CAP-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}

	result := deriveBlockedDecisions(obs, nil, patches, vrs, nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 pending gate, got %d", len(result))
	}
	if result[0].GateType != "merge_review" {
		t.Errorf("gate_type: got %q, want merge_review", result[0].GateType)
	}
	if result[0].RelatedID != "PATCH-1" {
		t.Errorf("related_id: got %q, want PATCH-1", result[0].RelatedID)
	}
}

func TestDeriveBlockedDecisions_mergeReview_alreadyDecided(t *testing.T) {
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "high"}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CapsuleID: "CAP-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}
	decisions := []decisionDisk{{
		DecisionID: "DEC-MR-1",
		Context:    "merge_review",
		Decision:   "approved",
		RelatedIDs: []string{"PATCH-1"},
	}}

	result := deriveBlockedDecisions(obs, nil, patches, vrs, decisions)
	if len(result) != 0 {
		t.Errorf("expected no pending gates (decision exists), got %d", len(result))
	}
}

func TestDeriveBlockedDecisions_mergeReview_lowRisk_noGateRequired(t *testing.T) {
	// Low-risk obligation → merge_review gate must NOT be raised.
	obs := []obligationDisk{{ObligationID: "OB-1", Status: "satisfied", Blocking: false, RiskLevel: "low"}}
	patches := []patchDisk{{PatchID: "PATCH-1", Status: "accepted"}}
	vrs := []verifierResultDisk{{
		PatchID:   "PATCH-1",
		CapsuleID: "CAP-1",
		CreatedAt: time.Now(),
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-1", Verdict: "satisfied"},
		},
	}}

	result := deriveBlockedDecisions(obs, nil, patches, vrs, nil)
	if len(result) != 0 {
		t.Errorf("expected no pending gates for low-risk, got %d", len(result))
	}
}

// ── evidenceSupportsClaimed tests ─────────────────────────────────────────────

func TestEvidenceSupportsClaimed(t *testing.T) {
	claimed := map[string]bool{"OB-1": true, "OB-2": true}
	tests := []struct {
		name     string
		supports []string
		want     bool
	}{
		{"matching single", []string{"OB-1"}, true},
		{"matching one of many", []string{"OB-3", "OB-2"}, true},
		{"no match", []string{"OB-4", "OB-5"}, false},
		{"empty supports", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evidenceSupportsClaimed(tc.supports, claimed)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ── loadActiveGoalScope tests ─────────────────────────────────────────────────

func TestLoadActiveGoalScopeFromDir_scopsObligationsToActiveGoal(t *testing.T) {
	orcaDir := t.TempDir()
	goalDir := filepath.Join(orcaDir, "state", "goals")
	oblDir := filepath.Join(orcaDir, "state", "obligations")
	for _, d := range []string{goalDir, oblDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	// Active goal with condition GC-A.
	writeJSON(t, filepath.Join(goalDir, "GOAL-A.json"), goalDisk{
		GoalID:    "GOAL-A",
		Status:    "active",
		CreatedAt: now,
		GoalConditions: []goalConditionDisk{
			{ID: "GC-A", Description: "active condition", Status: "unmet"},
		},
	})
	// Completed goal with condition GC-OLD.
	writeJSON(t, filepath.Join(goalDir, "GOAL-OLD.json"), goalDisk{
		GoalID:    "GOAL-OLD",
		Status:    "complete",
		CreatedAt: now.Add(-time.Hour),
		GoalConditions: []goalConditionDisk{
			{ID: "GC-OLD", Description: "old condition", Status: "met"},
		},
	})
	// Obligation for the active goal.
	writeJSON(t, filepath.Join(oblDir, "OB-A.json"), obligationDisk{
		ObligationID:    "OB-A",
		GoalConditionID: "GC-A",
		Status:          "open",
		Blocking:        true,
	})
	// Obligation for the old goal — must NOT appear in scope.
	writeJSON(t, filepath.Join(oblDir, "OB-OLD.json"), obligationDisk{
		ObligationID:    "OB-OLD",
		GoalConditionID: "GC-OLD",
		Status:          "satisfied",
	})

	scope, err := loadActiveGoalScopeFromDir(orcaDir)
	if err != nil {
		t.Fatalf("loadActiveGoalScopeFromDir: %v", err)
	}
	if !scope.obligationIDs["OB-A"] {
		t.Error("OB-A should be in scope")
	}
	if scope.obligationIDs["OB-OLD"] {
		t.Error("OB-OLD from completed goal must not be in scope")
	}
	if len(scope.obligations) != 1 || scope.obligations[0].ObligationID != "OB-A" {
		t.Errorf("scope.obligations: got %v, want [OB-A]", scope.obligations)
	}
}

// ── filterVerifierResultsByScope tests ───────────────────────────────────────

func TestFilterVerifierResultsByScope_noActiveGoal_returnsAll(t *testing.T) {
	scope := goalScope{} // no conditionIDs → no active goal
	vrs := []verifierResultDisk{
		{VerifierResultID: "VR-1", CapsuleID: "CAP-OLD"},
		{VerifierResultID: "VR-2", CapsuleID: "CAP-ALSO-OLD"},
	}
	got := filterVerifierResultsByScope(vrs, scope)
	if len(got) != 2 {
		t.Errorf("expected all 2 results returned when no active goal, got %d", len(got))
	}
}

func TestFilterVerifierResultsByScope_activeGoalNoCapsules_returnsNone(t *testing.T) {
	scope := goalScope{
		goalID:       "GOAL-A",
		conditionIDs: map[string]bool{"GC-A": true},
		capsuleIDs:   map[string]bool{}, // active goal exists but has no capsules yet
	}
	vrs := []verifierResultDisk{
		{VerifierResultID: "VR-OLD", CapsuleID: "CAP-OLD"},
	}
	got := filterVerifierResultsByScope(vrs, scope)
	if len(got) != 0 {
		t.Errorf("expected empty slice when active goal has no capsules, got %d results", len(got))
	}
}

func TestFilterVerifierResultsByScope_filtersToActiveGoalCapsules(t *testing.T) {
	scope := goalScope{
		goalID:       "GOAL-A",
		conditionIDs: map[string]bool{"GC-A": true},
		capsuleIDs:   map[string]bool{"CAP-A": true},
	}
	vrs := []verifierResultDisk{
		{VerifierResultID: "VR-A", CapsuleID: "CAP-A"},
		{VerifierResultID: "VR-OLD", CapsuleID: "CAP-OLD"},
	}
	got := filterVerifierResultsByScope(vrs, scope)
	if len(got) != 1 || got[0].VerifierResultID != "VR-A" {
		t.Errorf("expected only VR-A, got %v", got)
	}
}

// ── GetMergeReadiness goal-scoping test ───────────────────────────────────────

func TestGetMergeReadiness_ignoresObligationsFromOtherGoals(t *testing.T) {
	orcaDir := t.TempDir()
	goalDir := filepath.Join(orcaDir, "state", "goals")
	oblDir := filepath.Join(orcaDir, "state", "obligations")
	patchDir := filepath.Join(orcaDir, "artifacts", "patches")
	vrDir := filepath.Join(orcaDir, "artifacts", "verifier_results")
	capsuleDir := filepath.Join(orcaDir, "state", "capsules")
	for _, d := range []string{goalDir, oblDir, patchDir, vrDir, capsuleDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	// Active goal with one satisfied condition.
	writeJSON(t, filepath.Join(goalDir, "GOAL-A.json"), goalDisk{
		GoalID: "GOAL-A", Status: "active", CreatedAt: now,
		GoalConditions: []goalConditionDisk{{ID: "GC-A", Status: "met"}},
	})
	// Old completed goal whose open obligation must not block readiness.
	writeJSON(t, filepath.Join(goalDir, "GOAL-OLD.json"), goalDisk{
		GoalID: "GOAL-OLD", Status: "complete", CreatedAt: now.Add(-time.Hour),
		GoalConditions: []goalConditionDisk{{ID: "GC-OLD", Status: "unmet"}},
	})

	// Obligation for active goal — satisfied.
	writeJSON(t, filepath.Join(oblDir, "OB-A.json"), obligationDisk{
		ObligationID: "OB-A", GoalConditionID: "GC-A", Status: "satisfied",
		Blocking: false, RiskLevel: "low",
	})
	// Orphaned open blocking obligation from old goal.
	writeJSON(t, filepath.Join(oblDir, "OB-OLD.json"), obligationDisk{
		ObligationID: "OB-OLD", GoalConditionID: "GC-OLD", Status: "open",
		Blocking: true, RiskLevel: "high",
	})

	writeJSON(t, filepath.Join(capsuleDir, "CAP-A.json"), capsuleDisk{
		CapsuleID: "CAP-A", ObligationIDs: []string{"OB-A"},
	})
	writeJSON(t, filepath.Join(patchDir, "PATCH-A.json"), patchDisk{
		PatchID: "PATCH-A", Status: "accepted",
		ObligationIDsClaimed: []string{"OB-A"},
	})
	writeJSON(t, filepath.Join(vrDir, "VR-A.json"), verifierResultDisk{
		VerifierResultID: "VR-A",
		PatchID:          "PATCH-A",
		CapsuleID:        "CAP-A",
		CreatedAt:        now,
		ObligationResults: []obligationVerdictDisk{
			{ObligationID: "OB-A", Verdict: "satisfied"},
		},
	})
	// Historical VR from a different (old-goal) capsule with blocking failures —
	// must not affect the active-goal readiness result.
	writeJSON(t, filepath.Join(vrDir, "VR-OLD.json"), verifierResultDisk{
		VerifierResultID: "VR-OLD",
		PatchID:          "PATCH-OLD",
		CapsuleID:        "CAP-OLD",
		CreatedAt:        now.Add(-30 * time.Minute),
		BlockingFailures: []string{"historical test failure"},
	})

	app := NewApp(orcaDir)
	readiness, err := app.GetMergeReadiness()
	if err != nil {
		t.Fatalf("GetMergeReadiness: %v", err)
	}
	// Should be "ready"; without VR scoping it would return "blocked" due to VR-OLD's
	// blocking failures, and without obligation scoping it would also be blocked by OB-OLD.
	if readiness != "ready" {
		t.Errorf("got %q, want ready (old-goal VR and obligation must not affect active-goal readiness)", readiness)
	}
}

func TestDesktopListsScopePrimaryDataToActiveGoal(t *testing.T) {
	orcaDir := t.TempDir()
	for _, d := range []string{
		filepath.Join(orcaDir, "state", "goals"),
		filepath.Join(orcaDir, "state", "obligations"),
		filepath.Join(orcaDir, "state", "capsules"),
		filepath.Join(orcaDir, "artifacts", "patches"),
		filepath.Join(orcaDir, "artifacts", "failures"),
		filepath.Join(orcaDir, "artifacts", "budgets"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	writeJSON(t, filepath.Join(orcaDir, "state", "goals", "GOAL-A.json"), goalDisk{
		GoalID: "GOAL-A", Status: "active", CreatedAt: now,
		GoalConditions: []goalConditionDisk{{ID: "GC-A", Status: "unmet"}},
	})
	writeJSON(t, filepath.Join(orcaDir, "state", "goals", "GOAL-OLD.json"), goalDisk{
		GoalID: "GOAL-OLD", Status: "complete", CreatedAt: now.Add(-time.Hour),
		GoalConditions: []goalConditionDisk{{ID: "GC-OLD", Status: "met"}},
	})
	writeJSON(t, filepath.Join(orcaDir, "state", "obligations", "OB-A.json"), obligationDisk{
		ObligationID: "OB-A", GoalConditionID: "GC-A", Status: "open",
	})
	writeJSON(t, filepath.Join(orcaDir, "state", "obligations", "OB-OLD.json"), obligationDisk{
		ObligationID: "OB-OLD", GoalConditionID: "GC-OLD", Status: "satisfied",
	})
	writeJSON(t, filepath.Join(orcaDir, "state", "capsules", "CAP-A.json"), capsuleDisk{
		CapsuleID: "CAP-A", ObligationIDs: []string{"OB-A"},
	})
	writeJSON(t, filepath.Join(orcaDir, "state", "capsules", "CAP-OLD.json"), capsuleDisk{
		CapsuleID: "CAP-OLD", ObligationIDs: []string{"OB-OLD"},
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "patches", "PATCH-A.json"), patchDisk{
		PatchID: "PATCH-A", CapsuleID: "CAP-A",
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "patches", "PATCH-OLD.json"), patchDisk{
		PatchID: "PATCH-OLD", CapsuleID: "CAP-OLD",
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "failures", "FAIL-A.json"), failureDisk{
		FailureID: "FAIL-A", SourceCapsuleID: "CAP-A",
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "failures", "FAIL-OLD.json"), failureDisk{
		FailureID: "FAIL-OLD", SourceCapsuleID: "CAP-OLD",
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "budgets", "BUD-A.json"), budgetDisk{
		BudgetID: "BUD-A", GoalID: "GOAL-A", TokensSpent: 10,
	})
	writeJSON(t, filepath.Join(orcaDir, "artifacts", "budgets", "BUD-OLD.json"), budgetDisk{
		BudgetID: "BUD-OLD", GoalID: "GOAL-OLD", TokensSpent: 99,
	})

	app := NewApp(orcaDir)
	obligations, err := app.ListObligations()
	if err != nil {
		t.Fatalf("ListObligations: %v", err)
	}
	if len(obligations) != 1 || obligations[0].ObligationID != "OB-A" {
		t.Fatalf("ListObligations = %+v, want only OB-A", obligations)
	}
	capsules, err := app.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(capsules) != 1 || capsules[0].CapsuleID != "CAP-A" {
		t.Fatalf("ListCapsules = %+v, want only CAP-A", capsules)
	}
	patches, err := app.ListPatches()
	if err != nil {
		t.Fatalf("ListPatches: %v", err)
	}
	if len(patches) != 1 || patches[0].PatchID != "PATCH-A" {
		t.Fatalf("ListPatches = %+v, want only PATCH-A", patches)
	}
	failures, err := app.ListFailures()
	if err != nil {
		t.Fatalf("ListFailures: %v", err)
	}
	if len(failures) != 1 || failures[0].FailureID != "FAIL-A" {
		t.Fatalf("ListFailures = %+v, want only FAIL-A", failures)
	}
	budget, err := app.GetBudgetSummary()
	if err != nil {
		t.Fatalf("GetBudgetSummary: %v", err)
	}
	if budget.TotalTokensSpent != 10 {
		t.Fatalf("TotalTokensSpent = %d, want 10", budget.TotalTokensSpent)
	}
}

// ── GetGoal integration test with fixture directory ───────────────────────────

func TestGetGoal_activeGoal(t *testing.T) {
	orcaDir := t.TempDir()
	goalDir := filepath.Join(orcaDir, "state", "goals")
	if err := os.MkdirAll(goalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	writeJSON(t, filepath.Join(goalDir, "GOAL-1.json"), goalDisk{
		GoalID:         "GOAL-1",
		OriginalIntent: "add String() method",
		Status:         "active",
		RiskLevel:      "low",
		CreatedAt:      now,
		GoalConditions: []goalConditionDisk{
			{ID: "GC-1", Description: "method exists", Status: "unmet"},
		},
	})

	app := NewApp(orcaDir)
	goal, err := app.GetGoal()
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if goal == nil {
		t.Fatal("expected non-nil goal")
	}
	if goal.GoalID != "GOAL-1" {
		t.Errorf("goal_id: got %q, want GOAL-1", goal.GoalID)
	}
	if goal.Status != "active" {
		t.Errorf("status: got %q, want active", goal.Status)
	}
	if len(goal.Conditions) != 1 {
		t.Errorf("conditions count: got %d, want 1", len(goal.Conditions))
	}
}

func TestGetGoal_noGoals(t *testing.T) {
	app := NewApp(t.TempDir())
	goal, err := app.GetGoal()
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if goal != nil {
		t.Errorf("expected nil goal, got %+v", goal)
	}
}

// ── ListEvidence integration test ─────────────────────────────────────────────

func TestListEvidence_filtersByPatchObligations(t *testing.T) {
	orcaDir := t.TempDir()
	patchDir := filepath.Join(orcaDir, "artifacts", "patches")
	evidDir := filepath.Join(orcaDir, "artifacts", "evidence")
	for _, d := range []string{patchDir, evidDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeJSON(t, filepath.Join(patchDir, "PATCH-1.json"), patchDisk{
		PatchID:              "PATCH-1",
		ObligationIDsClaimed: []string{"OB-1", "OB-2"},
		Status:               "accepted",
	})

	// Evidence that supports OB-1 — should be included.
	writeJSON(t, filepath.Join(evidDir, "EV-1.json"), evidenceDisk{
		EvidenceID: "EV-1",
		Type:       "test_result",
		Supports:   []string{"OB-1"},
	})
	// Evidence for an unrelated obligation — should NOT be included.
	writeJSON(t, filepath.Join(evidDir, "EV-2.json"), evidenceDisk{
		EvidenceID: "EV-2",
		Type:       "lint_result",
		Supports:   []string{"OB-99"},
	})

	app := NewApp(orcaDir)
	ev, err := app.ListEvidence("PATCH-1")
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("expected 1 evidence item, got %d", len(ev))
	}
	if ev[0].EvidenceID != "EV-1" {
		t.Errorf("evidence_id: got %q, want EV-1", ev[0].EvidenceID)
	}
}

func TestListEvidence_emptyPatchID(t *testing.T) {
	app := NewApp(t.TempDir())
	ev, err := app.ListEvidence("")
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	if len(ev) != 0 {
		t.Errorf("expected empty slice, got %d items", len(ev))
	}
}

// ── GetTimeline tests ─────────────────────────────────────────────────────────

func TestGetTimeline_noLog(t *testing.T) {
	app := NewApp(t.TempDir())
	entries, err := app.GetTimeline()
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty slice for missing log, got %d entries", len(entries))
	}
}

func TestGetTimeline_parsesAndFilters(t *testing.T) {
	orcaDir := t.TempDir()
	now := time.Now().UTC()

	eventsData := []map[string]any{
		{"type": "goal_created", "goal_id": "G-1", "created_at": now.Add(-3 * time.Minute), "payload": json.RawMessage("{}")},
		{"type": "obligation_created", "goal_id": "G-1", "created_at": now.Add(-2 * time.Minute), "payload": json.RawMessage("{}")},
		// state_snapshot_saved should be excluded by timelineSignificant
		{"type": "state_snapshot_saved", "goal_id": "G-1", "created_at": now.Add(-90 * time.Second), "payload": json.RawMessage("{}")},
		{"type": "patch_accepted", "goal_id": "G-1", "created_at": now, "payload": json.RawMessage("{}")},
	}
	logPath := filepath.Join(orcaDir, "events.log")
	if err := writeEventLog(t, logPath, eventsData); err != nil {
		t.Fatal(err)
	}

	app := NewApp(orcaDir)
	entries, err := app.GetTimeline()
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (snapshot filtered), got %d", len(entries))
	}
	if entries[0].Type != "goal_created" {
		t.Errorf("entries[0].Type = %q; want goal_created", entries[0].Type)
	}
	if entries[2].Type != "patch_accepted" {
		t.Errorf("entries[2].Type = %q; want patch_accepted", entries[2].Type)
	}
	if entries[2].Status != "ok" {
		t.Errorf("entries[2].Status = %q; want ok", entries[2].Status)
	}
}

func TestGetTimeline_limitsToMaxEntries(t *testing.T) {
	orcaDir := t.TempDir()
	now := time.Now().UTC()
	events := make([]map[string]any, 210)
	for i := range 210 {
		events[i] = map[string]any{
			"type":       "obligation_created",
			"goal_id":    "G-1",
			"created_at": now.Add(time.Duration(i) * time.Second),
			"payload":    json.RawMessage("{}"),
		}
	}
	logPath := filepath.Join(orcaDir, "events.log")
	if err := writeEventLog(t, logPath, events); err != nil {
		t.Fatal(err)
	}

	app := NewApp(orcaDir)
	entries, err := app.GetTimeline()
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(entries) != 200 {
		t.Errorf("expected 200 capped entries, got %d", len(entries))
	}
}

func TestGetTimeline_topologyPayloadExtracted(t *testing.T) {
	orcaDir := t.TempDir()
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]string{"topology": "implementer_reviewer", "decision_id": "DEC-1"})
	events := []map[string]any{
		{"type": "topology_selected", "goal_id": "G-1", "created_at": now, "payload": json.RawMessage(payload)},
	}
	logPath := filepath.Join(orcaDir, "events.log")
	if err := writeEventLog(t, logPath, events); err != nil {
		t.Fatal(err)
	}

	app := NewApp(orcaDir)
	entries, err := app.GetTimeline()
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Summary != "Topology: implementer_reviewer" {
		t.Errorf("Summary = %q; want Topology: implementer_reviewer", entries[0].Summary)
	}
}

func TestGetTimeline_scopesEventsToActiveGoal(t *testing.T) {
	orcaDir := t.TempDir()
	goalDir := filepath.Join(orcaDir, "state", "goals")
	if err := os.MkdirAll(goalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeJSON(t, filepath.Join(goalDir, "GOAL-A.json"), goalDisk{
		GoalID: "GOAL-A", Status: "active", CreatedAt: now,
		GoalConditions: []goalConditionDisk{{ID: "GC-A"}},
	})
	writeJSON(t, filepath.Join(goalDir, "GOAL-OLD.json"), goalDisk{
		GoalID: "GOAL-OLD", Status: "complete", CreatedAt: now.Add(-time.Hour),
		GoalConditions: []goalConditionDisk{{ID: "GC-OLD"}},
	})
	events := []map[string]any{
		{"type": "goal_created", "goal_id": "GOAL-OLD", "created_at": now.Add(-time.Hour), "payload": json.RawMessage("{}")},
		{"type": "goal_created", "goal_id": "GOAL-A", "created_at": now, "payload": json.RawMessage("{}")},
	}
	if err := writeEventLog(t, filepath.Join(orcaDir, "events.log"), events); err != nil {
		t.Fatal(err)
	}

	app := NewApp(orcaDir)
	entries, err := app.GetTimeline()
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(entries) != 1 || entries[0].Type != "goal_created" || !entries[0].At.Equal(now) {
		t.Fatalf("entries = %+v, want only active goal event", entries)
	}
}

// ── GetSetupHealth tests ──────────────────────────────────────────────────────

func TestGetSetupHealth_configMissing(t *testing.T) {
	app := NewApp(t.TempDir())
	h, err := app.GetSetupHealth()
	if err != nil {
		t.Fatalf("GetSetupHealth: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil health")
	}
	if h.ConfigExists {
		t.Error("config_exists should be false when file is absent")
	}
	if h.Warning == "" {
		t.Error("expected a warning message when config.yaml is missing")
	}
}

func TestGetSetupHealth_configExists(t *testing.T) {
	orcaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(orcaDir, "config.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := NewApp(orcaDir)
	h, err := app.GetSetupHealth()
	if err != nil {
		t.Fatalf("GetSetupHealth: %v", err)
	}
	if !h.ConfigExists {
		t.Error("config_exists should be true when config.yaml is present")
	}
	if h.Warning != "" {
		t.Errorf("expected no warning, got %q", h.Warning)
	}
}

func TestGetSetupHealth_eventLogDetected(t *testing.T) {
	orcaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(orcaDir, "events.log"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	app := NewApp(orcaDir)
	h, err := app.GetSetupHealth()
	if err != nil {
		t.Fatalf("GetSetupHealth: %v", err)
	}
	if !h.EventLogExists {
		t.Error("event_log_exists should be true when events.log is present")
	}
}

// ── Helpers (timeline) ────────────────────────────────────────────────────────

func writeEventLog(t *testing.T, path string, events []map[string]any) error {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		sb.Write(data)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
