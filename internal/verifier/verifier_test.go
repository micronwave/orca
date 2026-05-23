package verifier

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type fakeGateRunner struct {
	results map[string]gateResult
}

func TestEvidenceContentHashIsTruncatedHex(t *testing.T) {
	got := evidenceContentHash(
		schema.EvidenceTestResult,
		"go test ./...",
		".",
		0,
		"ok",
		[]string{"OB-2", "OB-1"},
		"G-1",
		"SNAP-1",
	)
	if len(got) != 16 {
		t.Fatalf("content hash length = %d, want 16", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("content hash %q is not hex: %v", got, err)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("content hash %q must not contain a prefix or path separator", got)
	}
}

type gateResult struct {
	exitCode int
	output   string
	err      error
}

func (f fakeGateRunner) Run(ctx context.Context, command, workingDir string) (int, string, error) {
	result, ok := f.results[command]
	if !ok {
		return 0, "", nil
	}
	return result.exitCode, result.output, result.err
}

type countingGateRunner struct {
	calls   int
	results map[string]gateResult
}

func (f *countingGateRunner) Run(ctx context.Context, command, workingDir string) (int, string, error) {
	f.calls++
	result, ok := f.results[command]
	if !ok {
		return 0, "", nil
	}
	return result.exitCode, result.output, result.err
}

func TestProposeObligations_createsTemplatesForUnmetConditions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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
	goal := &schema.GoalIR{
		GoalID:         "G-propose",
		OriginalIntent: "test",
		GoalConditions: []schema.GoalCondition{
			{ID: "GC-unmet", Status: schema.GoalConditionUnmet},
			{ID: "GC-partial", Status: schema.GoalConditionPartiallyMet},
			{ID: "GC-met", Status: schema.GoalConditionMet},
		},
		RiskLevel: schema.RiskHigh,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	engine := New(st, config.VerifierConfig{}, nil)
	ids, err := engine.ProposeObligations(ctx, goal.GoalID)
	if err != nil {
		t.Fatalf("ProposeObligations: %v", err)
	}
	if len(ids) != 6 {
		t.Fatalf("created IDs len = %d, want 6", len(ids))
	}

	unmet, err := st.LoadObligationsForCondition(ctx, "GC-unmet")
	if err != nil {
		t.Fatalf("LoadObligationsForCondition: %v", err)
	}
	if len(unmet) != 3 {
		t.Fatalf("unmet obligations len = %d, want 3", len(unmet))
	}
	foundTestObligation := false
	for _, obligation := range unmet {
		if obligation.Description == "Run all tests and confirm exit code 0" {
			foundTestObligation = true
			if !obligation.Blocking {
				t.Fatalf("test obligation blocking = false, want true")
			}
			if obligation.RiskLevel != schema.RiskHigh {
				t.Fatalf("test obligation risk = %s, want %s", obligation.RiskLevel, schema.RiskHigh)
			}
		}
	}
	if !foundTestObligation {
		t.Fatal("missing tests obligation template")
	}
}

func TestVerify_savesEvidenceAndResult(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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

	goal := &schema.GoalIR{
		GoalID:         "G-verify",
		OriginalIntent: "verify",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-verify",
			Description:          "verify",
			EffectiveDescription: "verify",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	obligation := &schema.Obligation{
		ObligationID:     "OB-verify",
		GoalConditionID:  "GC-verify",
		Description:      "proof",
		EvidenceRequired: []string{string(schema.EvidenceDiffRiskReport), string(schema.EvidenceLintResult), string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}
	if err := st.SaveObligation(ctx, obligation); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:      "CAP-verify",
		ObligationIDs:  []string{"OB-verify"},
		AllowedPaths:   []string{"internal"},
		ForbiddenPaths: []string{"secrets"},
		State:          schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-verify",
		CapsuleID:            "CAP-verify",
		ChangedFiles:         []string{`internal\verifier\verifier.go`},
		ObligationIDsClaimed: []string{"OB-verify"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	engine := New(st, config.VerifierConfig{
		Gates: []config.VerifierGate{
			{Name: "go_vet", Command: "go vet ./...", Blocking: true},
			{Name: "go_test", Command: "go test ./...", Blocking: true},
		},
	}, fakeGateRunner{
		results: map[string]gateResult{
			"go vet ./...":  {exitCode: 0, output: "ok"},
			"go test ./...": {exitCode: 0, output: "ok"},
		},
	})
	engine.commandChecker = func(string) error { return nil }

	result, err := engine.Verify(ctx, "PATCH-verify", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.RecommendedAction != schema.ActionAccept {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionAccept)
	}
	if len(result.ObligationResults) != 1 {
		t.Fatalf("ObligationResults len = %d, want 1", len(result.ObligationResults))
	}
	if result.ObligationResults[0].Verdict != schema.VerdictSatisfied {
		t.Fatalf("verdict = %s, want %s", result.ObligationResults[0].Verdict, schema.VerdictSatisfied)
	}
	if len(result.ObligationResults[0].EvidenceIDs) != 3 {
		t.Fatalf("EvidenceIDs len = %d, want 3", len(result.ObligationResults[0].EvidenceIDs))
	}

	if _, err := st.LoadVerifierResultForPatch(ctx, "PATCH-verify"); err != nil {
		t.Fatalf("LoadVerifierResultForPatch: %v", err)
	}
	evidenceForObligation, err := st.LoadEvidenceForObligation(ctx, "OB-verify")
	if err != nil {
		t.Fatalf("LoadEvidenceForObligation: %v", err)
	}
	if len(evidenceForObligation) != 3 {
		t.Fatalf("evidence count for obligation = %d, want 3", len(evidenceForObligation))
	}
}

func TestVerify_failsBlockingObligationWhenTestGateFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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

	goal := &schema.GoalIR{
		GoalID: "G-fail",
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-fail",
			Status: schema.GoalConditionUnmet,
		}},
		Status: schema.GoalStatusActive,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-fail",
		GoalConditionID:  "GC-fail",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-fail",
		ObligationIDs: []string{"OB-fail"},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-fail",
		CapsuleID:            "CAP-fail",
		ChangedFiles:         []string{"internal/verifier/verifier.go"},
		ObligationIDsClaimed: []string{"OB-fail"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	engine := New(st, config.VerifierConfig{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: true},
		},
	}, fakeGateRunner{
		results: map[string]gateResult{
			"go test ./...": {exitCode: 1, output: "FAIL"},
		},
	})
	engine.commandChecker = func(string) error { return nil }

	result, err := engine.Verify(ctx, "PATCH-fail", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.RecommendedAction != schema.ActionRetry {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionRetry)
	}
	if len(result.ObligationResults) != 1 {
		t.Fatalf("ObligationResults len = %d, want 1", len(result.ObligationResults))
	}
	if result.ObligationResults[0].Verdict != schema.VerdictFailed {
		t.Fatalf("verdict = %s, want %s", result.ObligationResults[0].Verdict, schema.VerdictFailed)
	}
	if len(result.BlockingFailures) == 0 {
		t.Fatal("BlockingFailures is empty, want at least one failure")
	}
	if len(result.FailureIDs) != 1 {
		t.Fatalf("FailureIDs = %v, want one verifier gate failure", result.FailureIDs)
	}
	failure, err := st.LoadFailure(ctx, result.FailureIDs[0])
	if err != nil {
		t.Fatalf("LoadFailure: %v", err)
	}
	if failure.SourceCapsuleID != "CAP-fail" || failure.FailureType != schema.FailureTest {
		t.Fatalf("failure = %+v, want test failure for capsule CAP-fail", failure)
	}
	if failure.ErrorSignature != "go test ./...\ntest gate \"go_test\" failed" {
		t.Fatalf("ErrorSignature = %q, want normalized gate signature", failure.ErrorSignature)
	}
	failuresForCapsule, err := st.LoadFailuresForCapsule(ctx, "CAP-fail")
	if err != nil {
		t.Fatalf("LoadFailuresForCapsule: %v", err)
	}
	if len(failuresForCapsule) != 1 || failuresForCapsule[0].FailureID != result.FailureIDs[0] {
		t.Fatalf("LoadFailuresForCapsule = %+v, want verifier failure", failuresForCapsule)
	}
}

func TestVerify_ReusesMatchingGateEvidenceForCurrentSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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
	seedVerifierReuseScenario(t, ctx, st, "SNAP-reuse")
	workingDir := root + `\worktree`
	reuseKey := verifierReuseKey(schema.EvidenceTestResult, "go test ./...", workingDir, []string{"OB-reuse"}, "SNAP-reuse")
	if err := st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID:       "EV-reuse-source",
		Type:             schema.EvidenceTestResult,
		Source:           "verifier",
		Command:          "go test ./...",
		ExitCode:         0,
		Summary:          "cached pass",
		Supports:         []string{"OB-reuse"},
		ContentHash:      "sha256:old",
		ReuseKey:         reuseKey,
		ValidatedAgainst: "SNAP-reuse",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence reusable: %v", err)
	}

	runner := &countingGateRunner{results: map[string]gateResult{
		"go test ./...": {exitCode: 1, output: "should not run"},
	}}
	engine := NewWithConfig(st, Config{
		Gates:      []config.VerifierGate{{Name: "go_test", Command: "go test ./...", Blocking: true}},
		WorkingDir: workingDir,
	}, runner)
	engine.commandChecker = func(string) error { return nil }

	result, err := engine.Verify(ctx, "PATCH-reuse", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want 0 for reused gate", runner.calls)
	}
	if result.RecommendedAction != schema.ActionAccept {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionAccept)
	}
	var reused *schema.EvidenceArtifact
	for _, evidenceID := range result.ObligationResults[0].EvidenceIDs {
		ev, err := st.LoadEvidence(ctx, evidenceID)
		if err != nil {
			t.Fatalf("LoadEvidence %s: %v", evidenceID, err)
		}
		if ev.ReusedFromID == "EV-reuse-source" {
			reused = ev
		}
	}
	if reused == nil {
		t.Fatal("verifier result did not reference reused evidence")
	}
	if reused.ValidatedAgainst != "SNAP-reuse" || reused.ReuseKey != reuseKey || reused.ContentHash != "sha256:old" {
		t.Fatalf("reused evidence metadata = %+v", reused)
	}
}

func TestVerify_ChangedSnapshotOrNoLearningForcesFreshGateRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		noLearning bool
		snapshotID string
	}{
		{name: "changed snapshot", snapshotID: "SNAP-new"},
		{name: "no learning", noLearning: true, snapshotID: "SNAP-old"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
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
			seedVerifierReuseScenario(t, ctx, st, tc.snapshotID)
			workingDir := root + `\worktree`
			reuseKey := verifierReuseKey(schema.EvidenceTestResult, "go test ./...", workingDir, []string{"OB-reuse"}, "SNAP-old")
			if err := st.SaveEvidence(ctx, &schema.EvidenceArtifact{
				EvidenceID:       "EV-old",
				Type:             schema.EvidenceTestResult,
				Source:           "verifier",
				Command:          "go test ./...",
				ExitCode:         0,
				Summary:          "old pass",
				Supports:         []string{"OB-reuse"},
				ContentHash:      "sha256:same-content",
				ReuseKey:         reuseKey,
				ValidatedAgainst: "SNAP-old",
				CreatedAt:        time.Now().UTC(),
			}); err != nil {
				t.Fatalf("SaveEvidence old: %v", err)
			}
			runner := &countingGateRunner{results: map[string]gateResult{
				"go test ./...": {exitCode: 0, output: "fresh pass"},
			}}
			engine := NewWithConfig(st, Config{
				Gates:      []config.VerifierGate{{Name: "go_test", Command: "go test ./...", Blocking: true}},
				WorkingDir: workingDir,
				NoLearning: tc.noLearning,
			}, runner)
			engine.commandChecker = func(string) error { return nil }

			result, err := engine.Verify(ctx, "PATCH-reuse", VerifyInput{})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if runner.calls != 1 {
				t.Fatalf("runner calls = %d, want 1", runner.calls)
			}
			for _, evidenceID := range result.ObligationResults[0].EvidenceIDs {
				ev, err := st.LoadEvidence(ctx, evidenceID)
				if err != nil {
					t.Fatalf("LoadEvidence %s: %v", evidenceID, err)
				}
				if ev.ReusedFromID != "" {
					t.Fatalf("fresh run produced reused evidence: %+v", ev)
				}
			}
		})
	}
}

func TestParseCommand_supportsQuotedExecutablePath(t *testing.T) {
	t.Parallel()

	executable, args, err := parseCommand(`"C:\Program Files\Go\bin\go" test ./...`)
	if err != nil {
		t.Fatalf("parseCommand: %v", err)
	}
	if executable != `C:\Program Files\Go\bin\go` {
		t.Fatalf("executable = %q", executable)
	}
	if strings.Join(args, " ") != "test ./..." {
		t.Fatalf("args = %v", args)
	}
}

func TestVerify_UsesSupplementalEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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

	goal := &schema.GoalIR{
		GoalID: "G-supp-ev",
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-supp-ev",
			Status: schema.GoalConditionUnmet,
		}},
		Status: schema.GoalStatusActive,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-supp-ev",
		GoalConditionID:  "GC-supp-ev",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-supp-ev",
		ObligationIDs: []string{"OB-supp-ev"},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-supp-ev",
		CapsuleID:            "CAP-supp-ev",
		ChangedFiles:         []string{"internal/verifier/verifier.go"},
		ObligationIDsClaimed: []string{"OB-supp-ev"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-supp-ev",
		Type:       schema.EvidenceTestResult,
		Source:     "claude",
		ExitCode:   0,
		Supports:   []string{"OB-supp-ev"},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	engine := New(st, config.VerifierConfig{}, fakeGateRunner{})
	engine.commandChecker = func(string) error { return nil }
	result, err := engine.Verify(ctx, "PATCH-supp-ev", VerifyInput{
		SupplementalEvidenceIDs: []string{"EV-supp-ev"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.RecommendedAction != schema.ActionAccept {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionAccept)
	}
	if len(result.ObligationResults) != 1 || result.ObligationResults[0].Verdict != schema.VerdictSatisfied {
		t.Fatalf("ObligationResults = %+v, want one satisfied result", result.ObligationResults)
	}
}

func TestVerify_ErrorsWhenNoActiveGoal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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

	// Save a completed goal with a condition so we can reference it in an obligation.
	// The goal is NOT active — LoadActiveGoal will return (nil, nil).
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         "G-nogoal-completed",
		OriginalIntent: "completed goal",
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-nogoal",
			Status: schema.GoalConditionMet,
		}},
		Status:    schema.GoalStatusComplete,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-nogoal",
		GoalConditionID:  "GC-nogoal",
		Description:      "test",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-nogoal",
		ObligationIDs: []string{"OB-nogoal"},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-nogoal",
		CapsuleID:            "CAP-nogoal",
		ObligationIDsClaimed: []string{"OB-nogoal"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	engine := New(st, config.VerifierConfig{}, fakeGateRunner{})
	engine.commandChecker = func(string) error { return nil }
	_, err = engine.Verify(ctx, "PATCH-nogoal", VerifyInput{})
	if err == nil {
		t.Fatal("Verify: expected error when no active goal, got nil")
	}
	if !strings.Contains(err.Error(), "no active goal") {
		t.Fatalf("Verify error = %q, want message containing 'no active goal'", err.Error())
	}
}

func seedVerifierReuseScenario(t *testing.T, ctx context.Context, st *store.FileStore, snapshotID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         "G-reuse",
		OriginalIntent: "reuse verifier evidence",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-reuse",
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-reuse",
		GoalConditionID:  "GC-reuse",
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceDiffRiskReport), string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-reuse",
		ObligationIDs: []string{"OB-reuse"},
		State:         schema.CapsuleStateCompleted,
		Sandbox:       schema.CapsuleSandbox{WorktreePath: `E:\unused`},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-reuse",
		CapsuleID:            "CAP-reuse",
		BaseCommit:           "abc123",
		ChangedFiles:         []string{"internal/verifier/verifier.go"},
		ObligationIDsClaimed: []string{"OB-reuse"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := st.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID:  snapshotID,
		GoalID:      "G-reuse",
		EventID:     "EVT-reuse",
		SequenceNum: 1,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
}

func TestVerify_ProposedRiskClaimForcesHumanReview(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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

	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: "G-supp-claim",
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-supp-claim",
			Status: schema.GoalConditionUnmet,
		}},
		Status: schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     "OB-supp-claim",
		GoalConditionID:  "GC-supp-claim",
		EvidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
		Blocking:         true,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-supp-claim",
		ObligationIDs: []string{"OB-supp-claim"},
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-supp-claim",
		CapsuleID:            "CAP-supp-claim",
		ChangedFiles:         []string{"internal/verifier/verifier.go"},
		ObligationIDsClaimed: []string{"OB-supp-claim"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-reviewer",
		ObligationIDs: []string{"OB-supp-claim"},
		Role:          schema.RoleReviewer,
	}); err != nil {
		t.Fatalf("SaveCapsule reviewer: %v", err)
	}
	if err := st.SaveClaim(ctx, &schema.ClaimArtifact{
		ClaimID:         "CLM-supp-risk",
		Text:            "reviewer found unresolved risk",
		ClaimType:       schema.ClaimRisk,
		SourceCapsuleID: "CAP-reviewer",
		Status:          schema.ClaimProposed,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	engine := New(st, config.VerifierConfig{}, fakeGateRunner{})
	engine.commandChecker = func(string) error { return nil }
	result, err := engine.Verify(ctx, "PATCH-supp-claim", VerifyInput{
		SupplementalClaimIDs: []string{"CLM-supp-risk"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.RecommendedAction != schema.ActionHumanReview {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionHumanReview)
	}
	if !strings.Contains(result.RecommendationRationale, "CLM-supp-risk") {
		t.Fatalf("RecommendationRationale = %q, want claim id", result.RecommendationRationale)
	}
}

func TestMAVEN_FactualProbe_MissingEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newMAVENStore(t, ctx, mavenScenario{
		obligationID:     "OB-maven-factual",
		evidenceRequired: []string{string(schema.EvidenceDiffRiskReport), string(schema.EvidenceStaticAnalysis)},
		blocking:         false,
		expectedFiles:    []string{"internal/verifier/verifier.go"},
		patchID:          "PATCH-maven-factual",
		changedFiles:     []string{"internal/verifier/verifier.go"},
	})
	engine := newMAVENEngine(st, config.AdvancedConfig{Enabled: true, Maven: true})

	result, err := engine.Verify(ctx, "PATCH-maven-factual", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertWarningContains(t, result.Warnings, "[maven] factual")
	if result.RecommendedAction != schema.ActionHumanReview {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionHumanReview)
	}
}

func TestMAVEN_LogicalProbe_SatisfiedWithFailedEvidence(t *testing.T) {
	t.Parallel()

	engine := &Engine{}
	findings := engine.runMAVEN(
		&schema.PatchArtifact{PatchID: "PATCH-maven-logical", ChangedFiles: []string{"internal/verifier/verifier.go"}},
		[]*schema.Obligation{{
			ObligationID:     "OB-maven-logical",
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			ExpectedFiles:    []string{"internal/verifier/verifier.go"},
		}},
		[]schema.ObligationVerdict{{
			ObligationID: "OB-maven-logical",
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{"EV-maven-failed"},
		}},
		map[string]*schema.EvidenceArtifact{
			"EV-maven-failed": {
				EvidenceID: "EV-maven-failed",
				Type:       schema.EvidenceTestResult,
				ExitCode:   1,
				Supports:   []string{"OB-maven-logical"},
			},
		},
		nil,
	)
	assertWarningContains(t, findings.warnings, "[maven] logical")
	if !findings.requiresHumanReview {
		t.Fatal("requiresHumanReview = false, want true")
	}
}

func TestMAVEN_CausalProbe_OutOfScopeFiles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newMAVENStore(t, ctx, mavenScenario{
		obligationID:     "OB-maven-causal",
		evidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
		blocking:         true,
		expectedFiles:    []string{"internal/verifier/verifier.go"},
		patchID:          "PATCH-maven-causal",
		changedFiles:     []string{"README.md"},
	})
	engine := newMAVENEngine(st, config.AdvancedConfig{Enabled: true, Maven: true})

	result, err := engine.Verify(ctx, "PATCH-maven-causal", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertWarningContains(t, result.Warnings, "[maven] causal")
	if result.RecommendedAction == schema.ActionRetry {
		t.Fatalf("RecommendedAction = %s, want no rejection from causal warning", result.RecommendedAction)
	}
}

func TestMAVEN_AssumptionProbe_UnverifiedRiskClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newMAVENStore(t, ctx, mavenScenario{
		obligationID:     "OB-maven-assumption",
		evidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
		blocking:         true,
		expectedFiles:    []string{"internal/verifier/verifier.go"},
		patchID:          "PATCH-maven-assumption",
		changedFiles:     []string{"internal/verifier/verifier.go"},
		claim: &schema.ClaimArtifact{
			ClaimID:         "CLM-maven-risk",
			Text:            "unresolved risk",
			ClaimType:       schema.ClaimRisk,
			SourceCapsuleID: "CAP-maven-reviewer",
			Status:          schema.ClaimProposed,
		},
	})
	engine := newMAVENEngine(st, config.AdvancedConfig{Enabled: true, Maven: true})

	result, err := engine.Verify(ctx, "PATCH-maven-assumption", VerifyInput{
		SupplementalClaimIDs: []string{"CLM-maven-risk"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertWarningContains(t, result.Warnings, "[maven] assumption")
	if result.RecommendedAction != schema.ActionHumanReview {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionHumanReview)
	}
}

func TestMAVEN_DisabledByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newMAVENStore(t, ctx, mavenScenario{
		obligationID:     "OB-maven-disabled",
		evidenceRequired: []string{string(schema.EvidenceDiffRiskReport), string(schema.EvidenceStaticAnalysis)},
		blocking:         false,
		expectedFiles:    []string{"internal/verifier/verifier.go"},
		patchID:          "PATCH-maven-disabled",
		changedFiles:     []string{"internal/verifier/verifier.go"},
	})
	engine := newMAVENEngine(st, config.AdvancedConfig{})

	result, err := engine.Verify(ctx, "PATCH-maven-disabled", VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertNoMAVENWarnings(t, result.Warnings)
	if result.RecommendedAction == schema.ActionHumanReview && strings.Contains(result.RecommendationRationale, "[maven]") {
		t.Fatalf("MAVEN affected disabled recommendation: %+v", result)
	}
}

func TestMAVEN_FalsePositiveRepresentable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newMAVENStore(t, ctx, mavenScenario{
		obligationID:     "OB-maven-fp",
		evidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
		blocking:         true,
		expectedFiles:    []string{"internal/verifier/verifier.go"},
		patchID:          "PATCH-maven-fp",
		changedFiles:     []string{"internal/verifier/verifier.go"},
		claim: &schema.ClaimArtifact{
			ClaimID:         "CLM-maven-assumption",
			Text:            "possibly stale assumption",
			ClaimType:       schema.ClaimAssumption,
			SourceCapsuleID: "CAP-maven-reviewer",
			Status:          schema.ClaimProposed,
		},
	})
	engine := newMAVENEngine(st, config.AdvancedConfig{Enabled: true, Maven: true})

	result, err := engine.Verify(ctx, "PATCH-maven-fp", VerifyInput{
		SupplementalClaimIDs: []string{"CLM-maven-assumption"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.RecommendedAction != schema.ActionHumanReview {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionHumanReview)
	}
	if !strings.Contains(result.RecommendationRationale, "[maven]") {
		t.Fatalf("RecommendationRationale = %q, want [maven]", result.RecommendationRationale)
	}
	if len(result.ObligationResults) != 1 || result.ObligationResults[0].Verdict != schema.VerdictSatisfied {
		t.Fatalf("ObligationResults = %+v, want intact satisfied verdict", result.ObligationResults)
	}
	if len(result.BlockingFailures) != 0 {
		t.Fatalf("BlockingFailures = %v, want unchanged empty gate failures", result.BlockingFailures)
	}
}

type mavenScenario struct {
	obligationID     string
	evidenceRequired []string
	blocking         bool
	expectedFiles    []string
	patchID          string
	changedFiles     []string
	claim            *schema.ClaimArtifact
}

func newMAVENStore(t *testing.T, ctx context.Context, scenario mavenScenario) *store.FileStore {
	t.Helper()
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
	if err := st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: "G-maven",
		GoalConditions: []schema.GoalCondition{{
			ID:     "GC-maven",
			Status: schema.GoalConditionUnmet,
		}},
		Status: schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := st.SaveObligation(ctx, &schema.Obligation{
		ObligationID:     scenario.obligationID,
		GoalConditionID:  "GC-maven",
		EvidenceRequired: scenario.evidenceRequired,
		Blocking:         scenario.blocking,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
		ExpectedFiles:    scenario.expectedFiles,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-" + scenario.patchID,
		ObligationIDs: []string{scenario.obligationID},
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID:              scenario.patchID,
		CapsuleID:            "CAP-" + scenario.patchID,
		BaseCommit:           "abc123",
		ChangedFiles:         scenario.changedFiles,
		ObligationIDsClaimed: []string{scenario.obligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if scenario.claim != nil {
		if err := st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID:     "CAP-maven-reviewer",
			ObligationIDs: []string{scenario.obligationID},
			Role:          schema.RoleReviewer,
		}); err != nil {
			t.Fatalf("SaveCapsule reviewer: %v", err)
		}
		if err := st.SaveClaim(ctx, scenario.claim); err != nil {
			t.Fatalf("SaveClaim: %v", err)
		}
	}
	return st
}

func newMAVENEngine(st *store.FileStore, advanced config.AdvancedConfig) *Engine {
	engine := NewWithConfig(st, Config{Advanced: advanced}, fakeGateRunner{})
	engine.commandChecker = func(string) error { return nil }
	return engine
}

func assertWarningContains(t *testing.T, warnings []string, want string) {
	t.Helper()
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return
		}
	}
	t.Fatalf("warnings = %v, want substring %q", warnings, want)
}

func assertNoMAVENWarnings(t *testing.T, warnings []string) {
	t.Helper()
	for _, warning := range warnings {
		if strings.Contains(warning, "[maven]") {
			t.Fatalf("warnings = %v, want no MAVEN warnings", warnings)
		}
	}
}
