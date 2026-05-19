package planner

import (
	"context"
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
	})

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

	classifier := topologyClassifier{}
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
			name: "failure fingerprint forces human gated",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-2", RiskLevel: schema.RiskLow},
				},
				Fingerprints: []*schema.FailureFingerprint{
					{FailureID: "FAIL-1", AffectedFiles: []string{`internal\planner\planner.go`}},
				},
			},
			want:        schema.TopologyHumanGated,
			rationaleIn: "FAIL-1",
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
			name: "low risk disjoint expected files can run parallel",
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
			want:        schema.TopologyParallel,
			rationaleIn: "disjoint unprotected expected files",
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
			name: "explicit test first signal selects test first",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-13", RiskLevel: schema.RiskLow, EvidenceRequired: []string{"test_result"}},
				},
				RequiredTools: []string{"test_first"},
			},
			want:        schema.TopologyTestFirst,
			rationaleIn: "test_first",
		},
		{
			name: "explicit investigation signal selects investigate then implement",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-14", RiskLevel: schema.RiskLow},
				},
				RequiredTools: []string{"investigate"},
			},
			want:        schema.TopologyInvestigateThenImpl,
			rationaleIn: "investigate_then_implement",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, rationale, err := classifier.Classify(tt.input)
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

func TestPlan_parallelCreatesOneExecutorCapsulePerDisjointObligation(t *testing.T) {
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

	goalID := "G-plan-parallel"
	conditionID := "GC-plan-parallel"
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
	expectedFiles := map[string]string{
		"OB-plan-parallel-1": "internal/planner/service.go",
		"OB-plan-parallel-2": `internal\budget\service.go`,
	}
	for obligationID, expectedFile := range expectedFiles {
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
	planner := New(st, Config{
		OrcaDir:            orcaDir,
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	})
	result, err := planner.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyParallel {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologyParallel)
	}
	if len(result.CapsuleIDs) != len(expectedFiles) {
		t.Fatalf("CapsuleIDs len = %d, want %d", len(result.CapsuleIDs), len(expectedFiles))
	}

	seenObligations := make(map[string]bool)
	for _, capsuleID := range result.CapsuleIDs {
		capsule, err := st.LoadCapsule(ctx, capsuleID)
		if err != nil {
			t.Fatalf("LoadCapsule %s: %v", capsuleID, err)
		}
		if capsule.Role != schema.RoleExecutor {
			t.Fatalf("capsule %s Role = %s, want %s", capsuleID, capsule.Role, schema.RoleExecutor)
		}
		if capsule.TopologyDecisionID != result.DecisionID {
			t.Fatalf("capsule %s TopologyDecisionID = %q, want %q", capsuleID, capsule.TopologyDecisionID, result.DecisionID)
		}
		if len(capsule.ObligationIDs) != 1 {
			t.Fatalf("capsule %s obligation count = %d, want 1", capsuleID, len(capsule.ObligationIDs))
		}
		obligationID := capsule.ObligationIDs[0]
		seenObligations[obligationID] = true
		if len(capsule.AllowedPaths) != 1 || normalizePath(capsule.AllowedPaths[0]) != normalizePath(expectedFiles[obligationID]) {
			t.Fatalf("capsule %s AllowedPaths = %#v, want only %q", capsuleID, capsule.AllowedPaths, expectedFiles[obligationID])
		}
		wantWorktree := filepath.Join(orcaDir, "capsules", capsuleID, "worktree")
		if capsule.Sandbox.WorktreePath != wantWorktree {
			t.Fatalf("capsule %s Sandbox.WorktreePath = %q, want %q", capsuleID, capsule.Sandbox.WorktreePath, wantWorktree)
		}
	}
	for obligationID := range expectedFiles {
		if !seenObligations[obligationID] {
			t.Fatalf("obligation %s was not assigned to a parallel capsule", obligationID)
		}
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
	})
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

func TestPlan_selectsTestFirstFromGoalIntent(t *testing.T) {
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

	plannerSvc := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	})
	result, err := plannerSvc.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyTestFirst {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologyTestFirst)
	}
	if len(result.CapsuleIDs) != 2 {
		t.Fatalf("CapsuleIDs len = %d, want 2", len(result.CapsuleIDs))
	}
	roles := map[schema.CapsuleRole]bool{}
	for _, capsuleID := range result.CapsuleIDs {
		capsule, err := st.LoadCapsule(ctx, capsuleID)
		if err != nil {
			t.Fatalf("LoadCapsule: %v", err)
		}
		roles[capsule.Role] = true
	}
	if !roles[schema.RoleTester] || !roles[schema.RoleExecutor] {
		t.Fatalf("roles = %+v, want tester and executor", roles)
	}
}

func TestPlan_selectsInvestigateThenImplementFromGoalIntent(t *testing.T) {
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

	plannerSvc := New(st, Config{
		OrcaDir:            filepath.Join(root, ".orca"),
		ApprovalPolicy:     "auto",
		DefaultMaxTokens:   32000,
		DefaultMaxWallTime: 300,
		DefaultMaxRetries:  3,
	})
	result, err := plannerSvc.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Topology != schema.TopologyInvestigateThenImpl {
		t.Fatalf("Topology = %s, want %s", result.Topology, schema.TopologyInvestigateThenImpl)
	}
	if len(result.CapsuleIDs) != 2 {
		t.Fatalf("CapsuleIDs len = %d, want 2", len(result.CapsuleIDs))
	}
	roles := map[schema.CapsuleRole]bool{}
	for _, capsuleID := range result.CapsuleIDs {
		capsule, err := st.LoadCapsule(ctx, capsuleID)
		if err != nil {
			t.Fatalf("LoadCapsule: %v", err)
		}
		roles[capsule.Role] = true
	}
	if !roles[schema.RoleInvestigator] || !roles[schema.RoleExecutor] {
		t.Fatalf("roles = %+v, want investigator and executor", roles)
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
			FailureID:       failureID,
			SourceCapsuleID: capsuleID,
			FailureType:     schema.FailureTest,
			Summary:         "historical failure",
			AffectedFiles:   []string{"internal/planner/service.go"},
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
	})
	result, err := plannerSvc.Plan(ctx, goalID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return ctx, st, result
}
