package projector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

func TestCompileExecutor_handlesNoSnapshotAndBudgetOmissions(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-exec"
		conditionID = "GC-proj-exec"
		obligation  = "OB-proj-exec"
		capsuleID   = "CAP-proj-exec"
		claimID     = "CL-proj-exec"
		failureID   = "FAIL-proj-exec"
		evidenceID  = "EV-proj-exec"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{`internal\projector`},
		RequiredOutputs:    []string{"patch.diff", "evidence.json"},
		Budget:             schema.CapsuleBudget{MaxTokens: 4, MaxWallTimeSeconds: 120, MaxRetries: 2},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: "DEC-unused",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: evidenceID,
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./...",
		ExitCode:   0,
		Summary:    strings.Repeat("long evidence summary ", 8),
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         claimID,
		Text:            strings.Repeat("verified claim text ", 6),
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{`internal\projector`},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	if err := env.st.SaveFailure(env.ctx, &schema.FailureFingerprint{
		FailureID:       failureID,
		SourceCapsuleID: capsuleID,
		FailureType:     schema.FailureTest,
		Summary:         strings.Repeat("prior failure detail ", 6),
		AffectedFiles:   []string{`internal\projector`},
		ErrorSignature:  "sig-projector",
	}); err != nil {
		t.Fatalf("SaveFailure: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	projection, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if projection.FreshnessBase != "" {
		t.Fatalf("FreshnessBase = %q, want empty for no-snapshot case", projection.FreshnessBase)
	}
	if projection.TokenBudget != 2 {
		t.Fatalf("TokenBudget = %d, want 2", projection.TokenBudget)
	}
	if len(projection.OmittedSections) == 0 {
		t.Fatal("OmittedSections is empty, want least-important sections omitted under tight budget")
	}
	if projection.OmittedSections[0] != "prior_evidence" {
		t.Fatalf("first omitted section = %q, want prior_evidence", projection.OmittedSections[0])
	}
	if _, err := env.st.LoadProjection(env.ctx, projection.ContextProjectionID); err != nil {
		t.Fatalf("LoadProjection: %v", err)
	}
}

func TestCompileExecutorLabelsReusedPriorEvidence(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-reuse"
		conditionID = "GC-proj-reuse"
		obligation  = "OB-proj-reuse"
		capsuleID   = "CAP-proj-reuse"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{`internal\projector`},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: "DEC-unused",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID:   "EV-proj-reused",
		Type:         schema.EvidenceTestResult,
		Source:       "verifier",
		Command:      "go test ./internal/projector",
		ExitCode:     0,
		Summary:      "pass",
		Supports:     []string{obligation},
		ReusedFromID: "EV-proj-source",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	projection, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "EV-proj-reused [reused from EV-proj-source]") {
		t.Fatalf("projection missing reused evidence label: %+v", projection.IncludedSections)
	}
}

func TestCompileHumanSummary_buildsImplementationApproachAndTopologyRationale(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-human"
		conditionID = "GC-proj-human"
		obligation  = "OB-proj-human"
		capsuleID   = "CAP-proj-human"
		decisionID  = "DEC-proj-human"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decisionID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "all obligations are low risk and no fingerprints -> single",
		MadeBy:     "system",
		RelatedIDs: []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{`internal\projector`},
		ForbiddenPaths:     []string{`internal\runner`},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300, MaxRetries: 3},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{
		Gates: []config.VerifierGate{
			{Name: "go_test", Command: "go test ./...", Blocking: true},
			{Name: "go_vet", Command: "go vet ./...", Blocking: true},
		},
	})
	summary, err := compiler.CompileHumanSummary(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileHumanSummary: %v", err)
	}
	wantApproach := "Agent will address the following obligations: implement deterministic context compiler"
	if summary.ImplementationApproach != wantApproach {
		t.Fatalf("ImplementationApproach = %q, want %q", summary.ImplementationApproach, wantApproach)
	}
	if summary.Topology.Rationale != "all obligations are low risk and no fingerprints -> single" {
		t.Fatalf("Topology.Rationale = %q", summary.Topology.Rationale)
	}
	if summary.GoalPlain == "" {
		t.Fatal("GoalPlain is empty")
	}
	if len(summary.ConditionsAddressed) == 0 {
		t.Fatal("ConditionsAddressed is empty")
	}
	if len(summary.ObligationsAddressed) == 0 {
		t.Fatal("ObligationsAddressed is empty")
	}
	if len(summary.EvidencePlan.VerifierGates) != 2 {
		t.Fatalf("EvidencePlan.VerifierGates len = %d, want 2", len(summary.EvidencePlan.VerifierGates))
	}
	if _, err := env.st.LoadHumanSummaryProjection(env.ctx, summary.ContextProjectionID); err != nil {
		t.Fatalf("LoadHumanSummaryProjection: %v", err)
	}
}

func TestCompileReviewerAndTesterUseRoleSpecificBriefings(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-role"
		conditionID = "GC-proj-role"
		obligation  = "OB-proj-role"
		implID      = "CAP-proj-impl"
		reviewerID  = "CAP-proj-reviewer"
		testerID    = "CAP-proj-tester"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, capsule := range []schema.ExecutionCapsule{
		{
			CapsuleID:     implID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleExecutor,
			AllowedPaths:  []string{`internal\projector`},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     reviewerID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleReviewer,
			AllowedPaths:  []string{`internal\projector`},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     testerID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleTester,
			AllowedPaths:  []string{`internal\projector`},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300},
			State:         schema.CapsuleStatePending,
		},
	} {
		capsule := capsule
		if err := env.st.SaveCapsule(env.ctx, &capsule); err != nil {
			t.Fatalf("SaveCapsule %s: %v", capsule.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-impl",
		CapsuleID:            implID,
		DiffPath:             `E:\orca\.orca\capsules\CAP-proj-impl\patch.diff`,
		Summary:              "implementer patch under review",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	reviewer, err := compiler.CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if reviewer.Role != schema.ProjectionRoleReviewer {
		t.Fatalf("reviewer Role = %s, want reviewer", reviewer.Role)
	}
	if !projectionIncludes(reviewer, "review the implementer output") {
		t.Fatalf("reviewer projection missing role contract: %+v", reviewer.IncludedSections)
	}
	if !projectionIncludes(reviewer, "PATCH-proj-impl") {
		t.Fatalf("reviewer projection missing candidate patch: %+v", reviewer.IncludedSections)
	}

	tester, err := compiler.CompileTester(env.ctx, testerID)
	if err != nil {
		t.Fatalf("CompileTester: %v", err)
	}
	if tester.Role != schema.ProjectionRoleTester {
		t.Fatalf("tester Role = %s, want tester", tester.Role)
	}
	if !projectionIncludes(tester, "test or challenge the patch") {
		t.Fatalf("tester projection missing role contract: %+v", tester.IncludedSections)
	}
}

func projectionIncludes(p *schema.ContextProjection, want string) bool {
	for _, section := range p.IncludedSections {
		if strings.Contains(section, want) {
			return true
		}
	}
	return false
}

type projectorEnv struct {
	ctx context.Context
	st  *store.FileStore
}

func newProjectorEnv(t *testing.T) projectorEnv {
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
	return projectorEnv{
		ctx: context.Background(),
		st:  st,
	}
}

func seedGoalScenario(t *testing.T, env projectorEnv, goalID, conditionID, obligationID string) {
	t.Helper()
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "summarize projector implementation",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "compile capsule context from persisted artifacts",
			EffectiveDescription: "compile capsule context from persisted artifacts",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     obligationID,
		GoalConditionID:  conditionID,
		Description:      "implement deterministic context compiler",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
}
