// Package intent defines the IntentCompiler interface, which converts raw user
// goal text into a persisted GoalIR with initial GoalConditions.
//
// Phase 1 decision: the MVP compiler is deterministic and rule-based. It treats
// the raw intent as the primary goal condition, adds a no-regression condition,
// and defaults risk without calling a model.
//
// Dependency contract:
//
//	Reads  (store):   GoalIR via LoadActiveGoal (to enforce one active goal per
//	                  repo: return an error if a non-nil goal is returned before
//	                  creating a new one)
//	Writes (store):   GoalIR via SaveGoal (conditions embedded in GoalIR)
//	Writes (log):     none directly — store emits goal_created on SaveGoal
//
//	Must NOT import:  internal/planner, internal/runner, internal/verifier,
//	                  internal/reconciler, internal/projector, internal/budget,
//	                  internal/gate
package intent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// IntentCompiler converts a raw user goal string into a GoalIR.
// It derives initial GoalConditions from the input, assigns IDs, persists
// the GoalIR, and returns the saved record.
//
// The compiler may call a model to clarify intent, but it must not create
// Obligations or Capsules — those belong to the ObligationPlanner.
// One active goal per repo is the MVP constraint; the compiler should reject
// a second Compile call when a goal is already active.
type IntentCompiler interface {
	// Compile parses rawIntent, creates a GoalIR with initial GoalConditions,
	// persists it via the store and
	// returns the saved GoalIR.
	Compile(ctx context.Context, rawIntent string) (*schema.GoalIR, error)
}

type service struct {
	store store.ArtifactStore
}

// New returns the deterministic Phase 1 IntentCompiler implementation.
func New(st store.ArtifactStore) IntentCompiler {
	return &service{store: st}
}

func (s *service) Compile(ctx context.Context, rawIntent string) (*schema.GoalIR, error) {
	if s.store == nil {
		return nil, fmt.Errorf("intent: store is required")
	}
	intentText := strings.TrimSpace(rawIntent)
	if intentText == "" {
		return nil, fmt.Errorf("intent: raw intent is required")
	}
	active, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return nil, fmt.Errorf("intent: load active goal: %w", err)
	}
	if active != nil {
		return nil, fmt.Errorf(
			"active goal %s already exists; complete or cancel it before creating a new goal",
			active.GoalID,
		)
	}

	goal := schema.GoalIR{
		GoalID:         idgen.New("G"),
		OriginalIntent: intentText,
		GoalConditions: []schema.GoalCondition{
			{
				ID:                   idgen.New("GC"),
				Description:          intentText,
				EffectiveDescription: intentText,
				Status:               schema.GoalConditionUnmet,
			},
			{
				ID:                   idgen.New("GC"),
				Description:          "All existing tests continue to pass",
				EffectiveDescription: "All existing tests continue to pass",
				Status:               schema.GoalConditionUnmet,
			},
		},
		RiskLevel: inferRiskLevel(intentText),
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}
	if err := s.store.SaveGoal(ctx, &goal); err != nil {
		return nil, fmt.Errorf("intent: save goal %s: %w", goal.GoalID, err)
	}
	return &goal, nil
}

func inferRiskLevel(intentText string) schema.RiskLevel {
	lower := strings.ToLower(intentText)
	highRiskKeywords := []string{
		"delete", "drop", "remove all", "migration", "dependency",
		"security", "auth", "credentials",
	}
	for _, keyword := range highRiskKeywords {
		if strings.Contains(lower, keyword) {
			return schema.RiskHigh
		}
	}
	return schema.RiskMedium
}
