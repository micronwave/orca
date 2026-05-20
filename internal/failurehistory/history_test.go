package failurehistory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/failurehistory"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

func newTestStore(t *testing.T) store.ArtifactStore {
	t.Helper()
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
	return st
}

func seedGoal(t *testing.T, ctx context.Context, st store.ArtifactStore, goalID string) {
	t.Helper()
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID,
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-" + goalID,
			Status: schema.GoalConditionUnmet,
		}},
		Status: schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
}

func seedCapsule(t *testing.T, ctx context.Context, st store.ArtifactStore, capsuleID, conditionID string) {
	t.Helper()
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:    "OB-" + capsuleID,
		GoalConditionID: conditionID,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{"OB-" + capsuleID},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
}

// TestPrepare_FirstOccurrenceHasNoPriorHistory verifies that the first failure
// for a given signature has PriorAttemptCount=0 and empty PriorCapsuleIDs.
func TestPrepare_FirstOccurrenceHasNoPriorHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)
	const goalID = "G-fp-first"
	seedGoal(t, ctx, st, goalID)
	seedCapsule(t, ctx, st, "CAP-fp-first-1", "GC-"+goalID)

	f := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-first-1",
		SourceCapsuleID: "CAP-fp-first-1",
		FailureType:     schema.FailureTest,
		Summary:         "test failed",
		ErrorSignature:  "go test ./...\nfailure",
	}
	if err := failurehistory.Prepare(ctx, st, goalID, f, false); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if f.PriorAttemptCount != 0 {
		t.Fatalf("PriorAttemptCount = %d, want 0 for first occurrence", f.PriorAttemptCount)
	}
	if len(f.PriorCapsuleIDs) != 0 {
		t.Fatalf("PriorCapsuleIDs = %v, want empty for first occurrence", f.PriorCapsuleIDs)
	}
	if f.RecommendedNextAction == "" {
		t.Fatal("RecommendedNextAction empty, want non-empty recommendation for test failure")
	}
	// Signature must be normalized.
	if f.ErrorSignature != "go test ./...\nfailure" {
		t.Fatalf("ErrorSignature = %q, want normalized lowercase", f.ErrorSignature)
	}
}

// TestPrepare_SecondOccurrenceIncludesPriorCapsule verifies that after saving
// the first fingerprint, a second Prepare call for the same normalized signature
// produces PriorAttemptCount=1 and PriorCapsuleIDs=[first capsule].
func TestPrepare_SecondOccurrenceIncludesPriorCapsule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)
	const goalID = "G-fp-second"
	seedGoal(t, ctx, st, goalID)
	seedCapsule(t, ctx, st, "CAP-fp-second-1", "GC-"+goalID)
	seedCapsule(t, ctx, st, "CAP-fp-second-2", "GC-"+goalID)

	first := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-second-1",
		SourceCapsuleID: "CAP-fp-second-1",
		FailureType:     schema.FailureTest,
		Summary:         "test failed",
		ErrorSignature:  "go test ./...\nfailure",
	}
	if err := failurehistory.Prepare(ctx, st, goalID, first, false); err != nil {
		t.Fatalf("Prepare first: %v", err)
	}
	if err := st.SaveFailure(ctx, first); err != nil {
		t.Fatalf("SaveFailure first: %v", err)
	}

	// Second capsule, same normalized signature (raw whitespace variations are normalized).
	second := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-second-2",
		SourceCapsuleID: "CAP-fp-second-2",
		FailureType:     schema.FailureTest,
		Summary:         "test failed",
		ErrorSignature:  "  GO TEST ./...\n\nFAILURE  ", // normalizes to same value
	}
	if err := failurehistory.Prepare(ctx, st, goalID, second, false); err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	if second.PriorAttemptCount != 1 {
		t.Fatalf("PriorAttemptCount = %d, want 1 for second occurrence", second.PriorAttemptCount)
	}
	if len(second.PriorCapsuleIDs) != 1 || second.PriorCapsuleIDs[0] != "CAP-fp-second-1" {
		t.Fatalf("PriorCapsuleIDs = %v, want [CAP-fp-second-1]", second.PriorCapsuleIDs)
	}
}

// TestPrepare_RepeatedFailuresEscalateToHumanReview verifies that after two
// prior occurrences, Prepare sets the "route to human review" recommendation.
func TestPrepare_RepeatedFailuresEscalateToHumanReview(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)
	const goalID = "G-fp-escalate"
	seedGoal(t, ctx, st, goalID)
	seedCapsule(t, ctx, st, "CAP-fp-esc-1", "GC-"+goalID)
	seedCapsule(t, ctx, st, "CAP-fp-esc-2", "GC-"+goalID)
	seedCapsule(t, ctx, st, "CAP-fp-esc-3", "GC-"+goalID)

	const rawSig = "go test ./...\nescalation failure"
	priorCapsules := []string{"CAP-fp-esc-1", "CAP-fp-esc-2"}
	for i, capsuleID := range priorCapsules {
		f := &schema.FailureFingerprint{
			FailureID:       "FAIL-fp-esc-" + strings.TrimPrefix(strings.TrimPrefix(capsuleID, "CAP-fp-esc-"), ""),
			SourceCapsuleID: capsuleID,
			FailureType:     schema.FailureTest,
			Summary:         "escalation failure",
			ErrorSignature:  rawSig,
		}
		if err := failurehistory.Prepare(ctx, st, goalID, f, false); err != nil {
			t.Fatalf("Prepare[%d]: %v", i, err)
		}
		if err := st.SaveFailure(ctx, f); err != nil {
			t.Fatalf("SaveFailure[%d]: %v", i, err)
		}
	}

	third := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-esc-3",
		SourceCapsuleID: "CAP-fp-esc-3",
		FailureType:     schema.FailureTest,
		Summary:         "escalation failure",
		ErrorSignature:  rawSig,
	}
	if err := failurehistory.Prepare(ctx, st, goalID, third, false); err != nil {
		t.Fatalf("Prepare third: %v", err)
	}
	if third.PriorAttemptCount != 2 {
		t.Fatalf("PriorAttemptCount = %d, want 2", third.PriorAttemptCount)
	}
	if len(third.PriorCapsuleIDs) != 2 {
		t.Fatalf("PriorCapsuleIDs len = %d, want 2", len(third.PriorCapsuleIDs))
	}
	if !strings.Contains(third.RecommendedNextAction, "human review") {
		t.Fatalf("RecommendedNextAction = %q, want 'human review' keyword for repeated failures", third.RecommendedNextAction)
	}
}

// TestPrepare_NoLearningSkipsHistoryAndRecommendation verifies that when
// noLearning=true, Prepare does not load prior history and leaves
// RecommendedNextAction empty.
func TestPrepare_NoLearningSkipsHistoryAndRecommendation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)
	const goalID = "G-fp-nolearn"
	seedGoal(t, ctx, st, goalID)
	seedCapsule(t, ctx, st, "CAP-fp-nolearn-1", "GC-"+goalID)
	seedCapsule(t, ctx, st, "CAP-fp-nolearn-2", "GC-"+goalID)

	// Persist a prior failure so history would show PriorAttemptCount=1 without noLearning.
	first := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-nolearn-1",
		SourceCapsuleID: "CAP-fp-nolearn-1",
		FailureType:     schema.FailureTest,
		Summary:         "test fail",
		ErrorSignature:  "go test ./...\nnolearn failure",
	}
	if err := failurehistory.Prepare(ctx, st, goalID, first, false); err != nil {
		t.Fatalf("Prepare prior: %v", err)
	}
	if err := st.SaveFailure(ctx, first); err != nil {
		t.Fatalf("SaveFailure prior: %v", err)
	}

	second := &schema.FailureFingerprint{
		FailureID:       "FAIL-fp-nolearn-2",
		SourceCapsuleID: "CAP-fp-nolearn-2",
		FailureType:     schema.FailureTest,
		Summary:         "test fail",
		ErrorSignature:  "go test ./...\nnolearn failure",
	}
	if err := failurehistory.Prepare(ctx, st, goalID, second, true); err != nil {
		t.Fatalf("Prepare noLearning: %v", err)
	}
	if second.PriorAttemptCount != 0 {
		t.Fatalf("PriorAttemptCount = %d, want 0 when noLearning=true", second.PriorAttemptCount)
	}
	if len(second.PriorCapsuleIDs) != 0 {
		t.Fatalf("PriorCapsuleIDs = %v, want empty when noLearning=true", second.PriorCapsuleIDs)
	}
	if second.RecommendedNextAction != "" {
		t.Fatalf("RecommendedNextAction = %q, want empty when noLearning=true", second.RecommendedNextAction)
	}
}

// TestNormalizeSignature verifies whitespace normalization.
func TestNormalizeSignature(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "lowercase", input: "GO TEST ./...", want: "go test ./..."},
		{name: "trim", input: "  go test  ", want: "go test"},
		{name: "crlf", input: "line1\r\nline2", want: "line1\nline2"},
		{name: "collapse double newlines", input: "line1\n\nline2", want: "line1\nline2"},
		{name: "mixed upper and whitespace", input: "  GO TEST ./...\n\nFAILURE  ", want: "go test ./...\nfailure"},
		{name: "empty", input: "", want: ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := failurehistory.NormalizeSignature(tc.input)
			if got != tc.want {
				t.Fatalf("NormalizeSignature(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestRecommendedNextAction_RuleTable verifies the rule-table dispatch.
func TestRecommendedNextAction_RuleTable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		failure     *schema.FailureFingerprint
		wantContain string
	}{
		{
			name:        "test failure first attempt",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailureTest, PriorAttemptCount: 0},
			wantContain: "reproduce",
		},
		{
			name:        "lint failure",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailureLint, PriorAttemptCount: 0},
			wantContain: "static gate",
		},
		{
			name:        "typecheck failure",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailureTypecheck, PriorAttemptCount: 0},
			wantContain: "static gate",
		},
		{
			name:        "infra failure",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailureInfra, PriorAttemptCount: 0},
			wantContain: "infrastructure",
		},
		{
			name:        "policy failure",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailurePolicy, PriorAttemptCount: 0},
			wantContain: "policy",
		},
		{
			name:        "repeated failure forces human review route",
			failure:     &schema.FailureFingerprint{FailureType: schema.FailureTest, PriorAttemptCount: 2},
			wantContain: "human review",
		},
		{
			name:        "nil failure returns empty",
			failure:     nil,
			wantContain: "",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := failurehistory.RecommendedNextAction(tc.failure)
			if tc.wantContain == "" {
				if got != "" {
					t.Fatalf("RecommendedNextAction = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantContain) {
				t.Fatalf("RecommendedNextAction = %q, want substring %q", got, tc.wantContain)
			}
		})
	}
}
