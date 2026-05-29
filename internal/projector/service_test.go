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
	if !projectionIncludes(projection, "EV-proj-reused(type=test_result exit=0 reused=EV-proj-source)") {
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
