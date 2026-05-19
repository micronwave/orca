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
//	Writes (log):     EventGoalCreated
//
//	Must NOT import:  internal/planner, internal/runner, internal/verifier,
//	                  internal/reconciler, internal/projector, internal/budget,
//	                  internal/gate
package intent

import (
	"context"

	"github.com/micronwave/orca/internal/schema"
)

// IntentCompiler converts a raw user goal string into a GoalIR.
// It derives initial GoalConditions from the input, assigns IDs, persists
// the GoalIR, emits a goal_created event, and returns the saved record.
//
// The compiler may call a model to clarify intent, but it must not create
// Obligations or Capsules — those belong to the ObligationPlanner.
// One active goal per repo is the MVP constraint; the compiler should reject
// a second Compile call when a goal is already active.
type IntentCompiler interface {
	// Compile parses rawIntent, creates a GoalIR with initial GoalConditions,
	// persists it via the store, emits goal_created to the event log, and
	// returns the saved GoalIR.
	Compile(ctx context.Context, rawIntent string) (*schema.GoalIR, error)
}
