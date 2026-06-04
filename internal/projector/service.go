package projector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// maxDiffBytes is the per-patch diff truncation limit for reviewer/tester
// projections. Diffs exceeding this length are truncated with a [truncated] marker.
const maxDiffBytes = 8192

// Compiler builds role-specific context projections from the artifact graph.
// Human summaries remain separate from agent projections. orca.md §5.4.
type Compiler struct {
	store    *store.FileStore
	config   config.VerifierConfig
	advanced config.AdvancedConfig
}

// New returns a projector Compiler.
func New(st *store.FileStore, cfg config.VerifierConfig) *Compiler {
	return NewWithConfig(st, cfg, config.AdvancedConfig{})
}

// NewWithConfig returns a projector Compiler with advanced verification config.
func NewWithConfig(st *store.FileStore, cfg config.VerifierConfig, advanced config.AdvancedConfig) *Compiler {
	return &Compiler{
		store:    st,
		config:   cfg,
		advanced: advanced,
	}
}

func (s *Compiler) CompileExecutor(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleExecutor)
}

func (s *Compiler) CompileReviewer(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleReviewer)
}

func (s *Compiler) CompileTester(ctx context.Context, capsuleID string) (*schema.ContextProjection, error) {
	return s.compileAgentProjection(ctx, capsuleID, schema.ProjectionRoleTester)
}

func (s *Compiler) compileAgentProjection(ctx context.Context, capsuleID string, role schema.ProjectionRole) (*schema.ContextProjection, error) {
	if s.store == nil {
		return nil, fmt.Errorf("projector: store is required")
	}
	capsule, goal, obligations, sourceArtifactIDs, err := s.loadCapsuleBundle(ctx, capsuleID)
	if err != nil {
		return nil, err
	}
	freshnessBase, err := s.latestSnapshotID(ctx, goal.GoalID)
	if err != nil {
		return nil, err
	}
	topology, err := s.loadTopologyForCapsule(ctx, capsule)
	if err != nil {
		return nil, err
	}
	query := buildProjectionQuery(goal, obligations)
	claims, claimIDs, err := s.loadClaimsForProjection(ctx, goal.GoalID, query, role, topology, maxRiskLevel(obligations))
	if err != nil {
		return nil, err
	}
	// T12: load candidate patches first; used by T11 reviewer/tester evidence filter.
	patches, patchIDs, err := s.loadPatchesByObligation(ctx, obligations)
	if err != nil {
		return nil, err
	}
	coveredByPatch := obligationsCoveredByPatches(patches)
	// T11: apply role-specific evidence freshness filtering.
	evidenceByObligation, evidenceIDs, err := s.loadEvidenceByObligation(ctx, obligations, role, freshnessBase, coveredByPatch)
	if err != nil {
		return nil, err
	}
	// T10: compute evidence sub-section hash; reuse stored text when evidence is unchanged.
	evidenceHash := computeSourceHash(evidenceIDs, freshnessBase, nil)
	evidenceSectionText := s.cachedEvidenceSectionText(ctx, role, evidenceHash, evidenceByObligation)

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
		{key: "prior_evidence", text: evidenceSectionText, removable: true},
		{key: "claims", text: "claims: " + summarizeClaims(claims, freshnessBase), removable: true},
		{key: "failure_fingerprints", text: "failure fingerprints: " + summarizeFailures(failures), removable: true},
	}

	sourceHash := computeSourceHash(sourceArtifactIDs, freshnessBase, sectionTexts(filterNonEmpty(sections)))
	reuseKey := string(role) + "|" + sourceHash
	const briefingBudgetFraction = 0.70
	tokenBudget := int(float64(capsule.Budget.MaxTokens) * briefingBudgetFraction)
	included, omittedRaw := enforceProjectionBudget(sections, tokenBudget)
	contentHash := computeContentHash(included)

	// Reuse detection: if the rendered source context is identical for this role,
	// record reuse and return the stored projection instead of creating a duplicate.
	if existing, lookupErr := s.store.LoadProjectionBySourceHashAndRole(ctx, role, sourceHash); lookupErr == nil && existing.ContentHash == contentHash {
		reuseRecord := &schema.ProjectionReuseRecord{
			ReuseID:              idgen.New("REUSE"),
			CapsuleID:            capsuleID,
			GoalID:               goal.GoalID,
			Role:                 role,
			SourceHash:           sourceHash,
			OriginalProjectionID: existing.ContextProjectionID,
			TokensSaved:          existing.TokensAfter,
			RecordedAt:           time.Now().UTC(),
		}
		// Best-effort: a failed reuse-record save must not block returning the projection.
		_ = s.store.SaveProjectionReuseRecord(ctx, reuseRecord)
		return existing, nil
	}

	tokensBefore := projectionBytes(filterNonEmpty(sections))
	tokensAfter := sumLengths(included)

	// Build both legacy string slice and structured slice for OmittedWithReasons.
	omittedKeys := make([]string, 0, len(omittedRaw))
	omittedWithReasons := make([]schema.OmittedSection, 0, len(omittedRaw))
	for _, o := range omittedRaw {
		omittedKeys = append(omittedKeys, o.key)
		omittedWithReasons = append(omittedWithReasons, schema.OmittedSection{Key: o.key, Reason: o.reason})
	}

	projection := &schema.ContextProjection{
		ContextProjectionID: idgen.New("CTX"),
		Role:                role,
		SourceArtifactIDs:   sourceArtifactIDs,
		IncludedSections:    included,
		OmittedSections:     omittedKeys,
		TokenBudget:         tokenBudget,
		FreshnessBase:       freshnessBase,
		CreatedAt:           time.Now().UTC(),
		SourceHash:          sourceHash,
		ContentHash:         contentHash,
		TokensBefore:        tokensBefore,
		TokensAfter:         tokensAfter,
		OmittedWithReasons:  omittedWithReasons,
		ReuseKey:            reuseKey,
		EvidenceHash:        evidenceHash,
	}
	if err := s.store.SaveProjection(ctx, projection); err != nil {
		return nil, fmt.Errorf("projector: save %s projection %s: %w", role, projection.ContextProjectionID, err)
	}
	return projection, nil
}

// omittedSectionResult carries an omitted section key and the reason it was removed.
type omittedSectionResult struct {
	key    string
	reason string
}

// computeSourceHash returns a stable SHA-256 hex digest of the projection source
// state: sorted source IDs, freshness base, and the rendered pre-budget source
// sections. Including rendered source text prevents stale reuse when an artifact
// is updated in place without changing its ID.
func computeSourceHash(sourceArtifactIDs []string, freshnessBase string, sourceSections []string) string {
	sorted := make([]string, len(sourceArtifactIDs))
	copy(sorted, sourceArtifactIDs)
	sort.Strings(sorted)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s", strings.Join(sorted, "|"), freshnessBase)
	for _, section := range sourceSections {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(section))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// computeContentHash returns a stable SHA-256 hex digest of the included
// section texts joined with newlines. Identical content → identical hash.
func computeContentHash(sections []string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.Join(sections, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}

// filterNonEmpty returns only sections with non-empty text.
func filterNonEmpty(sections []projectionSection) []projectionSection {
	out := make([]projectionSection, 0, len(sections))
	for _, s := range sections {
		if strings.TrimSpace(s.text) != "" {
			out = append(out, s)
		}
	}
	return out
}

// sumLengths returns the total byte length of a slice of strings.
func sumLengths(sections []string) int {
	total := 0
	for _, s := range sections {
		total += len(s)
	}
	return total
}

func (s *Compiler) CompileHumanSummary(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error) {
	if s.store == nil {
		return nil, fmt.Errorf("projector: store is required")
	}
	capsule, goal, obligations, sourceArtifactIDs, err := s.loadCapsuleBundle(ctx, capsuleID)
	if err != nil {
		return nil, err
	}
	freshnessBase, err := s.latestSnapshotID(ctx, goal.GoalID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(capsule.TopologyDecisionID) == "" {
		return nil, fmt.Errorf("projector: capsule %s topology_decision_id is required", capsule.CapsuleID)
	}
	sourceArtifactIDs = append(sourceArtifactIDs, capsule.TopologyDecisionID)
	decision, err := s.store.LoadDecision(ctx, capsule.TopologyDecisionID)
	if err != nil {
		return nil, fmt.Errorf("projector: load topology decision %s: %w", capsule.TopologyDecisionID, err)
	}
	topology := schema.Topology(decision.Decision)
	query := buildProjectionQuery(goal, obligations)
	claims, claimIDs, err := s.loadClaimsForProjection(ctx, goal.GoalID, query, schema.ProjectionRoleHumanSummary, topology, maxRiskLevel(obligations))
	if err != nil {
		return nil, err
	}
	contestedClaims, contestedClaimIDs, err := s.loadContestedClaims(ctx, goal.GoalID, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	_, evidenceIDs, err := s.loadEvidenceByObligation(ctx, obligations, schema.ProjectionRoleHumanSummary, freshnessBase, nil)
	if err != nil {
		return nil, err
	}
	failures, failureIDs, err := s.loadFailures(ctx, capsule.AllowedPaths)
	if err != nil {
		return nil, err
	}
	sourceArtifactIDs = append(sourceArtifactIDs, claimIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, contestedClaimIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, evidenceIDs...)
	sourceArtifactIDs = append(sourceArtifactIDs, failureIDs...)
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
			ToWrite:  nil,
			ToCreate: collectExpectedFiles(obligations),
		},
		ExplicitExclusions: append([]string(nil), capsule.ForbiddenPaths...),
		Topology: schema.TopologyDecision{
			Selected:  topology,
			Rationale: decision.Rationale,
		},
		DesignDecisions: []schema.DesignDecision{
			{
				Decision: "use topology selected by planner",
				AlternativesConsidered: []string{
					string(schema.TopologySingle),
					string(schema.TopologyImplementerReviewer),
					string(schema.TopologyHumanGated),
					string(schema.TopologyParallel),
					string(schema.TopologyTestFirst),
					string(schema.TopologyInvestigateThenImpl),
				},
				Reason: decision.Rationale,
			},
		},
		PreExecutionRisks: summarizePreExecutionRisks(obligations, failures, claims, contestedClaims, freshnessBase),
		EvidencePlan:      s.buildEvidencePlan(),
		Budget: schema.ProjectionBudget{
			MaxTokens:          capsule.Budget.MaxTokens,
			MaxWallTimeSeconds: capsule.Budget.MaxWallTimeSeconds,
		},
		RequiredApprovals: requiredApprovals(topology, obligations),
	}
	humanSections := humanSummaryContentSections(summary)
	summary.SourceHash = computeSourceHash(sourceArtifactIDs, freshnessBase, humanSections)
	summary.ContentHash = computeContentHash(humanSections)
	summary.TokensBefore = sumLengths(humanSections)
	summary.TokensAfter = summary.TokensBefore
	summary.ReuseKey = string(schema.ProjectionRoleHumanSummary) + "|" + summary.SourceHash
	if err := s.store.SaveHumanSummaryProjection(ctx, summary); err != nil {
		return nil, fmt.Errorf("projector: save human summary projection %s: %w", summary.ContextProjectionID, err)
	}
	return summary, nil
}

func humanSummaryContentSections(summary *schema.HumanSummaryProjection) []string {
	if summary == nil {
		return nil
	}
	sections := []string{
		"goal: " + summary.GoalPlain,
		"approach: " + summary.ImplementationApproach,
		"topology: " + string(summary.Topology.Selected) + " " + summary.Topology.Rationale,
		"scope_read: " + strings.Join(summary.ExpectedFileScope.ToRead, ","),
		"scope_write: " + strings.Join(summary.ExpectedFileScope.ToWrite, ","),
		"scope_create: " + strings.Join(summary.ExpectedFileScope.ToCreate, ","),
		"exclusions: " + strings.Join(summary.ExplicitExclusions, ","),
	}
	for _, condition := range summary.ConditionsAddressed {
		sections = append(sections, "condition: "+condition.ConditionID+" "+condition.Description)
	}
	for _, obligation := range summary.ObligationsAddressed {
		sections = append(sections, "obligation: "+obligation.ObligationID+" "+obligation.Description+" "+string(obligation.RiskLevel))
	}
	for _, risk := range summary.PreExecutionRisks {
		sections = append(sections, "risk: "+risk.Description+" "+risk.Source)
	}
	sections = append(sections,
		"verifier_gates: "+strings.Join(summary.EvidencePlan.VerifierGates, ","),
		"tests: "+strings.Join(summary.EvidencePlan.TestsToRun, ","),
		"static_checks: "+strings.Join(summary.EvidencePlan.StaticChecks, ","),
		"required_approvals: "+strings.Join(summary.RequiredApprovals, ","),
	)
	return sections
}

type projectionSection struct {
	key       string
	text      string
	removable bool
}

func enforceProjectionBudget(sections []projectionSection, tokenBudget int) ([]string, []omittedSectionResult) {
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
	const bytesPerToken = 3
	limit := tokenBudget * bytesPerToken
	omitted := make([]omittedSectionResult, 0, 3)
	for projectionBytes(included) > limit {
		removed := false
		for i := range included {
			if !included[i].removable {
				continue
			}
			omitted = append(omitted, omittedSectionResult{key: included[i].key, reason: "budget_exceeded"})
			included = append(included[:i], included[i+1:]...)
			removed = true
			break
		}
		if !removed {
			trimToLimit(&included, limit)
			omitted = append(omitted, omittedSectionResult{key: "content_truncated", reason: "content_truncated"})
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
		return "role contract: review the implementer output against obligations, scope, risks, and evidence quality; " +
			"check adjacent-code convention consistency (naming, error handling, output routing patterns) and flag divergence from nearby code patterns; " +
			"verify tests specify behavior not implementation details (test-independence); " +
			"confirm verifier-gate expectations are met; " +
			"produce review evidence and claims, not implementation changes"
	case schema.ProjectionRoleTester:
		return "role contract: test or challenge the patch against obligations and verifier gates; " +
			"write tests that specify behavior not implementation details (test-independence); " +
			"produce test evidence, risk notes, and follow-up claims, not implementation changes"
	default:
		return "role contract: implement the assigned obligations within allowed scope and produce a proof-carrying patch"
	}
}

func (s *Compiler) latestSnapshotID(ctx context.Context, goalID string) (string, error) {
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
	shortIDToFull := make(map[string]string)
	shortIDCollides := make(map[string]bool)
	for _, evidence := range evidenceByObligation {
		for _, item := range evidence {
			short := abbreviateEvidenceID(item.EvidenceID)
			if existing, ok := shortIDToFull[short]; !ok {
				shortIDToFull[short] = item.EvidenceID
				continue
			} else if existing != item.EvidenceID {
				shortIDCollides[short] = true
			}
		}
	}

	parts := make([]string, 0, len(evidenceByObligation))
	for obligationID, evidence := range evidenceByObligation {
		var supports, weakens []string
		for _, item := range evidence {
			displayID := abbreviateEvidenceID(item.EvidenceID)
			if shortIDCollides[displayID] {
				displayID = item.EvidenceID
			}
			label := fmt.Sprintf("%s(type=%s exit=%d", displayID, item.Type, item.ExitCode)
			// Command provenance: must always accompany an evidence result so
			// readers know which tool produced it. Evidence without a command is
			// labeled [no-provenance] and must not be treated as proof.
			if cmd := strings.TrimSpace(item.Command); cmd != "" {
				if len(cmd) > 40 {
					cmd = cmd[:37] + "..."
				}
				label += " cmd=" + cmd
			} else {
				label += " [no-provenance]"
			}
			if strings.TrimSpace(item.ReusedFromID) != "" {
				label += " reused=" + item.ReusedFromID
			}
			label += ")"
			if evidenceWeakens(item, obligationID) {
				weakens = append(weakens, label)
			} else {
				supports = append(supports, label)
			}
		}
		sort.Strings(supports)
		sort.Strings(weakens)
		var sb strings.Builder
		sb.WriteString(obligationID)
		sb.WriteString("=")
		if len(supports) > 0 {
			sb.WriteString("supports=[")
			sb.WriteString(strings.Join(supports, ", "))
			sb.WriteString("]")
		}
		if len(weakens) > 0 {
			if len(supports) > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString("weakens=[")
			sb.WriteString(strings.Join(weakens, ", "))
			sb.WriteString("]")
		}
		if len(supports) == 0 && len(weakens) == 0 {
			sb.WriteString("none")
		}
		parts = append(parts, sb.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func abbreviateEvidenceID(id string) string {
	short := id
	if len(short) > 8 {
		short = short[len(short)-6:]
	}
	return short
}

func evidenceWeakens(item *schema.EvidenceArtifact, obligationID string) bool {
	return slices.Contains(item.Weakens, obligationID)
}

// readDiffContent loads the diff file at path and returns its text.
// Returns "(no diff)" when path is empty, "(diff unavailable: <err>)" on I/O
// error, and appends "[truncated]" when the content exceeds maxDiffBytes.
func readDiffContent(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(no diff)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(diff unavailable: %v)", err)
	}
	if len(data) == 0 {
		return "(no diff)"
	}
	text := string(data)
	if len(text) > maxDiffBytes {
		return text[:maxDiffBytes] + "\n[truncated]"
	}
	return text
}

func summarizeCandidatePatches(role schema.ProjectionRole, patches []*schema.PatchArtifact) string {
	if role != schema.ProjectionRoleReviewer && role != schema.ProjectionRoleTester {
		return ""
	}
	if len(patches) == 0 {
		return "candidate patches: none"
	}
	parts := make([]string, 0, len(patches))
	for _, patch := range patches {
		summary := strings.TrimSpace(patch.Summary)
		if summary == "" {
			summary = "(no summary)"
		}
		diffPath := strings.TrimSpace(patch.DiffPath)
		displayPath := diffPath
		if displayPath == "" {
			displayPath = "(no diff path)"
		}
		diffText := readDiffContent(diffPath)
		parts = append(parts, fmt.Sprintf("%s status=%s diff=%s summary=%s\ndiff content:\n%s",
			patch.PatchID, patch.Status, displayPath, summary, diffText))
	}
	return "candidate patches: " + strings.Join(parts, "\n")
}

func summarizeClaims(claims []*schema.ClaimArtifact, freshnessBase string) string {
	if len(claims) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(claims))
	for _, claim := range claims {
		text := claim.Text
		if len(text) > 200 {
			text = text[:197] + "..."
		}
		parts = append(parts, claimLabel(claim, freshnessBase)+": "+text)
	}
	return strings.Join(parts, "; ")
}

func claimLabel(claim *schema.ClaimArtifact, freshnessBase string) string {
	switch claim.Status {
	case schema.ClaimVerified:
		if freshnessBase != "" && (strings.TrimSpace(claim.LastValidatedAgainst) == "" || claim.LastValidatedAgainst != freshnessBase) {
			return claim.ClaimID + " [stale - freshness unverified]"
		}
		return claim.ClaimID
	case schema.ClaimProposed:
		return claim.ClaimID + " [proposed]"
	case schema.ClaimStale:
		return claim.ClaimID + " [stale]"
	case schema.ClaimContested:
		return claim.ClaimID + " [contested]"
	default:
		return claim.ClaimID + " [" + string(claim.Status) + "]"
	}
}

func summarizeFailures(failures []*schema.FailureFingerprint) string {
	if len(failures) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		summary := failure.Summary
		if len(summary) > 150 {
			summary = summary[:147] + "..."
		}
		parts = append(parts, failure.FailureID+": "+summary)
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
	contestedClaims []*schema.ClaimArtifact,
	freshnessBase string,
) []schema.PreExecutionRisk {
	risks := make([]schema.PreExecutionRisk, 0, len(obligations)+len(failures)+len(claims)+len(contestedClaims))
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
			Description: fmt.Sprintf("%s: %s", claimLabel(claim, freshnessBase), claim.Text),
			Source:      "claim",
		})
	}
	for _, claim := range contestedClaims {
		risks = append(risks, schema.PreExecutionRisk{
			Description: fmt.Sprintf("contested claim %s: %s", claim.ClaimID, claim.Text),
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
	case schema.TopologySingle, schema.TopologyParallel, schema.TopologyTestFirst, schema.TopologyInvestigateThenImpl:
		approvals = append(approvals, "review_projection (review window)")
	}
	if high {
		approvals = append(approvals, "review_merge (high-risk)")
	}
	return approvals
}

func (s *Compiler) buildEvidencePlan() schema.EvidencePlan {
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
	if s.advanced.Enabled {
		checks := []string{
			"Advanced verification: MAVEN=" + onOff(s.advanced.Maven) +
				" Mutation=" + onOff(s.advanced.Mutation) +
				" Adversarial=" + onOff(s.advanced.AdversarialTests),
		}
		if s.advanced.Maven || s.advanced.Mutation || s.advanced.AdversarialTests {
			plan.AdvancedChecks = checks
		}
	}
	return plan
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// collectExpectedFiles returns the union of ExpectedFiles across all
// obligations, deduplicated and in stable order.
func collectExpectedFiles(obligations []*schema.Obligation) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, ob := range obligations {
		for _, f := range ob.ExpectedFiles {
			if f = strings.TrimSpace(f); f != "" && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

func (s *Compiler) loadCapsuleBundle(
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
	seenConditions := make(map[string]bool)
	for _, obligationID := range capsule.ObligationIDs {
		obligation, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("projector: load obligation %s: %w", obligationID, err)
		}
		obligations = append(obligations, obligation)
		sourceArtifactIDs = append(sourceArtifactIDs, obligationID)
		if obligation.GoalConditionID != "" && !seenConditions[obligation.GoalConditionID] {
			seenConditions[obligation.GoalConditionID] = true
			sourceArtifactIDs = append(sourceArtifactIDs, obligation.GoalConditionID)
		}
	}
	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("projector: load active goal: %w", err)
	}
	if goal == nil {
		return nil, nil, nil, nil, fmt.Errorf("projector: active goal not found")
	}
	sourceArtifactIDs = append(sourceArtifactIDs, goal.GoalID)
	return capsule, goal, obligations, sourceArtifactIDs, nil
}

func (s *Compiler) loadEvidenceByObligation(
	ctx context.Context,
	obligations []*schema.Obligation,
	role schema.ProjectionRole,
	freshnessBase string,
	coveredByPatch map[string]bool,
) (map[string][]*schema.EvidenceArtifact, []string, error) {
	out := make(map[string][]*schema.EvidenceArtifact, len(obligations))
	seenIDs := make(map[string]bool)
	sourceIDs := make([]string, 0)
	for _, obligation := range obligations {
		evidence, err := s.store.LoadEvidenceForObligation(ctx, obligation.ObligationID)
		if err != nil {
			return nil, nil, fmt.Errorf("projector: load evidence for obligation %s: %w", obligation.ObligationID, err)
		}
		// T11: role-specific evidence freshness trim.
		evidence = applyEvidenceFreshnessFilter(evidence, obligation.ObligationID, role, freshnessBase, coveredByPatch)
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

// applyEvidenceFreshnessFilter applies T11 role-specific evidence trimming.
// Executor: cap at 3 most recent per obligation. Reviewer: drop stale evidence
// and evidence for obligations with no candidate patch. Tester: freshness filter
// plus drop lint_result when test_result evidence already exists.
func applyEvidenceFreshnessFilter(
	evidence []*schema.EvidenceArtifact,
	obligationID string,
	role schema.ProjectionRole,
	freshnessBase string,
	coveredByPatch map[string]bool,
) []*schema.EvidenceArtifact {
	switch role {
	case schema.ProjectionRoleExecutor:
		if len(evidence) <= 3 {
			return evidence
		}
		sorted := make([]*schema.EvidenceArtifact, len(evidence))
		copy(sorted, evidence)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		})
		return sorted[:3]

	case schema.ProjectionRoleReviewer:
		if !coveredByPatch[obligationID] {
			return nil
		}
		if freshnessBase == "" {
			return evidence
		}
		out := make([]*schema.EvidenceArtifact, 0, len(evidence))
		for _, ev := range evidence {
			if ev.ValidatedAgainst == freshnessBase {
				out = append(out, ev)
			}
		}
		return out

	case schema.ProjectionRoleTester:
		if freshnessBase != "" {
			fresh := make([]*schema.EvidenceArtifact, 0, len(evidence))
			for _, ev := range evidence {
				if ev.ValidatedAgainst == freshnessBase {
					fresh = append(fresh, ev)
				}
			}
			evidence = fresh
		}
		hasTest := false
		for _, ev := range evidence {
			if ev.Type == schema.EvidenceTestResult {
				hasTest = true
				break
			}
		}
		if !hasTest {
			return evidence
		}
		out := make([]*schema.EvidenceArtifact, 0, len(evidence))
		for _, ev := range evidence {
			if ev.Type != schema.EvidenceLintResult {
				out = append(out, ev)
			}
		}
		return out
	}
	return evidence
}

// obligationsCoveredByPatches returns the set of obligation IDs claimed by
// at least one patch in the slice. Used by T11 reviewer evidence filtering.
func obligationsCoveredByPatches(patches []*schema.PatchArtifact) map[string]bool {
	covered := make(map[string]bool, len(patches)*2)
	for _, patch := range patches {
		for _, obID := range patch.ObligationIDsClaimed {
			covered[obID] = true
		}
	}
	return covered
}

// cachedEvidenceSectionText returns the evidence section text, reusing a prior
// projection's stored text when the evidence hash matches (T10). Falls back to
// serializing evidenceByObligation directly when no cached text is found.
func (s *Compiler) cachedEvidenceSectionText(
	ctx context.Context,
	role schema.ProjectionRole,
	evidenceHash string,
	evidenceByObligation map[string][]*schema.EvidenceArtifact,
) string {
	if evidenceHash != "" {
		if cached, err := s.store.LoadProjectionByEvidenceHashAndRole(ctx, role, evidenceHash); err == nil {
			for _, section := range cached.IncludedSections {
				if strings.HasPrefix(section, "prior evidence: ") {
					return section
				}
			}
		}
	}
	return "prior evidence: " + summarizeEvidence(evidenceByObligation)
}

func (s *Compiler) loadPatchesByObligation(
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
		// T12: keep only the most recent candidate patch per obligation.
		var latest *schema.PatchArtifact
		for _, patch := range items {
			if patch.Status != schema.PatchCandidate {
				continue
			}
			if latest == nil || patch.CreatedAt.After(latest.CreatedAt) ||
				(patch.CreatedAt.Equal(latest.CreatedAt) && patch.PatchID > latest.PatchID) {
				latest = patch
			}
		}
		if latest != nil && !seenIDs[latest.PatchID] {
			seenIDs[latest.PatchID] = true
			patches = append(patches, latest)
			sourceIDs = append(sourceIDs, latest.PatchID)
		}
	}
	return patches, sourceIDs, nil
}

// loadClaimsForProjection implements two-stage retrieval: load all goal-scoped
// and repo-scoped claims as candidates (Stage 1), then score each against the
// query string composed of goal intent + obligation descriptions (Stage 2).
// Claims are returned sorted by relevance score, highest first. Hard gates:
// invalidated and superseded claims are excluded; claims with InjectionKeywords
// must match the query; claims with InjectionConditions must satisfy role,
// topology, and risk predicates. orca.md §5.8, todo_add.md item 10.
func (s *Compiler) loadClaimsForProjection(ctx context.Context, goalID, query string, role schema.ProjectionRole, topology schema.Topology, maxRisk schema.RiskLevel) ([]*schema.ClaimArtifact, []string, error) {
	goalClaims, err := s.store.LoadClaimsForGoal(ctx, goalID)
	if err != nil {
		return nil, nil, fmt.Errorf("projector: load goal claims: %w", err)
	}
	repoClaims, err := s.store.LoadRepoScopedClaims(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("projector: load repo-scoped claims: %w", err)
	}
	candidates := make([]*schema.ClaimArtifact, 0, len(goalClaims)+len(repoClaims))
	candidates = append(candidates, goalClaims...)
	candidates = append(candidates, repoClaims...)

	queryLower := strings.ToLower(query)
	type scoredClaim struct {
		claim *schema.ClaimArtifact
		score float32
	}
	scored := make([]scoredClaim, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, claim := range candidates {
		if seen[claim.ClaimID] {
			continue
		}
		seen[claim.ClaimID] = true
		if claim.Status == schema.ClaimInvalidated {
			continue
		}
		if strings.TrimSpace(claim.SupersededBy) != "" {
			continue
		}
		if len(claim.InjectionKeywords) > 0 {
			matched := false
			for _, kw := range claim.InjectionKeywords {
				if strings.Contains(queryLower, strings.ToLower(kw)) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if !evaluateInjectionConditions(claim.InjectionConditions, role, topology, maxRisk) {
			continue
		}
		scored = append(scored, scoredClaim{
			claim: claim,
			score: scoreClaimRelevance(claim, query),
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].claim.Confidence > scored[j].claim.Confidence
	})
	out := make([]*schema.ClaimArtifact, 0, len(scored))
	ids := make([]string, 0, len(scored))
	for _, sc := range scored {
		out = append(out, sc.claim)
		ids = append(ids, sc.claim.ClaimID)
	}
	return out, ids, nil
}

func (s *Compiler) loadContestedClaims(ctx context.Context, goalID string, files []string) ([]*schema.ClaimArtifact, []string, error) {
	claims, err := s.store.LoadClaimsByStatus(ctx, goalID, schema.ClaimContested)
	if err != nil {
		return nil, nil, fmt.Errorf("projector: load contested claims: %w", err)
	}
	fileSet := normalizedProjectionFiles(files)
	out := make([]*schema.ClaimArtifact, 0, len(claims))
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		if !claimMatchesFiles(claim, fileSet) {
			continue
		}
		out = append(out, claim)
		ids = append(ids, claim.ClaimID)
	}
	return out, ids, nil
}

func (s *Compiler) loadFailures(ctx context.Context, files []string) ([]*schema.FailureFingerprint, []string, error) {
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

func (s *Compiler) loadConditionRefs(ctx context.Context, obligations []*schema.Obligation) ([]schema.ConditionRef, error) {
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

func normalizedProjectionFiles(files []string) map[string]bool {
	out := make(map[string]bool, len(files))
	for _, file := range files {
		normalized := strings.TrimSpace(file)
		normalized = strings.ReplaceAll(normalized, "\\", "/")
		normalized = strings.TrimPrefix(normalized, "./")
		normalized = strings.Trim(normalized, "/")
		normalized = strings.ToLower(normalized)
		if normalized != "" {
			out[normalized] = true
		}
	}
	return out
}

func claimMatchesFiles(claim *schema.ClaimArtifact, fileSet map[string]bool) bool {
	if len(fileSet) == 0 || fileSet["."] {
		return true
	}
	for _, file := range claim.AffectedFiles {
		normalized := strings.TrimSpace(file)
		normalized = strings.ReplaceAll(normalized, "\\", "/")
		normalized = strings.TrimPrefix(normalized, "./")
		normalized = strings.Trim(normalized, "/")
		normalized = strings.ToLower(normalized)
		if fileSet[normalized] {
			return true
		}
		for scope := range fileSet {
			if strings.HasPrefix(normalized, scope+"/") || strings.HasPrefix(scope, normalized+"/") {
				return true
			}
		}
	}
	return false
}

// tokenizeText splits s into lowercase alphanumeric tokens, splitting on
// whitespace and punctuation. Used for keyword-overlap scoring.
func tokenizeText(s string) []string {
	tokens := make([]string, 0, len(s)/5+1)
	var buf strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			buf.WriteRune(r)
		} else {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
		}
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}
	return tokens
}

// buildProjectionQuery builds the relevance query from goal intent and
// obligation descriptions. Used by loadClaimsForProjection.
func buildProjectionQuery(goal *schema.GoalIR, obligations []*schema.Obligation) string {
	parts := make([]string, 0, 1+len(obligations))
	if goal != nil && strings.TrimSpace(goal.OriginalIntent) != "" {
		parts = append(parts, goal.OriginalIntent)
	}
	for _, o := range obligations {
		if d := strings.TrimSpace(o.Description); d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, " ")
}

// maxRiskLevel returns the highest RiskLevel among obligations.
func maxRiskLevel(obligations []*schema.Obligation) schema.RiskLevel {
	for _, o := range obligations {
		if o.RiskLevel == schema.RiskHigh {
			return schema.RiskHigh
		}
	}
	for _, o := range obligations {
		if o.RiskLevel == schema.RiskMedium {
			return schema.RiskMedium
		}
	}
	return schema.RiskLow
}

// scoreClaimRelevance scores a claim against a query string.
// Base score = (unique claim tokens that appear in query) / (unique claim tokens).
// Bonuses: +0.2 if any InjectionKeyword matches; +0.1*Confidence.
func scoreClaimRelevance(claim *schema.ClaimArtifact, query string) float32 {
	claimTokens := tokenizeText(claim.Text)
	claimSet := make(map[string]bool, len(claimTokens))
	for _, t := range claimTokens {
		claimSet[t] = true
	}
	queryTokens := tokenizeText(query)
	querySet := make(map[string]bool, len(queryTokens))
	for _, t := range queryTokens {
		querySet[t] = true
	}
	var base float32
	if len(claimSet) > 0 {
		var matches int
		for t := range claimSet {
			if querySet[t] {
				matches++
			}
		}
		base = float32(matches) / float32(len(claimSet))
	}
	queryLower := strings.ToLower(query)
	for _, kw := range claim.InjectionKeywords {
		if strings.Contains(queryLower, strings.ToLower(kw)) {
			base += 0.2
			break
		}
	}
	base += 0.1 * claim.Confidence
	return base
}

// evaluateInjectionConditions checks whether all structured predicates in
// conditions pass for the given capsule state. Supported keys: risk, role,
// topology. An unknown topology value causes topology conditions to pass
// vacuously to avoid over-exclusion when topology is not yet determined.
func evaluateInjectionConditions(conditions []string, role schema.ProjectionRole, topology schema.Topology, maxRisk schema.RiskLevel) bool {
	for _, cond := range conditions {
		key, value, ok := strings.Cut(cond, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "risk":
			if string(maxRisk) != value {
				return false
			}
		case "role":
			if string(role) != value {
				return false
			}
		case "topology":
			if topology != "" && string(topology) != value {
				return false
			}
		}
	}
	return true
}

// loadTopologyForCapsule loads the topology from the capsule's topology
// decision record. Returns ("", nil) when TopologyDecisionID is empty or the
// decision does not yet exist; errors only on genuine I/O failures.
func (s *Compiler) loadTopologyForCapsule(ctx context.Context, capsule *schema.ExecutionCapsule) (schema.Topology, error) {
	if strings.TrimSpace(capsule.TopologyDecisionID) == "" {
		return "", nil
	}
	decision, err := s.store.LoadDecision(ctx, capsule.TopologyDecisionID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("projector: load topology decision %s: %w", capsule.TopologyDecisionID, err)
	}
	return schema.Topology(decision.Decision), nil
}
