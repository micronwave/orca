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

	classifyInput := ClassifyInput{
		Obligations:         obligations,
		Fingerprints:        failures,
		ApprovalPolicy:      s.config.ApprovalPolicy,
		BudgetRemaining:     s.config.DefaultMaxTokens,
		ExpectedFileOverlap: false,
		TestsExist:          false,
		RequiredTools:       nil,
	}
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

	capsules := s.buildCapsules(topology, obligations, goal, decision.DecisionID)
	capsuleIDs := make([]string, 0, len(capsules))
	for i := range capsules {
		if err := s.store.SaveCapsule(ctx, &capsules[i]); err != nil {
			return PlanResult{}, fmt.Errorf("planner: save capsule %s: %w", capsules[i].CapsuleID, err)
		}
		capsuleIDs = append(capsuleIDs, capsules[i].CapsuleID)
	}

	return PlanResult{
		CapsuleIDs: capsuleIDs,
		Topology:   topology,
		DecisionID: decision.DecisionID,
	}, nil
}

func (s *service) buildCapsules(topology schema.Topology, obligations []*schema.Obligation, goal *schema.GoalIR, decisionID string) []schema.ExecutionCapsule {
	switch topology {
	case schema.TopologyImplementerReviewer:
		implID := idgen.New("CAP")
		revID := idgen.New("CAP")
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(implID, schema.AgentCodex, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
			s.newCapsule(revID, schema.AgentClaude, schema.RoleReviewer, obligationIDs, goal.ScopeConstraints, decisionID),
		}
	default:
		capsuleID := idgen.New("CAP")
		obligationIDs := make([]string, 0, len(obligations))
		for _, obligation := range obligations {
			obligationIDs = append(obligationIDs, obligation.ObligationID)
		}
		return []schema.ExecutionCapsule{
			s.newCapsule(capsuleID, schema.AgentCodex, schema.RoleExecutor, obligationIDs, goal.ScopeConstraints, decisionID),
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
	for _, obligation := range input.Obligations {
		if obligation.RiskLevel == schema.RiskHigh {
			return schema.TopologyHumanGated,
				fmt.Sprintf("obligation %s has risk level high -> human_gated", obligation.ObligationID),
				nil
		}
	}
	for _, failure := range input.Fingerprints {
		file := "(unknown file)"
		if len(failure.AffectedFiles) > 0 && strings.TrimSpace(failure.AffectedFiles[0]) != "" {
			file = failure.AffectedFiles[0]
		}
		return schema.TopologyHumanGated,
			fmt.Sprintf("failure fingerprint %s affects %s -> human_gated", failure.FailureID, file),
			nil
	}

	if len(input.Obligations) > 1 {
		for _, obligation := range input.Obligations {
			if obligation.RiskLevel == schema.RiskMedium {
				return schema.TopologyImplementerReviewer,
					fmt.Sprintf("obligation %s has risk level medium with %d obligations -> implementer_reviewer", obligation.ObligationID, len(input.Obligations)),
					nil
			}
		}
	}

	allLow := len(input.Obligations) > 0
	for _, obligation := range input.Obligations {
		if obligation.RiskLevel != schema.RiskLow {
			allLow = false
			break
		}
	}
	if allLow && len(input.Fingerprints) == 0 {
		return schema.TopologySingle,
			fmt.Sprintf("all %d obligations are low risk and no failure fingerprints exist -> single", len(input.Obligations)),
			nil
	}

	return schema.TopologySingle,
		fmt.Sprintf("obligation count is %d with no high-risk obligations and no failure fingerprints -> single", len(input.Obligations)),
		nil
}

var _ ObligationPlanner = (*service)(nil)
var _ TopologyClassifier = topologyClassifier{}
