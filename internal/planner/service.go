package planner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/orcapath"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// Config defines planner inputs derived from runtime configuration.
type Config struct {
	OrcaDir                  string
	ApprovalPolicy           string
	DefaultMaxTokens         int
	DefaultMaxWallTime       int
	DefaultMaxRetries        int
	DefaultPermissionMode    schema.PermissionMode
	NoLearning               bool
	ReviewerDiversityEnabled bool
	PreferredReviewerAdapter string
}

// Planner reads open obligations for a goal, classifies topology, generates
// ExecutionCapsules, and returns the new capsule IDs ready for projection and
// execution.
type Planner struct {
	store    *store.FileStore
	config   Config
	outcomes OutcomeReader // may be nil; nil disables historical routing hints
}

// New returns a Planner.
// outcomes may be nil; when nil, historical routing hints are disabled.
func New(st *store.FileStore, cfg Config, outcomes OutcomeReader) *Planner {
	return &Planner{
		store:    st,
		config:   cfg,
		outcomes: outcomes,
	}
}

func (s *Planner) Plan(ctx context.Context, goalID string) (PlanResult, error) {
	if s.store == nil {
		return PlanResult{}, fmt.Errorf("planner: store is required")
	}
	if strings.TrimSpace(goalID) == "" {
		return PlanResult{}, fmt.Errorf("planner: goal ID is required")
	}
	goal, err := s.store.LoadGoal(ctx, goalID)
	if err != nil {
		return PlanResult{}, fmt.Errorf("planner: load goal %s: %w", goalID, err)
	}
	obligations, err := s.store.LoadOpenObligations(ctx, goalID)
	if err != nil {
		return PlanResult{}, fmt.Errorf("planner: load open obligations for goal %s: %w", goalID, err)
	}
	if len(obligations) == 0 {
		return PlanResult{}, fmt.Errorf("planner: no open obligations for goal %s", goalID)
	}
	expectedFiles := expectedFilesByObligation(obligations)
	failures, err := s.loadRelevantFailures(ctx, goalID, expectedFiles)
	if err != nil {
		return PlanResult{}, fmt.Errorf("planner: load failures for goal %s: %w", goalID, err)
	}

	// Suppress historical failure fingerprints when --no-learning is set.
	// §13: no-learning disables all adaptive reuse including prior failure fingerprints,
	// which would otherwise force TopologyHumanGated in the classifier.
	classifyFingerprints := failures
	if s.config.NoLearning {
		classifyFingerprints = nil
	}
	classifyInput := ClassifyInput{
		Obligations:               obligations,
		Fingerprints:              classifyFingerprints,
		ApprovalPolicy:            s.config.ApprovalPolicy,
		BudgetRemaining:           s.config.DefaultMaxTokens,
		ExpectedFilesByObligation: expectedFiles,
	}
	classifyInput.ExpectedFileOverlap = hasExpectedFileOverlap(classifyInput.ExpectedFilesByObligation)
	topology, rationale, err := classify(classifyInput)
	if err != nil {
		return PlanResult{}, fmt.Errorf("planner: classify topology: %w", err)
	}

	if !s.config.NoLearning && s.outcomes != nil {
		topology, rationale, err = s.applyHistoricalRoutingHint(ctx, topology, rationale, obligations)
		if err != nil {
			return PlanResult{}, fmt.Errorf("planner: apply historical routing hint: %w", err)
		}
	}

	switch topology {
	case schema.TopologySingle, schema.TopologyImplementerReviewer, schema.TopologyHumanGated:
	default:
		return PlanResult{}, fmt.Errorf("planner: topology %q is not supported in Phase 1", topology)
	}

	obligationIDs := make([]string, 0, len(obligations))
	for _, obligation := range obligations {
		obligationIDs = append(obligationIDs, obligation.ObligationID)
	}

	decision := &schema.DecisionRecord{
		DecisionID: idgen.New("DEC"),
		Context:    "topology_selection",
		Decision:   string(topology),
		Rationale:  rationale,
		MadeBy:     "system",
		RelatedIDs: obligationIDs,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.store.SaveDecision(ctx, decision); err != nil {
		return PlanResult{}, fmt.Errorf("planner: save topology decision %s: %w", decision.DecisionID, err)
	}

	hints := routingHints{}
	if !s.config.NoLearning {
		hints = deriveRoutingHints(obligations, failures)
	}
	capsules := s.buildCapsules(topology, obligations, goal, decision.DecisionID, hints)
	capsuleIDs := make([]string, 0, len(capsules))
	for i := range capsules {
		if err := s.store.SaveCapsule(ctx, &capsules[i]); err != nil {
			return PlanResult{}, fmt.Errorf("planner: save capsule %s: %w", capsules[i].CapsuleID, err)
		}
		capsuleIDs = append(capsuleIDs, capsules[i].CapsuleID)
	}

	return PlanResult{
		CapsuleIDs:        capsuleIDs,
		Topology:          topology,
		DecisionID:        decision.DecisionID,
		MaxObligationRisk: maxRisk(obligations),
	}, nil
}

func (s *Planner) loadRelevantFailures(ctx context.Context, goalID string, expectedFiles map[string][]string) ([]*schema.FailureFingerprint, error) {
	files := uniqueExpectedFiles(expectedFiles)
	if len(files) == 0 {
		return s.store.LoadAllFailures(ctx, goalID)
	}
	fileMatches, err := s.store.LoadFailuresForFiles(ctx, files)
	if err != nil {
		return nil, err
	}
	goalMatches, err := s.store.LoadAllFailures(ctx, goalID)
	if err != nil {
		return nil, err
	}
	inGoal := make(map[string]bool, len(goalMatches))
	for _, failure := range goalMatches {
		inGoal[failure.FailureID] = true
	}
	out := make([]*schema.FailureFingerprint, 0, len(fileMatches))
	for _, failure := range fileMatches {
		if inGoal[failure.FailureID] {
			out = append(out, failure)
		}
	}
	return out, nil
}

func (s *Planner) buildCapsules(topology schema.Topology, obligations []*schema.Obligation, goal *schema.GoalIR, decisionID string, hints routingHints) []schema.ExecutionCapsule {
	executorAgent := selectExecutorAgent(hints)
	switch topology {
	case schema.TopologyImplementerReviewer:
		implID := idgen.New("CAP")
		revID := idgen.New("CAP")
		reviewerAgent := schema.AgentClaude
		if executorAgent == schema.AgentClaude {
			reviewerAgent = schema.AgentCodex
		}
		if s.config.ReviewerDiversityEnabled && s.config.PreferredReviewerAdapter != "" {
			preferred := schema.AgentType(s.config.PreferredReviewerAdapter)
			if preferred != executorAgent {
				reviewerAgent = preferred
			}
		}
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(implID, executorAgent, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
			s.newCapsule(revID, reviewerAgent, schema.RoleReviewer, obligationIDs, goal.ScopeConstraints, decisionID),
		}
	default:
		capsuleID := idgen.New("CAP")
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(capsuleID, executorAgent, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
		}
	}
}

func (s *Planner) newCapsule(
	capsuleID string,
	agent schema.AgentType,
	role schema.CapsuleRole,
	obligationIDs []string,
	scope schema.ScopeConstraints,
	decisionID string,
) schema.ExecutionCapsule {
	return schema.ExecutionCapsule{
		CapsuleID:          capsuleID,
		ObligationIDs:      append([]string(nil), obligationIDs...),
		Agent:              agent,
		Role:               role,
		RequiredOutputs:    roleRequiredOutputs(role),
		AllowedPaths:       append([]string(nil), scope.AllowedFiles...),
		ForbiddenPaths:     append([]string(nil), scope.ForbiddenFiles...),
		ForbiddenActions:   append([]string(nil), scope.ForbiddenActions...),
		PermissionMode:     s.config.DefaultPermissionMode,
		Budget:             s.defaultCapsuleBudget(),
		Sandbox:            s.defaultSandbox(capsuleID),
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}
}

func roleRequiredOutputs(role schema.CapsuleRole) []string {
	switch role {
	case schema.RoleReviewer:
		return []string{"evidence.json", "claim.json"}
	default:
		return []string{"patch.diff", "evidence.json"}
	}
}

func (s *Planner) defaultCapsuleBudget() schema.CapsuleBudget {
	return schema.CapsuleBudget{
		MaxTokens:          s.config.DefaultMaxTokens,
		MaxWallTimeSeconds: s.config.DefaultMaxWallTime,
		MaxRetries:         s.config.DefaultMaxRetries,
	}
}

func (s *Planner) defaultSandbox(capsuleID string) schema.CapsuleSandbox {
	return schema.CapsuleSandbox{
		WorktreePath: orcapath.CapsuleWorktreePath(s.config.OrcaDir, capsuleID),
		Network:      schema.NetworkDeny,
		WriteScope:   "worktree_only",
	}
}

func classify(input ClassifyInput) (schema.Topology, string, error) {
	input.ExpectedFileOverlap = input.ExpectedFileOverlap || hasExpectedFileOverlap(input.ExpectedFilesByObligation)
	summary := classifySummary(input)
	protected, protectedPath := hasProtectedPath(input.ExpectedFilesByObligation, input.ProtectedPaths)
	for _, obligation := range input.Obligations {
		if obligation.RiskLevel == schema.RiskHigh {
			return schema.TopologyHumanGated,
				fmt.Sprintf("%s; obligation %s has risk level high -> human_gated", summary, obligation.ObligationID),
				nil
		}
	}
	for _, failure := range input.Fingerprints {
		if failure.PriorAttemptCount < 2 {
			continue
		}
		file := "(unknown file)"
		if len(failure.AffectedFiles) > 0 && strings.TrimSpace(failure.AffectedFiles[0]) != "" {
			file = failure.AffectedFiles[0]
		}
		return schema.TopologyHumanGated,
			fmt.Sprintf("%s; failure fingerprint %s has prior_attempt_count=%d and affects %s -> human_gated", summary, failure.FailureID, failure.PriorAttemptCount, file),
			nil
	}

	for _, obligation := range input.Obligations {
		if obligation.RiskLevel != schema.RiskMedium {
			continue
		}
		if input.ExpectedFileOverlap {
			return schema.TopologySingle,
				fmt.Sprintf("%s; obligation %s is medium risk but expected file overlap is high, so coordination cost exceeds expected value -> single", summary, obligation.ObligationID),
				nil
		}
		if protected {
			return schema.TopologySingle,
				fmt.Sprintf("%s; obligation %s touches protected path %s, so shared-file work is serialized -> single", summary, obligation.ObligationID, protectedPath),
				nil
		}
		if input.BudgetRemaining > 0 && input.BudgetRemaining < 2*defaultReviewerCoordinationTokens {
			return schema.TopologySingle,
				fmt.Sprintf("%s; obligation %s is medium risk but budget_remaining=%d is below implementer_reviewer coordination cost -> single", summary, obligation.ObligationID, input.BudgetRemaining),
				nil
		}
		return schema.TopologyImplementerReviewer,
			fmt.Sprintf("%s; obligation %s has risk level medium -> implementer_reviewer", summary, obligation.ObligationID),
			nil
	}

	if allLowRisk(input.Obligations) {
		if protected {
			return schema.TopologySingle,
				fmt.Sprintf("%s; protected path %s is in expected files, so shared-file work is serialized -> single", summary, protectedPath),
				nil
		}
		if input.ExpectedFileOverlap {
			return schema.TopologySingle,
				fmt.Sprintf("%s; expected file overlap is high, so coordination cost exceeds expected value -> single", summary),
				nil
		}
	}

	return schema.TopologySingle,
		fmt.Sprintf("%s; all obligations are low risk, no failure fingerprints -> single", summary),
		nil
}

const defaultReviewerCoordinationTokens = 1000

func classifySummary(input ClassifyInput) string {
	return fmt.Sprintf(
		"inputs: obligations=%d max_risk=%s expected_file_overlap=%t protected_path=%t fingerprints=%d budget_remaining=%d",
		len(input.Obligations),
		maxRisk(input.Obligations),
		input.ExpectedFileOverlap,
		mustSerializeExpectedFiles(input.ExpectedFilesByObligation, input.ProtectedPaths),
		len(input.Fingerprints),
		input.BudgetRemaining,
	)
}

func maxRisk(obligations []*schema.Obligation) schema.RiskLevel {
	max := schema.RiskLow
	for _, obligation := range obligations {
		switch obligation.RiskLevel {
		case schema.RiskHigh:
			return schema.RiskHigh
		case schema.RiskMedium:
			max = schema.RiskMedium
		}
	}
	return max
}

func expectedFilesByObligation(obligations []*schema.Obligation) map[string][]string {
	filesByObligation := make(map[string][]string, len(obligations))
	for _, obligation := range obligations {
		if obligation == nil || len(obligation.ExpectedFiles) == 0 {
			continue
		}
		filesByObligation[obligation.ObligationID] = normalizePaths(obligation.ExpectedFiles)
	}
	return filesByObligation
}

func uniqueExpectedFiles(filesByObligation map[string][]string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, files := range filesByObligation {
		for _, file := range normalizePaths(files) {
			if file == "" || seen[file] {
				continue
			}
			seen[file] = true
			out = append(out, file)
		}
	}
	return out
}

func hasExpectedFileOverlap(filesByObligation map[string][]string) bool {
	seen := make(map[string]string)
	for obligationID, files := range filesByObligation {
		for _, file := range normalizePaths(files) {
			if owner, ok := seen[file]; ok && owner != obligationID {
				return true
			}
			seen[file] = obligationID
		}
	}
	return false
}

func hasProtectedPath(filesByObligation map[string][]string, configured []string) (bool, string) {
	for _, files := range filesByObligation {
		for _, file := range normalizePaths(files) {
			if isProtectedPath(file, configured) {
				return true, file
			}
		}
	}
	return false, ""
}

func mustSerializeExpectedFiles(filesByObligation map[string][]string, configured []string) bool {
	protected, _ := hasProtectedPath(filesByObligation, configured)
	return protected
}

func isProtectedPath(path string, configured []string) bool {
	normalized := normalizePath(path)
	if normalized == "" {
		return false
	}
	for _, item := range normalizePaths(configured) {
		if normalized == item || strings.HasPrefix(normalized, strings.TrimSuffix(item, "/")+"/") {
			return true
		}
	}
	base := normalized
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if strings.HasSuffix(normalized, ".lock") ||
		strings.Contains(normalized, "/migrations/") ||
		strings.Contains(normalized, "/generated/") ||
		strings.Contains(normalized, "/api/") ||
		strings.Contains(normalized, "/schema/") {
		return true
	}
	switch base {
	case "go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml",
		"yarn.lock", "cargo.toml", "cargo.lock", "requirements.txt", "pyproject.toml",
		"poetry.lock", "gemfile", "gemfile.lock", "makefile", "dockerfile":
		return true
	}
	return false
}

func allLowRisk(obligations []*schema.Obligation) bool {
	if len(obligations) == 0 {
		return false
	}
	for _, obligation := range obligations {
		if obligation.RiskLevel != schema.RiskLow {
			return false
		}
	}
	return true
}

func normalizePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if normalized := normalizePath(path); normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	path = strings.Trim(path, "/")
	return strings.ToLower(path)
}

type routingHints struct {
	recurringFailureCount int
}

func deriveRoutingHints(obligations []*schema.Obligation, failures []*schema.FailureFingerprint) routingHints {
	if len(failures) == 0 {
		return routingHints{}
	}
	expectedFiles := make(map[string]bool)
	for _, obligation := range obligations {
		for _, file := range obligation.ExpectedFiles {
			if normalized := normalizePath(file); normalized != "" {
				expectedFiles[normalized] = true
			}
		}
	}
	count := 0
	for _, failure := range failures {
		if failure.PriorAttemptCount < 2 {
			continue
		}
		if len(expectedFiles) == 0 {
			count++
			continue
		}
		if failureTouchesExpectedFiles(failure, expectedFiles) {
			count++
		}
	}
	return routingHints{recurringFailureCount: count}
}

func failureTouchesExpectedFiles(failure *schema.FailureFingerprint, expectedFiles map[string]bool) bool {
	if failure == nil {
		return false
	}
	for _, file := range failure.AffectedFiles {
		if expectedFiles[normalizePath(file)] {
			return true
		}
	}
	return false
}

func selectExecutorAgent(hints routingHints) schema.AgentType {
	if hints.recurringFailureCount >= 2 {
		return schema.AgentClaude
	}
	return schema.AgentCodex
}

const (
	historicalMinSamples = 3
	historicalThreshold  = 0.15
)

// applyHistoricalRoutingHint overrides the classifier's topology to
// implementer_reviewer when historical outcome data shows its acceptance rate
// exceeds single's acceptance rate by more than historicalThreshold, with at
// least historicalMinSamples for each topology at the same risk level.
// The hint is only applied when the classifier returned single or
// implementer_reviewer; forced topologies (human_gated, parallel, etc.) are
// not overridden.
func (s *Planner) applyHistoricalRoutingHint(
	ctx context.Context,
	topology schema.Topology,
	rationale string,
	obligations []*schema.Obligation,
) (schema.Topology, string, error) {
	// The hint can only upgrade single → implementer_reviewer.
	// If the classifier already chose implementer_reviewer, or if the topology
	// is a forced type (human_gated, parallel, etc.), there is nothing to do.
	if topology != schema.TopologySingle {
		return topology, rationale, nil
	}
	risk := maxRisk(obligations)
	irOutcomes, err := s.outcomes.LoadTopologyOutcomes(ctx, schema.TopologyImplementerReviewer, risk)
	if err != nil {
		return topology, rationale, fmt.Errorf("load implementer_reviewer outcomes: %w", err)
	}
	singleOutcomes, err := s.outcomes.LoadTopologyOutcomes(ctx, schema.TopologySingle, risk)
	if err != nil {
		return topology, rationale, fmt.Errorf("load single outcomes: %w", err)
	}
	if len(irOutcomes) < historicalMinSamples || len(singleOutcomes) < historicalMinSamples {
		return topology, rationale, nil
	}
	irRate := acceptanceRate(irOutcomes)
	singleRate := acceptanceRate(singleOutcomes)
	if irRate > singleRate+historicalThreshold {
		hint := fmt.Sprintf(
			"historical routing: implementer_reviewer accepted %.0f%% (n=%d) vs single %.0f%% (n=%d) at risk=%s, threshold +%.0f%% met -> implementer_reviewer",
			irRate*100, len(irOutcomes), singleRate*100, len(singleOutcomes), risk, historicalThreshold*100,
		)
		return schema.TopologyImplementerReviewer, rationale + "; " + hint, nil
	}
	return topology, rationale, nil
}

func acceptanceRate(outcomes []*schema.TopologyOutcomeRecord) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	accepted := 0
	for _, o := range outcomes {
		if o.PatchAccepted {
			accepted++
		}
	}
	return float64(accepted) / float64(len(outcomes))
}
