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
	"regexp"
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

	primaryParts := splitPrimaryConditions(intentText)
	conditions := make([]schema.GoalCondition, 0, len(primaryParts)+1)
	for _, part := range primaryParts {
		if isScopeOnlyClause(part) {
			continue
		}
		conditions = append(conditions, schema.GoalCondition{
			ID:                   idgen.New("GC"),
			Description:          part,
			EffectiveDescription: part,
			Status:               schema.GoalConditionUnmet,
		})
	}
	conditions = append(conditions, schema.GoalCondition{
		ID:                   idgen.New("GC"),
		Description:          "All existing tests continue to pass",
		EffectiveDescription: "All existing tests continue to pass",
		Status:               schema.GoalConditionUnmet,
	})
	goal := schema.GoalIR{
		GoalID:           idgen.New("G"),
		OriginalIntent:   intentText,
		GoalConditions:   conditions,
		ScopeConstraints: parseScopeConstraints(intentText),
		RiskLevel:        inferRiskLevel(intentText),
		CreatedAt:        time.Now().UTC(),
		Status:           schema.GoalStatusActive,
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

// splitPrimaryConditions splits intentText on newlines and semicolons to
// produce one GoalCondition per distinct sub-goal. Single-line intents
// without separators return a one-element slice.
func splitPrimaryConditions(intentText string) []string {
	var parts []string
	for _, raw := range strings.FieldsFunc(intentText, func(r rune) bool {
		return r == '\n' || r == ';'
	}) {
		p := strings.TrimSpace(raw)
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return []string{intentText}
	}
	return parts
}

const scopeVerbAlts = "touch|edit|modify|change|update"

var (
	// reAllowedTail captures the path list after "only <verb>", stopping at
	// clause boundaries (semicolon, period, newline, or sentence-end punctuation).
	reAllowedTail = regexp.MustCompile(
		"(?i)\\bonly\\s+(?:" + scopeVerbAlts + ")\\s+([^;.\\n!?]+)")
	// reForbiddenTail captures the path list after "do not/don't <verb>".
	reForbiddenTail = regexp.MustCompile(
		"(?i)\\b(?:do\\s+not|don'?t)\\s+(?:" + scopeVerbAlts + ")\\s+([^;.\\n!?]+)")
	// reSplitPathList splits a captured tail on commas and "and".
	reSplitPathList = regexp.MustCompile(`\s*,\s*|\s+and\s+`)
	// rePureScopeClause matches a clause that begins with a scope directive.
	rePureScopeClause = regexp.MustCompile(
		"(?i)^\\s*(?:only\\s+(?:" + scopeVerbAlts + ")|(?:do\\s+not|don'?t)\\s+(?:" + scopeVerbAlts + "))\\b")
)

// parseScopeConstraints extracts AllowedFiles and ForbiddenFiles from
// structured scope phrases in the intent text. Only slash-containing tokens
// are extracted to avoid false positives on natural language words.
func parseScopeConstraints(intentText string) schema.ScopeConstraints {
	var sc schema.ScopeConstraints
	for _, m := range reAllowedTail.FindAllStringSubmatch(intentText, -1) {
		if len(m) > 1 {
			sc.AllowedFiles = append(sc.AllowedFiles, extractPathsFromTail(m[1])...)
		}
	}
	for _, m := range reForbiddenTail.FindAllStringSubmatch(intentText, -1) {
		if len(m) > 1 {
			sc.ForbiddenFiles = append(sc.ForbiddenFiles, extractPathsFromTail(m[1])...)
		}
	}
	return sc
}

// extractPathsFromTail splits a captured tail on commas and "and", strips
// surrounding punctuation and whitespace, and returns slash-containing tokens.
func extractPathsFromTail(tail string) []string {
	parts := reSplitPathList.Split(tail, -1)
	var paths []string
	for _, p := range parts {
		p = strings.Trim(p, " \t.,;!?`'\"")
		if strings.ContainsRune(p, '/') {
			paths = append(paths, p)
		}
	}
	return paths
}

// isScopeOnlyClause reports whether s is a pure scope directive with no
// substantive work objective. It returns true when the clause starts with a
// scope directive verb phrase AND that directive resolves at least one path.
func isScopeOnlyClause(s string) bool {
	if !rePureScopeClause.MatchString(s) {
		return false
	}
	sc := parseScopeConstraints(s)
	return len(sc.AllowedFiles) > 0 || len(sc.ForbiddenFiles) > 0
}
