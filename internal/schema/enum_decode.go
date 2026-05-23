package schema

import (
	"encoding/json"
	"fmt"
)

// UnmarshalJSON implementations for string-typed enums so that unknown values
// in disk artifacts and event payloads are rejected at decode time rather than
// silently entering state as pass-through unknowns.
//
// Empty string is permitted as the Go zero value; callers that care about
// unset fields must validate presence themselves. The guard here catches typos
// and values from incompatible future versions (e.g. "satisfyed" or "deny_all").
//
// EventType is deliberately excluded: the replay engine tolerates unknown event
// types so that newer event logs can be replayed by older binaries without error.

func (r *RiskLevel) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*r = ""
		return nil
	}
	switch RiskLevel(s) {
	case RiskLow, RiskMedium, RiskHigh:
		*r = RiskLevel(s)
		return nil
	}
	return fmt.Errorf("unknown risk_level %q", s)
}

func (t *Topology) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*t = ""
		return nil
	}
	switch Topology(s) {
	case TopologySingle, TopologyImplementerReviewer, TopologyHumanGated,
		TopologyParallel, TopologyTestFirst, TopologyInvestigateThenImpl:
		*t = Topology(s)
		return nil
	}
	return fmt.Errorf("unknown topology %q", s)
}

func (g *GoalStatus) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*g = ""
		return nil
	}
	switch GoalStatus(s) {
	case GoalStatusActive, GoalStatusBlocked, GoalStatusComplete, GoalStatusCancelled:
		*g = GoalStatus(s)
		return nil
	}
	return fmt.Errorf("unknown goal_status %q", s)
}

func (g *GoalConditionStatus) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*g = ""
		return nil
	}
	switch GoalConditionStatus(s) {
	case GoalConditionUnmet, GoalConditionPartiallyMet, GoalConditionMet, GoalConditionBlocked:
		*g = GoalConditionStatus(s)
		return nil
	}
	return fmt.Errorf("unknown goal_condition_status %q", s)
}

func (o *ObligationStatus) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*o = ""
		return nil
	}
	switch ObligationStatus(s) {
	case ObligationOpen, ObligationSatisfied, ObligationFailed, ObligationWaived:
		*o = ObligationStatus(s)
		return nil
	}
	return fmt.Errorf("unknown obligation_status %q", s)
}

func (c *CapsuleState) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*c = ""
		return nil
	}
	switch CapsuleState(s) {
	case CapsuleStatePending, CapsuleStateWorktreeCreated, CapsuleStateWorkspaceAttached,
		CapsuleStateSetupRun, CapsuleStateAgentRunning, CapsuleStateCompleted, CapsuleStateFailed:
		*c = CapsuleState(s)
		return nil
	}
	return fmt.Errorf("unknown capsule_state %q", s)
}

func (c *CapsuleRole) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*c = ""
		return nil
	}
	switch CapsuleRole(s) {
	case RoleExecutor, RoleReviewer, RoleTester, RoleInvestigator:
		*c = CapsuleRole(s)
		return nil
	}
	return fmt.Errorf("unknown capsule_role %q", s)
}

func (a *AgentType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*a = ""
		return nil
	}
	switch AgentType(s) {
	case AgentCodex, AgentClaude, AgentCopilot, AgentTool:
		*a = AgentType(s)
		return nil
	}
	return fmt.Errorf("unknown agent_type %q", s)
}

func (n *NetworkPolicy) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*n = ""
		return nil
	}
	switch NetworkPolicy(s) {
	case NetworkDeny, NetworkAllowlist, NetworkAllow:
		*n = NetworkPolicy(s)
		return nil
	}
	return fmt.Errorf("unknown network_policy %q", s)
}

func (p *PatchStatus) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*p = ""
		return nil
	}
	switch PatchStatus(s) {
	case PatchCandidate, PatchAccepted, PatchRejected, PatchSuperseded:
		*p = PatchStatus(s)
		return nil
	}
	return fmt.Errorf("unknown patch_status %q", s)
}

func (c *ClaimStatus) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*c = ""
		return nil
	}
	switch ClaimStatus(s) {
	case ClaimProposed, ClaimVerified, ClaimStale, ClaimContested, ClaimInvalidated:
		*c = ClaimStatus(s)
		return nil
	}
	return fmt.Errorf("unknown claim_status %q", s)
}

func (c *ClaimType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*c = ""
		return nil
	}
	switch ClaimType(s) {
	case ClaimAssumption, ClaimInvariant, ClaimExclusion, ClaimOpenQuestion, ClaimRisk, ClaimTestGap:
		*c = ClaimType(s)
		return nil
	}
	return fmt.Errorf("unknown claim_type %q", s)
}

func (s *SidecarClaimStatus) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v == "" {
		*s = ""
		return nil
	}
	switch SidecarClaimStatus(v) {
	case SidecarClaimVerified, SidecarClaimProposed:
		*s = SidecarClaimStatus(v)
		return nil
	}
	return fmt.Errorf("unknown sidecar_claim_status %q", v)
}

func (p *ProjectionRole) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*p = ""
		return nil
	}
	switch ProjectionRole(s) {
	case ProjectionRoleExecutor, ProjectionRoleHumanSummary, ProjectionRoleReviewer, ProjectionRoleTester:
		*p = ProjectionRole(s)
		return nil
	}
	return fmt.Errorf("unknown projection_role %q", s)
}

func (v *VerifierVerdict) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*v = ""
		return nil
	}
	switch VerifierVerdict(s) {
	case VerdictSatisfied, VerdictFailed, VerdictWaived, VerdictBlocked:
		*v = VerifierVerdict(s)
		return nil
	}
	return fmt.Errorf("unknown verifier_verdict %q", s)
}

func (r *RecommendedAction) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*r = ""
		return nil
	}
	switch RecommendedAction(s) {
	case ActionAccept, ActionRetry, ActionSplit, ActionReject, ActionHumanReview:
		*r = RecommendedAction(s)
		return nil
	}
	return fmt.Errorf("unknown recommended_action %q", s)
}

func (e *EvidenceType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*e = ""
		return nil
	}
	switch EvidenceType(s) {
	case EvidenceTestResult, EvidenceLintResult, EvidenceTypecheckResult,
		EvidenceDiffRiskReport, EvidenceAgentOutput:
		*e = EvidenceType(s)
		return nil
	}
	return fmt.Errorf("unknown evidence_type %q", s)
}

func (f *FailureType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*f = ""
		return nil
	}
	switch FailureType(s) {
	case FailureTest, FailureLint, FailureTypecheck, FailureRuntime,
		FailureMerge, FailurePolicy, FailureInfra, FailureAgent:
		*f = FailureType(s)
		return nil
	}
	return fmt.Errorf("unknown failure_type %q", s)
}
