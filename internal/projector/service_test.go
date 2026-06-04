package projector

import (
	"context"
	"os"
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
		Budget:             schema.CapsuleBudget{MaxTokens: 10, MaxWallTimeSeconds: 120, MaxRetries: 2},
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
	if projection.TokenBudget != 7 {
		t.Fatalf("TokenBudget = %d, want 7 (int(10 * 0.70))", projection.TokenBudget)
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
	// Phase C: evidence labels use abbreviated IDs (last 6 chars) with cmd= provenance.
	// "EV-proj-reused"[8:] = "reused" (14 chars > 8, abbreviated).
	if !projectionIncludes(projection, "reused(type=test_result exit=0 cmd=go test ./internal/projector reused=EV-proj-source)") {
		t.Fatalf("projection missing reused evidence label: %+v", projection.IncludedSections)
	}
}

func TestCompileExecutorDotScopeMatchesAllClaims(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-dotscope"
		conditionID = "GC-proj-dotscope"
		obligation  = "OB-proj-dotscope"
		capsuleID   = "CAP-proj-dotscope"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"."},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-proj-dotscope",
		Text:            "repo-wide claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/projector/service.go"},
		Status:          schema.ClaimProposed,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	projection, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "CL-proj-dotscope") {
		t.Fatalf("dot-scope capsule projection missing claim from any file: %+v", projection.IncludedSections)
	}
}

func TestCompileExecutorLabelsClaimTrustStates(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-claims"
		conditionID = "GC-proj-claims"
		obligation  = "OB-proj-claims"
		capsuleID   = "CAP-proj-claims"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector/service.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-proj-current",
		GoalID:      goalID,
		EventID:     "EVT-proj-current",
		SequenceNum: 10,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	for _, claim := range []*schema.ClaimArtifact{
		{ClaimID: "CL-proj-verified", Text: "verified fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimVerified, LastValidatedAgainst: "SNAP-proj-current"},
		{ClaimID: "CL-proj-mismatch", Text: "old verified fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimVerified, LastValidatedAgainst: "SNAP-proj-old"},
		{ClaimID: "CL-proj-proposed", Text: "proposed fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimProposed},
		{ClaimID: "CL-proj-stale", Text: "stale fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimStale},
		{ClaimID: "CL-proj-contested", Text: "contested fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimContested},
		{ClaimID: "CL-proj-invalid", Text: "invalidated fact", ClaimType: schema.ClaimInvariant, SourceCapsuleID: capsuleID, AffectedFiles: []string{"internal/projector/service.go"}, Status: schema.ClaimInvalidated},
	} {
		if err := env.st.SaveClaim(env.ctx, claim); err != nil {
			t.Fatalf("SaveClaim %s: %v", claim.ClaimID, err)
		}
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	for _, want := range []string{
		"CL-proj-verified: verified fact",
		"CL-proj-mismatch [stale - freshness unverified]: old verified fact",
		"CL-proj-proposed [proposed]: proposed fact",
		"CL-proj-stale [stale]: stale fact",
		"CL-proj-contested [contested]: contested fact",
	} {
		if !projectionIncludes(projection, want) {
			t.Fatalf("projection missing %q: %+v", want, projection.IncludedSections)
		}
	}
	if projectionIncludes(projection, "invalidated fact") {
		t.Fatalf("projection included invalidated claim: %+v", projection.IncludedSections)
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

func TestCompileHumanSummaryAddsAdvancedChecksToEvidencePlan(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-advanced"
		conditionID = "GC-proj-advanced"
		obligation  = "OB-proj-advanced"
		capsuleID   = "CAP-proj-advanced"
		decisionID  = "DEC-proj-advanced"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decisionID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "low risk",
		MadeBy:     "system",
		RelatedIDs: []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 300},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := NewWithConfig(env.st, config.VerifierConfig{}, config.AdvancedConfig{
		Enabled:          true,
		Maven:            true,
		Mutation:         true,
		AdversarialTests: false,
	})
	summary, err := compiler.CompileHumanSummary(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileHumanSummary: %v", err)
	}
	want := "Advanced verification: MAVEN=on Mutation=on Adversarial=off"
	if len(summary.EvidencePlan.AdvancedChecks) != 1 || summary.EvidencePlan.AdvancedChecks[0] != want {
		t.Fatalf("AdvancedChecks = %#v, want %q", summary.EvidencePlan.AdvancedChecks, want)
	}
}

func TestCompileHumanSummaryAddsContestedClaimRisk(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-contested"
		conditionID = "GC-proj-contested"
		obligation  = "OB-proj-contested"
		capsuleID   = "CAP-proj-contested"
		decisionID  = "DEC-proj-contested"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decisionID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "low risk",
		MadeBy:     "system",
		RelatedIDs: []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{"internal/projector/service.go"},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-proj-risk-contested",
		Text:            "API ownership is disputed",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/projector/service.go"},
		Status:          schema.ClaimContested,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	summary, err := New(env.st, config.VerifierConfig{}).CompileHumanSummary(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileHumanSummary: %v", err)
	}
	var found bool
	for _, risk := range summary.PreExecutionRisks {
		if strings.Contains(risk.Description, "contested claim CL-proj-risk-contested") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PreExecutionRisks = %+v, want contested claim risk", summary.PreExecutionRisks)
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
	diffContent := "--- a/internal/projector/service.go\n+++ b/internal/projector/service.go\n@@ -1 +1,2 @@\n+// reviewer diff visibility test\n"
	diffFile := filepath.Join(t.TempDir(), "patch.diff")
	if err := os.WriteFile(diffFile, []byte(diffContent), 0o600); err != nil {
		t.Fatalf("WriteFile diff: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-impl",
		CapsuleID:            implID,
		DiffPath:             diffFile,
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
	// Diff body text must appear in the reviewer projection (Fix 1).
	if !projectionIncludes(reviewer, "--- a/internal/projector/service.go") {
		t.Fatalf("reviewer projection missing diff body text: %+v", reviewer.IncludedSections)
	}
	if !projectionIncludes(reviewer, "reviewer diff visibility test") {
		t.Fatalf("reviewer projection missing diff body content: %+v", reviewer.IncludedSections)
	}
	// New role-contract phrases must be present (Fix 2).
	if !projectionIncludes(reviewer, "adjacent-code convention consistency") {
		t.Fatalf("reviewer projection missing convention-check phrase: %+v", reviewer.IncludedSections)
	}
	if !projectionIncludes(reviewer, "test-independence") {
		t.Fatalf("reviewer projection missing test-independence phrase: %+v", reviewer.IncludedSections)
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
	if !projectionIncludes(tester, "test-independence") {
		t.Fatalf("tester projection missing test-independence phrase: %+v", tester.IncludedSections)
	}
	if !projectionIncludes(tester, "--- a/internal/projector/service.go") {
		t.Fatalf("tester projection missing diff body text: %+v", tester.IncludedSections)
	}
	if !projectionIncludes(tester, "reviewer diff visibility test") {
		t.Fatalf("tester projection missing diff body content: %+v", tester.IncludedSections)
	}
}

// ── Repo-scoped claims and supersession (item 9) ─────────────────────────────

func TestCompileExecutor_includesRepoScopedClaims(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-repo"
		conditionID = "GC-proj-repo"
		obligation  = "OB-proj-repo"
		capsuleID   = "CAP-proj-repo"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{"internal/schema/common.go"},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: "DEC-unused",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// Repo-scoped claim: no GoalID or SourceCapsuleID, but file overlaps with capsule scope.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:       "CL-repo-scope",
		Text:          "errors.As is the preferred error inspection idiom",
		ClaimType:     schema.ClaimInvariant,
		AffectedFiles: []string{"internal/schema/common.go"},
		Status:        schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim repo-scoped: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "CL-repo-scope") {
		t.Fatalf("projection missing repo-scoped claim: %+v", projection.IncludedSections)
	}
}

func TestCompileExecutor_excludesSupersededClaims(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-sup"
		conditionID = "GC-proj-sup"
		obligation  = "OB-proj-sup"
		capsuleID   = "CAP-proj-sup"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{"internal/schema"},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: "DEC-unused",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// Goal-scoped claim that has been superseded.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-superseded",
		Text:            "old fact that was replaced",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
		SupersededBy:    "PATCH-new",
	}); err != nil {
		t.Fatalf("SaveClaim superseded: %v", err)
	}
	// Goal-scoped claim that is NOT superseded — must appear.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-active",
		Text:            "current active fact",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim active: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if projectionIncludes(projection, "CL-superseded") {
		t.Fatal("projection must not include superseded claim")
	}
	if !projectionIncludes(projection, "CL-active") {
		t.Fatalf("projection missing active claim: %+v", projection.IncludedSections)
	}
}

// ── Associative retrieval — keyword-scored context injection (item 10) ────────

func TestCompileExecutor_keywordGateExcludesNonMatchingClaim(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-kw-gate"
		conditionID = "GC-kw-gate"
		obligation  = "OB-kw-gate"
		capsuleID   = "CAP-kw-gate"
	)
	// seedGoalScenario: OriginalIntent="summarize projector implementation",
	// obligation description="implement deterministic context compiler"
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/schema/common.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// "projector" appears in the goal query; this claim passes the keyword gate.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:           "CL-kw-match",
		Text:              "projector compiles context from artifacts",
		ClaimType:         schema.ClaimInvariant,
		SourceCapsuleID:   capsuleID,
		AffectedFiles:     []string{"internal/schema/common.go"},
		Status:            schema.ClaimVerified,
		InjectionKeywords: []string{"projector"},
	}); err != nil {
		t.Fatalf("SaveClaim match: %v", err)
	}
	// "SSH" does not appear in the query; this claim must be excluded.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:           "CL-kw-nomatch",
		Text:              "SSH host verification should use knownhosts",
		ClaimType:         schema.ClaimInvariant,
		SourceCapsuleID:   capsuleID,
		AffectedFiles:     []string{"internal/schema/common.go"},
		Status:            schema.ClaimVerified,
		InjectionKeywords: []string{"SSH"},
	}); err != nil {
		t.Fatalf("SaveClaim nomatch: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "CL-kw-match") {
		t.Fatalf("projection missing keyword-matching claim: %+v", projection.IncludedSections)
	}
	if projectionIncludes(projection, "CL-kw-nomatch") {
		t.Fatal("projection must not include claim with non-matching keyword")
	}
}

func TestCompileExecutor_injectionConditionExcludesWrongRole(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-ic-role"
		conditionID = "GC-ic-role"
		obligation  = "OB-ic-role"
		capsuleID   = "CAP-ic-role"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/schema/common.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:             "CL-ic-reviewer-only",
		Text:                "reviewer-only claim text",
		ClaimType:           schema.ClaimInvariant,
		SourceCapsuleID:     capsuleID,
		AffectedFiles:       []string{"internal/schema/common.go"},
		Status:              schema.ClaimVerified,
		InjectionConditions: []string{"role=reviewer"},
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if projectionIncludes(projection, "CL-ic-reviewer-only") {
		t.Fatal("executor projection must not include claim gated to role=reviewer")
	}
}

func TestCompileExecutor_outOfScopeFileClaimIncluded(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-oos-file"
		conditionID = "GC-oos-file"
		obligation  = "OB-oos-file"
		capsuleID   = "CAP-oos-file"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/schema/common.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// AffectedFiles is outside AllowedPaths; item 10 removes the file-path gate.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-oos-verifier",
		Text:            "verifier engine reads RiskLevel for gate decisions",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/verifier/verifier.go"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "CL-oos-verifier") {
		t.Fatalf("projection must include out-of-scope file claim (item 10 removes file-path gate): %+v", projection.IncludedSections)
	}
}

func TestCompileExecutor_highScoreClaimRankedFirst(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-score-rank"
		conditionID = "GC-score-rank"
		obligation  = "OB-score-rank"
		capsuleID   = "CAP-score-rank"
	)
	// seedGoalScenario: OriginalIntent="summarize projector implementation",
	// obligation description="implement deterministic context compiler"
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/schema/common.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// High-relevance: shares tokens with "projector", "context", "compiler", "deterministic".
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-z-score-high",
		Text:            "projector context compiler implements deterministic summarization",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim high: %v", err)
	}
	// Low-relevance: no token overlap with the query.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-a-score-low",
		Text:            "SSH host verification requires knownhosts database",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim low: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if !projectionIncludes(projection, "CL-z-score-high") || !projectionIncludes(projection, "CL-a-score-low") {
		t.Fatalf("projection missing expected claims: %+v", projection.IncludedSections)
	}
	// Both claims land in the same "claims: ..." section. High-score must precede low-score.
	claimsSection := ""
	for _, s := range projection.IncludedSections {
		if strings.Contains(s, "CL-z-score-high") && strings.Contains(s, "CL-a-score-low") {
			claimsSection = s
			break
		}
	}
	if claimsSection == "" {
		t.Fatalf("expected both claims in one section: %+v", projection.IncludedSections)
	}
	highIdx := strings.Index(claimsSection, "CL-z-score-high")
	lowIdx := strings.Index(claimsSection, "CL-a-score-low")
	if highIdx >= lowIdx {
		t.Fatalf("high-score claim (pos %d) must appear before low-score claim (pos %d)", highIdx, lowIdx)
	}
}

func TestCompileExecutor_equalScoreHigherConfidenceFirst(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-score-confidence"
		conditionID = "GC-score-confidence"
		obligation  = "OB-score-confidence"
		capsuleID   = "CAP-score-confidence"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/schema/common.go"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-z-high-confidence",
		Text:            "projector context compiler",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
		Confidence:      0.9,
	}); err != nil {
		t.Fatalf("SaveClaim high-confidence: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-a-low-confidence",
		Text:            "projector context compiler",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/schema/common.go"},
		Status:          schema.ClaimVerified,
		Confidence:      0.1,
	}); err != nil {
		t.Fatalf("SaveClaim low-confidence: %v", err)
	}

	projection, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	claimsSection := ""
	for _, s := range projection.IncludedSections {
		if strings.Contains(s, "CL-z-high-confidence") && strings.Contains(s, "CL-a-low-confidence") {
			claimsSection = s
			break
		}
	}
	if claimsSection == "" {
		t.Fatalf("expected both confidence claims in one section: %+v", projection.IncludedSections)
	}
	highIdx := strings.Index(claimsSection, "CL-z-high-confidence")
	lowIdx := strings.Index(claimsSection, "CL-a-low-confidence")
	if highIdx >= lowIdx {
		t.Fatalf("higher-confidence claim (pos %d) must appear before lower-confidence claim (pos %d)", highIdx, lowIdx)
	}
}

// TestBudgetMath_seventyPercentFraction verifies that TokenBudget is 70% of
// MaxTokens, not 50%.
func TestBudgetMath_seventyPercentFraction(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-budgetfrac"
		conditionID = "GC-budgetfrac"
		obligation  = "OB-budgetfrac"
		capsuleID   = "CAP-budgetfrac"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		Budget:        schema.CapsuleBudget{MaxTokens: 100},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	projection, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if projection.TokenBudget != 70 {
		t.Fatalf("TokenBudget = %d, want 70 (70%% of 100)", projection.TokenBudget)
	}
}

// TestEnforceProjectionBudget_bytesPerTokenIsThree verifies that the byte limit
// is tokenBudget*3. Content of 34 bytes exceeds limit=30 (tokenBudget=10 * 3)
// but would fit within the old limit=40 (tokenBudget * 4), so an omission must
// occur.
func TestEnforceProjectionBudget_bytesPerTokenIsThree(t *testing.T) {
	t.Parallel()

	const tokenBudget = 10
	// projectionBytes = len(text)+1 (separator). text=34 → bytes=35 > limit=30.
	// With the old multiplier of 4: limit=40 → no omission.
	sections := []projectionSection{{key: "required", text: strings.Repeat("x", 34)}}
	_, omitted := enforceProjectionBudget(sections, tokenBudget)
	if len(omitted) == 0 {
		t.Fatal("expected content_truncated when bytes exceed tokenBudget*3; got no omissions")
	}
	if omitted[0].key != "content_truncated" {
		t.Fatalf("omission key = %q, want content_truncated", omitted[0].key)
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

// ── Phase C §7: projection compaction, hashing, and reuse tests ──────────────

// TestProjectionHash_sameGraphProducesSameHash verifies that
// computeSourceHash is deterministic and order-independent, and that
// computeContentHash is stable for identical content.
func TestProjectionHash_sameGraphProducesSameHash(t *testing.T) {
	t.Parallel()

	ids1 := []string{"CAP-h1", "OB-h1", "GC-h1", "EV-h1"}
	ids2 := []string{"OB-h1", "EV-h1", "CAP-h1", "GC-h1"} // same IDs, different order
	freshness := "SNAP-h1"

	sourceSections := []string{"obligations: OB-1: fix bug", "scope: allowed=internal/foo"}
	h1 := computeSourceHash(ids1, freshness, sourceSections)
	h2 := computeSourceHash(ids2, freshness, sourceSections)
	if h1 != h2 {
		t.Fatalf("computeSourceHash: same IDs in different order should produce same hash; h1=%s h2=%s", h1, h2)
	}
	if h1 == "" {
		t.Fatal("computeSourceHash: returned empty hash")
	}

	sections := []string{"obligations: OB-1: fix bug", "scope: allowed=internal/foo"}
	c1 := computeContentHash(sections)
	c2 := computeContentHash(sections)
	if c1 != c2 {
		t.Fatalf("computeContentHash: identical content should produce identical hash; c1=%s c2=%s", c1, c2)
	}
	if c1 == "" {
		t.Fatal("computeContentHash: returned empty hash")
	}
}

// TestProjectionHash_freshSnapshotInvalidatesReuse verifies that changing the
// FreshnessBase (new snapshot) produces a different SourceHash, preventing stale reuse.
func TestProjectionHash_freshSnapshotInvalidatesReuse(t *testing.T) {
	t.Parallel()

	ids := []string{"CAP-snap", "OB-snap"}

	sourceSections := []string{"obligations: OB-1: fix bug"}
	h1 := computeSourceHash(ids, "SNAP-old", sourceSections)
	h2 := computeSourceHash(ids, "SNAP-new", sourceSections)
	if h1 == h2 {
		t.Fatalf("different snapshots must produce different source hashes; both = %s", h1)
	}

	// Empty freshness base produces a distinct hash from a named snapshot.
	h3 := computeSourceHash(ids, "", sourceSections)
	if h3 == h1 || h3 == h2 {
		t.Fatalf("empty freshness base should produce a hash distinct from named snapshots")
	}
}

// TestProjectionHash_evidenceWithoutCommandNotPreservedAsProof verifies that
// evidence artifacts lacking a Command field are labeled [no-provenance] in
// the projection, and that evidence with a command includes the cmd= field.
func TestProjectionHash_evidenceWithoutCommandNotPreservedAsProof(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-prov"
		conditionID = "GC-prov"
		obligation  = "OB-prov"
		capsuleID   = "CAP-prov"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	// Evidence with a command — has provenance.
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-prov-cmd",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./internal/projector",
		ExitCode:   0,
		Summary:    "tests pass",
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence with-command: %v", err)
	}
	// Evidence without a command — no provenance.
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-prov-noprov",
		Type:       schema.EvidenceTestResult,
		Source:     "external",
		Command:    "", // no command
		ExitCode:   0,
		Summary:    "externally verified",
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence no-command: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	proj, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}

	// IDs are abbreviated to last 6 chars (T7): "EV-prov-cmd"[5:] = "ov-cmd".
	if !projectionIncludes(proj, "ov-cmd") {
		t.Fatalf("projection missing evidence with command (abbreviated ID 'ov-cmd'): %+v", proj.IncludedSections)
	}
	if !projectionIncludes(proj, "cmd=go test ./internal/projector") {
		t.Fatalf("projection missing cmd= provenance label: %+v", proj.IncludedSections)
	}
	// "EV-prov-noprov"[8:] = "noprov".
	if !projectionIncludes(proj, "noprov") {
		t.Fatalf("projection missing no-provenance evidence ID (abbreviated 'noprov'): %+v", proj.IncludedSections)
	}
	if !projectionIncludes(proj, "[no-provenance]") {
		t.Fatalf("projection must label evidence with empty command as [no-provenance]: %+v", proj.IncludedSections)
	}
}

// TestProjectionBudget_omissionsAreDeterministic verifies that calling
// enforceProjectionBudget with the same sections twice produces identical output.
func TestProjectionBudget_omissionsAreDeterministic(t *testing.T) {
	t.Parallel()

	sections := []projectionSection{
		{key: "required", text: "this section is always kept"},
		{key: "optional_a", text: strings.Repeat("long removable content a ", 20), removable: true},
		{key: "optional_b", text: strings.Repeat("long removable content b ", 20), removable: true},
	}

	// Very tight budget to force omissions.
	budget := 50

	included1, omitted1 := enforceProjectionBudget(sections, budget)
	included2, omitted2 := enforceProjectionBudget(sections, budget)

	if strings.Join(included1, "|") != strings.Join(included2, "|") {
		t.Fatalf("enforceProjectionBudget: included sections differ across identical calls:\n  run1: %v\n  run2: %v", included1, included2)
	}
	omittedKeys1 := make([]string, len(omitted1))
	omittedKeys2 := make([]string, len(omitted2))
	for i, o := range omitted1 {
		omittedKeys1[i] = o.key + ":" + o.reason
	}
	for i, o := range omitted2 {
		omittedKeys2[i] = o.key + ":" + o.reason
	}
	if strings.Join(omittedKeys1, "|") != strings.Join(omittedKeys2, "|") {
		t.Fatalf("enforceProjectionBudget: omitted sections differ across identical calls:\n  run1: %v\n  run2: %v", omittedKeys1, omittedKeys2)
	}
	if len(omitted1) == 0 {
		t.Fatal("expected at least one omission under tight budget, got none")
	}
	for _, o := range omitted1 {
		if o.reason == "" {
			t.Errorf("omitted section %q has empty reason", o.key)
		}
	}
}

// TestProjectionBudget_humanSummaryAndExecutorAreDistinct verifies that
// CompileHumanSummary and CompileExecutor produce separate artifacts that
// are never merged. orca.md §5.4.
func TestProjectionBudget_humanSummaryAndExecutorAreDistinct(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-two-docs"
		conditionID = "GC-two-docs"
		obligation  = "OB-two-docs"
		capsuleID   = "CAP-two-docs"
		decisionID  = "DEC-two-docs"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decisionID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "low risk single obligation",
		MadeBy:     "system",
		RelatedIDs: []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      []string{obligation},
		AllowedPaths:       []string{"internal/projector"},
		Budget:             schema.CapsuleBudget{MaxTokens: 32000, MaxWallTimeSeconds: 60},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	executorProj, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	humanProj, err := compiler.CompileHumanSummary(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileHumanSummary: %v", err)
	}

	// Must be separate documents with distinct IDs.
	if executorProj.ContextProjectionID == humanProj.ContextProjectionID {
		t.Fatalf("executor and human summary share the same projection ID %s — they must be separate artifacts", executorProj.ContextProjectionID)
	}
	// The human summary must have a GoalPlain field; the executor projection has none.
	if humanProj.GoalPlain == "" {
		t.Fatal("HumanSummaryProjection.GoalPlain is empty")
	}
	// Roles must differ.
	if executorProj.Role == humanProj.Role {
		t.Fatalf("executor and human summary have the same Role %s — they must be distinct", executorProj.Role)
	}
	if executorProj.Role != schema.ProjectionRoleExecutor {
		t.Fatalf("executor projection Role = %s, want executor", executorProj.Role)
	}
	if humanProj.Role != schema.ProjectionRoleHumanSummary {
		t.Fatalf("human summary projection Role = %s, want human_summary", humanProj.Role)
	}

	// Phase C: executor projection must carry SourceHash and ContentHash.
	if executorProj.SourceHash == "" {
		t.Fatal("executor projection SourceHash is empty — must be set after Phase C")
	}
	if executorProj.ContentHash == "" {
		t.Fatal("executor projection ContentHash is empty — must be set after Phase C")
	}
	if humanProj.SourceHash == "" {
		t.Fatal("human summary projection SourceHash is empty — must be set after Phase C")
	}
	if humanProj.ContentHash == "" {
		t.Fatal("human summary projection ContentHash is empty — must be set after Phase C")
	}
}

// TestProjectionReuse_sameSourceHashReturnsCachedProjection verifies that
// compiling a projection for the same capsule twice (with no snapshot change)
// returns the original projection on the second call and records a reuse entry.
func TestProjectionReuse_sameSourceHashReturnsCachedProjection(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-reuse-detect"
		conditionID = "GC-reuse-detect"
		obligation  = "OB-reuse-detect"
		capsuleID   = "CAP-reuse-detect"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})

	// First compilation: creates a new projection.
	p1, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("first CompileExecutor: %v", err)
	}
	if p1.SourceHash == "" {
		t.Fatal("first projection SourceHash is empty")
	}

	// Second compilation with the same capsule and no snapshot change:
	// the source hash is identical, so the projector should return p1.
	p2, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("second CompileExecutor: %v", err)
	}
	if p2.ContextProjectionID != p1.ContextProjectionID {
		t.Fatalf("second compilation returned different projection %s, want reuse of %s",
			p2.ContextProjectionID, p1.ContextProjectionID)
	}

	// A reuse record must have been saved.
	reuseRecords, err := env.st.LoadProjectionReuseRecordsForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadProjectionReuseRecordsForGoal: %v", err)
	}
	if len(reuseRecords) == 0 {
		t.Fatal("no projection reuse records saved — expected one after second CompileExecutor call")
	}
	r := reuseRecords[0]
	if r.OriginalProjectionID != p1.ContextProjectionID {
		t.Fatalf("reuse record OriginalProjectionID = %s, want %s", r.OriginalProjectionID, p1.ContextProjectionID)
	}
	if r.SourceHash != p1.SourceHash {
		t.Fatalf("reuse record SourceHash = %s, want %s", r.SourceHash, p1.SourceHash)
	}
}

// ── Role contract phrase assertions (Fix 2) ──────────────────────────────────

func TestRoleContract_reviewerAndTesterPhrases(t *testing.T) {
	t.Parallel()

	reviewer := roleContract(schema.ProjectionRoleReviewer)
	for _, phrase := range []string{
		"adjacent-code convention consistency",
		"divergence from nearby code patterns",
		"test-independence",
		"verifier-gate expectations",
	} {
		if !strings.Contains(reviewer, phrase) {
			t.Errorf("reviewer role contract missing %q: %q", phrase, reviewer)
		}
	}

	tester := roleContract(schema.ProjectionRoleTester)
	if !strings.Contains(tester, "test-independence") {
		t.Errorf("tester role contract missing test-independence: %q", tester)
	}
}

// ── Diff body in reviewer/tester projections (Fix 1) ─────────────────────────

// TestCompileReviewer_nonexistentDiffRendersUnavailable verifies that a patch
// pointing to a nonexistent diff path renders the unavailable placeholder rather
// than failing projection compilation.
func TestCompileReviewer_nonexistentDiffRendersUnavailable(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-nofile"
		conditionID = "GC-proj-nofile"
		obligation  = "OB-proj-nofile"
		capsuleID   = "CAP-proj-nofile"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
		Role:          schema.RoleReviewer,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-nofile",
		CapsuleID:            capsuleID,
		DiffPath:             filepath.Join(t.TempDir(), "does-not-exist", "patch.diff"),
		Summary:              "patch with missing diff",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	reviewer, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(reviewer, "diff unavailable") {
		t.Fatalf("reviewer projection missing unavailable placeholder for nonexistent diff: %+v", reviewer.IncludedSections)
	}
}

// TestCompileReviewer_emptyDiffPathRendersNoDiff verifies that a patch with an
// empty DiffPath renders the "(no diff)" placeholder in the reviewer projection.
func TestCompileReviewer_emptyDiffPathRendersNoDiff(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-nodiffpath"
		conditionID = "GC-proj-nodiffpath"
		obligation  = "OB-proj-nodiffpath"
		capsuleID   = "CAP-proj-nodiffpath"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
		Role:          schema.RoleReviewer,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-nodiffpath",
		CapsuleID:            capsuleID,
		DiffPath:             "",
		Summary:              "patch with no diff path",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	reviewer, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(reviewer, "(no diff)") {
		t.Fatalf("reviewer projection missing (no diff) for empty DiffPath: %+v", reviewer.IncludedSections)
	}
}

func TestCompileReviewer_largeDiffRendersTruncationMarker(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-truncated"
		conditionID = "GC-proj-truncated"
		obligation  = "OB-proj-truncated"
		capsuleID   = "CAP-proj-truncated"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
		Role:          schema.RoleReviewer,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	largeDiff := strings.Repeat("x", maxDiffBytes+64)
	diffFile := filepath.Join(t.TempDir(), "large.patch.diff")
	if err := os.WriteFile(diffFile, []byte(largeDiff), 0o600); err != nil {
		t.Fatalf("WriteFile large diff: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-truncated",
		CapsuleID:            capsuleID,
		DiffPath:             diffFile,
		Summary:              "patch with oversized diff",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	reviewer, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(reviewer, "[truncated]") {
		t.Fatalf("reviewer projection missing truncation marker: %+v", reviewer.IncludedSections)
	}
}

// TestCompileReviewer_diffContentChangeInvalidatesReuse verifies that changing
// the diff file content causes a new projection with a different SourceHash to
// be compiled, preventing stale reuse when only the diff body changes.
func TestCompileReviewer_diffContentChangeInvalidatesReuse(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-proj-diffreuse"
		conditionID = "GC-proj-diffreuse"
		obligation  = "OB-proj-diffreuse"
		implID      = "CAP-proj-diffreuse-impl"
		reviewerID  = "CAP-proj-diffreuse-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)

	diffFile := filepath.Join(t.TempDir(), "patch.diff")
	if err := os.WriteFile(diffFile, []byte("--- a/file.go\n+++ b/file.go\n@@ -1 +1,2 @@\n+// v1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	for _, capsule := range []schema.ExecutionCapsule{
		{
			CapsuleID:     implID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleExecutor,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     reviewerID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleReviewer,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
	} {
		capsule := capsule
		if err := env.st.SaveCapsule(env.ctx, &capsule); err != nil {
			t.Fatalf("SaveCapsule %s: %v", capsule.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-proj-diffreuse",
		CapsuleID:            implID,
		DiffPath:             diffFile,
		Summary:              "patch for diff reuse test",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	p1, err := compiler.CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("first CompileReviewer: %v", err)
	}
	if !projectionIncludes(p1, "// v1") {
		t.Fatalf("first projection missing initial diff content: %+v", p1.IncludedSections)
	}

	// Change diff file content — same path, new body.
	if err := os.WriteFile(diffFile, []byte("--- a/file.go\n+++ b/file.go\n@@ -1 +1,2 @@\n+// v2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}

	p2, err := compiler.CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("second CompileReviewer: %v", err)
	}
	if p2.ContextProjectionID == p1.ContextProjectionID {
		t.Fatalf("changed diff content reused stale projection %s", p1.ContextProjectionID)
	}
	if p2.SourceHash == p1.SourceHash {
		t.Fatalf("SourceHash did not change after diff file content changed")
	}
	if !projectionIncludes(p2, "// v2") {
		t.Fatalf("new projection missing updated diff content: %+v", p2.IncludedSections)
	}
}

func TestProjectionReuse_changedRenderedSourceDoesNotReuseStaleProjection(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-reuse-source-change"
		conditionID = "GC-reuse-source-change"
		obligation  = "OB-reuse-source-change"
		capsuleID   = "CAP-reuse-source-change"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	p1, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("first CompileExecutor: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-reuse-source-change",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./internal/projector",
		ExitCode:   0,
		Summary:    "tests pass",
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	p2, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("second CompileExecutor: %v", err)
	}
	if p2.ContextProjectionID == p1.ContextProjectionID {
		t.Fatalf("changed source context reused stale projection %s", p1.ContextProjectionID)
	}
	if p2.SourceHash == p1.SourceHash {
		t.Fatalf("SourceHash did not change after rendered source context changed")
	}
	// "EV-reuse-source-change"[16:] = "change" (22 chars > 8, abbreviated).
	if !projectionIncludes(p2, "change(type=test_result") {
		t.Fatalf("new projection missing newly added evidence (abbreviated 'change'): %+v", p2.IncludedSections)
	}
}

// ── T5/T6/T7: content compaction helpers ─────────────────────────────────────

func TestSummarizeClaims_capsLongTextAt200(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 210)
	claims := []*schema.ClaimArtifact{{
		ClaimID: "CL-T5",
		Text:    long,
		Status:  schema.ClaimVerified,
	}}
	got := summarizeClaims(claims, "")
	if strings.Contains(got, long) {
		t.Error("full uncapped text must not appear in output")
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated text ending with '...': %q", got)
	}
	// text portion: 197 visible chars + "..." = 200 chars
	if len(got) >= len("CL-T5: ")+211 {
		t.Errorf("claim text not capped: result len=%d", len(got))
	}
}

func TestSummarizeClaims_shortTextNotCapped(t *testing.T) {
	t.Parallel()
	claims := []*schema.ClaimArtifact{{
		ClaimID: "CL-T5-short",
		Text:    "exact fit",
		Status:  schema.ClaimVerified,
	}}
	got := summarizeClaims(claims, "")
	if !strings.Contains(got, "exact fit") {
		t.Errorf("short text must be preserved verbatim: %q", got)
	}
}

func TestSummarizeFailures_capsLongSummaryAt150(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("b", 160)
	failures := []*schema.FailureFingerprint{{
		FailureID: "FAIL-T6",
		Summary:   long,
	}}
	got := summarizeFailures(failures)
	if strings.Contains(got, long) {
		t.Error("full uncapped summary must not appear in output")
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated summary ending with '...': %q", got)
	}
}

func TestSummarizeFailures_shortSummaryNotCapped(t *testing.T) {
	t.Parallel()
	failures := []*schema.FailureFingerprint{{
		FailureID: "FAIL-T6-short",
		Summary:   "quick fail",
	}}
	got := summarizeFailures(failures)
	if !strings.Contains(got, "quick fail") {
		t.Errorf("short summary must be preserved verbatim: %q", got)
	}
}

func TestSummarizeEvidence_abbreviatesLongID(t *testing.T) {
	t.Parallel()
	// "EV-01HZ4R9QCXM8F7B3VD6N5T2PW" is 28 chars; last 6 = "N5T2PW".
	longID := "EV-01HZ4R9QCXM8F7B3VD6N5T2PW"
	items := map[string][]*schema.EvidenceArtifact{
		"OB-T7": {{
			EvidenceID: longID,
			Type:       schema.EvidenceTestResult,
			ExitCode:   0,
			Command:    "go test ./...",
			Supports:   []string{"OB-T7"},
		}},
	}
	got := summarizeEvidence(items)
	if strings.Contains(got, longID) {
		t.Errorf("full evidence ID must be abbreviated, got: %q", got)
	}
	if !strings.Contains(got, "N5T2PW") {
		t.Errorf("abbreviated ID (last 6 'N5T2PW') not in output: %q", got)
	}
}

func TestSummarizeEvidence_shortIDNotAbbreviated(t *testing.T) {
	t.Parallel()
	// IDs with ≤8 chars are kept verbatim.
	shortID := "EV-12345"
	items := map[string][]*schema.EvidenceArtifact{
		"OB-T7s": {{
			EvidenceID: shortID,
			Type:       schema.EvidenceTestResult,
			ExitCode:   0,
			Command:    "go build ./...",
			Supports:   []string{"OB-T7s"},
		}},
	}
	got := summarizeEvidence(items)
	if !strings.Contains(got, shortID) {
		t.Errorf("short ID %q must appear verbatim in output: %q", shortID, got)
	}
}

func TestSummarizeEvidence_narrowsCommandAt40(t *testing.T) {
	t.Parallel()
	longCmd := strings.Repeat("c", 50)
	items := map[string][]*schema.EvidenceArtifact{
		"OB-T7c": {{
			EvidenceID: "EV-T7C",
			Type:       schema.EvidenceTestResult,
			ExitCode:   0,
			Command:    longCmd,
			Supports:   []string{"OB-T7c"},
		}},
	}
	got := summarizeEvidence(items)
	// Command truncated to 37 + "..." = 40 chars — 41+ consecutive 'c's must not appear.
	if strings.Contains(got, strings.Repeat("c", 41)) {
		t.Errorf("command not truncated at 40 chars: %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("truncated command must end with '...': %q", got)
	}
}

func TestSummarizeEvidence_collisionFallsBackToFullID(t *testing.T) {
	t.Parallel()
	id1 := "EV-AAAAAAAAAAABCDEF"
	id2 := "EV-ZZZZZZZZZZABCDEF"
	items := map[string][]*schema.EvidenceArtifact{
		"OB-T7-collision": {
			{
				EvidenceID: id1,
				Type:       schema.EvidenceTestResult,
				ExitCode:   0,
				Command:    "go test ./...",
				Supports:   []string{"OB-T7-collision"},
			},
			{
				EvidenceID: id2,
				Type:       schema.EvidenceLintResult,
				ExitCode:   0,
				Command:    "go vet ./...",
				Supports:   []string{"OB-T7-collision"},
			},
		},
	}
	got := summarizeEvidence(items)
	if !strings.Contains(got, id1+"(type=") {
		t.Errorf("colliding ID %q must fall back to full form: %q", id1, got)
	}
	if !strings.Contains(got, id2+"(type=") {
		t.Errorf("colliding ID %q must fall back to full form: %q", id2, got)
	}
}

// ── T10: Evidence sub-section reuse ─────────────────────────────────────────

// TestT10_evidenceHashSetOnProjection verifies EvidenceHash is non-empty on a
// compiled projection and is stable across identical evidence + freshness.
func TestT10_evidenceHashSetOnProjection(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t10-hash"
		conditionID = "GC-t10-hash"
		obligation  = "OB-t10-hash"
		capsuleID   = "CAP-t10-hash"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t10-hash",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	p, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	if p.EvidenceHash == "" {
		t.Fatal("EvidenceHash must be non-empty on compiled projection")
	}
}

// TestT10_evidenceHashStableWhenEvidenceUnchanged verifies that two compilations
// with identical evidence but different claims produce the same EvidenceHash.
func TestT10_evidenceHashStableWhenEvidenceUnchanged(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t10-stable"
		conditionID = "GC-t10-stable"
		obligation  = "OB-t10-stable"
		capsuleID   = "CAP-t10-stable"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t10-stable",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	compiler := New(env.st, config.VerifierConfig{})
	p1, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("first CompileExecutor: %v", err)
	}

	// Add a new claim (changes sourceHash but not evidenceHash).
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-t10-stable-new",
		Text:            "new claim after first projection",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: capsuleID,
		AffectedFiles:   []string{"internal/projector"},
		Status:          schema.ClaimProposed,
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	p2, err := compiler.CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("second CompileExecutor: %v", err)
	}
	if p2.ContextProjectionID == p1.ContextProjectionID {
		t.Fatal("new claim must cause a new projection (different ID)")
	}
	if p1.EvidenceHash == "" || p2.EvidenceHash == "" {
		t.Fatalf("EvidenceHash must be set on both projections: p1=%q p2=%q", p1.EvidenceHash, p2.EvidenceHash)
	}
	if p1.EvidenceHash != p2.EvidenceHash {
		t.Fatalf("EvidenceHash must be stable when evidence is unchanged: p1=%q p2=%q", p1.EvidenceHash, p2.EvidenceHash)
	}
}

// ── T11: Role-based evidence freshness filtering ─────────────────────────────

// TestT11_executorCaps3Evidence verifies that an executor projection retains
// at most 3 evidence items per obligation, keeping the 3 most recent.
func TestT11_executorCaps3Evidence(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-exec"
		conditionID = "GC-t11-exec"
		obligation  = "OB-t11-exec"
		capsuleID   = "CAP-t11-exec"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obligation},
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	base := time.Now().UTC()
	for i, id := range []string{"EV-t11-e1", "EV-t11-e2", "EV-t11-e3", "EV-t11-e4-oldest"} {
		if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
			EvidenceID: id,
			Type:       schema.EvidenceTestResult,
			Source:     "verifier",
			Command:    "go test ./...",
			ExitCode:   0,
			Summary:    id,
			Supports:   []string{obligation},
			// e1 is newest, e4-oldest is oldest
			CreatedAt: base.Add(-time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("SaveEvidence %s: %v", id, err)
		}
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileExecutor(env.ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	// 3 most recent: e1, e2, e3. Oldest (e4-oldest) must be absent.
	for _, want := range []string{"t11-e1", "t11-e2", "t11-e3"} {
		if !projectionIncludes(proj, want) {
			t.Errorf("executor projection missing recent evidence %q: %+v", want, proj.IncludedSections)
		}
	}
	if projectionIncludes(proj, "t11-e4-oldest") {
		t.Fatal("executor projection must not include 4th evidence item (oldest)")
	}
}

// TestT11_reviewerDropsStaleEvidence verifies that reviewer projections exclude
// evidence validated against a different (older) snapshot than the current one.
func TestT11_reviewerDropsStaleEvidence(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-rev-stale"
		conditionID = "GC-t11-rev-stale"
		obligation  = "OB-t11-rev-stale"
		implID      = "CAP-t11-rev-impl"
		reviewerID  = "CAP-t11-rev-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-t11-current",
		GoalID:      goalID,
		EventID:     "EVT-t11-current",
		SequenceNum: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	for _, c := range []schema.ExecutionCapsule{
		{
			CapsuleID:     implID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleExecutor,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     reviewerID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleReviewer,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t11-rev",
		CapsuleID:            implID,
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	// Fresh evidence: validated against the current snapshot.
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID:       "EV-t11-rev-fresh",
		Type:             schema.EvidenceTestResult,
		Source:           "verifier",
		Command:          "go test ./fresh",
		ExitCode:         0,
		Supports:         []string{obligation},
		ValidatedAgainst: "SNAP-t11-current",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence fresh: %v", err)
	}
	// Stale evidence: validated against an old snapshot.
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID:       "EV-t11-rev-stale",
		Type:             schema.EvidenceTestResult,
		Source:           "verifier",
		Command:          "go test ./stale",
		ExitCode:         0,
		Supports:         []string{obligation},
		ValidatedAgainst: "SNAP-t11-old",
		CreatedAt:        time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SaveEvidence stale: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "cmd=go test ./fresh") {
		t.Fatalf("reviewer projection missing fresh evidence: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "cmd=go test ./stale") {
		t.Fatal("reviewer projection must not include stale evidence (validated against old snapshot)")
	}
}

func TestT11_reviewerDropsUnvalidatedEvidenceWhenFreshnessSet(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-rev-unvalidated"
		conditionID = "GC-t11-rev-unvalidated"
		obligation  = "OB-t11-rev-unvalidated"
		implID      = "CAP-t11-rev-unvalidated-impl"
		reviewerID  = "CAP-t11-rev-unvalidated-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-t11-unvalidated-current",
		GoalID:      goalID,
		EventID:     "EVT-t11-unvalidated-current",
		SequenceNum: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	for _, c := range []schema.ExecutionCapsule{
		{
			CapsuleID:     implID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleExecutor,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     reviewerID,
			ObligationIDs: []string{obligation},
			Role:          schema.RoleReviewer,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t11-rev-unvalidated",
		CapsuleID:            implID,
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID:       "EV-t11-rev-unvalidated-fresh",
		Type:             schema.EvidenceTestResult,
		Source:           "verifier",
		Command:          "go test ./fresh",
		ExitCode:         0,
		Supports:         []string{obligation},
		ValidatedAgainst: "SNAP-t11-unvalidated-current",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence fresh: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-rev-unvalidated-none",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./unvalidated",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence unvalidated: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "cmd=go test ./fresh") {
		t.Fatalf("reviewer projection missing fresh validated evidence: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "cmd=go test ./unvalidated") {
		t.Fatal("reviewer projection must not include unvalidated evidence when freshness_base is set")
	}
}

func TestT11_reviewerNoCandidatePatchDropsEvidence(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-rev-nopatch"
		conditionID = "GC-t11-rev-nopatch"
		obligation  = "OB-t11-rev-nopatch"
		reviewerID  = "CAP-t11-rev-nopatch-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-t11-nopatch-current",
		GoalID:      goalID,
		EventID:     "EVT-t11-nopatch-current",
		SequenceNum: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     reviewerID,
		ObligationIDs: []string{obligation},
		Role:          schema.RoleReviewer,
		AllowedPaths:  []string{"internal/projector"},
		Budget:        schema.CapsuleBudget{MaxTokens: 32000},
		State:         schema.CapsuleStatePending,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID:       "EV-t11-rev-nopatch",
		Type:             schema.EvidenceTestResult,
		Source:           "verifier",
		Command:          "go test ./withoutpatch",
		ExitCode:         0,
		Supports:         []string{obligation},
		ValidatedAgainst: "SNAP-t11-nopatch-current",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if projectionIncludes(proj, "cmd=go test ./withoutpatch") {
		t.Fatal("reviewer projection must not include evidence when there is no candidate patch coverage")
	}
}

// TestT11_reviewerDropsEvidenceForUncoveredObligation verifies that reviewer
// projections omit evidence for obligations that have no candidate patch.
func TestT11_reviewerDropsEvidenceForUncoveredObligation(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-rev-cov"
		conditionID = "GC-t11-rev-cov"
		obligation1 = "OB-t11-rv-cov1"
		obligation2 = "OB-t11-rv-cov2"
		implID      = "CAP-t11-rv-impl"
		reviewerID  = "CAP-t11-rv-reviewer"
	)
	// Two obligations under one goal.
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test reviewer evidence filtering",
		GoalConditions: []schema.GoalCondition{{
			ID: conditionID, Description: "filter evidence",
			EffectiveDescription: "filter evidence", Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	for _, id := range []string{obligation1, obligation2} {
		if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
			ObligationID:    id,
			GoalConditionID: conditionID,
			Description:     "obligation " + id,
			RiskLevel:       schema.RiskLow,
			Status:          schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", id, err)
		}
	}
	for _, c := range []schema.ExecutionCapsule{
		{
			CapsuleID:     implID,
			ObligationIDs: []string{obligation1, obligation2},
			Role:          schema.RoleExecutor,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
		{
			CapsuleID:     reviewerID,
			ObligationIDs: []string{obligation1, obligation2},
			Role:          schema.RoleReviewer,
			AllowedPaths:  []string{"internal/projector"},
			Budget:        schema.CapsuleBudget{MaxTokens: 32000},
			State:         schema.CapsuleStatePending,
		},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	// Candidate patch only covers obligation1; obligation2 has no patch.
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t11-rv-cov",
		CapsuleID:            implID,
		ObligationIDsClaimed: []string{obligation1},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-rv-ob1",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./covered",
		ExitCode:   0,
		Supports:   []string{obligation1},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence ob1: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-rv-ob2",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./uncovered",
		ExitCode:   0,
		Supports:   []string{obligation2},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence ob2: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "cmd=go test ./covered") {
		t.Fatalf("reviewer projection missing evidence for covered obligation: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "cmd=go test ./uncovered") {
		t.Fatal("reviewer projection must not include evidence for obligation with no candidate patch")
	}
}

// TestT11_testerDropsLintWhenTestExists verifies that tester projections exclude
// lint_result evidence when test_result evidence is also present.
func TestT11_testerDropsLintWhenTestExists(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-tester-lint"
		conditionID = "GC-t11-tester-lint"
		obligation  = "OB-t11-tester-lint"
		implID      = "CAP-t11-tester-impl"
		testerID    = "CAP-t11-tester"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, c := range []schema.ExecutionCapsule{
		{CapsuleID: implID, ObligationIDs: []string{obligation}, Role: schema.RoleExecutor, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
		{CapsuleID: testerID, ObligationIDs: []string{obligation}, Role: schema.RoleTester, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t11-tester",
		CapsuleID:            implID,
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-tester-test",
		Type:       schema.EvidenceTestResult,
		Source:     "verifier",
		Command:    "go test ./testpkg",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence test: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-tester-lint",
		Type:       schema.EvidenceLintResult,
		Source:     "verifier",
		Command:    "go vet ./lintpkg",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence lint: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileTester(env.ctx, testerID)
	if err != nil {
		t.Fatalf("CompileTester: %v", err)
	}
	if !projectionIncludes(proj, "cmd=go test ./testpkg") {
		t.Fatalf("tester projection missing test_result evidence: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "cmd=go vet ./lintpkg") {
		t.Fatal("tester projection must not include lint_result when test_result exists")
	}
}

// TestT11_testerKeepsLintWhenNoTestExists verifies that tester projections retain
// lint_result evidence when no test_result evidence is present.
func TestT11_testerKeepsLintWhenNoTestExists(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t11-tester-keeplint"
		conditionID = "GC-t11-tester-keeplint"
		obligation  = "OB-t11-tester-keeplint"
		implID      = "CAP-t11-tester-kl-impl"
		testerID    = "CAP-t11-tester-kl"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, c := range []schema.ExecutionCapsule{
		{CapsuleID: implID, ObligationIDs: []string{obligation}, Role: schema.RoleExecutor, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
		{CapsuleID: testerID, ObligationIDs: []string{obligation}, Role: schema.RoleTester, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t11-tester-kl",
		CapsuleID:            implID,
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: "EV-t11-tester-kl-lint",
		Type:       schema.EvidenceLintResult,
		Source:     "verifier",
		Command:    "go vet ./lintonly",
		ExitCode:   0,
		Supports:   []string{obligation},
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveEvidence lint: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileTester(env.ctx, testerID)
	if err != nil {
		t.Fatalf("CompileTester: %v", err)
	}
	if !projectionIncludes(proj, "cmd=go vet ./lintonly") {
		t.Fatalf("tester projection must retain lint_result when no test_result exists: %+v", proj.IncludedSections)
	}
}

// ── T12: Candidate patch fan-out filtering ───────────────────────────────────

// TestT12_onlyCandidatePatchesIncluded verifies that accepted/rejected patches
// are excluded from reviewer projections; only candidate patches appear.
func TestT12_onlyCandidatePatchesIncluded(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t12-status"
		conditionID = "GC-t12-status"
		obligation  = "OB-t12-status"
		implID      = "CAP-t12-status-impl"
		reviewerID  = "CAP-t12-status-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, c := range []schema.ExecutionCapsule{
		{CapsuleID: implID, ObligationIDs: []string{obligation}, Role: schema.RoleExecutor, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
		{CapsuleID: reviewerID, ObligationIDs: []string{obligation}, Role: schema.RoleReviewer, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-candidate",
		CapsuleID:            implID,
		Summary:              "current candidate patch",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SavePatch candidate: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-accepted",
		CapsuleID:            implID,
		Summary:              "previously accepted patch",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchAccepted,
		CreatedAt:            time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SavePatch accepted: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "PATCH-t12-candidate") {
		t.Fatalf("reviewer projection missing candidate patch: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "PATCH-t12-accepted") {
		t.Fatal("reviewer projection must not include accepted patch (T12: candidate-only)")
	}
}

// TestT12_latestCandidatePerObligation verifies that when multiple candidate
// patches exist for an obligation, only the most recent (by CreatedAt) appears.
func TestT12_latestCandidatePerObligation(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t12-latest"
		conditionID = "GC-t12-latest"
		obligation  = "OB-t12-latest"
		implID      = "CAP-t12-latest-impl"
		reviewerID  = "CAP-t12-latest-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, c := range []schema.ExecutionCapsule{
		{CapsuleID: implID, ObligationIDs: []string{obligation}, Role: schema.RoleExecutor, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
		{CapsuleID: reviewerID, ObligationIDs: []string{obligation}, Role: schema.RoleReviewer, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	base := time.Now().UTC()
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-older",
		CapsuleID:            implID,
		Summary:              "older candidate patch",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            base.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SavePatch older: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-newer",
		CapsuleID:            implID,
		Summary:              "newer candidate patch",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            base,
	}); err != nil {
		t.Fatalf("SavePatch newer: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "PATCH-t12-newer") {
		t.Fatalf("reviewer projection missing newer candidate patch: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "PATCH-t12-older") {
		t.Fatal("reviewer projection must not include older candidate when a newer one exists (T12)")
	}
}

func TestT12_latestCandidateTieBreaksByPatchID(t *testing.T) {
	t.Parallel()

	env := newProjectorEnv(t)
	const (
		goalID      = "G-t12-tie"
		conditionID = "GC-t12-tie"
		obligation  = "OB-t12-tie"
		implID      = "CAP-t12-tie-impl"
		reviewerID  = "CAP-t12-tie-reviewer"
	)
	seedGoalScenario(t, env, goalID, conditionID, obligation)
	for _, c := range []schema.ExecutionCapsule{
		{CapsuleID: implID, ObligationIDs: []string{obligation}, Role: schema.RoleExecutor, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
		{CapsuleID: reviewerID, ObligationIDs: []string{obligation}, Role: schema.RoleReviewer, AllowedPaths: []string{"internal/projector"}, Budget: schema.CapsuleBudget{MaxTokens: 32000}, State: schema.CapsuleStatePending},
	} {
		c := c
		if err := env.st.SaveCapsule(env.ctx, &c); err != nil {
			t.Fatalf("SaveCapsule %s: %v", c.CapsuleID, err)
		}
	}
	created := time.Now().UTC()
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-tie-a",
		CapsuleID:            implID,
		Summary:              "candidate A",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            created,
	}); err != nil {
		t.Fatalf("SavePatch A: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              "PATCH-t12-tie-z",
		CapsuleID:            implID,
		Summary:              "candidate Z",
		ObligationIDsClaimed: []string{obligation},
		Status:               schema.PatchCandidate,
		CreatedAt:            created,
	}); err != nil {
		t.Fatalf("SavePatch Z: %v", err)
	}

	proj, err := New(env.st, config.VerifierConfig{}).CompileReviewer(env.ctx, reviewerID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}
	if !projectionIncludes(proj, "PATCH-t12-tie-z") {
		t.Fatalf("reviewer projection missing tie-break winner PATCH-t12-tie-z: %+v", proj.IncludedSections)
	}
	if projectionIncludes(proj, "PATCH-t12-tie-a") {
		t.Fatal("reviewer projection must not include tie-break loser when timestamps are equal")
	}
}
