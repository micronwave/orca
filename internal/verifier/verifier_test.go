package verifier

import (
	"context"
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
	}).(*service)
	engine.commandChecker = func(string) error { return nil }

	result, err := engine.Verify(ctx, "PATCH-verify")
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
	}).(*service)
	engine.commandChecker = func(string) error { return nil }

	result, err := engine.Verify(ctx, "PATCH-fail")
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

func TestVerifyWithSupplements_UsesSupplementalEvidence(t *testing.T) {
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

	engine := New(st, config.VerifierConfig{}, fakeGateRunner{}).(*service)
	engine.commandChecker = func(string) error { return nil }
	result, err := engine.VerifyWithSupplements(ctx, "PATCH-supp-ev", VerifyInput{
		SupplementalEvidenceIDs: []string{"EV-supp-ev"},
	})
	if err != nil {
		t.Fatalf("VerifyWithSupplements: %v", err)
	}
	if result.RecommendedAction != schema.ActionAccept {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionAccept)
	}
	if len(result.ObligationResults) != 1 || result.ObligationResults[0].Verdict != schema.VerdictSatisfied {
		t.Fatalf("ObligationResults = %+v, want one satisfied result", result.ObligationResults)
	}
}

func TestVerifyWithSupplements_ProposedRiskClaimForcesHumanReview(t *testing.T) {
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

	engine := New(st, config.VerifierConfig{}, fakeGateRunner{}).(*service)
	engine.commandChecker = func(string) error { return nil }
	result, err := engine.VerifyWithSupplements(ctx, "PATCH-supp-claim", VerifyInput{
		SupplementalClaimIDs: []string{"CLM-supp-risk"},
	})
	if err != nil {
		t.Fatalf("VerifyWithSupplements: %v", err)
	}
	if result.RecommendedAction != schema.ActionHumanReview {
		t.Fatalf("RecommendedAction = %s, want %s", result.RecommendedAction, schema.ActionHumanReview)
	}
	if !strings.Contains(result.RecommendationRationale, "CLM-supp-risk") {
		t.Fatalf("RecommendationRationale = %q, want claim id", result.RecommendationRationale)
	}
}
