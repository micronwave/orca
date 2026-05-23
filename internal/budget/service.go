package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// Controller enforces capsule budget limits and computes ROI metrics.
// Budget limits come from the capsule_created event payload; accumulated spend
// comes from budget_record_saved/updated event payloads in the event log.
type Controller struct {
	log *eventlog.FileLog
}

// New returns an event-log-only budget Controller.
func New(log *eventlog.FileLog) *Controller {
	return &Controller{log: log}
}

func (s *Controller) CheckCapsuleBudget(ctx context.Context, capsuleID string) (BudgetCheck, error) {
	if capsuleID == "" {
		return BudgetCheck{}, fmt.Errorf("budget: capsuleID is required")
	}
	capsule, goalID, err := s.loadCapsuleFromLog(ctx, capsuleID)
	if err != nil {
		return BudgetCheck{}, err
	}
	spend, _, err := s.spendForGoal(ctx, goalID, capsule.CapsuleID)
	if err != nil {
		return BudgetCheck{}, err
	}
	check := BudgetCheck{
		Allowed:      true,
		CurrentSpend: spend,
		BudgetLimit:  capsule.Budget,
	}
	// Adapter token reporting is not wired yet. A zero token count means
	// "unknown", not "free"; do not block on MaxTokens until spend is non-zero.
	if capsule.Budget.MaxTokens > 0 && spend.TokensUsed > 0 && spend.TokensUsed >= capsule.Budget.MaxTokens {
		check.Allowed = false
		check.Reason = "token budget exhausted"
		return check, nil
	}
	if capsule.Budget.MaxWallTimeSeconds > 0 && spend.WallTimeSeconds >= float64(capsule.Budget.MaxWallTimeSeconds) {
		check.Allowed = false
		check.Reason = "wall time budget exhausted"
		return check, nil
	}
	if capsule.Budget.MaxRetries > 0 && spend.Retries >= capsule.Budget.MaxRetries {
		check.Allowed = false
		check.Reason = "retry budget exhausted"
		return check, nil
	}
	return check, nil
}

func (s *Controller) ComputeROI(ctx context.Context, goalID string) (ROI, error) {
	if goalID == "" {
		return ROI{}, fmt.Errorf("budget: goalID is required")
	}
	spend, records, err := s.spendForGoal(ctx, goalID, "")
	if err != nil {
		return ROI{}, err
	}
	roi := ROI{
		TotalTokensSpent:      spend.TokensUsed,
		TotalWallTimeSeconds:  spend.WallTimeSeconds,
		TotalCoordinationCost: spend.CoordinationCostUnits,
	}
	for _, record := range records {
		if record.ObligationID != "" {
			continue
		}
		roi.ObligationsDischarged += record.ObligationsDischarged
		roi.PatchesAccepted += record.PatchesAccepted
		roi.PatchesRejected += record.PatchesRejected
		roi.EvidenceArtifactsReused += record.EvidenceArtifactsReused
		roi.AvoidedRetries += record.AvoidedRetries
		roi.HumanInterventions += record.HumanInterventions
	}
	if roi.TotalTokensSpent > 0 {
		value := roi.ObligationsDischarged + roi.PatchesAccepted + roi.EvidenceArtifactsReused + roi.AvoidedRetries
		roi.VerifiedValuePer1KTokens = float64(value) * 1000 / float64(roi.TotalTokensSpent)
	}
	return roi, nil
}

func (s *Controller) loadCapsuleFromLog(ctx context.Context, capsuleID string) (schema.ExecutionCapsule, string, error) {
	var seq int64
	for {
		events, err := s.log.ReadByType(ctx, schema.EventCapsuleCreated, seq, 200)
		if err != nil {
			return schema.ExecutionCapsule{}, "", fmt.Errorf("budget: read capsule_created events: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			if event.ArtifactID == capsuleID {
				var capsule schema.ExecutionCapsule
				if err := json.Unmarshal(event.Payload, &capsule); err != nil {
					return schema.ExecutionCapsule{}, "", fmt.Errorf("budget: unmarshal capsule_created payload for %s: %w", capsuleID, err)
				}
				if capsule.CapsuleID == "" {
					return schema.ExecutionCapsule{}, "", fmt.Errorf("budget: capsule_created payload for %s missing capsule_id", capsuleID)
				}
				if capsule.CapsuleID != capsuleID {
					return schema.ExecutionCapsule{}, "", fmt.Errorf("budget: capsule_created artifact_id %s payload capsule_id %s mismatch", capsuleID, capsule.CapsuleID)
				}
				return capsule, event.GoalID, nil
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return schema.ExecutionCapsule{}, "", fmt.Errorf("budget: capsule %s: %w", capsuleID, errors.New("not found in event log"))
}

func (s *Controller) spendForGoal(ctx context.Context, goalID string, capsuleIDFilter string) (Spend, map[string]schema.BudgetRecord, error) {
	records := make(map[string]schema.BudgetRecord)
	var seq int64
	for {
		events, err := s.log.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return Spend{}, nil, fmt.Errorf("budget: read events for goal %s: %w", goalID, err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			if event.Type != schema.EventBudgetRecordSaved && event.Type != schema.EventBudgetRecordUpdated {
				continue
			}
			var record schema.BudgetRecord
			if err := json.Unmarshal(event.Payload, &record); err != nil {
				return Spend{}, nil, fmt.Errorf("budget: unmarshal %s payload for %s: %w", event.Type, event.ArtifactID, err)
			}
			if record.BudgetID == "" {
				return Spend{}, nil, fmt.Errorf("budget: %s event %s missing budget_id", event.Type, event.EventID)
			}
			if err := validateBudgetRecord(&record); err != nil {
				return Spend{}, nil, fmt.Errorf("budget: invalid record %s: %w", record.BudgetID, err)
			}
			records[record.BudgetID] = record
		}
		seq = events[len(events)-1].SequenceNum
	}
	var spend Spend
	for _, record := range records {
		if record.ObligationID != "" {
			continue
		}
		if capsuleIDFilter != "" && record.CapsuleID != capsuleIDFilter {
			continue
		}
		spend.TokensUsed += record.TokensSpent
		spend.WallTimeSeconds += record.WallTimeSeconds
		spend.ToolCalls += record.ToolCalls
		spend.Retries += record.Retries
		spend.CoordinationCostUnits += coordinationCost(record)
	}
	return spend, records, nil
}

func coordinationCost(record schema.BudgetRecord) int {
	return record.Retries + record.DuplicatedFileReads + record.OverlappingEdits + record.HumanInterventions
}

func validateBudgetRecord(r *schema.BudgetRecord) error {
	if r.TokensSpent < 0 || r.WallTimeSeconds < 0 || r.Retries < 0 || r.ToolCalls < 0 ||
		r.DuplicatedFileReads < 0 || r.OverlappingEdits < 0 || r.HumanInterventions < 0 ||
		r.ObligationsDischarged < 0 || r.PatchesAccepted < 0 || r.PatchesRejected < 0 ||
		r.EvidenceArtifactsReused < 0 || r.AvoidedRetries < 0 {
		return fmt.Errorf("consumption fields must be non-negative")
	}
	return nil
}
