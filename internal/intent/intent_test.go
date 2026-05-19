package intent

import (
	"context"
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

func TestCompile_createsGoalAndSingleCreationEvent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	log, err := eventlog.Open(root + `\events.log`)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	compiler := New(st)
	goal, err := compiler.Compile(context.Background(), "Remove all legacy dependency wiring")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if goal.Status != schema.GoalStatusActive {
		t.Fatalf("goal status = %s, want %s", goal.Status, schema.GoalStatusActive)
	}
	if goal.RiskLevel != schema.RiskHigh {
		t.Fatalf("goal risk = %s, want %s", goal.RiskLevel, schema.RiskHigh)
	}
	if len(goal.GoalConditions) != 2 {
		t.Fatalf("goal conditions len = %d, want 2", len(goal.GoalConditions))
	}
	if goal.GoalConditions[0].Description != "Remove all legacy dependency wiring" {
		t.Fatalf("primary condition = %q", goal.GoalConditions[0].Description)
	}
	if goal.GoalConditions[1].Description != "All existing tests continue to pass" {
		t.Fatalf("regression condition = %q", goal.GoalConditions[1].Description)
	}
	if !strings.HasPrefix(goal.GoalID, "G-") {
		t.Fatalf("goal ID = %q, want G-*", goal.GoalID)
	}
	if !strings.HasPrefix(goal.GoalConditions[0].ID, "GC-") {
		t.Fatalf("goal condition ID = %q, want GC-*", goal.GoalConditions[0].ID)
	}

	events, err := log.ReadForGoal(context.Background(), goal.GoalID, 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("goal events len = %d, want 1", len(events))
	}
	if events[0].Type != schema.EventGoalCreated {
		t.Fatalf("event type = %s, want %s", events[0].Type, schema.EventGoalCreated)
	}
}

func TestCompile_rejectsWhenActiveGoalExists(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	log, err := eventlog.Open(root + `\events.log`)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveGoal(context.Background(), &schema.GoalIR{
		GoalID:         "G-existing",
		OriginalIntent: "existing",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-existing",
			Description:          "existing",
			EffectiveDescription: "existing",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	compiler := New(st)
	_, err = compiler.Compile(context.Background(), "new goal")
	if err == nil {
		t.Fatal("Compile() expected error, got nil")
	}
	const want = "active goal G-existing already exists; complete or cancel it before creating a new goal"
	if err.Error() != want {
		t.Fatalf("Compile() error = %q, want %q", err.Error(), want)
	}
}
