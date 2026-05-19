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
			name: "multi medium risk uses implementer reviewer",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-3", RiskLevel: schema.RiskMedium},
					{ObligationID: "OB-4", RiskLevel: schema.RiskLow},
				},
			},
			want:        schema.TopologyImplementerReviewer,
			rationaleIn: "OB-3",
		},
		{
			name: "all low and no failures uses single",
			input: ClassifyInput{
				Obligations: []*schema.Obligation{
					{ObligationID: "OB-5", RiskLevel: schema.RiskLow},
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
			if !strings.Contains(rationale, tt.rationaleIn) {
				t.Fatalf("Classify rationale = %q, want substring %q", rationale, tt.rationaleIn)
			}
		})
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
	for _, obligationID := range []string{"OB-plan-ir-1", "OB-plan-ir-2"} {
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
