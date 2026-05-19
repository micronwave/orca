package projector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type service struct {
	store  store.ArtifactStore
	config config.VerifierConfig
}

// New returns the default ContextCompiler implementation.
func New(st store.ArtifactStore, cfg config.VerifierConfig) ContextCompiler {
	return &service{
		store:  st,
		config: cfg,
	}
}

func (s *service) CompileExecutor(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleExecutor)
}

func (s *service) CompileReviewer(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleReviewer)
}

func (s *service) CompileTester(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleTester)
}

func (s *service) compileAgentProjection(ctx context.Context, capsuleID string, role schema.ProjectionRole) (*schema.ContextProjection, error) {
	if s.store == nil {
		return nil, fmt.Errorf("projector: store is required")
	}
	capsule, goal, obligations, sourceArtifactIDs, err := s.loadCapsuleBundle(ctx, capsuleID)
	if err != nil {
		return nil, err
	}
	claims, claimIDs, err := s.loadVerifiedClaims(ctx, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	evidenceByObligation, evidenceIDs, err := s.loadEvidenceByObligation(ctx, obligations)
	if err != nil {
		return nil, err
	}
	patches, patchIDs, err := s.loadPatchesByObligation(ctx, obligations)
	if err != nil {
		return nil, err
	}
	failures, failureIDs, err := s.loadFailures(ctx, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	sourceArtifactIDs = append(sourceArtifactIDs, claimIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, evidenceIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, patchIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, failureIDs...)

	sections := []projectionSection{
		{key: "role_contract", text: roleContract(role)},
		{key: "goal_conditions", text: "goal conditions: " + summarizeGoalConditions(goal, obligations)},
		{key: "obligations", text: "obligations: " + summarizeObligations(obligations)},
		{key: "scope_constraints", text: "scope: " + summarizeScope(capsule)},
		{key: "required_outputs", text: "required outputs: " + summarizeRequiredOutputs(capsule.RequiredOutputs)},
		{key: "candidate_patches", text: summarizeCandidatePatches(role, patches)},
		{key: "prior_evidence", text: "prior evidence: " + summarizeEvidence(evidenceByObligation), removable: true},
		{key: "verified_claims", text: "verified claims: " + summarizeClaims(claims), removable: true},
		{key: "failure_fingerprints", text: "failure fingerprints: " + summarizeFailures(failures), removable: true},
	}

	tokenBudget := capsule.Budget.MaxTokens / 2
	included, omitted := enforceProjectionBudget(sections, tokenBudget)
	freshnessBase, err := s.latestSnapshotID(ctx, goal.GoalID)
	if err != nil {
		return nil, err
	}

	projection := &schema.ContextProjection{
		ContextProjectionID: idgen.New("CTX"),
		Role:                role,
		SourceArtifactIDs:   sourceArtifactIDs,
		IncludedSections:    included,
		OmittedSections:     omitted,
		TokenBudget:         tokenBudget,
		FreshnessBase:       freshnessBase,
		CreatedAt:           time.Now().UTC(),
	}
	if err := s.store.SaveProjection(ctx, projection); err != nil {
		return nil, fmt.Errorf("projector: save %s projection %s: %w", role, projection.ContextProjectionID, err)
	}
	return projection, nil
}

func (s *service) CompileHumanSummary(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error) {
	if s.store == nil {
		return nil, fmt.Errorf("projector: store is required")
	}
	capsule, goal, obligations, sourceArtifactIDs, err := s.loadCapsuleBundle(ctx, capsuleID)
	if err != nil {
		return nil, err
	}
	claims, claimIDs, err := s.loadVerifiedClaims(ctx, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	_, evidenceIDs, err := s.loadEvidenceByObligation(ctx, obligations)
	if err != nil {
		return nil, err
	}
	failures, failureIDs, err := s.loadFailures(ctx, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	sourceArtifactIDs = append(sourceArtifactIDs, claimIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, evidenceIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, failureIDs...)

	if strings.TrimSpace(capsule.TopologyDecisionID) == "" {
		return nil, fmt.Errorf("projector: capsule %s topology_decision_id is required", capsule.CapsuleID)
	}
	decision, err := s.store.LoadDecision(ctx, capsule.TopologyDecisionID)
	if err != nil {
		return nil, fmt.Errorf("projector: load topology decision %s: %w", capsule.TopologyDecisionID, err)
	}
	topology := schema.Topology(decision.Decision)
	freshnessBase, err := s.latestSnapshotID(ctx, goal.GoalID)
	if err != nil {
		return nil, err
	}

	conditionRefs, err := s.loadConditionRefs(ctx, obligations)
	if err != nil {
		return nil, err
	}
	obligationRefs := make([]schema.ObligationRef, 0, len(obligations))
	for _, obligation := range obligations {
		obligationRefs = append(obligationRefs, schema.ObligationRef{
			ObligationID: obligation.ObligationID,
			Description:  obligation.Description,
			RiskLevel:    obligation.RiskLevel,
		})
	}

	summary := &schema.HumanSummaryProjection{
		ContextProjection: schema.ContextProjection{
			ContextProjectionID: idgen.New("CTX"),
			Role:                schema.ProjectionRoleHumanSummary,
			SourceArtifactIDs:   sourceArtifactIDs,
			IncludedSections: []string{
				"goal_and_conditions",
				"implementation_approach",
				"scope_and_exclusions",
				"topology",
				"pre_execution_risks",
				"evidence_plan",
			},
			OmittedSections: nil,
			TokenBudget:     capsule.Budget.MaxTokens / 2,
			FreshnessBase:   freshnessBase,
			CreatedAt:       time.Now().UTC(),
		},
		GoalPlain:              goal.OriginalIntent,
		ConditionsAddressed:    conditionRefs,
		ObligationsAddressed:   obligationRefs,
		ImplementationApproach: implementationApproach(obligations),
		ExpectedFileScope: schema.ExpectedFileScope{
			ToRead:   append([]string(nil), capsule.AllowedPaths...),
			ToWrite:  append([]string(nil), capsule.AllowedPaths...),
			ToCreate: nil,
		},
		ExplicitExclusions: append([]string(nil), capsule.ForbiddenPaths...),
		Topology: schema.TopologyDecision{
			Selected:  topology,
			Rationale: decision.Rationale,
		},
		DesignDecisions: []schema.DesignDecision{
			{
				Decision:               "use topology selected by planner",
				AlternativesConsidered: []string{string(schema.TopologySingle), string(schema.TopologyImplementerReviewer), string(schema.TopologyHumanGated)},
				Reason:                 decision.Rationale,
			},
		},
		PreExecutionRisks: summarizePreExecutionRisks(obligations, failures, claims),
		EvidencePlan:      s.buildEvidencePlan(),
		Budget: schema.ProjectionBudget{
			MaxTokens:          capsule.Budget.MaxTokens,
			MaxWallTimeSeconds: capsule.Budget.MaxWallTimeSeconds,
		},
		RequiredApprovals: requiredApprovals(topology, obligations),
	}
	if err := s.store.SaveHumanSummaryProjection(ctx, summary); err != nil {
		return nil, fmt.Errorf("projector: save human summary projection %s: %w", summary.ContextProjectionID, err)
	}
	return summary, nil
}

type projectionSection struct {
	key       string
	text      string
	removable bool
}

func enforceProjectionBudget(sections []projectionSection, tokenBudget int) ([]string, []string) {
	included := make([]projectionSection, 0, len(sections))
	for _, section := range sections {
		if strings.TrimSpace(section.text) == "" {
			continue
		}
		included = append(included, section)
	}
	if tokenBudget <= 0 {
		return sectionTexts(included), nil
	}
	limit := tokenBudget * 4
	omitted := make([]string, 0, 3)
	for projectionBytes(included) > limit {
		removed := false
		for i := range included {
			if !included[i].removable {
				continue
			}
			omitted = append(omitted, included[i].key)
			included = append(included[:i], included[i+1:]...)
			removed = true
			break
		}
		if !removed {
			trimToLimit(&included, limit)
			omitted = append(omitted, "content_truncated")
			break
		}
	}
	return sectionTexts(included), omitted
}

func projectionBytes(sections []projectionSection) int {
	total := 0
	for _, section := range sections {
		total += len(section.text)
		total += 1
	}
	return total
}

func trimToLimit(sections *[]projectionSection, limit int) {
	if len(*sections) == 0 {
		return
	}
	remaining := limit
	for i := range *sections {
		if remaining <= 0 {
			(*sections)[i].text = ""
			continue
		}
		if len((*sections)[i].text) <= remaining {
			remaining -= len((*sections)[i].text) + 1
			continue
		}
		if remaining > 3 {
			(*sections)[i].text = (*sections)[i].text[:remaining-3] + "..."
		} else {
			(*sections)[i].text = ""
		}
		remaining = 0
	}
	out := (*sections)[:0]
	for _, section := range *sections {
		if strings.TrimSpace(section.text) == "" {
			continue
		}
		out = append(out, section)
	}
	*sections = out
}

func sectionTexts(sections []projectionSection) []string {
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		out = append(out, section.text)
	}
	return out
}

func roleContract(role schema.ProjectionRole) string {
	switch role {
	case schema.ProjectionRoleReviewer:
		return "role contract: review the implementer output against obligations, scope, risks, and evidence quality; produce review evidence and claims, not implementation changes"
	case schema.ProjectionRoleTester:
		return "role contract: test or challenge the patch against obligations and verifier gates; produce test evidence, risk notes, and follow-up claims, not implementation changes"
	default:
		return "role contract: implement the assigned obligations within allowed scope and produce a proof-carrying patch"
	}
}

func (s *service) latestSnapshotID(ctx context.Context, goalID string) (string, error) {
	snapshot, err := s.store.LoadLatestSnapshot(ctx, goalID)
	if err == nil {
		return snapshot.SnapshotID, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("projector: load latest snapshot for goal %s: %w", goalID, err)
}

func summarizeGoalConditions(goal *schema.GoalIR, obligations []*schema.Obligation) string {
	conditionByID := make(map[string]schema.GoalCondition, len(goal.GoalConditions))
	for _, condition := range goal.GoalConditions {
		conditionByID[condition.ID] = condition
	}
	parts := make([]string, 0, len(obligations))
	seen := make(map[string]bool, len(obligations))
	for _, obligation := range obligations {
		condition, ok := conditionByID[obligation.GoalConditionID]
		if !ok || seen[condition.ID] {
			continue
		}
		seen[condition.ID] = true
		parts = append(parts, condition.Description)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func summarizeObligations(obligations []*schema.Obligation) string {
	parts := make([]string, 0, len(obligations))
	for _, obligation := range obligations {
		parts = append(parts, obligation.ObligationID+": "+obligation.Description)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func summarizeScope(capsule *schema.ExecutionCapsule) string {
	allowed := strings.Join(capsule.AllowedPaths, ", ")
	if strings.TrimSpace(allowed) == "" {
		allowed = "(unspecified)"
	}
	forbidden := strings.Join(capsule.ForbiddenPaths, ", ")
	if strings.TrimSpace(forbidden) == "" {
		forbidden = "(none)"
	}
	return "allowed paths: " + allowed + "; forbidden paths: " + forbidden
}

func summarizeRequiredOutputs(required []string) string {
	if len(required) == 0 {
		return "none"
	}
	return strings.Join(required, ", ")
}

func summarizeEvidence(evidenceByObligation map[string][]*schema.EvidenceArtifact) string {
	if len(evidenceByObligation) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(evidenceByObligation))
	for obligationID, evidence := range evidenceByObligation {
		parts = append(parts, fmt.Sprintf("%s=%d artifacts", obligationID, len(evidence)))
	}
	return strings.Join(parts, "; ")
}

func summarizePatches(patches []*schema.PatchArtifact) string {
	if len(patches) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(patches))
	for _, patch := range patches {
		summary := strings.TrimSpace(patch.Summary)
		if summary == "" {
			summary = "(no summary)"
		}
		diffPath := strings.TrimSpace(patch.DiffPath)
		if diffPath == "" {
			diffPath = "(no diff path)"
		}
		parts = append(parts, fmt.Sprintf("%s status=%s diff=%s summary=%s", patch.PatchID, patch.Status, diffPath, summary))
	}
	return strings.Join(parts, "; ")
}

func summarizeCandidatePatches(role schema.ProjectionRole, patches []*schema.PatchArtifact) string {
	if role != schema.ProjectionRoleReviewer && role != schema.ProjectionRoleTester {
		return ""
	}
	return "candidate patches: " + summarizePatches(patches)
}

func summarizeClaims(claims []*schema.ClaimArtifact) string {
	if len(claims) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(claims))
	for _, claim := range claims {
		parts = append(parts, claim.ClaimID+": "+claim.Text)
	}
	return strings.Join(parts, "; ")
}

func summarizeFailures(failures []*schema.FailureFingerprint) string {
	if len(failures) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.FailureID+": "+failure.Summary)
	}
	return strings.Join(parts, "; ")
}

func implementationApproach(obligations []*schema.Obligation) string {
	descriptions := make([]string, 0, len(obligations))
	for _, obligation := range obligations {
		desc := strings.TrimSpace(obligation.Description)
		if desc == "" {
			continue
		}
		descriptions = append(descriptions, desc)
	}
	if len(descriptions) == 0 {
		descriptions = append(descriptions, "(no obligation descriptions provided)")
	}
	return "Agent will address the following obligations: " + strings.Join(descriptions, "; ")
}

func summarizePreExecutionRisks(
	obligations []*schema.Obligation,
	failures []*schema.FailureFingerprint,
	claims []*schema.ClaimArtifact,
) []schema.PreExecutionRisk {
	risks := make([]schema.PreExecutionRisk, 0, len(obligations)+len(failures)+len(claims))
	for _, obligation := range obligations {
		if obligation.RiskLevel == schema.RiskLow {
			continue
		}
		risks = append(risks, schema.PreExecutionRisk{
			Description: fmt.Sprintf("obligation %s risk level is %s", obligation.ObligationID, obligation.RiskLevel),
			Source:      "obligation_risk",
		})
	}
	for _, failure := range failures {
		risks = append(risks, schema.PreExecutionRisk{
			Description: fmt.Sprintf("prior failure %s may recur: %s", failure.FailureID, failure.Summary),
			Source:      "failure_fingerprint",
		})
	}
	for _, claim := range claims {
		if claim.ClaimType != schema.ClaimRisk {
			continue
		}
		risks = append(risks, schema.PreExecutionRisk{
			Description: fmt.Sprintf("verified risk claim %s: %s", claim.ClaimID, claim.Text),
			Source:      "claim",
		})
	}
	return risks
}

func requiredApprovals(topology schema.Topology, obligations []*schema.Obligation) []string {
	highOrMedium := false
	high := false
	for _, obligation := range obligations {
		if obligation.RiskLevel == schema.RiskHigh {
			high = true
			highOrMedium = true
			break
		}
		if obligation.RiskLevel == schema.RiskMedium {
			highOrMedium = true
		}
	}
	approvals := make([]string, 0, 2)
	switch topology {
	case schema.TopologyHumanGated:
		approvals = append(approvals, "review_projection (blocking)")
	case schema.TopologyImplementerReviewer:
		if highOrMedium {
			approvals = append(approvals, "review_projection (blocking)")
		}
	case schema.TopologySingle:
		approvals = append(approvals, "review_projection (review window)")
	}
	if high {
		approvals = append(approvals, "review_merge (high-risk)")
	}
	return approvals
}

func (s *service) buildEvidencePlan() schema.EvidencePlan {
	plan := schema.EvidencePlan{
		VerifierGates: make([]string, 0, len(s.config.Gates)),
		TestsToRun:    []string{},
		StaticChecks:  []string{},
	}
	for _, gate := range s.config.Gates {
		plan.VerifierGates = append(plan.VerifierGates, gate.Name)
		lower := strings.ToLower(gate.Name + " " + gate.Command)
		if strings.Contains(lower, "test") {
			plan.TestsToRun = append(plan.TestsToRun, gate.Command)
			continue
		}
		plan.StaticChecks = append(plan.StaticChecks, gate.Command)
	}
	return plan
}

func (s *service) loadCapsuleBundle(
	ctx context.Context,
	capsuleID string,
) (*schema.ExecutionCapsule, *schema.GoalIR, []*schema.Obligation, []string, error) {
	capsule, err := s.store.LoadCapsule(ctx, capsuleID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("projector: load capsule %s: %w", capsuleID, err)
	}
	if len(capsule.ObligationIDs) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("projector: capsule %s has no obligation IDs", capsuleID)
	}
	obligations := make([]*schema.Obligation, 0, len(capsule.ObligationIDs))
	sourceArtifactIDs := make([]string, 0, len(capsule.ObligationIDs)+1)
	sourceArtifactIDs = append(sourceArtifactIDs, capsule.CapsuleID)
	for _, obligationID := range capsule.ObligationIDs {
		obligation, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("projector: load obligation %s: %w", obligationID, err)
		}
		obligations = append(obligations, obligation)
		sourceArtifactIDs = append(sourceArtifactIDs, obligationID)
	}
	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("projector: load active goal: %w", err)
	}
	if goal == nil {
		return nil, nil, nil, nil, fmt.Errorf("projector: active goal not found")
	}
	return capsule, goal, obligations, sourceArtifactIDs, nil
}

func (s *service) loadEvidenceByObligation(
	ctx context.Context,
	obligations []*schema.Obligation,
) (map[string][]*schema.EvidenceArtifact, []string, error) {
	out := make(map[string][]*schema.EvidenceArtifact, len(obligations))
	seenIDs := make(map[string]bool)
	sourceIDs := make([]string, 0)
	for _, obligation := range obligations {
		evidence, err := s.store.LoadEvidenceForObligation(ctx, obligation.ObligationID)
		if err != nil {
			return nil, nil, fmt.Errorf("projector: load evidence for obligation %s: %w", obligation.ObligationID, err)
		}
		out[obligation.ObligationID] = evidence
		for _, artifact := range evidence {
			if seenIDs[artifact.EvidenceID] {
				continue
			}
			seenIDs[artifact.EvidenceID] = true
			sourceIDs = append(sourceIDs, artifact.EvidenceID)
		}
	}
	return out, sourceIDs, nil
}

func (s *service) loadPatchesByObligation(
	ctx context.Context,
	obligations []*schema.Obligation,
) ([]*schema.PatchArtifact, []string, error) {
	seenIDs := make(map[string]bool)
	patches := make([]*schema.PatchArtifact, 0)
	sourceIDs := make([]string, 0)
	for _, obligation := range obligations {
		items, err := s.store.LoadPatchesForObligation(ctx, obligation.ObligationID)
		if err != nil {
			return nil, nil, fmt.Errorf("projector: load patches for obligation %s: %w", obligation.ObligationID, err)
		}
		for _, patch := range items {
			if seenIDs[patch.PatchID] {
				continue
			}
			seenIDs[patch.PatchID] = true
			patches = append(patches, patch)
			sourceIDs = append(sourceIDs, patch.PatchID)
		}
	}
	return patches, sourceIDs, nil
}

func (s *service) loadVerifiedClaims(ctx context.Context, files []string) ([]*schema.ClaimArtifact, []string, error) {
	claims, err := s.store.LoadVerifiedClaimsForFiles(ctx, files)
	if err != nil {
		return nil, nil, fmt.Errorf("projector: load verified claims: %w", err)
	}
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.ClaimID)
	}
	return claims, ids, nil
}

func (s *service) loadFailures(ctx context.Context, files []string) ([]*schema.FailureFingerprint, []string, error) {
	failures, err := s.store.LoadFailuresForFiles(ctx, files)
	if err != nil {
		return nil, nil, fmt.Errorf("projector: load failures for files: %w", err)
	}
	ids := make([]string, 0, len(failures))
	for _, failure := range failures {
		ids = append(ids, failure.FailureID)
	}
	return failures, ids, nil
}

func (s *service) loadConditionRefs(ctx context.Context, obligations []*schema.Obligation) ([]schema.ConditionRef, error) {
	refs := make([]schema.ConditionRef, 0, len(obligations))
	seen := make(map[string]bool, len(obligations))
	for _, obligation := range obligations {
		if seen[obligation.GoalConditionID] {
			continue
		}
		seen[obligation.GoalConditionID] = true
		condition, err := s.store.LoadGoalCondition(ctx, obligation.GoalConditionID)
		if err != nil {
			return nil, fmt.Errorf("projector: load goal condition %s: %w", obligation.GoalConditionID, err)
		}
		refs = append(refs, schema.ConditionRef{
			ConditionID: condition.ID,
			Description: condition.Description,
		})
	}
	return refs, nil
}

var _ ContextCompiler = (*service)(nil)
