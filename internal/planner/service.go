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
	OrcaDir            string
	ApprovalPolicy     string
	DefaultMaxTokens   int
	DefaultMaxWallTime int
	DefaultMaxRetries  int
	NoLearning         bool
}

type service struct {
	store      store.ArtifactStore
	config     Config
	classifier TopologyClassifier
}

type topologyClassifier struct{}

// New returns the default ObligationPlanner implementation.
func New(st store.ArtifactStore, cfg Config) ObligationPlanner {
	return &service{
		store:      st,
		config:     cfg,
		classifier: topologyClassifier{},
	}
}

func (s *service) Plan(ctx context.Context, goalID string) (PlanResult, error) {
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
	failures, err := s.store.LoadAllFailures(ctx, goalID)
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
		ExpectedFilesByObligation: expectedFilesByObligation(obligations),
		TestsExist:                testsExist(goal, obligations),
		RequiredTools:             requiredTools(goal, obligations),
	}
	classifyInput.ExpectedFileOverlap = hasExpectedFileOverlap(classifyInput.ExpectedFilesByObligation)
	topology, rationale, err := s.classifier.Classify(classifyInput)
	if err != nil {
		return PlanResult{}, fmt.Errorf("planner: classify topology: %w", err)
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

func (s *service) buildCapsules(topology schema.Topology, obligations []*schema.Obligation, goal *schema.GoalIR, decisionID string, hints routingHints) []schema.ExecutionCapsule {
	executorAgent := selectExecutorAgent(hints)
	switch topology {
	case schema.TopologyImplementerReviewer:
		implID := idgen.New("CAP")
		revID := idgen.New("CAP")
		reviewerAgent := schema.AgentClaude
		if executorAgent == schema.AgentClaude {
			reviewerAgent = schema.AgentCodex
		}
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(implID, executorAgent, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
			s.newCapsule(revID, reviewerAgent, schema.RoleReviewer, obligationIDs, goal.ScopeConstraints, decisionID),
		}
	case schema.TopologyParallel:
		capsules := make([]schema.ExecutionCapsule, 0, len(obligations))
		for _, obligation := range obligations {
			scope := goal.ScopeConstraints
			if len(obligation.ExpectedFiles) > 0 {
				scope.AllowedFiles = append([]string(nil), obligation.ExpectedFiles...)
			}
			capsuleID := idgen.New("CAP")
			capsules = append(capsules, s.newCapsule(capsuleID, executorAgent, schema.RoleExecutor, []string{obligation.ObligationID}, scope, decisionID))
		}
		return capsules
	case schema.TopologyTestFirst:
		testID := idgen.New("CAP")
		implID := idgen.New("CAP")
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(testID, schema.AgentCodex, schema.RoleTester, obligationIDs, goal.ScopeConstraints, decisionID),
			s.newCapsule(implID, executorAgent, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
		}
	case schema.TopologyInvestigateThenImpl:
		investigateID := idgen.New("CAP")
		implID := idgen.New("CAP")
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(investigateID, schema.AgentCodex, schema.RoleInvestigator, obligationIDs, goal.ScopeConstraints, decisionID),
			s.newCapsule(implID, executorAgent, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
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

func (s *service) newCapsule(
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
		AllowedPaths:       append([]string(nil), scope.AllowedFiles...),
		ForbiddenPaths:     append([]string(nil), scope.ForbiddenFiles...),
		ForbiddenActions:   append([]string(nil), scope.ForbiddenActions...),
		Budget:             s.defaultCapsuleBudget(),
		Sandbox:            s.defaultSandbox(capsuleID),
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: decisionID,
	}
}

func (s *service) defaultCapsuleBudget() schema.CapsuleBudget {
	return schema.CapsuleBudget{
		MaxTokens:          s.config.DefaultMaxTokens,
		MaxWallTimeSeconds: s.config.DefaultMaxWallTime,
		MaxRetries:         s.config.DefaultMaxRetries,
	}
}

func (s *service) defaultSandbox(capsuleID string) schema.CapsuleSandbox {
	return schema.CapsuleSandbox{
		WorktreePath: orcapath.CapsuleWorktreePath(s.config.OrcaDir, capsuleID),
		Network:      schema.NetworkDeny,
		WriteScope:   "worktree_only",
	}
}

func (topologyClassifier) Classify(input ClassifyInput) (schema.Topology, string, error) {
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
		file := "(unknown file)"
		if len(failure.AffectedFiles) > 0 && strings.TrimSpace(failure.AffectedFiles[0]) != "" {
			file = failure.AffectedFiles[0]
		}
		return schema.TopologyHumanGated,
			fmt.Sprintf("%s; failure fingerprint %s affects %s -> human_gated", summary, failure.FailureID, file),
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
		if requiresInvestigation(input) && !hasAnyExpectedFiles(input.ExpectedFilesByObligation) {
			return schema.TopologyInvestigateThenImpl,
				fmt.Sprintf("%s; low-risk obligation set requires investigation before implementation -> investigate_then_implement", summary),
				nil
		}
		if requiresTestFirst(input) {
			return schema.TopologyTestFirst,
				fmt.Sprintf("%s; low-risk obligation set requires test evidence before implementation and tests_exist=%t -> test_first", summary, input.TestsExist),
				nil
		}
		if canParallelize(input) {
			return schema.TopologyParallel,
				fmt.Sprintf("%s; low-risk obligations have disjoint unprotected expected files -> parallel", summary),
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

func canParallelize(input ClassifyInput) bool {
	if len(input.Obligations) < 2 || input.BudgetRemaining > 0 && input.BudgetRemaining < len(input.Obligations)*defaultReviewerCoordinationTokens {
		return false
	}
	if input.ExpectedFileOverlap || mustSerializeExpectedFiles(input.ExpectedFilesByObligation, input.ProtectedPaths) {
		return false
	}
	return allLowRisk(input.Obligations) && allObligationsHaveExpectedFiles(input.Obligations, input.ExpectedFilesByObligation)
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

func allObligationsHaveExpectedFiles(obligations []*schema.Obligation, filesByObligation map[string][]string) bool {
	for _, obligation := range obligations {
		if len(filesByObligation[obligation.ObligationID]) == 0 {
			return false
		}
	}
	return true
}

func hasAnyExpectedFiles(filesByObligation map[string][]string) bool {
	for _, files := range filesByObligation {
		if len(files) > 0 {
			return true
		}
	}
	return false
}

func requiresTestFirst(input ClassifyInput) bool {
	if input.TestsExist {
		return false
	}
	for _, tool := range input.RequiredTools {
		if strings.EqualFold(strings.TrimSpace(tool), "test_first") {
			return true
		}
	}
	return false
}

func requiresInvestigation(input ClassifyInput) bool {
	for _, tool := range input.RequiredTools {
		tool = strings.ToLower(strings.TrimSpace(tool))
		if tool == "investigate" || tool == "search" || tool == "inspect" {
			return true
		}
	}
	return false
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

func requiredTools(goal *schema.GoalIR, obligations []*schema.Obligation) []string {
	text := strings.ToLower(strings.Join(classifierText(goal, obligations), " "))
	tools := make([]string, 0, 2)
	if strings.Contains(text, "test first") ||
		strings.Contains(text, "tests first") ||
		strings.Contains(text, "write tests first") ||
		strings.Contains(text, "tdd") ||
		strings.Contains(text, "red green") {
		tools = append(tools, "test_first")
	}
	if strings.Contains(text, "investigate") ||
		strings.Contains(text, "investigation") ||
		strings.Contains(text, "inspect") ||
		strings.Contains(text, "triage") ||
		strings.Contains(text, "debug") ||
		strings.Contains(text, "root cause") ||
		strings.Contains(text, "analyze") {
		tools = append(tools, "investigate")
	}
	return uniqueNormalized(tools)
}

func testsExist(goal *schema.GoalIR, obligations []*schema.Obligation) bool {
	for _, obligation := range obligations {
		for _, file := range obligation.ExpectedFiles {
			if looksLikeTestFile(file) {
				return true
			}
		}
	}
	text := strings.ToLower(strings.Join(classifierText(goal, obligations), " "))
	if strings.Contains(text, "without tests") || strings.Contains(text, "no tests") {
		return false
	}
	return strings.Contains(text, "existing test") ||
		strings.Contains(text, "failing test") ||
		strings.Contains(text, "broken test") ||
		strings.Contains(text, "regression test")
}

func classifierText(goal *schema.GoalIR, obligations []*schema.Obligation) []string {
	text := make([]string, 0, len(obligations)+2)
	if goal != nil {
		text = append(text, goal.OriginalIntent)
		for _, condition := range goal.GoalConditions {
			text = append(text, condition.Description, condition.EffectiveDescription)
		}
	}
	for _, obligation := range obligations {
		text = append(text, obligation.Description)
		for _, required := range obligation.EvidenceRequired {
			text = append(text, required)
		}
	}
	return text
}

func looksLikeTestFile(path string) bool {
	normalized := normalizePath(path)
	return strings.Contains(normalized, "_test.") ||
		strings.Contains(normalized, ".test.") ||
		strings.Contains(normalized, "/test/") ||
		strings.Contains(normalized, "/tests/")
}

func uniqueNormalized(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
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

var _ ObligationPlanner = (*service)(nil)
var _ TopologyClassifier = topologyClassifier{}
