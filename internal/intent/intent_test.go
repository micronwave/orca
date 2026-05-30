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

func TestCompile_multiLineGoalCreatesMultipleConditions(t *testing.T) {
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
	goal, err := compiler.Compile(context.Background(), "Add String() method to RiskLevel\nFix the existing unit tests")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// 2 primary conditions + 1 regression condition
	if len(goal.GoalConditions) != 3 {
		t.Fatalf("goal conditions len = %d, want 3", len(goal.GoalConditions))
	}
	if goal.GoalConditions[0].Description != "Add String() method to RiskLevel" {
		t.Fatalf("condition[0] = %q", goal.GoalConditions[0].Description)
	}
	if goal.GoalConditions[1].Description != "Fix the existing unit tests" {
		t.Fatalf("condition[1] = %q", goal.GoalConditions[1].Description)
	}
	if goal.GoalConditions[2].Description != "All existing tests continue to pass" {
		t.Fatalf("condition[2] = %q", goal.GoalConditions[2].Description)
	}
}

func TestCompile_scopeConstraintsParsed(t *testing.T) {
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
	goal, err := compiler.Compile(context.Background(),
		"Refactor the parser: only touch internal/intent; do not edit cmd/orca")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(goal.ScopeConstraints.AllowedFiles) != 1 || goal.ScopeConstraints.AllowedFiles[0] != "internal/intent" {
		t.Fatalf("AllowedFiles = %v, want [internal/intent]", goal.ScopeConstraints.AllowedFiles)
	}
	if len(goal.ScopeConstraints.ForbiddenFiles) != 1 || goal.ScopeConstraints.ForbiddenFiles[0] != "cmd/orca" {
		t.Fatalf("ForbiddenFiles = %v, want [cmd/orca]", goal.ScopeConstraints.ForbiddenFiles)
	}
	// Pure scope clauses must not appear as GoalConditions.
	for _, c := range goal.GoalConditions {
		if strings.Contains(c.Description, "do not edit") {
			t.Fatalf("scope clause appeared as GoalCondition: %q", c.Description)
		}
	}
}

func TestCompile_scopeConstraintsMultiPath(t *testing.T) {
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
	goal, err := compiler.Compile(context.Background(),
		"Fix the parser; only touch internal/intent and internal/store")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(goal.ScopeConstraints.AllowedFiles) != 2 {
		t.Fatalf("AllowedFiles = %v, want [internal/intent internal/store]", goal.ScopeConstraints.AllowedFiles)
	}
	// The pure scope clause must not appear as a GoalCondition.
	for _, c := range goal.GoalConditions {
		if strings.Contains(c.Description, "only touch") {
			t.Fatalf("scope clause appeared as GoalCondition: %q", c.Description)
		}
	}
}

func TestCompile_docsGoalSkipsTestRegressionAndSetsLowRisk(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		intent string
	}{
		{"readme keyword", `write a 50 line readme for "E:\project\guide.md"`},
		{"markdown keyword", "add a Markdown reference section to the project"},
		{"dot-md path", `update the docs at docs/overview.md`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
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
			goal, err := compiler.Compile(context.Background(), tc.intent)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if goal.RiskLevel != schema.RiskLow {
				t.Fatalf("risk = %s, want low for docs goal", goal.RiskLevel)
			}
			for _, c := range goal.GoalConditions {
				if c.Description == "All existing tests continue to pass" {
					t.Fatal("docs goal must not include test-regression condition")
				}
			}
		})
	}
}

func TestCompile_mixedCodeDocsGoalIsNotDocsOnly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		intent string
	}{
		{"fix and readme", "fix parser bug and update README.md"},
		{"implement and md", "implement the parser; update docs/api.md"},
		{"refactor and markdown", "refactor the schema and add a Markdown changelog"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
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
			goal, err := compiler.Compile(context.Background(), tc.intent)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			hasRegression := false
			for _, c := range goal.GoalConditions {
				if c.Description == "All existing tests continue to pass" {
					hasRegression = true
				}
			}
			if !hasRegression {
				t.Fatal("mixed code+docs goal must include test-regression condition")
			}
			if goal.RiskLevel == schema.RiskLow {
				t.Fatalf("mixed code+docs goal risk = %s, want medium or high", goal.RiskLevel)
			}
		})
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
