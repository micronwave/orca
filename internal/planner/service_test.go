package planner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

func TestPlan_setsDeterministicWorktreePathAndEmitsSingleCapsuleEvent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	goalID := "G-plan-single"
	conditionID := "GC-plan-single"
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "implement planner phase",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "implement planner phase",
			EffectiveDescription: "implement planner phase",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-plan-single",
		GoalConditionID:  conditionID,
		Description:      "update planner implementation",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	orcaDir := filepath.Join(root, ".orca")
	planner := New(st, Config{
		OrcaDir:            orcaDir,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil)

	result, err := planner.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologySingle)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	if result.DecisionID == "" {
		t.Fatal("DecisionID is empty")
	}
	if result.MaxObligationRisk != schema.RiskLow {
		t.Fatalf("MaxObligationRisk = %s, want %s", result.MaxObligationRisk, schema.RiskLow)
	}

	capsuleID := result.CapsuleIDs[0]
	capsule, err := st.LoadCapsule(ctx, capsuleID)
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	wantWorktree := filepath.Join(orcaDir, "capsules", capsuleID, "worktree")
	if capsule.Sandbox.WorktreePath != wantWorktree {
		t.Fatalf("Sandbox.WorktreePath = %q, want %q", capsule.Sandbox.WorktreePath, wantWorktree)
	}
	if capsule.TopologyDecisionID != result.DecisionID {
		t.Fatalf("TopologyDecisionID = %q, want %q", capsule.TopologyDecisionID, result.DecisionID)
	}

	events, err := log.ReadForGoal(ctx, goalID, 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal: %v", err)
	}
	capsuleEventCount := 0
	seenCapsuleEvents := make(map[string]int)
	for _, ev := range events {
		if ev.Type == schema.EventCapsuleCreated {
			capsuleEventCount++
			seenCapsuleEvents[ev.ArtifactID]++
		}
	}
	if capsuleEventCount != len(result.CapsuleIDs) {
		t.Fatalf("capsule_created events = %d, want %d", capsuleEventCount, len(result.CapsuleIDs))
	}
	if seenCapsuleEvents[capsuleID] != 1 {
		t.Fatalf("capsule %s capsule_created event count = %d, want 1", capsuleID, seenCapsuleEvents[capsuleID])
	}
}

func TestTopologyClassifier_rules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       ClassifyInput
		want        schema.Topology
		rationaleIn string
	}{
		{
			name: "high risk obligation forces human gated",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-1", RiskLevel: schema.RiskHigh},
				},
			},
			want:        schema.TopologyHumanGated,
			rationaleIn: "OB-1",
		},
		{
			name: "repeated failure fingerprint forces human gated",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-2", RiskLevel: schema.RiskLow},
				},
				Fingerprints: []*schema.FailureFingerprint{
					{FailureID: "FAIL-1", AffectedFiles: []string{`internal\planner\planner.go`}, PriorAttemptCount: 2},
				},
			},
			want:        schema.TopologyHumanGated,
			rationaleIn: "FAIL-1",
		},
		{
			name: "single prior failure does not force human gated",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-2a", RiskLevel: schema.RiskLow},
				},
				Fingerprints: []*schema.FailureFingerprint{
					{FailureID: "FAIL-1a", AffectedFiles: []string{`internal\planner\planner.go`}, PriorAttemptCount: 1},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "low risk",
		},
		{
			name: "medium risk uses implementer reviewer",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-3", RiskLevel: schema.RiskMedium},
				},
			},
			want:        schema.TopologyImplementerReviewer,
			rationaleIn: "OB-3",
		},
		{
			name: "medium risk with expected file overlap collapses to single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-4", RiskLevel: schema.RiskMedium},
				},
				ExpectedFileOverlap: true,
			},
			want:        schema.TopologySingle,
			rationaleIn: "coordination cost exceeds expected value",
		},
		{
			name: "medium risk with insufficient budget collapses to single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-5", RiskLevel: schema.RiskMedium},
				},
				BudgetRemaining: 100,
			},
			want:        schema.TopologySingle,
			rationaleIn: "coordination cost",
		},
		{
			name: "all low and no failures uses single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-6", RiskLevel: schema.RiskLow},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "low risk",
		},
		{
			name: "low risk disjoint expected files collapses to single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-7", RiskLevel: schema.RiskLow},
					{ObligationID: "OB-8", RiskLevel: schema.RiskLow},
				},
				ExpectedFilesByObligation: map[string][]string{
					"OB-7": {"internal/planner/service.go"},
					"OB-8": {`internal\budget\service.go`},
				},
				BudgetRemaining: 32000,
			},
			want:        schema.TopologySingle,
			rationaleIn: "low risk",
		},
		{
			name: "low risk expected file overlap serializes",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-9", RiskLevel: schema.RiskLow},
					{ObligationID: "OB-10", RiskLevel: schema.RiskLow},
				},
				ExpectedFilesByObligation: map[string][]string{
					"OB-9":  {"./internal/planner/service.go"},
					"OB-10": {`internal\planner\service.go`},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "expected file overlap",
		},
		{
			name: "protected path serializes",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-11", RiskLevel: schema.RiskLow},
					{ObligationID: "OB-12", RiskLevel: schema.RiskLow},
				},
				ExpectedFilesByObligation: map[string][]string{
					"OB-11": {"go.mod"},
					"OB-12": {"internal/planner/service.go"},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "protected path",
		},
		{
			name: "mixed-case protected filenames are still detected as protected",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-11a", RiskLevel: schema.RiskLow},
					{ObligationID: "OB-12a", RiskLevel: schema.RiskLow},
				},
				ExpectedFilesByObligation: map[string][]string{
					"OB-11a": {"Dockerfile"},
					"OB-12a": {"internal/runner/service.go"},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "protected path",
		},
		{
			name: "Makefile and Cargo.toml are detected as protected",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-11b", RiskLevel: schema.RiskLow},
					{ObligationID: "OB-12b", RiskLevel: schema.RiskLow},
				},
				ExpectedFilesByObligation: map[string][]string{
					"OB-11b": {"Makefile"},
					"OB-12b": {"Cargo.toml"},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "protected path",
		},
		{
			name: "low risk test-first intent collapses to single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-13", RiskLevel: schema.RiskLow, EvidenceRequired: []string{"test_result"}},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "low risk",
		},
		{
			name: "low risk investigation intent collapses to single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-14", RiskLevel: schema.RiskLow},
				},
			},
			want:        schema.TopologySingle,
			rationaleIn: "low risk",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, rationale, err := classify(tt.input)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Classify topology = %s, want %s", got, tt.want)
			}
			if strings.TrimSpace(rationale) == "" {
				t.Fatal("Classify rationale is empty")
			}
			for _, required := range []string{"obligations=", "max_risk=", "expected_file_overlap=", "fingerprints=", "budget_remaining="} {
				if !strings.Contains(rationale, required) {
					t.Fatalf("Classify rationale = %q, want classifier input %q", rationale, required)
				}
			}
			if !strings.Contains(rationale, tt.rationaleIn) {
				t.Fatalf("Classify rationale = %q, want substring %q", rationale, tt.rationaleIn)
			}
		})
	}
}

func TestPlan_disjointLowRiskObligationsCollapsesToSingle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	goalID := "G-plan-disjoint"
	conditionID := "GC-plan-disjoint"
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "update independent files",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "update independent files",
			EffectiveDescription: "update independent files",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	for obligationID, expectedFile := range map[string]string{
		"OB-plan-disjoint-1": "internal/planner/service.go",
		"OB-plan-disjoint-2": `internal\budget\service.go`,
	} {
		if err := st.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     obligationID,
			GoalConditionID:  conditionID,
			Description:      "obligation " + obligationID,
			EvidenceRequired: []string{"static_check"},
			Blocking:         true,
			RiskLevel:        schema.RiskLow,
			Status:           schema.ObligationOpen,
			ExpectedFiles:    []string{expectedFile},
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", obligationID, err)
		}
	}

	orcaDir := filepath.Join(root, ".orca")
	result, err := New(st, Config{
		OrcaDir:            orcaDir,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologySingle)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	capsule, err := st.LoadCapsule(ctx, result.CapsuleIDs[0])
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.Role != schema.RoleExecutor {
		t.Fatalf("Role = %s, want %s", capsule.Role, schema.RoleExecutor)
	}
}

func TestPlan_implementerReviewerCreatesTwoCapsules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	goalID := "G-plan-ir"
	conditionID := "GC-plan-ir"
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "implement medium-risk obligations",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "implement medium-risk obligations",
			EffectiveDescription: "implement medium-risk obligations",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	for _, obligationID := range []string{"OB-plan-ir-1"} {
		if err := st.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     obligationID,
			GoalConditionID:  conditionID,
			Description:      "obligation " + obligationID,
			EvidenceRequired: []string{"test_result"},
			Blocking:         true,
			RiskLevel:        schema.RiskMedium,
			Status:           schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", obligationID, err)
		}
	}

	orcaDir := filepath.Join(root, ".orca")
	planner := New(st, Config{
		OrcaDir:            orcaDir,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil)
	result, err := planner.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyImplementerReviewer {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologyImplementerReviewer)
	}
	if len(result.CapsuleIDs) != 2 {
		t.Fatalf("CapsuleIDs len = %d, want 2", len(result.CapsuleIDs))
	}
	if result.MaxObligationRisk != schema.RiskMedium {
		t.Fatalf("MaxObligationRisk = %s, want %s", result.MaxObligationRisk, schema.RiskMedium)
	}
	roles := make(map[schema.CapsuleRole]bool)
	for _, capsuleID := range result.CapsuleIDs {
		capsule, err := st.LoadCapsule(ctx, capsuleID)
		if err != nil {
			t.Fatalf("LoadCapsule %s: %v", capsuleID, err)
		}
		roles[capsule.Role] = true
		if capsule.TopologyDecisionID != result.DecisionID {
			t.Fatalf("capsule %s TopologyDecisionID = %q, want %q", capsuleID, capsule.TopologyDecisionID, result.DecisionID)
		}
		wantWorktree := filepath.Join(orcaDir, "capsules", capsuleID, "worktree")
		if capsule.Sandbox.WorktreePath != wantWorktree {
			t.Fatalf("capsule %s Sandbox.WorktreePath = %q, want %q", capsuleID, capsule.Sandbox.WorktreePath, wantWorktree)
		}
	}
	if !roles[schema.RoleExecutor] || !roles[schema.RoleReviewer] {
		t.Fatalf("capsule roles = %+v, want executor and reviewer", roles)
	}

	events, err := log.ReadForGoal(ctx, goalID, 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal: %v", err)
	}
	seenCapsuleEvents := make(map[string]int)
	for _, ev := range events {
		if ev.Type == schema.EventCapsuleCreated {
			seenCapsuleEvents[ev.ArtifactID]++
		}
	}
	if len(seenCapsuleEvents) != len(result.CapsuleIDs) {
		t.Fatalf("distinct capsule_created artifact IDs = %d, want %d", len(seenCapsuleEvents), len(result.CapsuleIDs))
	}
	for _, capsuleID := range result.CapsuleIDs {
		if seenCapsuleEvents[capsuleID] != 1 {
			t.Fatalf("capsule %s capsule_created event count = %d, want 1", capsuleID, seenCapsuleEvents[capsuleID])
		}
	}
}

func TestPlan_UsesHistoricalFailureRoutingHint(t *testing.T) {
	t.Parallel()

	ctx, st, result := seedPlanWithRecurringFailureHistory(t, false)
	if result.Topology != schema.TopologyHumanGated {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologyHumanGated)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	capsule, err := st.LoadCapsule(ctx, result.CapsuleIDs[0])
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.Agent != schema.AgentClaude {
		t.Fatalf("capsule agent = %s, want %s when recurring failures exist", capsule.Agent, schema.AgentClaude)
	}
}

func TestPlan_NoLearningDisablesHistoricalFailureRoutingHint(t *testing.T) {
	t.Parallel()

	ctx, st, result := seedPlanWithRecurringFailureHistory(t, true)
	// With --no-learning, fingerprints are suppressed from topology classification (§13).
	// The obligation is low-risk with no other forcing signals, so topology must be single.
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want %s when no-learning is enabled (fingerprints must not force human_gated)", result.Topology, schema.TopologySingle)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	capsule, err := st.LoadCapsule(ctx, result.CapsuleIDs[0])
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.Agent != schema.AgentCodex {
		t.Fatalf("capsule agent = %s, want %s when no-learning is enabled", capsule.Agent, schema.AgentCodex)
	}
}

func TestPlan_IgnoresMatchingFileFailuresFromOtherGoals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	now := time.Now().UTC()
	for _, goal := range []struct {
		goalID       string
		conditionID  string
		obligationID string
		status       schema.GoalStatus
	}{
		{"G-current", "GC-current", "OB-current", schema.GoalStatusActive},
		{"G-other", "GC-other", "OB-other", schema.GoalStatusComplete},
	} {
		if err := st.SaveGoal(ctx, &schema.GoalIR{
			GoalID: goal.goalID,
			GoalConditions: []schema.GoalCondition{{
				ID:     goal.conditionID,
				Status: schema.GoalConditionUnmet,
			}},
			RiskLevel: schema.RiskLow,
			CreatedAt: now,
			Status:    goal.status,
		}); err != nil {
			t.Fatalf("SaveGoal %s: %v", goal.goalID, err)
		}
		if err := st.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     goal.obligationID,
			GoalConditionID:  goal.conditionID,
			EvidenceRequired: []string{"test_result"},
			Blocking:         true,
			RiskLevel:        schema.RiskLow,
			Status:           schema.ObligationOpen,
			ExpectedFiles:    []string{"internal/planner/service.go"},
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", goal.obligationID, err)
		}
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-other",
		ObligationIDs: []string{"OB-other"},
		State:         schema.CapsuleStateFailed,
	}); err != nil {
		t.Fatalf("SaveCapsule other: %v", err)
	}
	if err := st.SaveFailure(ctx, &schema.FailureFingerprint{
		FailureID:         "FAIL-other",
		SourceCapsuleID:   "CAP-other",
		FailureType:       schema.FailureTest,
		AffectedFiles:     []string{"internal/planner/service.go"},
		PriorAttemptCount: 2,
	}); err != nil {
		t.Fatalf("SaveFailure other: %v", err)
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil).Plan(ctx, "G-current")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology == schema.TopologyHumanGated {
		t.Fatalf("Topology = %s, want other-goal failure ignored", result.Topology)
	}
}

func TestPlan_tddIntentCollapsesToSingle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const (
		goalID      = "G-plan-testfirst"
		conditionID = "GC-plan-testfirst"
		obligation  = "OB-plan-testfirst"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "Use test first TDD flow for this low-risk change",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "deliver low-risk change",
			EffectiveDescription: "deliver low-risk change",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "add coverage",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologySingle)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	capsule, err := st.LoadCapsule(ctx, result.CapsuleIDs[0])
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.Role != schema.RoleExecutor {
		t.Fatalf("Role = %s, want %s", capsule.Role, schema.RoleExecutor)
	}
}

func TestPlan_investigationIntentCollapsesToSingle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const (
		goalID      = "G-plan-investigate"
		conditionID = "GC-plan-investigate"
		obligation  = "OB-plan-investigate"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "Investigate the root cause before implementing a fix",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "resolve issue",
			EffectiveDescription: "resolve issue",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "fix issue",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, nil).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologySingle)
	}
	if len(result.CapsuleIDs) != 1 {
		t.Fatalf("CapsuleIDs len = %d, want 1", len(result.CapsuleIDs))
	}
	capsule, err := st.LoadCapsule(ctx, result.CapsuleIDs[0])
	if err != nil {
		t.Fatalf("LoadCapsule: %v", err)
	}
	if capsule.Role != schema.RoleExecutor {
		t.Fatalf("Role = %s, want %s", capsule.Role, schema.RoleExecutor)
	}
}

func seedPlanWithRecurringFailureHistory(t *testing.T, noLearning bool) (context.Context, *store.FileStore, PlanResult) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-history"
		conditionID = "GC-history"
		obligation  = "OB-history"
	)
	now := time.Now().UTC()
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "route retries using failure history",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "stabilize historical retry path",
			EffectiveDescription: "stabilize historical retry path",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "fix recurring failure",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
		ExpectedFiles:    []string{`internal\planner\service.go`},
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	for _, suffix := range []string{"1", "2"} {
		capsuleID := "CAP-history-" + suffix
		failureID := "FAIL-history-" + suffix
		if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID:     capsuleID,
			ObligationIDs: []string{obligation},
			Agent:         schema.AgentCodex,
			Role:          schema.RoleExecutor,
			State:         schema.CapsuleStateFailed,
		}); err != nil {
			t.Fatalf("SaveCapsule %s: %v", capsuleID, err)
		}
		if err := st.SaveFailure(ctx, &schema.FailureFingerprint{
			FailureID:         failureID,
			SourceCapsuleID:   capsuleID,
			FailureType:       schema.FailureTest,
			Summary:           "historical failure",
			AffectedFiles:     []string{"internal/planner/service.go"},
			PriorAttemptCount: 2,
		}); err != nil {
			t.Fatalf("SaveFailure %s: %v", failureID, err)
		}
	}

	plannerSvc := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
		NoLearning:         noLearning,
	}, nil)
	result, err := plannerSvc.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return ctx, st, result
}

// stubOutcomeReader is a minimal OutcomeReader used in historical-routing tests.
type stubOutcomeReader struct {
	outcomes map[string][]*schema.TopologyOutcomeRecord // key: topology+"|"+risk
}

func (s *stubOutcomeReader) LoadTopologyOutcomes(_ context.Context, topology schema.Topology, maxRisk schema.RiskLevel) ([]*schema.TopologyOutcomeRecord, error) {
	key := string(topology) + "|" + string(maxRisk)
	return s.outcomes[key], nil
}

func makeOutcomes(topology schema.Topology, risk schema.RiskLevel, total, accepted int) []*schema.TopologyOutcomeRecord {
	out := make([]*schema.TopologyOutcomeRecord, total)
	for i := range out {
		out[i] = &schema.TopologyOutcomeRecord{
			OutcomeID:     fmt.Sprintf("OUT-%s-%d", topology, i),
			Topology:      topology,
			MaxRiskLevel:  risk,
			PatchAccepted: i < accepted,
		}
	}
	return out
}

func TestPlan_HistoricalRoutingHintPreferImplementerReviewer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-ir"
		conditionID = "GC-histhint-ir"
		obligation  = "OB-histhint-ir"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test historical routing hint",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "low-risk obligation",
			EffectiveDescription: "low-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "implement feature",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// implementer_reviewer: 4/5 accepted (80%), single: 1/5 accepted (20%)
	// difference is 60%, well above 15% threshold
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|low": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskLow, 5, 4),
			"single|low":               makeOutcomes(schema.TopologySingle, schema.RiskLow, 5, 1),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyImplementerReviewer {
		t.Fatalf("Topology = %s, want implementer_reviewer (historical hint must override)", result.Topology)
	}

	decision, err := st.LoadDecision(ctx, result.DecisionID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if !strings.Contains(decision.Rationale, "historical routing") {
		t.Fatalf("Rationale = %q, want historical routing hint in rationale", decision.Rationale)
	}
	if !strings.Contains(decision.Rationale, "implementer_reviewer") {
		t.Fatalf("Rationale = %q, want implementer_reviewer mention", decision.Rationale)
	}
}

// TestPlan_HistoricalRoutingHintNoOpWhenAlreadyIR verifies that the hint does
// not fire (and does not append its rationale) when the classifier already
// chose implementer_reviewer.  The hint can only upgrade single→IR; it must
// not pollute the rationale when the classifier made the correct call without it.
func TestPlan_HistoricalRoutingHintNoOpWhenAlreadyIR(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-noop"
		conditionID = "GC-histhint-noop"
		obligation  = "OB-histhint-noop"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test hint no-op for already-IR classifier output",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "medium-risk obligation",
			EffectiveDescription: "medium-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	// Medium-risk obligation → classifier returns implementer_reviewer directly.
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "refactor medium-risk component",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// Strong historical signal favouring IR: the hint must NOT produce a
	// redundant "historical routing" entry in the rationale, since the
	// classifier already chose implementer_reviewer.
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|medium": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskMedium, 5, 5),
			"single|medium":               makeOutcomes(schema.TopologySingle, schema.RiskMedium, 5, 0),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyImplementerReviewer {
		t.Fatalf("Topology = %s, want implementer_reviewer", result.Topology)
	}

	decision, err := st.LoadDecision(ctx, result.DecisionID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if strings.Contains(decision.Rationale, "historical routing") {
		t.Fatalf("Rationale = %q, must not contain 'historical routing' when classifier already chose implementer_reviewer", decision.Rationale)
	}
}

func TestPlan_HistoricalRoutingHintDoesNotFireBelowThreshold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-nothresh"
		conditionID = "GC-histhint-nothresh"
		obligation  = "OB-histhint-nothresh"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test historical routing hint below threshold",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "low-risk obligation",
			EffectiveDescription: "low-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "implement feature",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// implementer_reviewer: 3/5 (60%), single: 3/5 (60%) — equal, threshold not met
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|low": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskLow, 5, 3),
			"single|low":               makeOutcomes(schema.TopologySingle, schema.RiskLow, 5, 3),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Classifier would choose single (low risk, no forcing signals)
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want single (threshold not met)", result.Topology)
	}
}

func TestPlan_HistoricalRoutingHintDoesNotFireBelowMinSamples(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-fewsamples"
		conditionID = "GC-histhint-fewsamples"
		obligation  = "OB-histhint-fewsamples"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test historical routing hint too few samples",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "low-risk obligation",
			EffectiveDescription: "low-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "implement feature",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// Only 2 samples each — below the minimum of 3
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|low": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskLow, 2, 2),
			"single|low":               makeOutcomes(schema.TopologySingle, schema.RiskLow, 2, 0),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want single (too few samples)", result.Topology)
	}
}

func TestPlan_HistoricalRoutingHintSkippedWhenNoLearning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-nolearn"
		conditionID = "GC-histhint-nolearn"
		obligation  = "OB-histhint-nolearn"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test historical routing hint with no-learning",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "low-risk obligation",
			EffectiveDescription: "low-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "implement feature",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// Even with a strong historical signal, NoLearning must suppress the hint
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|low": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskLow, 5, 5),
			"single|low":               makeOutcomes(schema.TopologySingle, schema.RiskLow, 5, 0),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
		NoLearning:         true,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologySingle {
		t.Fatalf("Topology = %s, want single (no-learning must suppress historical hint)", result.Topology)
	}
}

func TestPlan_HistoricalRoutingHintDoesNotOverrideHumanGated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const (
		goalID      = "G-histhint-humangate"
		conditionID = "GC-histhint-humangate"
		obligation  = "OB-histhint-humangate"
	)
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test historical routing hint does not override human_gated",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "high-risk obligation",
			EffectiveDescription: "high-risk obligation",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskHigh,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     obligation,
		GoalConditionID:  conditionID,
		Description:      "high-risk change",
		EvidenceRequired: []string{"test_result"},
		Blocking:         true,
		RiskLevel:        schema.RiskHigh,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}

	// Strong historical signal favouring implementer_reviewer, but classifier
	// must return human_gated for high risk, which must not be overridden.
	reader := &stubOutcomeReader{
		outcomes: map[string][]*schema.TopologyOutcomeRecord{
			"implementer_reviewer|high": makeOutcomes(schema.TopologyImplementerReviewer, schema.RiskHigh, 5, 5),
			"single|high":               makeOutcomes(schema.TopologySingle, schema.RiskHigh, 5, 0),
		},
	}

	result, err := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	}, reader).Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyHumanGated {
		t.Fatalf("Topology = %s, want human_gated (high-risk must not be overridden by historical hint)", result.Topology)
	}
}
