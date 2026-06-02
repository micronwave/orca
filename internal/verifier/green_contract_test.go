package verifier

import (
	"context"
	"os"
	"testing"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// setupGreenContractFixture creates a store + goal + capsule + patch for green contract tests.
func setupGreenContractFixture(t *testing.T) (context.Context, *store.FileStore, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	log, err := eventlog.Open(dir + "/events.log")
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(dir, log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	goal := &schema.GoalIR{
		GoalID:         "G-GREEN-1",
		OriginalIntent: "green contract test",
		Status:         schema.GoalStatusActive,
		GoalConditions: []schema.GoalCondition{
			{ID: "GC-1", Status: schema.GoalConditionUnmet},
		},
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	obl := &schema.Obligation{
		ObligationID:     "OB-G1",
		GoalConditionID:  "GC-1",
		Description:      "test obligation",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		Status:           schema.ObligationOpen,
	}
	if err := st.SaveObligation(ctx, obl); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	capsule := &schema.ExecutionCapsule{
		CapsuleID:     "CAP-G1",
		ObligationIDs: []string{"OB-G1"},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		Budget:        schema.CapsuleBudget{MaxWallTimeSeconds: 300},
		State:         schema.CapsuleStateCompleted,
	}
	if err := st.SaveCapsule(ctx, capsule); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// Write a dummy diff file.
	diffPath := dir + "/patch.diff"
	if err := os.WriteFile(diffPath, []byte("--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n+x\n"), 0o644); err != nil {
		t.Fatalf("write diff: %v", err)
	}
	patch := &schema.PatchArtifact{
		PatchID:              "PATCH-G1",
		CapsuleID:            "CAP-G1",
		BaseCommit:           "abc123",
		ChangedFiles:         []string{"foo.go"},
		DiffPath:             diffPath,
		Status:               schema.PatchCandidate,
		ObligationIDsClaimed: []string{"OB-G1"},
	}
	if err := st.SavePatch(ctx, patch); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	return ctx, st, dir, "PATCH-G1"
}

func TestGreenContract_TargetedTestsNotMergeReady(t *testing.T) {
	t.Parallel()
	ctx, st, _, patchID := setupGreenContractFixture(t)

	// Only a targeted_tests tier gate passes.
	runner := fakeGateRunner{results: map[string]gateResult{
		"go test ./...": {exitCode: 0, output: "ok"},
		"go vet ./...":  {exitCode: 0, output: "ok"},
	}}
	engine := NewWithConfig(st, Config{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: true, Tier: config.TierTargetedTests},
			{Name: "go_vet", Command: "go vet ./...", Blocking: true},
		},
	}, runner)
	result, err := engine.Verify(ctx, patchID, VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.GreenContract == nil {
		t.Fatal("expected GreenContract to be non-nil with a tier-annotated gate")
	}
	if result.GreenContract.ObservedGreenLevel != schema.GreenLevelTargetedTests {
		t.Errorf("expected targeted_tests level, got %q", result.GreenContract.ObservedGreenLevel)
	}
	// targeted_tests alone is not merge_ready — that requires the reconciler.
	if result.GreenContract.ObservedGreenLevel == schema.GreenLevelMergeReady {
		t.Error("targeted_tests should not imply merge_ready")
	}
}

func TestGreenContract_WorkspaceLevel(t *testing.T) {
	t.Parallel()
	ctx, st, _, patchID := setupGreenContractFixture(t)

	runner := fakeGateRunner{results: map[string]gateResult{
		"go test ./...":  {exitCode: 0},
		"go vet ./...":   {exitCode: 0},
		"go build ./...": {exitCode: 0},
	}}
	engine := NewWithConfig(st, Config{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: true, Tier: config.TierTargetedTests},
			{Name: "go_vet", Command: "go vet ./...", Blocking: true, Tier: config.TierPackage},
			{Name: "go_build", Command: "go build ./...", Blocking: true, Tier: config.TierWorkspace},
		},
	}, runner)
	result, err := engine.Verify(ctx, patchID, VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.GreenContract == nil {
		t.Fatal("expected non-nil GreenContract")
	}
	if result.GreenContract.ObservedGreenLevel != schema.GreenLevelWorkspace {
		t.Errorf("expected workspace level, got %q", result.GreenContract.ObservedGreenLevel)
	}
}

func TestGreenContract_NoTierAnnotations_NilContract(t *testing.T) {
	t.Parallel()
	ctx, st, _, patchID := setupGreenContractFixture(t)

	runner := fakeGateRunner{results: map[string]gateResult{
		"go test ./...": {exitCode: 0},
	}}
	engine := NewWithConfig(st, Config{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: true},
		},
	}, runner)
	result, err := engine.Verify(ctx, patchID, VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.GreenContract != nil {
		t.Errorf("expected nil GreenContract when no gates have tier annotations, got %+v", result.GreenContract)
	}
}

func TestGreenContract_FailedLowerTierBlocksHigherTier(t *testing.T) {
	t.Parallel()
	ctx, st, _, patchID := setupGreenContractFixture(t)

	runner := fakeGateRunner{results: map[string]gateResult{
		"go test ./...": {exitCode: 1, output: "FAIL"},
		"go vet ./...":  {exitCode: 0},
	}}
	engine := NewWithConfig(st, Config{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: false, Tier: config.TierTargetedTests},
			{Name: "go_vet", Command: "go vet ./...", Blocking: true, Tier: config.TierPackage},
		},
	}, runner)
	result, err := engine.Verify(ctx, patchID, VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.GreenContract != nil {
		t.Fatalf("GreenContract = %+v, want nil because failed targeted gate blocks package level", result.GreenContract)
	}
}

func TestGreenContract_FailedPackageCapsWorkspaceAtTargeted(t *testing.T) {
	t.Parallel()
	ctx, st, _, patchID := setupGreenContractFixture(t)

	runner := fakeGateRunner{results: map[string]gateResult{
		"go test ./...":  {exitCode: 0},
		"go vet ./...":   {exitCode: 1, output: "vet failed"},
		"go build ./...": {exitCode: 0},
	}}
	engine := NewWithConfig(st, Config{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: false, Tier: config.TierTargetedTests},
			{Name: "go_vet", Command: "go vet ./...", Blocking: false, Tier: config.TierPackage},
			{Name: "go_build", Command: "go build ./...", Blocking: false, Tier: config.TierWorkspace},
		},
	}, runner)
	result, err := engine.Verify(ctx, patchID, VerifyInput{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.GreenContract == nil {
		t.Fatal("expected targeted green contract")
	}
	if result.GreenContract.ObservedGreenLevel != schema.GreenLevelTargetedTests {
		t.Fatalf("ObservedGreenLevel = %s, want %s", result.GreenContract.ObservedGreenLevel, schema.GreenLevelTargetedTests)
	}
}
