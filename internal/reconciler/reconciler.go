// Package reconciler provides the Reconciler, which maps evidence to
// obligations, accepts or rejects patches, and creates follow-up obligations.
//
// Reconciliation happens after every capsule completion, verifier failure, and
// merge. It is the component that advances obligation status, updates budget
// records, and determines merge readiness. orca.md §11.
//
// Dependency contract:
//
//	Reads  (store):   VerifierResult via LoadVerifierResultForPatch,
//	                  PatchArtifact via LoadPatch,
//	                  Obligations via LoadObligation (one per ObligationVerdict),
//	                  EvidenceArtifacts via LoadEvidence,
//	                  FailureFingerprints via LoadFailuresForCapsule
//	                    (to build follow-up obligations from runner failures),
//	                  ClaimArtifacts via LoadClaimsForCapsule
//	                    (to perform claim verification on patch acceptance),
//	                  BudgetRecords via LoadBudgetForGoal
//	Writes (store):   Obligation status via UpdateObligationStatus,
//	                  Patch status via UpdatePatchStatus,
//	                  Claim status via UpdateClaimStatus (proposed → verified for
//	                    claims whose evidence_ids all resolve to store artifacts),
//	                  ClaimArtifacts via SaveClaim for verifier advanced-check
//	                    test-gap findings only,
//	                  new Obligations via SaveObligation (follow-ups from failures),
//	                  DecisionRecords via SaveDecision,
//	                  BudgetRecords via SaveBudgetRecord (on first reconcile for
//	                    a capsuleID, when LoadBudgetForGoal returns no record
//	                    for that capsuleID) or UpdateBudgetRecord (when a record
//	                    already exists); the reconciler must check
//	                    LoadBudgetForGoal before deciding which to call,
//	                  StateSnapshot via SaveSnapshot
//	Writes (log):     EventObligationStatusUpdated before UpdateObligationStatus,
//	                  EventPatchAccepted or EventPatchRejected before UpdatePatchStatus,
//	                  EventClaimStatusUpdated before UpdateClaimStatus,
//	                  EventMergeApplied. Follow-up obligations, decisions,
//	                  budgets, and snapshots are saved through the store, which
//	                  emits their creation/update events.
//
//	Must NOT import:  internal/runner, internal/verifier, internal/projector,
//	                  internal/budget, internal/gate
//	Must NOT call:    store.SaveGoal, store.UpdateGoalStatus,
//	                  store.SaveCapsule, store.UpdateCapsuleState,
//	                  store.SaveEvidence,
//	                  store.SaveVerifierResult,
//	                  store.SaveProjection, store.SaveHumanSummaryProjection,
//	                  store.SaveFailure
//	Must NOT run:     verifier stages or agent commands
//	Must NOT accept:  a patch without mapping evidence to every blocking obligation
package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/failurehistory"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// Config configures the Reconciler.
type Config struct {
	// NoLearning disables topology outcome recording. When true, Reconcile does
	// not write TopologyOutcomeRecords to the store. orca.md §13.
	NoLearning bool
}

// Reconciler processes the verifier result for a completed patch, advances
// obligation state, decides patch acceptance, creates follow-up obligations
// from failures, and records all decisions.
//
// Merge policy (orca.md §11):
//   - No merge while any blocking obligation is open
//   - High-risk obligations require human approval before merge
//   - Scope violations require human approval or capsule retry
//   - Failed static gates block merge unless explicitly waived
//   - Unverified claims may not justify merge
type Reconciler struct {
	store      *store.FileStore
	log        *eventlog.FileLog
	noLearning bool
}

// New returns a Reconciler.
func New(st *store.FileStore, log *eventlog.FileLog, cfg Config) *Reconciler {
	return &Reconciler{store: st, log: log, noLearning: cfg.NoLearning}
}

// ReconcileInput carries optional per-call options for Reconcile.
// Callers that do not need waivers can omit it entirely.
type ReconcileInput struct {
	// Waivers maps obligation ID to the gate decision ID that pre-approved
	// the waiver. When present for a blocking obligation whose verifier verdict
	// is failed, Reconcile treats the verdict as waived (VerdictWaived) with
	// WaivedBy set to the decision ID.
	Waivers map[string]string
}

// ReconcileResult summarizes the reconciler's decision for one patch.
type ReconcileResult struct {
	// PatchAccepted is true when all blocking obligations are satisfied or waived.
	PatchAccepted bool

	// MergeReady is true when PatchAccepted is true and the reconciler's merge
	// policy is satisfied: no open blocking obligations, static gates passed,
	// scope within contract.
	MergeReady bool

	// HumanGateRequired is true when MergeReady is true but the merge policy
	// requires an additional human approval before merge — e.g. high-risk
	// obligations, scope violations, or diffs above the configured size threshold.
	// The orchestrator must call gate.ReviewMerge before proceeding. orca.md §11.
	HumanGateRequired bool

	// FollowUpObligationIDs contains IDs of new Obligations created from blocking
	// failures. Non-empty only when PatchAccepted is false.
	FollowUpObligationIDs []string

	// DecisionID is the ID of the persisted DecisionRecord for this reconciliation.
	DecisionID string

	// BlockingReason is a human-readable explanation when PatchAccepted is false
	// or MergeReady is false. Implementors must populate this field whenever
	// PatchAccepted is false (evidence-bundle or obligation failure) and whenever
	// MergeReady is false (open obligations, gate failure, scope violation, etc.).
	BlockingReason string
}

func (s *Reconciler) Reconcile(ctx context.Context, patchID string, opts ...ReconcileInput) (ReconcileResult, error) {
	var in ReconcileInput
	if len(opts) > 0 {
		in = opts[0]
	}

	if s.store == nil {
		return ReconcileResult{}, errors.New("reconciler: store is required")
	}
	if s.log == nil {
		return ReconcileResult{}, errors.New("reconciler: event log is required")
	}

	vr, err := s.store.LoadVerifierResultForPatch(ctx, patchID)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: load verifier result for patch %s: %w", patchID, err)
	}

	// Apply pre-approved gate waivers: replace VerdictFailed with VerdictWaived
	// in an in-memory copy so the stored VerifierResult is never mutated.
	if len(in.Waivers) > 0 {
		vr = applyWaivers(vr, in.Waivers)
	}

	patch, err := s.store.LoadPatch(ctx, patchID)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: load patch %s: %w", patchID, err)
	}

	goal, err := s.store.LoadActiveGoal(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: load active goal: %w", err)
	}
	if goal == nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: active goal: %w", store.ErrNotFound)
	}

	now := time.Now().UTC()
	result := ReconcileResult{PatchAccepted: true}
	recommendationRequiresHumanReview := false
	switch vr.RecommendedAction {
	case schema.ActionReject, schema.ActionRetry, schema.ActionSplit:
		reject(&result, recommendationBlockingReason(vr))
	case schema.ActionHumanReview:
		recommendationRequiresHumanReview = true
	}
	// BlockingFailures must independently block acceptance regardless of
	// RecommendedAction. A malformed or stale VerifierResult could carry
	// RecommendedAction=accept while still listing blocking failures.
	if len(vr.BlockingFailures) > 0 {
		reject(&result, fmt.Sprintf("verifier reported %d blocking failure(s): %s",
			len(vr.BlockingFailures), strings.Join(vr.BlockingFailures, "; ")))
	}
	loadedObligations := make([]*schema.Obligation, 0, len(vr.ObligationResults))
	updatedStatuses := make(map[string]schema.ObligationStatus, len(vr.ObligationResults))
	satisfiedBy := make(map[string][]string, len(vr.ObligationResults))
	verdictsByObligation := make(map[string]bool, len(vr.ObligationResults))
	highRisk := false

	if len(vr.ObligationResults) == 0 {
		reject(&result, fmt.Sprintf("patch %s has no obligation verdicts", patch.PatchID))
	}

	for _, verdict := range vr.ObligationResults {
		verdictsByObligation[verdict.ObligationID] = true
		obl, err := s.store.LoadObligation(ctx, verdict.ObligationID)
		if err != nil {
			return ReconcileResult{}, fmt.Errorf("reconciler: load obligation %s: %w", verdict.ObligationID, err)
		}
		loadedObligations = append(loadedObligations, obl)
		if obl.RiskLevel == schema.RiskHigh {
			highRisk = true
		}

		status := statusForVerdict(verdict.Verdict)
		updatedStatuses[obl.ObligationID] = status
		if status == schema.ObligationSatisfied {
			satisfiedBy[obl.ObligationID] = append([]string(nil), verdict.EvidenceIDs...)
		}

		if obl.Blocking {
			if verdict.Verdict != schema.VerdictSatisfied && verdict.Verdict != schema.VerdictWaived {
				reject(&result, fmt.Sprintf("blocking obligation %s verdict is %s", obl.ObligationID, verdict.Verdict))
				updatedStatuses[obl.ObligationID] = schema.ObligationFailed
			}
			// A waiver does not require evidence IDs — WaivedBy carries the human
			// authorization token instead. An empty WaivedBy is not a valid waiver:
			// no human approved the bypass, so the obligation must be rejected.
			if verdict.Verdict == schema.VerdictWaived && strings.TrimSpace(verdict.WaivedBy) == "" {
				reject(&result, fmt.Sprintf("blocking obligation %s waiver has no WaivedBy authorization", obl.ObligationID))
				updatedStatuses[obl.ObligationID] = schema.ObligationFailed
				continue
			}
			if len(verdict.EvidenceIDs) == 0 && verdict.Verdict != schema.VerdictWaived {
				reject(&result, fmt.Sprintf("blocking obligation %s has no evidence IDs", obl.ObligationID))
				updatedStatuses[obl.ObligationID] = schema.ObligationFailed
				continue
			}
		}

		for _, evidenceID := range verdict.EvidenceIDs {
			if _, err := s.store.LoadEvidence(ctx, evidenceID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					updatedStatuses[obl.ObligationID] = schema.ObligationFailed
					if obl.Blocking {
						reject(&result, fmt.Sprintf("blocking obligation %s references absent evidence artifact %s", obl.ObligationID, evidenceID))
					}
					continue
				}
				return ReconcileResult{}, fmt.Errorf("reconciler: load evidence %s: %w", evidenceID, err)
			}
		}
	}

	for _, obligationID := range patch.ObligationIDsClaimed {
		if verdictsByObligation[obligationID] {
			continue
		}
		obl, err := s.store.LoadObligation(ctx, obligationID)
		if err != nil {
			return ReconcileResult{}, fmt.Errorf("reconciler: load claimed obligation %s: %w", obligationID, err)
		}
		if obl.Blocking {
			reject(&result, fmt.Sprintf("blocking obligation %s has no verifier verdict", obligationID))
		}
		if obl.RiskLevel == schema.RiskHigh {
			highRisk = true
		}
	}

	for _, obl := range loadedObligations {
		status := updatedStatuses[obl.ObligationID]
		ids := satisfiedBy[obl.ObligationID]
		if status != schema.ObligationSatisfied {
			ids = nil
		}
		// satisfiedByPtr is non-nil only when marking satisfied so that nil means
		// "no change to evidence IDs" and &ids means "set to exactly these IDs".
		// Passing nil for non-satisfied statuses is safe here: the reconciler only
		// operates on obligations that were open before this run (SatisfiedBy is
		// empty), so there are no stale evidence IDs to clear.
		var satisfiedByPtr *[]string
		if status == schema.ObligationSatisfied {
			satisfiedByPtr = &ids
		}
		var ev schema.Event
		ev, err = s.appendEvent(ctx, schema.EventObligationStatusUpdated, goal.GoalID, obl.ObligationID, schema.ObligationStatusPayload{
			ObligationID: obl.ObligationID,
			Status:       status,
			SatisfiedBy:  satisfiedByPtr,
		})
		if err != nil {
			return ReconcileResult{}, err
		}
		if err := s.store.UpdateObligationStatus(ctx, obl.ObligationID, status, satisfiedByPtr); err != nil {
			return ReconcileResult{}, &store.MaterializationError{Event: ev, Err: fmt.Errorf("reconciler: update obligation %s: %w", obl.ObligationID, err)}
		}
	}

	patchStatus := schema.PatchAccepted
	patchEventType := schema.EventPatchAccepted
	if !result.PatchAccepted {
		patchStatus = schema.PatchRejected
		patchEventType = schema.EventPatchRejected
	}
	patchEv, err := s.appendEvent(ctx, patchEventType, goal.GoalID, patch.PatchID, schema.PatchStatusPayload{
		PatchID: patch.PatchID,
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	if err := s.store.UpdatePatchStatus(ctx, patch.PatchID, patchStatus); err != nil {
		return ReconcileResult{}, &store.MaterializationError{Event: patchEv, Err: fmt.Errorf("reconciler: update patch %s: %w", patch.PatchID, err)}
	}

	// verifyClaims must only run for accepted patches. Promoting proposed claims
	// to verified on a rejected patch makes failed work appear factual and
	// eligible for downstream projection.
	if result.PatchAccepted {
		if err := s.verifyClaims(ctx, goal.GoalID, patch.CapsuleID); err != nil {
			return ReconcileResult{}, err
		}
		if err := s.processSupersededClaims(ctx, goal.GoalID, patch); err != nil {
			return ReconcileResult{}, err
		}
	}
	if err := s.detectClaimDisputes(ctx, goal.GoalID, vr); err != nil {
		return ReconcileResult{}, err
	}
	if result.PatchAccepted {
		if err := s.invalidateStaleClaims(ctx, goal.GoalID, patch); err != nil {
			return ReconcileResult{}, err
		}
	}
	if err := s.createAdvancedTestGapClaims(ctx, patch, vr); err != nil {
		return ReconcileResult{}, err
	}

	if !result.PatchAccepted {
		followUps, err := s.createFollowUpObligations(ctx, patch.CapsuleID, loadedObligations)
		if err != nil {
			return ReconcileResult{}, err
		}
		result.FollowUpObligationIDs = followUps
		if actions, err := s.recommendedFailureActions(ctx, patch.CapsuleID); err != nil {
			return ReconcileResult{}, err
		} else if len(actions) > 0 {
			result.BlockingReason = strings.TrimSpace(result.BlockingReason + "; recommended next action: " + strings.Join(actions, "; "))
		}
	}

	if err := s.saveBudgetRecords(ctx, goal.GoalID, patch, vr, updatedStatuses, result.PatchAccepted, now); err != nil {
		return ReconcileResult{}, err
	}

	decisionID := newArtifactID("DEC-RECON", patch.PatchID, now)
	decision := &schema.DecisionRecord{
		DecisionID: decisionID,
		Context:    "reconcile patch",
		Decision:   patchDecision(result.PatchAccepted),
		Rationale:  decisionRationale(vr, result),
		MadeBy:     "system",
		RelatedIDs: relatedIDs(patch.PatchID, vr.VerifierResultID, result.FollowUpObligationIDs),
		CreatedAt:  now,
	}
	if err := s.store.SaveDecision(ctx, decision); err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: save decision %s: %w", decisionID, err)
	}
	result.DecisionID = decisionID

	lastEvent, err := s.lastEventForGoal(ctx, goal.GoalID)
	if err != nil {
		return ReconcileResult{}, err
	}
	if err := s.store.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID:  newArtifactID("SNAP-RECON", patch.PatchID, now),
		GoalID:      goal.GoalID,
		EventID:     lastEvent.EventID,
		SequenceNum: lastEvent.SequenceNum,
		CreatedAt:   now,
	}); err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: save snapshot: %w", err)
	}

	if err := s.saveTopologyOutcome(ctx, goal.GoalID, patch, loadedObligations, updatedStatuses, result.PatchAccepted, now); err != nil {
		return ReconcileResult{}, err
	}

	if result.PatchAccepted {
		open, err := s.store.LoadOpenObligations(ctx, goal.GoalID)
		if err != nil {
			return ReconcileResult{}, fmt.Errorf("reconciler: load open obligations: %w", err)
		}
		for _, obl := range open {
			if obl.Blocking {
				result.MergeReady = false
				result.BlockingReason = fmt.Sprintf("blocking obligation %s remains open", obl.ObligationID)
				break
			}
		}
		if result.BlockingReason == "" {
			result.MergeReady = true
		}
	}
	// Scope violations require human approval regardless of risk level; they are
	// not cleared by passing other gates.
	scopeViolated := len(patch.ScopeViolations) > 0

	// highRisk is computed only from the current patch's obligations. In a
	// multi-patch run the final patch may be low-risk while an earlier accepted
	// patch satisfied high-risk obligations. Re-examine all goal obligations to
	// ensure the human gate is not skipped across reconcile rounds.
	if result.MergeReady && !highRisk {
		var err error
		highRisk, err = s.goalHasHighRiskObligations(ctx, goal)
		if err != nil {
			return ReconcileResult{}, err
		}
	}

	result.HumanGateRequired = result.MergeReady && (highRisk || recommendationRequiresHumanReview || scopeViolated)
	if scopeViolated && result.MergeReady && result.BlockingReason == "" {
		result.BlockingReason = fmt.Sprintf("patch has %d scope violation(s) requiring human approval: %s",
			len(patch.ScopeViolations), strings.Join(patch.ScopeViolations, ", "))
	}

	if result.MergeReady && !result.HumanGateRequired {
		if _, err := s.appendEvent(ctx, schema.EventMergeApplied, goal.GoalID, patch.PatchID, schema.PatchStatusPayload{
			PatchID: patch.PatchID,
		}); err != nil {
			return ReconcileResult{}, err
		}
	}

	return result, nil
}

// goalHasHighRiskObligations returns true when any blocking high-risk obligation
// exists for any condition in the goal, regardless of which reconcile round
// created or satisfied it. Used to enforce the human-gate invariant across
// multi-patch runs where the final low-risk patch triggers merge readiness.
func (s *Reconciler) goalHasHighRiskObligations(ctx context.Context, goal *schema.GoalIR) (bool, error) {
	for _, cond := range goal.GoalConditions {
		obligations, err := s.store.LoadObligationsForCondition(ctx, cond.ID)
		if err != nil {
			return false, fmt.Errorf("reconciler: load obligations for condition %s: %w", cond.ID, err)
		}
		for _, obl := range obligations {
			if obl.Blocking && obl.RiskLevel == schema.RiskHigh {
				return true, nil
			}
		}
	}
	return false, nil
}

func statusForVerdict(verdict schema.VerifierVerdict) schema.ObligationStatus {
	switch verdict {
	case schema.VerdictSatisfied:
		return schema.ObligationSatisfied
	case schema.VerdictWaived:
		return schema.ObligationWaived
	default:
		return schema.ObligationFailed
	}
}

func reject(result *ReconcileResult, reason string) {
	result.PatchAccepted = false
	if result.BlockingReason == "" {
		result.BlockingReason = reason
	}
}

// applyWaivers returns a shallow copy of vr with VerdictFailed obligation
// results replaced by VerdictWaived for any obligation ID present in waivers,
// with those IDs removed from BlockingFailures. When all blocking failures are
// cleared and the original action was ActionRetry, RecommendedAction is
// promoted to ActionAccept so the reconciler does not reject at the
// recommendation gate.
func applyWaivers(vr *schema.VerifierResult, waivers map[string]string) *schema.VerifierResult {
	vrCopy := *vr
	modifiedResults := make([]schema.ObligationVerdict, len(vr.ObligationResults))
	for i, v := range vr.ObligationResults {
		if v.Verdict == schema.VerdictFailed {
			if decID := strings.TrimSpace(waivers[v.ObligationID]); decID != "" {
				v.Verdict = schema.VerdictWaived
				v.WaivedBy = decID
				v.EvidenceIDs = nil
			}
		}
		modifiedResults[i] = v
	}
	vrCopy.ObligationResults = modifiedResults
	remaining := make([]string, 0, len(vr.BlockingFailures))
	for _, f := range vr.BlockingFailures {
		if strings.TrimSpace(waivers[f]) == "" {
			remaining = append(remaining, f)
		}
	}
	vrCopy.BlockingFailures = remaining
	if len(vrCopy.BlockingFailures) == 0 && vrCopy.RecommendedAction == schema.ActionRetry {
		vrCopy.RecommendedAction = schema.ActionAccept
		vrCopy.RecommendationRationale = "all blocking failures waived by human gate decision"
	}
	return &vrCopy
}

func (s *Reconciler) appendEvent(ctx context.Context, eventType schema.EventType, goalID, artifactID string, payload any) (schema.Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return schema.Event{}, fmt.Errorf("reconciler: marshal %s payload: %w", eventType, err)
	}
	ev, err := s.log.Append(ctx, schema.Event{
		Type:       eventType,
		GoalID:     goalID,
		ArtifactID: artifactID,
		Payload:    data,
	})
	if err != nil {
		return schema.Event{}, fmt.Errorf("reconciler: append %s: %w", eventType, err)
	}
	return ev, nil
}

func (s *Reconciler) createFollowUpObligations(ctx context.Context, capsuleID string, source []*schema.Obligation) ([]string, error) {
	failures, err := s.store.LoadFailuresForCapsule(ctx, capsuleID)
	if err != nil {
		return nil, fmt.Errorf("reconciler: load failures for capsule %s: %w", capsuleID, err)
	}
	if len(failures) == 0 || len(source) == 0 {
		return nil, nil
	}
	// Inherit condition and risk from the highest-risk source obligation so
	// follow-ups are not silently downgraded when the capsule covered mixed-risk obligations.
	conditionID := source[0].GoalConditionID
	risk := source[0].RiskLevel
	for _, obl := range source[1:] {
		if riskOrdinal(obl.RiskLevel) > riskOrdinal(risk) {
			risk = obl.RiskLevel
			conditionID = obl.GoalConditionID
		}
	}
	var ids []string
	seenSignatures := make(map[string]bool, len(failures))
	for _, failure := range failures {
		signature := failurehistory.NormalizeSignature(failure.ErrorSignature)
		if signature == "" {
			signature = failurehistory.NormalizeSignature(failure.Summary)
		}
		if signature == "" {
			signature = failure.FailureID
		}
		if seenSignatures[signature] {
			continue
		}
		seenSignatures[signature] = true
		id := "OB-FOLLOWUP-SIG-" + shortSignatureHash(signature)
		if _, err := s.store.LoadObligation(ctx, id); err == nil {
			ids = append(ids, id)
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("reconciler: check follow-up obligation %s: %w", id, err)
		}
		description := "address recurring failure: " + failure.Summary
		if action := strings.TrimSpace(failure.RecommendedNextAction); action != "" {
			description += "; recommended next action: " + action
		}
		if err := s.store.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     id,
			GoalConditionID:  conditionID,
			Description:      description,
			EvidenceRequired: evidenceRequiredForFailure(failure.FailureType),
			Blocking:         true,
			RiskLevel:        risk,
			Status:           schema.ObligationOpen,
		}); err != nil {
			return nil, fmt.Errorf("reconciler: save follow-up obligation %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func shortSignatureHash(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:])[:12]
}

// evidenceRequiredForFailure maps a FailureType to the EvidenceType strings
// that a follow-up obligation must require so that the verifier can satisfy it.
func evidenceRequiredForFailure(ft schema.FailureType) []string {
	switch ft {
	case schema.FailureLint:
		return []string{string(schema.EvidenceLintResult)}
	case schema.FailureTypecheck:
		return []string{string(schema.EvidenceTypecheckResult)}
	case schema.FailureMerge, schema.FailurePolicy:
		return []string{string(schema.EvidenceDiffRiskReport)}
	default:
		// FailureTest, FailureRuntime, FailureInfra, FailureAgent, unknown
		return []string{string(schema.EvidenceTestResult)}
	}
}

func (s *Reconciler) recommendedFailureActions(ctx context.Context, capsuleID string) ([]string, error) {
	failures, err := s.store.LoadFailuresForCapsule(ctx, capsuleID)
	if err != nil {
		return nil, fmt.Errorf("reconciler: load failure recommendations for capsule %s: %w", capsuleID, err)
	}
	actions := make([]string, 0, len(failures))
	seen := make(map[string]bool, len(failures))
	for _, failure := range failures {
		action := strings.TrimSpace(failure.RecommendedNextAction)
		if action == "" || seen[action] {
			continue
		}
		seen[action] = true
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions, nil
}

func (s *Reconciler) saveBudgetRecords(
	ctx context.Context,
	goalID string,
	patch *schema.PatchArtifact,
	vr *schema.VerifierResult,
	updatedStatuses map[string]schema.ObligationStatus,
	accepted bool,
	now time.Time,
) error {
	if patch == nil || vr == nil {
		return fmt.Errorf("reconciler: patch and verifier result are required for budget recording")
	}
	records, err := s.store.LoadBudgetForGoal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("reconciler: load budget for goal %s: %w", goalID, err)
	}
	recordsByID := make(map[string]*schema.BudgetRecord, len(records))
	for _, candidate := range records {
		recordsByID[candidate.BudgetID] = candidate
	}
	metrics, err := s.budgetMetricsForResult(ctx, vr)
	if err != nil {
		return err
	}
	advancedToolCalls := advancedGateToolCalls(vr.Warnings)
	metrics.toolCalls += advancedToolCalls
	// The patch carries the authoritative token count set by the adapter at
	// execution time. budgetMetricsForResult has no source for tokens, so we
	// override here rather than threading it through evidence-based metrics.
	metrics.tokensSpent = patch.TokensUsed
	metrics.wallTimeSeconds = patch.WallTimeSeconds

	// Tokens are a capsule-level cost. Split them evenly across obligation-level
	// records, assigning remainder tokens in verdict order so per-obligation
	// totals match the capsule summary exactly.
	eligibleVerdicts := make([]string, 0, len(vr.ObligationResults))
	for _, verdict := range vr.ObligationResults {
		if _, ok := updatedStatuses[verdict.ObligationID]; ok {
			eligibleVerdicts = append(eligibleVerdicts, verdict.ObligationID)
		}
	}
	tokenShareByObligation := make(map[string]int, len(eligibleVerdicts))
	wallTimeShareByObligation := make(map[string]float64, len(eligibleVerdicts))
	if patch.TokensUsed > 0 && len(eligibleVerdicts) > 0 {
		baseShare := patch.TokensUsed / len(eligibleVerdicts)
		remainder := patch.TokensUsed % len(eligibleVerdicts)
		for i, obligationID := range eligibleVerdicts {
			share := baseShare
			if i < remainder {
				share++
			}
			tokenShareByObligation[obligationID] = share
		}
	}
	if patch.WallTimeSeconds > 0 && len(eligibleVerdicts) > 0 {
		share := patch.WallTimeSeconds / float64(len(eligibleVerdicts))
		for _, obligationID := range eligibleVerdicts {
			wallTimeShareByObligation[obligationID] = share
		}
	}

	discharged := countDischarged(updatedStatuses)

	summaryID := "BUD-" + patch.CapsuleID
	summary := recordsByID[summaryID]
	if summary == nil {
		summary = &schema.BudgetRecord{
			BudgetID:  summaryID,
			GoalID:    goalID,
			CapsuleID: patch.CapsuleID,
			CreatedAt: now,
		}
	}
	summary.UpdatedAt = now
	summary.ObligationID = ""
	summary.TokensSpent = metrics.tokensSpent
	summary.WallTimeSeconds = metrics.wallTimeSeconds
	summary.ToolCalls = metrics.toolCalls
	summary.Retries = metrics.retries
	summary.DuplicatedFileReads = metrics.duplicatedFileReads
	summary.OverlappingEdits = metrics.overlappingEdits
	summary.EvidenceArtifactsReused = metrics.evidenceArtifactsReused
	summary.HumanInterventions = metrics.humanInterventions
	summary.ObligationsDischarged = discharged
	if accepted {
		summary.PatchesAccepted = 1
		summary.PatchesRejected = 0
	} else {
		summary.PatchesAccepted = 0
		summary.PatchesRejected = 1
	}
	if err := saveOrUpdateBudgetRecord(ctx, s.store, summary, recordsByID[summaryID] != nil); err != nil {
		return err
	}

	for _, verdict := range vr.ObligationResults {
		status, ok := updatedStatuses[verdict.ObligationID]
		if !ok {
			continue
		}
		obligationMetrics, err := s.budgetMetricsForEvidenceIDs(ctx, verdict.EvidenceIDs, vr.RecommendedAction)
		if err != nil {
			return err
		}
		obligationMetrics.toolCalls += advancedToolCalls
		obligationMetrics.tokensSpent = tokenShareByObligation[verdict.ObligationID]
		obligationMetrics.wallTimeSeconds = wallTimeShareByObligation[verdict.ObligationID]
		recordID := "BUD-" + patch.CapsuleID + "-" + verdict.ObligationID
		record := recordsByID[recordID]
		found := record != nil
		if record == nil {
			record = &schema.BudgetRecord{
				BudgetID:     recordID,
				GoalID:       goalID,
				CapsuleID:    patch.CapsuleID,
				ObligationID: verdict.ObligationID,
				CreatedAt:    now,
			}
		}
		record.UpdatedAt = now
		record.TokensSpent = obligationMetrics.tokensSpent
		record.WallTimeSeconds = obligationMetrics.wallTimeSeconds
		record.ToolCalls = obligationMetrics.toolCalls
		record.Retries = obligationMetrics.retries
		record.DuplicatedFileReads = obligationMetrics.duplicatedFileReads
		record.OverlappingEdits = obligationMetrics.overlappingEdits
		record.EvidenceArtifactsReused = obligationMetrics.evidenceArtifactsReused
		record.HumanInterventions = obligationMetrics.humanInterventions
		if status == schema.ObligationSatisfied || status == schema.ObligationWaived {
			record.ObligationsDischarged = 1
		} else {
			record.ObligationsDischarged = 0
		}
		if accepted {
			record.PatchesAccepted = 1
			record.PatchesRejected = 0
		} else {
			record.PatchesAccepted = 0
			record.PatchesRejected = 1
		}
		if err := saveOrUpdateBudgetRecord(ctx, s.store, record, found); err != nil {
			return err
		}
	}
	return nil
}

func (s *Reconciler) createAdvancedTestGapClaims(ctx context.Context, patch *schema.PatchArtifact, vr *schema.VerifierResult) error {
	if patch == nil || vr == nil {
		return fmt.Errorf("reconciler: patch and verifier result are required for advanced test-gap claims")
	}
	warnings := advancedTestGapWarnings(vr.Warnings)
	if len(warnings) == 0 {
		return nil
	}
	existing, err := s.store.LoadClaimsForCapsule(ctx, patch.CapsuleID)
	if err != nil {
		return fmt.Errorf("reconciler: load claims for capsule %s: %w", patch.CapsuleID, err)
	}
	seen := make(map[string]bool, len(existing))
	for _, claim := range existing {
		seen[claim.SourceCapsuleID+"\x00"+claim.Text] = true
	}
	for _, warning := range warnings {
		text := strings.TrimSpace(warning)
		key := patch.CapsuleID + "\x00" + text
		if seen[key] {
			continue
		}
		claim := &schema.ClaimArtifact{
			ClaimID:         advancedTestGapClaimID(patch.CapsuleID, text),
			Text:            text,
			ClaimType:       schema.ClaimTestGap,
			SourceCapsuleID: patch.CapsuleID,
			AffectedFiles:   append([]string(nil), patch.ChangedFiles...),
			Status:          schema.ClaimProposed,
		}
		if err := s.store.SaveClaim(ctx, claim); err != nil {
			return fmt.Errorf("reconciler: save advanced test-gap claim %s: %w", claim.ClaimID, err)
		}
		seen[key] = true
	}
	return nil
}

func advancedTestGapWarnings(warnings []string) []string {
	var out []string
	for _, warning := range warnings {
		trimmed := strings.TrimSpace(warning)
		if strings.HasPrefix(trimmed, "[mutation] survivor found: test gap candidate") ||
			strings.HasPrefix(trimmed, "[adversarial] challenge failed: test gap candidate") {
			out = append(out, trimmed)
		}
	}
	return out
}

func advancedTestGapClaimID(capsuleID, warning string) string {
	sum := sha256.Sum256([]byte(capsuleID + "\x00" + warning))
	return "CL-ADV-" + hex.EncodeToString(sum[:])[:16]
}

func advancedGateToolCalls(warnings []string) int {
	distinct := make(map[string]bool, 2)
	for _, warning := range warnings {
		switch {
		case strings.HasPrefix(warning, "[mutation]"):
			distinct["mutation"] = true
		case strings.HasPrefix(warning, "[adversarial]"):
			distinct["adversarial"] = true
		}
	}
	return len(distinct)
}

func countDischarged(statuses map[string]schema.ObligationStatus) int {
	var count int
	for _, status := range statuses {
		if status == schema.ObligationSatisfied || status == schema.ObligationWaived {
			count++
		}
	}
	return count
}

func (s *Reconciler) lastEventForGoal(ctx context.Context, goalID string) (schema.Event, error) {
	events, err := s.log.ReadForGoal(ctx, goalID, 0, 0)
	if err != nil {
		return schema.Event{}, fmt.Errorf("reconciler: read events for goal %s: %w", goalID, err)
	}
	if len(events) == 0 {
		return schema.Event{}, fmt.Errorf("reconciler: no events for goal %s: %w", goalID, store.ErrNotFound)
	}
	return events[len(events)-1], nil
}

func patchDecision(accepted bool) string {
	if accepted {
		return "patch accepted"
	}
	return "patch rejected"
}

func decisionRationale(vr *schema.VerifierResult, result ReconcileResult) string {
	if result.BlockingReason != "" {
		return result.BlockingReason
	}
	if vr.RecommendationRationale != "" {
		return vr.RecommendationRationale
	}
	return "all blocking obligations satisfied by present evidence"
}

func relatedIDs(patchID, verifierResultID string, followUps []string) []string {
	ids := []string{patchID, verifierResultID}
	ids = append(ids, followUps...)
	return ids
}

type budgetMetrics struct {
	tokensSpent             int
	wallTimeSeconds         float64
	toolCalls               int
	retries                 int
	duplicatedFileReads     int
	overlappingEdits        int
	evidenceArtifactsReused int
	humanInterventions      int
}

func (s *Reconciler) budgetMetricsForResult(ctx context.Context, vr *schema.VerifierResult) (budgetMetrics, error) {
	evidenceSet := make(map[string]bool)
	for _, verdict := range vr.ObligationResults {
		for _, evidenceID := range verdict.EvidenceIDs {
			evidenceSet[evidenceID] = true
		}
	}
	evidenceIDs := make([]string, 0, len(evidenceSet))
	for evidenceID := range evidenceSet {
		evidenceIDs = append(evidenceIDs, evidenceID)
	}
	return s.budgetMetricsForEvidenceIDs(ctx, evidenceIDs, vr.RecommendedAction)
}

func (s *Reconciler) budgetMetricsForEvidenceIDs(
	ctx context.Context,
	evidenceIDs []string,
	action schema.RecommendedAction,
) (budgetMetrics, error) {
	var metrics budgetMetrics
	seen := make(map[string]bool)
	for _, evidenceID := range evidenceIDs {
		if evidenceID == "" || seen[evidenceID] {
			continue
		}
		seen[evidenceID] = true
		evidence, err := s.store.LoadEvidence(ctx, evidenceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return budgetMetrics{}, fmt.Errorf("reconciler: load evidence %s for budget metrics: %w", evidenceID, err)
		}
		metrics.toolCalls++
		if strings.TrimSpace(evidence.ReusedFromID) != "" {
			metrics.evidenceArtifactsReused++
		}
	}
	if action == schema.ActionRetry || action == schema.ActionSplit {
		metrics.retries = 1
	}
	if action == schema.ActionHumanReview {
		metrics.humanInterventions = 1
	}
	return metrics, nil
}

func saveOrUpdateBudgetRecord(ctx context.Context, st *store.FileStore, record *schema.BudgetRecord, exists bool) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = record.UpdatedAt
	}
	if exists {
		return st.UpdateBudgetRecord(ctx, record)
	}
	return st.SaveBudgetRecord(ctx, record)
}

func recommendationBlockingReason(vr *schema.VerifierResult) string {
	if vr.RecommendationRationale != "" {
		return vr.RecommendationRationale
	}
	return fmt.Sprintf("verifier recommended %s", vr.RecommendedAction)
}

func newArtifactID(prefix, patchID string, now time.Time) string {
	return prefix + "-" + patchID + "-" + strconv.FormatInt(now.UnixNano(), 10)
}

func (s *Reconciler) saveTopologyOutcome(
	ctx context.Context,
	goalID string,
	patch *schema.PatchArtifact,
	loadedObligations []*schema.Obligation,
	updatedStatuses map[string]schema.ObligationStatus,
	patchAccepted bool,
	now time.Time,
) error {
	if s.noLearning {
		return nil
	}
	capsule, err := s.store.LoadCapsule(ctx, patch.CapsuleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reconciler: load capsule %s for topology outcome: %w", patch.CapsuleID, err)
	}
	if capsule.TopologyDecisionID == "" {
		return nil
	}
	decision, err := s.store.LoadDecision(ctx, capsule.TopologyDecisionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reconciler: load decision %s for topology outcome: %w", capsule.TopologyDecisionID, err)
	}
	topology := schema.Topology(decision.Decision)

	obligationsMet := 0
	for _, status := range updatedStatuses {
		if status == schema.ObligationSatisfied || status == schema.ObligationWaived {
			obligationsMet++
		}
	}

	maxRisk := schema.RiskLow
	for _, obl := range loadedObligations {
		switch obl.RiskLevel {
		case schema.RiskHigh:
			maxRisk = schema.RiskHigh
		case schema.RiskMedium:
			if maxRisk != schema.RiskHigh {
				maxRisk = schema.RiskMedium
			}
		}
	}

	failures, err := s.store.LoadFailuresForCapsule(ctx, patch.CapsuleID)
	if err != nil {
		return fmt.Errorf("reconciler: load failures for topology outcome %s: %w", patch.CapsuleID, err)
	}

	outcomeID := newArtifactID("TOP-OUT", patch.PatchID, now)
	record := &schema.TopologyOutcomeRecord{
		OutcomeID:       outcomeID,
		GoalID:          goalID,
		Topology:        topology,
		ObligationCount: len(loadedObligations),
		MaxRiskLevel:    maxRisk,
		AffectedFiles:   append([]string(nil), patch.ChangedFiles...),
		PatchAccepted:   patchAccepted,
		ObligationsMet:  obligationsMet,
		TokensSpent:     patch.TokensUsed,
		FailureCount:    len(failures),
		RecordedAt:      now,
	}
	if err := s.store.SaveTopologyOutcome(ctx, record); err != nil {
		return fmt.Errorf("reconciler: save topology outcome %s: %w", outcomeID, err)
	}
	return nil
}

func riskOrdinal(r schema.RiskLevel) int {
	switch r {
	case schema.RiskHigh:
		return 2
	case schema.RiskMedium:
		return 1
	default:
		return 0
	}
}

func normalizedSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		normalized = strings.ReplaceAll(normalized, "\\", "/")
		normalized = strings.TrimPrefix(normalized, "./")
		normalized = strings.Trim(normalized, "/")
		normalized = strings.ToLower(normalized)
		if normalized == "" {
			continue
		}
		out[normalized] = true
	}
	return out
}

func normalizedSymbols(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if normalized := normalizeSymbol(value); normalized != "" {
			out[normalized] = true
		}
	}
	return out
}

func normalizeSymbol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func hasOverlap(left, right map[string]bool) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	for value := range left {
		if right[value] {
			return true
		}
	}
	return false
}

func mergeStrings(existing, add []string) []string {
	seen := make(map[string]bool, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, value := range append(append([]string(nil), existing...), add...) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}
