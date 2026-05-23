// Package verifier provides the Engine, which has two jobs: (1) propose initial
// obligations from a GoalIR, and (2) check whether evidence satisfies
// obligations for a given patch. orca.md §6 step 3, §10.
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
//	                  verifier-owned EvidenceArtifacts via SaveEvidence,
//	                  verifier-owned gate FailureFingerprints via SaveFailure,
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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/failurehistory"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// VerifyInput carries supplemental artifacts produced by reviewer/tester/
// investigator capsules in the same plan cycle so verification can incorporate
// peer review signal into recommendation confidence.
type VerifyInput struct {
	SupplementalEvidenceIDs []string
	SupplementalClaimIDs    []string
}

// GateRunner abstracts subprocess execution for verifier gates.
type GateRunner interface {
	Run(ctx context.Context, command, workingDir string) (exitCode int, output string, err error)
}

// Engine implements the two verifier jobs: propose obligations and verify patch evidence.
type Engine struct {
	store          *store.FileStore
	config         config.VerifierConfig
	noLearning     bool
	advanced       config.AdvancedConfig
	runner         GateRunner
	commandChecker func(string) error
}

// Config defines verifier-owned options that are not part of the repo config
// file contract.
type Config struct {
	Gates      []config.VerifierGate
	WorkingDir string
	NoLearning bool
	Advanced   config.AdvancedConfig
}

// New returns the default Engine implementation.
func New(st *store.FileStore, cfg config.VerifierConfig, runner GateRunner) *Engine {
	return NewWithConfig(st, Config{Gates: cfg.Gates, WorkingDir: cfg.WorkingDir}, runner)
}

// NewWithConfig returns an Engine with verifier-local options.
func NewWithConfig(st *store.FileStore, cfg Config, runner GateRunner) *Engine {
	if runner == nil {
		runner = execGateRunner{}
	} else if v := reflect.ValueOf(runner); v.Kind() == reflect.Pointer && v.IsNil() {
		runner = execGateRunner{}
	}
	return &Engine{
		store:          st,
		config:         config.VerifierConfig{Gates: cfg.Gates, WorkingDir: cfg.WorkingDir},
		noLearning:     cfg.NoLearning,
		advanced:       cfg.Advanced,
		runner:         runner,
		commandChecker: checkCommandPresent,
	}
}

func (s *Engine) ProposeObligations(ctx context.Context, goalID string) ([]string, error) {
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
				Blocking:         true,
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

func (s *Engine) Verify(ctx context.Context, patchID string, in VerifyInput) (*schema.VerifierResult, error) {
	if s.store == nil {
		return nil, fmt.Errorf("verifier: store is required")
	}
	if s.runner == nil {
		return nil, fmt.Errorf("verifier: gate runner is required")
	}
	if strings.TrimSpace(patchID) == "" {
		return nil, fmt.Errorf("verifier: patch ID is required")
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
	goalID, err := s.goalIDForObligations(ctx, obligationRefs)
	if err != nil {
		return nil, err
	}
	latestSnapshotID, err := s.latestSnapshotID(ctx, goalID)
	if err != nil {
		return nil, err
	}

	var (
		createdEvidence  []*schema.EvidenceArtifact
		warnings         []string
		blockingFailures []string
		failureIDs       []string
	)

	if strings.TrimSpace(patch.BaseCommit) == "" {
		warnings = append(warnings, "preflight: patch base commit is empty; clean-base check skipped")
	}

	scopeExitCode := 0
	scopeSummary := "scope check passed"
	if violations := findScopeViolations(patch.ChangedFiles, capsule.AllowedPaths, capsule.ForbiddenPaths); len(violations) > 0 {
		scopeExitCode = 1
		scopeSummary = "scope check failed: " + strings.Join(violations, ", ")
	}
	scopeEvidence, err := s.saveEvidence(ctx, schema.EvidenceDiffRiskReport, "scope check", scopeExitCode, scopeSummary, obligationRefs, "", "", "")
	if err != nil {
		return nil, err
	}
	createdEvidence = append(createdEvidence, scopeEvidence)
	if scopeExitCode != 0 {
		failureID, err := s.saveGateFailure(ctx, goalID, capsule.CapsuleID, schema.FailurePolicy, "scope check", scopeSummary, patch.ChangedFiles)
		if err != nil {
			return nil, err
		}
		failureIDs = append(failureIDs, failureID)
	}

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
		evidenceType := staticEvidenceType(gate)
		evidence, exitCode, err := s.runOrReuseGate(ctx, goalID, latestSnapshotID, gate, evidenceType, workingDir, obligationRefs)
		if err != nil {
			return nil, fmt.Errorf("verifier: patch %s capsule %s: %w", patchID, capsule.CapsuleID, err)
		}
		createdEvidence = append(createdEvidence, evidence...)
		if gate.Blocking && exitCode != 0 {
			summary := fmt.Sprintf("static gate %q failed", gate.Name)
			blockingFailures = append(blockingFailures, summary)
			failureID, err := s.saveGateFailure(ctx, goalID, capsule.CapsuleID, failureTypeForEvidence(evidenceType), gate.Command, summary, patch.ChangedFiles)
			if err != nil {
				return nil, err
			}
			failureIDs = append(failureIDs, failureID)
		}
	}

	if testGateIndex >= 0 {
		testGate := s.config.Gates[testGateIndex]
		evidence, exitCode, err := s.runOrReuseGate(ctx, goalID, latestSnapshotID, testGate, schema.EvidenceTestResult, workingDir, obligationRefs)
		if err != nil {
			return nil, fmt.Errorf("verifier: patch %s capsule %s: %w", patchID, capsule.CapsuleID, err)
		}
		createdEvidence = append(createdEvidence, evidence...)
		if testGate.Blocking && exitCode != 0 {
			summary := fmt.Sprintf("test gate %q failed", testGate.Name)
			blockingFailures = append(blockingFailures, summary)
			failureID, err := s.saveGateFailure(ctx, goalID, capsule.CapsuleID, schema.FailureTest, testGate.Command, summary, patch.ChangedFiles)
			if err != nil {
				return nil, err
			}
			failureIDs = append(failureIDs, failureID)
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

	// allEvidenceByID collects every evidence object visible to MAVEN probes:
	// verifier-created gate evidence, supplemental evidence from the runner, and
	// any evidence loaded from the store during the obligation results loop. The
	// logical probe must see all evidence referenced in verdicts, including
	// store-sourced evidence that pre-dates this Verify call.
	allEvidenceByID := make(map[string]*schema.EvidenceArtifact, len(createdEvidence)+len(supplementalEvidenceByID))
	for _, ev := range createdEvidence {
		if ev != nil {
			allEvidenceByID[ev.EvidenceID] = ev
		}
	}
	for id, ev := range supplementalEvidenceByID {
		allEvidenceByID[id] = ev
	}

	obligationResults := make([]schema.ObligationVerdict, 0, len(obligationRefs))
	obligations := make([]*schema.Obligation, 0, len(obligationRefs))
	for _, obligationID := range obligationRefs {
		obligation, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return nil, fmt.Errorf("verifier: load obligation %s: %w", obligationID, err)
		}
		obligations = append(obligations, obligation)
		verdict := schema.VerdictSatisfied
		var failureNotes []string
		obligationEvidence, err := s.collectObligationEvidence(ctx, obligationID, createdEvidence, supplementalEvidenceByID)
		if err != nil {
			return nil, err
		}
		for _, ev := range obligationEvidence {
			if ev != nil {
				allEvidenceByID[ev.EvidenceID] = ev
			}
		}
		usedEvidenceIDs := make(map[string]bool, len(obligationEvidence))
		for _, required := range obligation.EvidenceRequired {
			relevant := evidenceForType(obligationEvidence, required)
			if len(relevant) == 0 {
				verdict = schema.VerdictFailed
				failureNotes = append(failureNotes, fmt.Sprintf("missing evidence type %s", required))
				continue
			}
			for _, evidence := range relevant {
				usedEvidenceIDs[evidence.EvidenceID] = true
			}
			if hasFailedEvidence(relevant) {
				verdict = schema.VerdictFailed
				failureNotes = append(failureNotes, fmt.Sprintf("evidence type %s contains failing result", required))
			}
		}
		note := "all required evidence checks passed"
		if len(failureNotes) > 0 {
			note = strings.Join(failureNotes, "; ")
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

	var mavenRequiresHumanReview bool
	if s.advanced.Enabled && s.advanced.Maven {
		maven := s.runMAVEN(
			patch,
			obligations,
			obligationResults,
			allEvidenceByID,
			reviewFindings.claims,
		)
		warnings = append(warnings, maven.warnings...)
		if maven.requiresHumanReview {
			mavenRequiresHumanReview = true
		}
	}

	mutationWarnings, mutationBlocking, err := s.runMutationGate(ctx, goalID, capsule.CapsuleID, workingDir, obligationRefs, patch.ChangedFiles)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, mutationWarnings...)

	adversarialWarnings, adversarialBlocking, err := s.runAdversarialGate(ctx, goalID, capsule.CapsuleID, workingDir, obligationRefs, patch.ChangedFiles)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, adversarialWarnings...)

	blockingFailures = uniqueStrings(blockingFailures)
	recommendedAction := schema.ActionAccept
	recommendationRationale := "all blocking verifier stages passed"
	if len(blockingFailures) > 0 {
		recommendedAction = schema.ActionRetry
		recommendationRationale = "one or more blocking checks failed"
	} else if mutationBlocking {
		recommendedAction = schema.ActionRetry
		recommendationRationale = "[mutation] blocking survivors found"
	} else if adversarialBlocking {
		recommendedAction = schema.ActionHumanReview
		recommendationRationale = "[adversarial] blocking challenge failure"
	} else if reviewFindings.requiresHumanReview || mavenRequiresHumanReview {
		recommendedAction = schema.ActionHumanReview
		if mavenRequiresHumanReview && reviewFindings.requiresHumanReview {
			recommendationRationale = "[maven] findings require human review; " + reviewFindings.rationale()
		} else if mavenRequiresHumanReview {
			recommendationRationale = "[maven] findings require human review"
		} else {
			recommendationRationale = reviewFindings.rationale()
		}
	}
	warnings = append(warnings, reviewFindings.warnings...)

	result := &schema.VerifierResult{
		VerifierResultID:        idgen.New("VR"),
		PatchID:                 patch.PatchID,
		CapsuleID:               patch.CapsuleID,
		ObligationResults:       obligationResults,
		BlockingFailures:        blockingFailures,
		FailureIDs:              uniqueStrings(failureIDs),
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

func (s *Engine) runMutationGate(
	ctx context.Context,
	goalID, capsuleID string,
	workingDir string,
	obligationRefs []string,
	changedFiles []string,
) ([]string, bool, error) {
	if !s.advanced.Enabled || !s.advanced.Mutation {
		return nil, false, nil
	}
	command := strings.TrimSpace(s.advanced.MutationCommand)
	if command == "" {
		return []string{"mutation gate skipped: mutation_command not configured"}, false, nil
	}

	timeout := 120 * time.Second
	if s.advanced.MutationTimeoutSeconds > 0 {
		timeout = time.Duration(s.advanced.MutationTimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, output, err := s.runner.Run(runCtx, command, workingDir)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		return nil, false, fmt.Errorf("verifier: mutation gate goal %s capsule %s: %w", goalID, capsuleID, err)
	}
	if timedOut {
		exitCode = -1
	}

	summary := "mutation testing passed: no survivors found"
	warnings := []string(nil)
	blocking := false
	if timedOut {
		summary = "mutation testing timed out"
		warnings = append(warnings, "[mutation] mutation gate timed out")
	} else if exitCode != 0 {
		summary = fmt.Sprintf("mutation testing found survivors (exit_code=%d): %s", exitCode, summarizeAdvancedOutput(output, 200))
		warnings = append(warnings, fmt.Sprintf("[mutation] survivor found: test gap candidate for %s", advancedGateFiles(changedFiles)))
		blocking = s.advanced.MutationBlocking
	} else {
		warnings = append(warnings, "[mutation] gate passed: no survivors found")
	}

	if _, err := s.saveAdvancedEvidence(ctx, schema.EvidenceMutationResult, "verifier", command, exitCode, summary, output, obligationRefs); err != nil {
		return nil, false, err
	}
	return warnings, blocking, nil
}

func (s *Engine) runAdversarialGate(
	ctx context.Context,
	goalID, capsuleID string,
	workingDir string,
	obligationRefs []string,
	changedFiles []string,
) ([]string, bool, error) {
	if !s.advanced.Enabled || !s.advanced.AdversarialTests {
		return nil, false, nil
	}
	command := strings.TrimSpace(s.advanced.AdversarialCommand)
	if command == "" {
		return []string{"adversarial gate skipped: adversarial_command not configured"}, false, nil
	}

	timeout := 60 * time.Second
	if s.advanced.AdversarialTimeoutSeconds > 0 {
		timeout = time.Duration(s.advanced.AdversarialTimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, output, err := s.runner.Run(runCtx, command, workingDir)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		return nil, false, fmt.Errorf("verifier: adversarial gate goal %s capsule %s: %w", goalID, capsuleID, err)
	}
	if timedOut {
		exitCode = -1
	}

	summary := "adversarial gate passed: no challenge failures"
	warnings := []string(nil)
	blocking := false
	if timedOut {
		summary = "adversarial gate timed out"
		warnings = append(warnings, "[adversarial] gate timed out")
	} else if exitCode != 0 {
		summary = fmt.Sprintf("adversarial challenge failed (exit_code=%d): %s", exitCode, summarizeAdvancedOutput(output, 200))
		warnings = append(warnings, "[adversarial] challenge failed: test gap candidate")
		blocking = s.advanced.AdversarialBlocking
	} else {
		warnings = append(warnings, "[adversarial] gate passed: no challenge failures")
	}

	if _, err := s.saveAdvancedEvidence(ctx, schema.EvidenceTestResult, "adversarial gate", command, exitCode, summary, output, obligationRefs); err != nil {
		return nil, false, err
	}
	_ = changedFiles
	return warnings, blocking, nil
}

type reviewFindings struct {
	warnings            []string
	requiresHumanReview bool
	claimIDs            []string
	claims              []*schema.ClaimArtifact
}

func (r reviewFindings) rationale() string {
	if len(r.claimIDs) == 0 {
		return "supplemental reviewer claims require human review"
	}
	return fmt.Sprintf("supplemental reviewer claims require human review: %s", strings.Join(r.claimIDs, ", "))
}

func (s *Engine) reviewFindingsFromClaims(ctx context.Context, claimIDs []string) (reviewFindings, error) {
	claimIDs = uniqueStrings(claimIDs)
	findings := reviewFindings{}
	for _, claimID := range claimIDs {
		claim, err := s.store.LoadClaim(ctx, claimID)
		if err != nil {
			return reviewFindings{}, fmt.Errorf("verifier: load supplemental claim %s: %w", claimID, err)
		}
		findings.claims = append(findings.claims, claim)
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

type mavenFindings struct {
	warnings            []string
	requiresHumanReview bool
}

func (s *Engine) runMAVEN(
	patch *schema.PatchArtifact,
	obligations []*schema.Obligation,
	obligationResults []schema.ObligationVerdict,
	allEvidenceByID map[string]*schema.EvidenceArtifact,
	supplementalClaims []*schema.ClaimArtifact,
) mavenFindings {
	findings := mavenFindings{}
	allEvidence := make([]*schema.EvidenceArtifact, 0, len(allEvidenceByID))
	for _, ev := range allEvidenceByID {
		if ev != nil {
			allEvidence = append(allEvidence, ev)
		}
	}

	expectedFilesByObligation := make(map[string][]string, len(obligations))
	for _, obligation := range obligations {
		if obligation == nil {
			continue
		}
		expectedFilesByObligation[obligation.ObligationID] = append([]string(nil), obligation.ExpectedFiles...)
		for _, required := range obligation.EvidenceRequired {
			if !hasEvidenceForObligationType(allEvidence, obligation.ObligationID, required) {
				findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] factual: obligation %s missing evidence type %s", obligation.ObligationID, required))
				findings.requiresHumanReview = true
			}
		}
	}

	for _, result := range obligationResults {
		if result.Verdict != schema.VerdictSatisfied {
			continue
		}
		for _, evidenceID := range result.EvidenceIDs {
			evidence := allEvidenceByID[evidenceID]
			if evidence != nil && evidence.ExitCode != 0 {
				findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] logical: obligation %s verdict=satisfied but evidence exit_code != 0", result.ObligationID))
				findings.requiresHumanReview = true
				break
			}
		}
	}

	outOfScope := patchFilesOutsideMAVENScope(patch.ChangedFiles, expectedFilesByObligation, allEvidence)
	if len(outOfScope) > 0 {
		findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] causal: patch changed files outside obligation scope: %s", strings.Join(outOfScope, ", ")))
	}

	for _, claim := range supplementalClaims {
		if claim == nil || claim.Status == schema.ClaimVerified {
			continue
		}
		switch claim.ClaimType {
		case schema.ClaimAssumption, schema.ClaimRisk, schema.ClaimOpenQuestion, schema.ClaimTestGap:
			findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] assumption: unverified %s claim %s is still relevant", claim.ClaimType, claim.ClaimID))
			findings.requiresHumanReview = true
		}
	}

	findings.warnings = uniqueStrings(findings.warnings)
	return findings
}

func hasEvidenceForObligationType(evidence []*schema.EvidenceArtifact, obligationID string, evidenceType string) bool {
	for _, item := range evidence {
		if item == nil || string(item.Type) != evidenceType {
			continue
		}
		if evidenceMatchesObligation(item, obligationID) {
			return true
		}
	}
	return false
}

func patchFilesOutsideMAVENScope(
	changedFiles []string,
	expectedFilesByObligation map[string][]string,
	evidence []*schema.EvidenceArtifact,
) []string {
	scopedFiles := make(map[string]bool)
	for _, files := range expectedFilesByObligation {
		addNormalizedFiles(scopedFiles, files)
	}
	for _, item := range evidence {
		if item == nil {
			continue
		}
		for _, obligationID := range item.Supports {
			addNormalizedFiles(scopedFiles, expectedFilesByObligation[obligationID])
		}
	}
	var out []string
	for _, file := range changedFiles {
		normalized := normalizeMAVENPath(file)
		if normalized == "" {
			continue
		}
		if !scopedFiles[normalized] {
			out = append(out, normalized)
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func addNormalizedFiles(dst map[string]bool, files []string) {
	for _, file := range files {
		normalized := normalizeMAVENPath(file)
		if normalized != "" {
			dst[normalized] = true
		}
	}
}

func normalizeMAVENPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	return strings.ReplaceAll(path, "\\", "/")
}

func (s *Engine) loadEvidenceByID(ctx context.Context, evidenceIDs []string) (map[string]*schema.EvidenceArtifact, error) {
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

func (s *Engine) collectObligationEvidence(
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

func (s *Engine) saveGateFailure(
	ctx context.Context,
	goalID string,
	capsuleID string,
	failureType schema.FailureType,
	command string,
	summary string,
	changedFiles []string,
) (string, error) {
	failure := &schema.FailureFingerprint{
		FailureID:       idgen.New("FAIL"),
		SourceCapsuleID: capsuleID,
		FailureType:     failureType,
		Summary:         summary,
		AffectedFiles:   append([]string(nil), changedFiles...),
		ErrorSignature:  command + "\n" + summary,
	}
	if err := failurehistory.Prepare(ctx, s.store, goalID, failure, s.noLearning); err != nil {
		return "", fmt.Errorf("verifier: prepare gate failure history for capsule %s: %w", capsuleID, err)
	}
	if err := s.store.SaveFailure(ctx, failure); err != nil {
		return "", fmt.Errorf("verifier: save gate failure %s: %w", failure.FailureID, err)
	}
	return failure.FailureID, nil
}

func failureTypeForEvidence(evidenceType schema.EvidenceType) schema.FailureType {
	switch evidenceType {
	case schema.EvidenceTestResult:
		return schema.FailureTest
	case schema.EvidenceTypecheckResult:
		return schema.FailureTypecheck
	case schema.EvidenceLintResult:
		return schema.FailureLint
	default:
		return schema.FailurePolicy
	}
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

func (s *Engine) saveEvidence(
	ctx context.Context,
	evidenceType schema.EvidenceType,
	command string,
	exitCode int,
	summary string,
	obligationRefs []string,
	reuseKey string,
	validatedAgainst string,
	contentHash string,
) (*schema.EvidenceArtifact, error) {
	supports := append([]string(nil), obligationRefs...)
	weakens := []string(nil)
	if exitCode != 0 {
		supports = nil
		weakens = append([]string(nil), obligationRefs...)
	}
	evidence := &schema.EvidenceArtifact{
		EvidenceID:       idgen.New("EV"),
		Type:             evidenceType,
		Source:           "verifier",
		Command:          command,
		ExitCode:         exitCode,
		Summary:          summary,
		Supports:         supports,
		Weakens:          weakens,
		ContentHash:      contentHash,
		ReuseKey:         reuseKey,
		ValidatedAgainst: validatedAgainst,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.store.SaveEvidence(ctx, evidence); err != nil {
		return nil, fmt.Errorf("verifier: save evidence %s: %w", evidence.EvidenceID, err)
	}
	return evidence, nil
}

func (s *Engine) saveAdvancedEvidence(
	ctx context.Context,
	evidenceType schema.EvidenceType,
	source string,
	command string,
	exitCode int,
	summary string,
	output string,
	obligationRefs []string,
) (*schema.EvidenceArtifact, error) {
	supports := append([]string(nil), obligationRefs...)
	weakens := []string(nil)
	if exitCode != 0 {
		supports = nil
		weakens = append([]string(nil), obligationRefs...)
	}
	evidence := &schema.EvidenceArtifact{
		EvidenceID:   idgen.New("EV"),
		Type:         evidenceType,
		Source:       source,
		Command:      command,
		ExitCode:     exitCode,
		Summary:      summary,
		InlineOutput: summarizeAdvancedOutput(output, 300),
		Supports:     supports,
		Weakens:      weakens,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.store.SaveEvidence(ctx, evidence); err != nil {
		return nil, fmt.Errorf("verifier: save evidence %s: %w", evidence.EvidenceID, err)
	}
	return evidence, nil
}

func (s *Engine) runOrReuseGate(
	ctx context.Context,
	goalID string,
	snapshotID string,
	gate config.VerifierGate,
	evidenceType schema.EvidenceType,
	workingDir string,
	obligationRefs []string,
) ([]*schema.EvidenceArtifact, int, error) {
	command := strings.TrimSpace(gate.Command)
	if command == "" {
		return nil, 1, nil
	}
	reuseKey := verifierReuseKey(evidenceType, command, workingDir, obligationRefs, snapshotID)
	if snapshotID != "" && !s.noLearning {
		reused, err := s.reuseGateEvidence(ctx, evidenceType, command, obligationRefs, reuseKey, snapshotID)
		if err != nil {
			return nil, 0, err
		}
		if len(reused) > 0 {
			reuseExitCode := 0
			for _, ev := range reused {
				if ev.ExitCode != 0 {
					reuseExitCode = ev.ExitCode
					break
				}
			}
			return reused, reuseExitCode, nil
		}
	}
	if err := s.commandChecker(command); err != nil {
		warnSummary := fmt.Sprintf("command not found for gate %q: %v", gate.Name, err)
		evidence, saveErr := s.saveEvidence(ctx, evidenceType, command, 1, warnSummary, obligationRefs, "", "", "")
		if saveErr != nil {
			return nil, 1, saveErr
		}
		return []*schema.EvidenceArtifact{evidence}, 1, nil
	}
	exitCode, output, runErr := s.runner.Run(ctx, command, workingDir)
	if runErr != nil {
		return nil, 0, fmt.Errorf("verifier: run gate %q goal %s: %w", gate.Name, goalID, runErr)
	}
	contentHash := evidenceContentHash(evidenceType, command, workingDir, exitCode, output, obligationRefs, goalID, snapshotID)
	evidence, err := s.saveEvidence(ctx, evidenceType, command, exitCode, summarizeOutput(output), obligationRefs, reuseKey, snapshotID, contentHash)
	if err != nil {
		return nil, 0, err
	}
	return []*schema.EvidenceArtifact{evidence}, exitCode, nil
}

func (s *Engine) reuseGateEvidence(
	ctx context.Context,
	evidenceType schema.EvidenceType,
	command string,
	obligationRefs []string,
	reuseKey string,
	snapshotID string,
) ([]*schema.EvidenceArtifact, error) {
	matches := make([]*schema.EvidenceArtifact, 0, len(obligationRefs))
	for _, obligationID := range obligationRefs {
		match, err := s.store.LoadReusableEvidenceForObligation(ctx, obligationID, evidenceType, reuseKey, snapshotID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("verifier: load reusable evidence for obligation %s: %w", obligationID, err)
		}
		matches = append(matches, match)
	}
	reused := make([]*schema.EvidenceArtifact, 0, len(matches))
	for _, match := range matches {
		evidence := &schema.EvidenceArtifact{
			EvidenceID:       idgen.New("EV"),
			Type:             match.Type,
			Source:           "verifier",
			Command:          command,
			ExitCode:         match.ExitCode,
			Summary:          match.Summary,
			RawLogPath:       match.RawLogPath,
			InlineOutput:     match.InlineOutput,
			Supports:         append([]string(nil), match.Supports...),
			Weakens:          append([]string(nil), match.Weakens...),
			ContentHash:      match.ContentHash,
			ReuseKey:         reuseKey,
			ValidatedAgainst: snapshotID,
			ReusedFromID:     match.EvidenceID,
			CreatedAt:        time.Now().UTC(),
		}
		if err := s.store.SaveEvidence(ctx, evidence); err != nil {
			return nil, fmt.Errorf("verifier: save reused evidence %s: %w", evidence.EvidenceID, err)
		}
		reused = append(reused, evidence)
	}
	return reused, nil
}

func (s *Engine) goalIDForObligations(ctx context.Context, obligationRefs []string) (string, error) {
	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return "", fmt.Errorf("verifier: load active goal for snapshot scope: %w", err)
	}
	if goal == nil {
		return "", fmt.Errorf("verifier: no active goal found")
	}
	conditionIDs := make(map[string]bool, len(goal.GoalConditions))
	for _, condition := range goal.GoalConditions {
		conditionIDs[condition.ID] = true
	}
	for _, obligationID := range obligationRefs {
		obligation, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return "", fmt.Errorf("verifier: load obligation %s for snapshot scope: %w", obligationID, err)
		}
		if !conditionIDs[obligation.GoalConditionID] {
			return "", fmt.Errorf("verifier: obligation %s is outside active goal %s", obligationID, goal.GoalID)
		}
	}
	return goal.GoalID, nil
}

func (s *Engine) latestSnapshotID(ctx context.Context, goalID string) (string, error) {
	snapshot, err := s.store.LoadLatestSnapshot(ctx, goalID)
	if err == nil {
		return snapshot.SnapshotID, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("verifier: load latest snapshot for goal %s: %w", goalID, err)
}

func verifierReuseKey(evidenceType schema.EvidenceType, command, workingDir string, obligationRefs []string, snapshotID string) string {
	normalizedObligations := append([]string(nil), obligationRefs...)
	sort.Strings(normalizedObligations)
	parts := []string{
		"type=" + string(evidenceType),
		"command=" + commandIdentity(command),
		"scope=" + normalizedWorkingDir(workingDir),
		"obligations=" + strings.Join(normalizedObligations, ","),
		"snapshot=" + strings.TrimSpace(snapshotID),
	}
	return strings.Join(parts, "|")
}

func commandIdentity(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

func normalizedWorkingDir(workingDir string) string {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		workingDir = "."
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = abs
	}
	workingDir = filepath.Clean(workingDir)
	workingDir = strings.ReplaceAll(workingDir, "\\", "/")
	if len(workingDir) >= 2 && workingDir[1] == ':' {
		workingDir = strings.ToLower(workingDir[:1]) + workingDir[1:]
	}
	return workingDir
}

func evidenceContentHash(
	evidenceType schema.EvidenceType,
	command string,
	workingDir string,
	exitCode int,
	output string,
	obligationRefs []string,
	goalID string,
	snapshotID string,
) string {
	obligations := append([]string(nil), obligationRefs...)
	sort.Strings(obligations)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(evidenceType),
		commandIdentity(command),
		normalizedWorkingDir(workingDir),
		fmt.Sprintf("exit=%d", exitCode),
		output,
		strings.Join(obligations, ","),
		goalID,
		snapshotID,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
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

func summarizeAdvancedOutput(output string, limit int) string {
	out := strings.TrimSpace(output)
	if limit <= 0 || len(out) <= limit {
		return out
	}
	return out[:limit]
}

func advancedGateFiles(changedFiles []string) string {
	files := uniqueStrings(changedFiles)
	if len(files) == 0 {
		return "changed files"
	}
	return strings.Join(files, ", ")
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
