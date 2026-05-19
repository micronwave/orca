// Package verifier defines the VerifierEngine interface, which has two jobs:
// (1) propose initial obligations from a GoalIR, and (2) check whether evidence
// satisfies obligations for a given patch. orca.md §6 step 3, §10.
//
// Phase 1 decision: ProposeObligations uses fixed deterministic templates for
// evidence mapping, test/static gate evidence, and scope preservation. It does
// not call a model or run verifier commands directly.
//
// Dependency contract:
//
//	Reads  (store):   GoalIR via LoadGoal,
//	                  GoalConditions via LoadGoalCondition,
//	                  PatchArtifact via LoadPatch,
//	                  ExecutionCapsule via LoadCapsule (for scope contract),
//	                  Obligations via LoadObligation (for each claimed obligation),
//	                  EvidenceArtifacts via LoadEvidenceForObligation and LoadEvidence,
//	                  ClaimArtifacts via LoadClaim (for supplemental review signals)
//	Writes (store):   Obligations via SaveObligation (ProposeObligations only),
//	                  VerifierResult via SaveVerifierResult (Verify only)
//	Writes (log):     none directly — the ArtifactStore implementation emits
//	                  obligation_created on SaveObligation,
//	                  verifier_result_created on SaveVerifierResult
//
//	Must NOT import:  internal/planner, internal/runner, internal/reconciler,
//	                  internal/projector, internal/budget, internal/gate
//	Must NOT call:    store.SaveGoal, store.SaveCapsule, store.SaveBudgetRecord,
//	                  store.UpdateObligationStatus
//	Must NOT update:  Obligation status — advancing obligation state belongs
//	                  exclusively to the Reconciler
//	Must NOT run:     agent commands or model calls directly; verifier gates
//	                  invoke pre-configured user commands via a subprocess interface
package verifier

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// VerifierEngine has two jobs (orca.md §6, §10):
//  1. ProposeObligations: derive the initial Obligation set from a GoalIR.
//  2. Verify: check whether collected evidence satisfies obligations for a patch.
//
// Default verification stages for Verify run in order (orca.md §10):
//  1. Preflight: repo status, auth, configured gates, clean base
//  2. Scope check: changed files and LOC within capsule contract
//  3. Static checks: lint, typecheck, formatting as configured
//  4. Targeted tests: tests relevant to changed files or obligations
//  5. Regression checks: reproduction or regression evidence for bugfix/security obligations
//  6. Patch review: model or human review for risk, assumptions, obligation fit
//  7. Merge readiness: all blocking obligations satisfied or waived
//
// Stages 1–4 run in order; the first blocking failure within that group stops
// the remaining stages in 1–4. Stages 5–7 always run for their applicable
// obligation types regardless of any stage 1–4 blocking failure.
type VerifierEngine interface {
	// ProposeObligations derives the initial Obligation set from the GoalIR
	// for goalID, persists each obligation via SaveObligation, and returns the
	// IDs of the created obligations. Called once by the orchestrator after
	// IntentCompiler.Compile and before ObligationPlanner.Plan. orca.md §6 step 3.
	ProposeObligations(ctx context.Context, goalID string) ([]string, error)

	// Verify runs all applicable verifier stages for the patch identified by
	// patchID and returns a VerifierResult mapping each obligation to its
	// verdict. The result is persisted via SaveVerifierResult before returning.
	//
	// The RecommendedAction field is the authoritative signal consumed by the
	// Reconciler: accept, retry, split, reject, or human_review.
	Verify(ctx context.Context, patchID string) (*schema.VerifierResult, error)
}

// VerifyInput carries supplemental artifacts produced by reviewer/tester/
// investigator capsules in the same plan cycle so verification can incorporate
// peer review signal into recommendation confidence.
type VerifyInput struct {
	SupplementalEvidenceIDs []string
	SupplementalClaimIDs    []string
}

// SupplementalVerifierEngine extends VerifierEngine with supplemental inputs.
// The orchestrator type-asserts this interface when available.
type SupplementalVerifierEngine interface {
	VerifierEngine
	VerifyWithSupplements(ctx context.Context, patchID string, in VerifyInput) (*schema.VerifierResult, error)
}

// GateRunner abstracts subprocess execution for verifier gates.
type GateRunner interface {
	Run(ctx context.Context, command, workingDir string) (exitCode int, output string, err error)
}

type service struct {
	store          store.ArtifactStore
	config         config.VerifierConfig
	runner         GateRunner
	commandChecker func(string) error
}

// New returns the default VerifierEngine implementation.
func New(st store.ArtifactStore, cfg config.VerifierConfig, runner GateRunner) VerifierEngine {
	if runner == nil {
		runner = execGateRunner{}
	}
	return &service{
		store:          st,
		config:         cfg,
		runner:         runner,
		commandChecker: checkCommandPresent,
	}
}

func (s *service) ProposeObligations(ctx context.Context, goalID string) ([]string, error) {
	if s.store == nil {
		return nil, fmt.Errorf("verifier: store is required")
	}
	goal, err := s.store.LoadGoal(ctx, goalID)
	if err != nil {
		return nil, fmt.Errorf("verifier: load goal %s: %w", goalID, err)
	}

	obligationIDs := make([]string, 0, len(goal.GoalConditions)*3)
	for _, condition := range goal.GoalConditions {
		if condition.Status != schema.GoalConditionUnmet && condition.Status != schema.GoalConditionPartiallyMet {
			continue
		}
		obligations := []schema.Obligation{
			{
				ObligationID:     idgen.New("OB"),
				GoalConditionID:  condition.ID,
				Description:      "Run all tests and confirm exit code 0",
				EvidenceRequired: []string{string(schema.EvidenceTestResult)},
				Blocking:         true,
				RiskLevel:        goal.RiskLevel,
				Status:           schema.ObligationOpen,
			},
			{
				ObligationID:     idgen.New("OB"),
				GoalConditionID:  condition.ID,
				Description:      "Run static checks (vet/lint/typecheck) and confirm pass",
				EvidenceRequired: []string{string(schema.EvidenceLintResult), string(schema.EvidenceTypecheckResult)},
				Blocking:         true,
				RiskLevel:        schema.RiskLow,
				Status:           schema.ObligationOpen,
			},
			{
				ObligationID:     idgen.New("OB"),
				GoalConditionID:  condition.ID,
				Description:      "Confirm only intended files changed (scope check)",
				EvidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
				Blocking:         false,
				RiskLevel:        schema.RiskLow,
				Status:           schema.ObligationOpen,
			},
		}
		for i := range obligations {
			if err := s.store.SaveObligation(ctx, &obligations[i]); err != nil {
				return nil, fmt.Errorf("verifier: save obligation %s: %w", obligations[i].ObligationID, err)
			}
			obligationIDs = append(obligationIDs, obligations[i].ObligationID)
		}
	}
	return obligationIDs, nil
}

func (s *service) Verify(ctx context.Context, patchID string) (*schema.VerifierResult, error) {
	return s.VerifyWithSupplements(ctx, patchID, VerifyInput{})
}

func (s *service) VerifyWithSupplements(ctx context.Context, patchID string, in VerifyInput) (*schema.VerifierResult, error) {
	if s.store == nil {
		return nil, fmt.Errorf("verifier: store is required")
	}
	if s.runner == nil {
		return nil, fmt.Errorf("verifier: gate runner is required")
	}

	patch, err := s.store.LoadPatch(ctx, patchID)
	if err != nil {
		return nil, fmt.Errorf("verifier: load patch %s: %w", patchID, err)
	}
	capsule, err := s.store.LoadCapsule(ctx, patch.CapsuleID)
	if err != nil {
		return nil, fmt.Errorf("verifier: load capsule %s: %w", patch.CapsuleID, err)
	}
	claimed := patch.ObligationIDsClaimed
	obligationRefs := claimed
	if len(obligationRefs) == 0 {
		obligationRefs = capsule.ObligationIDs
	}

	workingDir := strings.TrimSpace(s.config.WorkingDir)
	if workingDir == "" {
		workingDir = strings.TrimSpace(capsule.Sandbox.WorktreePath)
	}

	var (
		createdEvidence  []*schema.EvidenceArtifact
		warnings         []string
		blockingFailures []string
	)

	if strings.TrimSpace(patch.BaseCommit) == "" {
		warnings = append(warnings, "preflight: patch base commit is empty; clean-base check skipped")
	}
	for _, gate := range s.config.Gates {
		if strings.TrimSpace(gate.Command) == "" {
			blockingFailures = append(blockingFailures, fmt.Sprintf("preflight: verifier gate %q has empty command", gate.Name))
			continue
		}
		if err := s.commandChecker(gate.Command); err != nil {
			blockingFailures = append(
				blockingFailures,
				fmt.Sprintf("preflight: verifier gate %q command not found: %v", gate.Name, err),
			)
		}
	}

	scopeExitCode := 0
	scopeSummary := "scope check passed"
	if violations := findScopeViolations(patch.ChangedFiles, capsule.AllowedPaths, capsule.ForbiddenPaths); len(violations) > 0 {
		scopeExitCode = 1
		scopeSummary = "scope check failed: " + strings.Join(violations, ", ")
	}
	scopeEvidence, err := s.saveEvidence(ctx, schema.EvidenceDiffRiskReport, "scope check", scopeExitCode, scopeSummary, obligationRefs)
	if err != nil {
		return nil, err
	}
	createdEvidence = append(createdEvidence, scopeEvidence)

	testGateIndex := -1
	for i, gate := range s.config.Gates {
		if isTestGate(gate) {
			testGateIndex = i
			break
		}
	}

	for i, gate := range s.config.Gates {
		if i == testGateIndex {
			continue
		}
		exitCode, output, runErr := s.runner.Run(ctx, gate.Command, workingDir)
		if runErr != nil {
			return nil, fmt.Errorf("verifier: run static gate %q: %w", gate.Name, runErr)
		}
		evidenceType := staticEvidenceType(gate)
		evidence, err := s.saveEvidence(ctx, evidenceType, gate.Command, exitCode, summarizeOutput(output), obligationRefs)
		if err != nil {
			return nil, err
		}
		createdEvidence = append(createdEvidence, evidence)
		if gate.Blocking && exitCode != 0 {
			blockingFailures = append(blockingFailures, fmt.Sprintf("static gate %q failed", gate.Name))
		}
	}

	if testGateIndex >= 0 {
		testGate := s.config.Gates[testGateIndex]
		exitCode, output, runErr := s.runner.Run(ctx, testGate.Command, workingDir)
		if runErr != nil {
			return nil, fmt.Errorf("verifier: run test gate %q: %w", testGate.Name, runErr)
		}
		evidence, err := s.saveEvidence(ctx, schema.EvidenceTestResult, testGate.Command, exitCode, summarizeOutput(output), obligationRefs)
		if err != nil {
			return nil, err
		}
		createdEvidence = append(createdEvidence, evidence)
		if testGate.Blocking && exitCode != 0 {
			blockingFailures = append(blockingFailures, fmt.Sprintf("test gate %q failed", testGate.Name))
		}
	} else {
		warnings = append(warnings, "targeted tests stage: no test gate configured")
	}

	supplementalEvidenceByID, err := s.loadEvidenceByID(ctx, in.SupplementalEvidenceIDs)
	if err != nil {
		return nil, err
	}
	reviewFindings, err := s.reviewFindingsFromClaims(ctx, in.SupplementalClaimIDs)
	if err != nil {
		return nil, err
	}

	obligationResults := make([]schema.ObligationVerdict, 0, len(obligationRefs))
	for _, obligationID := range obligationRefs {
		obligation, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return nil, fmt.Errorf("verifier: load obligation %s: %w", obligationID, err)
		}
		verdict := schema.VerdictSatisfied
		note := "all required evidence checks passed"
		obligationEvidence, err := s.collectObligationEvidence(ctx, obligationID, createdEvidence, supplementalEvidenceByID)
		if err != nil {
			return nil, err
		}
		usedEvidenceIDs := make(map[string]bool, len(obligationEvidence))
		for _, required := range obligation.EvidenceRequired {
			relevant := evidenceForType(obligationEvidence, required)
			if len(relevant) == 0 {
				verdict = schema.VerdictFailed
				note = fmt.Sprintf("missing evidence type %s", required)
				continue
			}
			for _, evidence := range relevant {
				usedEvidenceIDs[evidence.EvidenceID] = true
			}
			if hasFailedEvidence(relevant) {
				verdict = schema.VerdictFailed
				note = fmt.Sprintf("evidence type %s contains failing result", required)
			}
		}
		evidenceIDs := mapKeys(usedEvidenceIDs)
		if obligation.Blocking && verdict == schema.VerdictFailed {
			blockingFailures = append(blockingFailures, obligation.ObligationID)
		}
		obligationResults = append(obligationResults, schema.ObligationVerdict{
			ObligationID: obligationID,
			Verdict:      verdict,
			EvidenceIDs:  evidenceIDs,
			Notes:        note,
		})
	}

	blockingFailures = uniqueStrings(blockingFailures)
	recommendedAction := schema.ActionAccept
	recommendationRationale := "all blocking verifier stages passed"
	if len(blockingFailures) > 0 {
		recommendedAction = schema.ActionRetry
		recommendationRationale = "one or more blocking checks failed"
	} else if reviewFindings.requiresHumanReview {
		recommendedAction = schema.ActionHumanReview
		recommendationRationale = reviewFindings.rationale()
	}
	warnings = append(warnings, reviewFindings.warnings...)

	result := &schema.VerifierResult{
		VerifierResultID:        idgen.New("VR"),
		PatchID:                 patch.PatchID,
		CapsuleID:               patch.CapsuleID,
		ObligationResults:       obligationResults,
		BlockingFailures:        blockingFailures,
		Warnings:                warnings,
		RecommendedAction:       recommendedAction,
		RecommendationRationale: recommendationRationale,
		CreatedAt:               time.Now().UTC(),
	}
	if err := s.store.SaveVerifierResult(ctx, result); err != nil {
		return nil, fmt.Errorf("verifier: save verifier result %s: %w", result.VerifierResultID, err)
	}
	return result, nil
}

type reviewFindings struct {
	warnings            []string
	requiresHumanReview bool
	claimIDs            []string
}

func (r reviewFindings) rationale() string {
	if len(r.claimIDs) == 0 {
		return "supplemental reviewer claims require human review"
	}
	return fmt.Sprintf("supplemental reviewer claims require human review: %s", strings.Join(r.claimIDs, ", "))
}

func (s *service) reviewFindingsFromClaims(ctx context.Context, claimIDs []string) (reviewFindings, error) {
	claimIDs = uniqueStrings(claimIDs)
	findings := reviewFindings{}
	for _, claimID := range claimIDs {
		claim, err := s.store.LoadClaim(ctx, claimID)
		if err != nil {
			return reviewFindings{}, fmt.Errorf("verifier: load supplemental claim %s: %w", claimID, err)
		}
		switch claim.ClaimType {
		case schema.ClaimRisk, schema.ClaimOpenQuestion, schema.ClaimTestGap:
			if claim.Status != schema.ClaimVerified {
				findings.requiresHumanReview = true
				findings.claimIDs = append(findings.claimIDs, claim.ClaimID)
			}
			findings.warnings = append(findings.warnings, fmt.Sprintf("review claim %s (%s) status=%s", claim.ClaimID, claim.ClaimType, claim.Status))
		case schema.ClaimAssumption:
			if claim.Status != schema.ClaimVerified {
				findings.requiresHumanReview = true
				findings.claimIDs = append(findings.claimIDs, claim.ClaimID)
			}
		}
	}
	findings.claimIDs = uniqueStrings(findings.claimIDs)
	findings.warnings = uniqueStrings(findings.warnings)
	return findings, nil
}

func (s *service) loadEvidenceByID(ctx context.Context, evidenceIDs []string) (map[string]*schema.EvidenceArtifact, error) {
	evidenceIDs = uniqueStrings(evidenceIDs)
	loaded := make(map[string]*schema.EvidenceArtifact, len(evidenceIDs))
	for _, evidenceID := range evidenceIDs {
		evidence, err := s.store.LoadEvidence(ctx, evidenceID)
		if err != nil {
			return nil, fmt.Errorf("verifier: load supplemental evidence %s: %w", evidenceID, err)
		}
		loaded[evidence.EvidenceID] = evidence
	}
	return loaded, nil
}

func (s *service) collectObligationEvidence(
	ctx context.Context,
	obligationID string,
	createdEvidence []*schema.EvidenceArtifact,
	supplementalEvidenceByID map[string]*schema.EvidenceArtifact,
) ([]*schema.EvidenceArtifact, error) {
	relevant := make(map[string]*schema.EvidenceArtifact)
	for _, evidence := range createdEvidence {
		if evidenceMatchesObligation(evidence, obligationID) {
			relevant[evidence.EvidenceID] = evidence
		}
	}
	storedEvidence, err := s.store.LoadEvidenceForObligation(ctx, obligationID)
	if err != nil {
		return nil, fmt.Errorf("verifier: load evidence for obligation %s: %w", obligationID, err)
	}
	for _, evidence := range storedEvidence {
		if evidenceMatchesObligation(evidence, obligationID) {
			relevant[evidence.EvidenceID] = evidence
		}
	}
	for _, evidence := range supplementalEvidenceByID {
		if evidenceMatchesObligation(evidence, obligationID) {
			relevant[evidence.EvidenceID] = evidence
		}
	}
	out := make([]*schema.EvidenceArtifact, 0, len(relevant))
	for _, evidence := range relevant {
		out = append(out, evidence)
	}
	return out, nil
}

func evidenceMatchesObligation(evidence *schema.EvidenceArtifact, obligationID string) bool {
	if evidence == nil {
		return false
	}
	return containsString(evidence.Supports, obligationID) || containsString(evidence.Weakens, obligationID)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type execGateRunner struct{}

func (execGateRunner) Run(ctx context.Context, command, workingDir string) (int, string, error) {
	executable, args, err := parseCommand(command)
	if err != nil {
		return -1, "", err
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	if strings.TrimSpace(workingDir) != "" {
		cmd.Dir = workingDir
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out), nil
	}
	return -1, string(out), fmt.Errorf("verifier: execute %q: %w", command, err)
}

func checkCommandPresent(command string) error {
	executable, _, err := parseCommand(command)
	if err != nil {
		return err
	}
	_, err = exec.LookPath(executable)
	return err
}

func parseCommand(command string) (string, []string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", nil, fmt.Errorf("verifier: command is required")
	}
	if trimmed[0] == '"' || trimmed[0] == '\'' {
		quote := trimmed[0]
		end := strings.IndexByte(trimmed[1:], quote)
		if end < 0 {
			return "", nil, fmt.Errorf("verifier: malformed command %q", command)
		}
		executable := trimmed[1 : end+1]
		rest := strings.TrimSpace(trimmed[end+2:])
		return executable, strings.Fields(rest), nil
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("verifier: command is required")
	}
	return parts[0], parts[1:], nil
}

func isTestGate(gate config.VerifierGate) bool {
	lower := strings.ToLower(gate.Name + " " + gate.Command)
	return strings.Contains(lower, "test")
}

func staticEvidenceType(gate config.VerifierGate) schema.EvidenceType {
	lower := strings.ToLower(gate.Name + " " + gate.Command)
	if strings.Contains(lower, "typecheck") || strings.Contains(lower, "go build") {
		return schema.EvidenceTypecheckResult
	}
	return schema.EvidenceLintResult
}

func summarizeOutput(output string) string {
	out := strings.TrimSpace(output)
	if len(out) <= 300 {
		return out
	}
	return out[:300]
}

func evidenceForType(all []*schema.EvidenceArtifact, evidenceType string) []*schema.EvidenceArtifact {
	out := make([]*schema.EvidenceArtifact, 0, len(all))
	for _, evidence := range all {
		if string(evidence.Type) == evidenceType {
			out = append(out, evidence)
		}
	}
	return out
}

func hasFailedEvidence(evidence []*schema.EvidenceArtifact) bool {
	for _, item := range evidence {
		if item.ExitCode != 0 {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s *service) saveEvidence(
	ctx context.Context,
	evidenceType schema.EvidenceType,
	command string,
	exitCode int,
	summary string,
	obligationRefs []string,
) (*schema.EvidenceArtifact, error) {
	supports := append([]string(nil), obligationRefs...)
	weakens := []string(nil)
	if exitCode != 0 {
		supports = nil
		weakens = append([]string(nil), obligationRefs...)
	}
	evidence := &schema.EvidenceArtifact{
		EvidenceID: idgen.New("EV"),
		Type:       evidenceType,
		Source:     "verifier",
		Command:    command,
		ExitCode:   exitCode,
		Summary:    summary,
		Supports:   supports,
		Weakens:    weakens,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.store.SaveEvidence(ctx, evidence); err != nil {
		return nil, fmt.Errorf("verifier: save evidence %s: %w", evidence.EvidenceID, err)
	}
	return evidence, nil
}

func findScopeViolations(changedFiles, allowedPaths, forbiddenPaths []string) []string {
	violations := make([]string, 0)
	for _, file := range changedFiles {
		file = filepath.Clean(file)
		if file == "." {
			continue
		}
		if inForbiddenPath(file, forbiddenPaths) {
			violations = append(violations, file+" matches forbidden path")
			continue
		}
		if len(allowedPaths) > 0 && !inAllowedPath(file, allowedPaths) {
			violations = append(violations, file+" is outside allowed paths")
		}
	}
	return violations
}

func inForbiddenPath(file string, forbiddenPaths []string) bool {
	for _, forbidden := range forbiddenPaths {
		forbidden = filepath.Clean(strings.TrimSpace(forbidden))
		if forbidden == "." || forbidden == "" {
			continue
		}
		if file == forbidden || strings.HasPrefix(file, forbidden+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func inAllowedPath(file string, allowedPaths []string) bool {
	for _, allowed := range allowedPaths {
		allowed = filepath.Clean(strings.TrimSpace(allowed))
		if allowed == "." || allowed == "" {
			continue
		}
		if file == allowed || strings.HasPrefix(file, allowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
