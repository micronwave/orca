// Package reconciler defines the Reconciler interface, which maps evidence to
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
//	                  store.SaveEvidence, store.SaveClaim,
//	                  store.SaveVerifierResult,
//	                  store.SaveProjection, store.SaveHumanSummaryProjection,
//	                  store.SaveFailure
//	Must NOT run:     verifier stages or agent commands
//	Must NOT accept:  a patch without mapping evidence to every blocking obligation
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

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
type Reconciler interface {
	// Reconcile processes the VerifierResult for patchID. It:
	//   1. Reads the VerifierResult and maps evidence to obligations
	//   2. Emits obligation_status_updated, then accepts/rejects each obligation
	//   3. Emits patch_accepted/rejected, then updates PatchArtifact status
	//   4. Creates follow-up Obligations from blocking failures (if rejected)
	//   5. Updates BudgetRecords with token spend per obligation
	//   6. Persists a DecisionRecord explaining the outcome
	//   7. Takes a StateSnapshot
	//   8. Emits merge_applied when merge policy permits a merge
	//
	// Returns ReconcileResult summarizing the decision. The orchestrator reads
	// MergeReady and HumanGateRequired to decide whether to surface a merge gate,
	// merge directly, or loop back to the planner with FollowUpObligationIDs.
	Reconcile(ctx context.Context, patchID string) (ReconcileResult, error)
}

type service struct {
	store store.ArtifactStore
	log   eventlog.EventLog
}

// New returns the default Reconciler implementation.
func New(st store.ArtifactStore, log eventlog.EventLog) Reconciler {
	return &service{store: st, log: log}
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

func (s *service) Reconcile(ctx context.Context, patchID string) (ReconcileResult, error) {
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
			if len(verdict.EvidenceIDs) == 0 {
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
		if _, err := s.appendEvent(ctx, schema.EventObligationStatusUpdated, goal.GoalID, obl.ObligationID, schema.ObligationStatusPayload{
			ObligationID: obl.ObligationID,
			Status:       status,
			SatisfiedBy:  ids,
		}); err != nil {
			return ReconcileResult{}, err
		}
		if err := s.store.UpdateObligationStatus(ctx, obl.ObligationID, status, ids); err != nil {
			return ReconcileResult{}, fmt.Errorf("reconciler: update obligation %s: %w", obl.ObligationID, err)
		}
	}

	patchStatus := schema.PatchAccepted
	patchEventType := schema.EventPatchAccepted
	if !result.PatchAccepted {
		patchStatus = schema.PatchRejected
		patchEventType = schema.EventPatchRejected
	}
	if _, err := s.appendEvent(ctx, patchEventType, goal.GoalID, patch.PatchID, schema.PatchStatusPayload{
		PatchID: patch.PatchID,
	}); err != nil {
		return ReconcileResult{}, err
	}
	if err := s.store.UpdatePatchStatus(ctx, patch.PatchID, patchStatus); err != nil {
		return ReconcileResult{}, fmt.Errorf("reconciler: update patch %s: %w", patch.PatchID, err)
	}

	if err := s.verifyClaims(ctx, goal.GoalID, patch.CapsuleID); err != nil {
		return ReconcileResult{}, err
	}
	if result.PatchAccepted {
		if err := s.invalidateStaleClaims(ctx, goal.GoalID, patch); err != nil {
			return ReconcileResult{}, err
		}
	}

	if !result.PatchAccepted {
		followUps, err := s.createFollowUpObligations(ctx, patch.CapsuleID, loadedObligations)
		if err != nil {
			return ReconcileResult{}, err
		}
		result.FollowUpObligationIDs = followUps
	}

	if err := s.saveBudgetRecord(ctx, goal.GoalID, patch.CapsuleID, result.PatchAccepted, countDischarged(updatedStatuses), now); err != nil {
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
	result.HumanGateRequired = result.MergeReady && highRisk

	if result.MergeReady && !result.HumanGateRequired {
		if _, err := s.appendEvent(ctx, schema.EventMergeApplied, goal.GoalID, patch.PatchID, schema.PatchStatusPayload{
			PatchID: patch.PatchID,
		}); err != nil {
			return ReconcileResult{}, err
		}
	}

	return result, nil
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

func (s *service) appendEvent(ctx context.Context, eventType schema.EventType, goalID, artifactID string, payload any) (schema.Event, error) {
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

func (s *service) verifyClaims(ctx context.Context, goalID, capsuleID string) error {
	claims, err := s.store.LoadClaimsForCapsule(ctx, capsuleID)
	if err != nil {
		return fmt.Errorf("reconciler: load claims for capsule %s: %w", capsuleID, err)
	}
	for _, claim := range claims {
		if claim.Status == schema.ClaimVerified || len(claim.EvidenceIDs) == 0 {
			continue
		}
		allPresent := true
		for _, evidenceID := range claim.EvidenceIDs {
			if _, err := s.store.LoadEvidence(ctx, evidenceID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					allPresent = false
					break
				}
				return fmt.Errorf("reconciler: load claim evidence %s: %w", evidenceID, err)
			}
		}
		if !allPresent {
			continue
		}
		if _, err := s.appendEvent(ctx, schema.EventClaimStatusUpdated, goalID, claim.ClaimID, schema.ClaimStatusPayload{
			ClaimID: claim.ClaimID,
			Status:  schema.ClaimVerified,
		}); err != nil {
			return err
		}
		if err := s.store.UpdateClaimStatus(ctx, claim.ClaimID, schema.ClaimVerified); err != nil {
			return fmt.Errorf("reconciler: update claim %s: %w", claim.ClaimID, err)
		}
	}
	return nil
}

func (s *service) invalidateStaleClaims(ctx context.Context, goalID string, patch *schema.PatchArtifact) error {
	if patch == nil {
		return nil
	}
	claims, err := s.store.LoadClaimsForGoal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("reconciler: load claims for goal %s: %w", goalID, err)
	}
	if len(claims) == 0 {
		return nil
	}
	currentCapsuleClaims, err := s.store.LoadClaimsForCapsule(ctx, patch.CapsuleID)
	if err != nil {
		return fmt.Errorf("reconciler: load claims for capsule %s: %w", patch.CapsuleID, err)
	}
	changedFiles := normalizedSet(patch.ChangedFiles)
	// changedSymbols is populated from the new capsule's claims, not from a static
	// analysis of the diff. File overlap is the primary signal; symbol overlap is
	// supplementary and only fires when producers populate AffectedSymbols. When
	// AffectedSymbols is empty for all current capsule claims, changedSymbols is
	// empty and hasOverlap returns false — file overlap alone drives stale detection.
	changedSymbols := make(map[string]bool)
	for _, claim := range currentCapsuleClaims {
		for _, symbol := range claim.AffectedSymbols {
			if normalized := normalizeSymbol(symbol); normalized != "" {
				changedSymbols[normalized] = true
			}
		}
	}
	for _, claim := range claims {
		if claim.Status != schema.ClaimVerified || claim.SourceCapsuleID == patch.CapsuleID {
			continue
		}
		fileOverlap := hasOverlap(normalizedSet(claim.AffectedFiles), changedFiles)
		symbolOverlap := hasOverlap(normalizedSymbols(claim.AffectedSymbols), changedSymbols)
		if !fileOverlap && !symbolOverlap {
			continue
		}
		if _, err := s.appendEvent(ctx, schema.EventClaimStatusUpdated, goalID, claim.ClaimID, schema.ClaimStatusPayload{
			ClaimID: claim.ClaimID,
			Status:  schema.ClaimStale,
		}); err != nil {
			return err
		}
		if err := s.store.UpdateClaimStatus(ctx, claim.ClaimID, schema.ClaimStale); err != nil {
			return fmt.Errorf("reconciler: stale claim %s: %w", claim.ClaimID, err)
		}
	}
	return nil
}

func (s *service) createFollowUpObligations(ctx context.Context, capsuleID string, source []*schema.Obligation) ([]string, error) {
	failures, err := s.store.LoadFailuresForCapsule(ctx, capsuleID)
	if err != nil {
		return nil, fmt.Errorf("reconciler: load failures for capsule %s: %w", capsuleID, err)
	}
	if len(failures) == 0 || len(source) == 0 {
		return nil, nil
	}
	conditionID := source[0].GoalConditionID
	risk := source[0].RiskLevel
	var ids []string
	for _, failure := range failures {
		id := "OB-FOLLOWUP-" + failure.FailureID
		if _, err := s.store.LoadObligation(ctx, id); err == nil {
			ids = append(ids, id)
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("reconciler: check follow-up obligation %s: %w", id, err)
		}
		if err := s.store.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     id,
			GoalConditionID:  conditionID,
			Description:      "address failure: " + failure.Summary,
			EvidenceRequired: []string{string(failure.FailureType)},
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

func (s *service) saveBudgetRecord(ctx context.Context, goalID, capsuleID string, accepted bool, discharged int, now time.Time) error {
	records, err := s.store.LoadBudgetForGoal(ctx, goalID)
	if err != nil {
		return fmt.Errorf("reconciler: load budget for goal %s: %w", goalID, err)
	}
	var record *schema.BudgetRecord
	found := false
	for _, candidate := range records {
		if candidate.CapsuleID == capsuleID {
			record = candidate
			found = true
			break
		}
	}
	if record == nil {
		record = &schema.BudgetRecord{
			BudgetID:  "BUD-" + capsuleID,
			GoalID:    goalID,
			CapsuleID: capsuleID,
			CreatedAt: now,
		}
	}
	record.UpdatedAt = now
	// Assignment (not +=) is intentional: if the reconciler re-runs for the same
	// capsule on crash recovery, the latest result wins rather than double-counting.
	record.ObligationsDischarged = discharged
	if accepted {
		record.PatchesAccepted = 1
		record.PatchesRejected = 0
	} else {
		record.PatchesAccepted = 0
		record.PatchesRejected = 1
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}

	if found {
		return s.store.UpdateBudgetRecord(ctx, record)
	}
	return s.store.SaveBudgetRecord(ctx, record)
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

func (s *service) lastEventForGoal(ctx context.Context, goalID string) (schema.Event, error) {
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

func newArtifactID(prefix, patchID string, now time.Time) string {
	return prefix + "-" + patchID + "-" + strconv.FormatInt(now.UnixNano(), 10)
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
