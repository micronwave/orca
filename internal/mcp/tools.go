package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// toolDef is the MCP tool definition returned in tools/list responses.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func toolDefinitions() []toolDef {
	return []toolDef{
		{
			Name:        "orca_get_goal",
			Description: "Load the active goal or a specific goal by ID.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal_id": map[string]any{
						"type":        "string",
						"description": "GoalID to fetch. Omit to load the active goal.",
					},
				},
			},
		},
		{
			Name:        "orca_list_open_obligations",
			Description: "List all open obligations for a goal.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"goal_id"},
				"properties": map[string]any{
					"goal_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_list_capsules",
			Description: "List all execution capsules created for a goal.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"goal_id"},
				"properties": map[string]any{
					"goal_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_get_patch",
			Description: "Load a patch artifact by ID.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"patch_id"},
				"properties": map[string]any{
					"patch_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_list_patch_evidence",
			Description: "List evidence artifacts associated with a patch's claimed obligations.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"patch_id"},
				"properties": map[string]any{
					"patch_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_get_verifier_result_for_patch",
			Description: "Load the verifier result for a patch.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"patch_id"},
				"properties": map[string]any{
					"patch_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_get_budget_for_goal",
			Description: "Load all budget records for a goal.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"goal_id"},
				"properties": map[string]any{
					"goal_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "orca_get_merge_readiness",
			Description: "Derive merge readiness status from the current artifact and event state for a goal.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"goal_id"},
				"properties": map[string]any{
					"goal_id": map[string]any{"type": "string"},
					"patch_id": map[string]any{
						"type":        "string",
						"description": "Specific patch to evaluate. Omit to use the latest verified patch.",
					},
				},
			},
		},
	}
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "orca_get_goal":
		return s.toolGetGoal(ctx, args)
	case "orca_list_open_obligations":
		return s.toolListOpenObligations(ctx, args)
	case "orca_list_capsules":
		return s.toolListCapsules(ctx, args)
	case "orca_get_patch":
		return s.toolGetPatch(ctx, args)
	case "orca_list_patch_evidence":
		return s.toolListPatchEvidence(ctx, args)
	case "orca_get_verifier_result_for_patch":
		return s.toolGetVerifierResultForPatch(ctx, args)
	case "orca_get_budget_for_goal":
		return s.toolGetBudgetForGoal(ctx, args)
	case "orca_get_merge_readiness":
		return s.toolGetMergeReadiness(ctx, args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) toolGetGoal(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		GoalID string `json:"goal_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	var goal *schema.GoalIR
	var err error
	if params.GoalID == "" {
		goal, err = s.store.LoadActiveGoal(ctx)
	} else {
		goal, err = s.store.LoadGoal(ctx, params.GoalID)
	}
	if err != nil {
		return "", fmt.Errorf("load goal: %w", err)
	}
	if goal == nil {
		return "", fmt.Errorf("no active goal")
	}
	return marshalResult(goal)
}

func (s *Server) toolListOpenObligations(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		GoalID string `json:"goal_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.GoalID == "" {
		return "", fmt.Errorf("goal_id is required")
	}
	obligations, err := s.store.LoadOpenObligations(ctx, params.GoalID)
	if err != nil {
		return "", fmt.Errorf("load open obligations: %w", err)
	}
	if obligations == nil {
		obligations = []*schema.Obligation{}
	}
	return marshalResult(obligations)
}

func (s *Server) toolListCapsules(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		GoalID string `json:"goal_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.GoalID == "" {
		return "", fmt.Errorf("goal_id is required")
	}
	seen := make(map[string]bool)
	type capsuleWithRuntime struct {
		*schema.ExecutionCapsule
		RuntimeStatus *schema.CapsuleRuntimeEvent `json:"runtime_status,omitempty"`
	}
	var capsules []capsuleWithRuntime
	var afterSeq int64
	for {
		events, err := s.log.ReadForGoal(ctx, params.GoalID, afterSeq, 200)
		if err != nil {
			return "", fmt.Errorf("read events: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Type == schema.EventCapsuleCreated && ev.ArtifactID != "" && !seen[ev.ArtifactID] {
				seen[ev.ArtifactID] = true
				capsule, err := s.store.LoadCapsule(ctx, ev.ArtifactID)
				if err != nil {
					return "", fmt.Errorf("load capsule %s: %w", ev.ArtifactID, err)
				}
				latest, err := s.store.LoadLatestRuntimeStatus(ctx, ev.ArtifactID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return "", fmt.Errorf("load capsule runtime status %s: %w", ev.ArtifactID, err)
				}
				capsules = append(capsules, capsuleWithRuntime{
					ExecutionCapsule: capsule,
					RuntimeStatus:    latest,
				})
			}
		}
		afterSeq = events[len(events)-1].SequenceNum
	}
	if capsules == nil {
		capsules = []capsuleWithRuntime{}
	}
	return marshalResult(capsules)
}

func (s *Server) toolGetPatch(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		PatchID string `json:"patch_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.PatchID == "" {
		return "", fmt.Errorf("patch_id is required")
	}
	patch, err := s.store.LoadPatch(ctx, params.PatchID)
	if err != nil {
		return "", fmt.Errorf("load patch: %w", err)
	}
	return marshalResult(patch)
}

func (s *Server) toolListPatchEvidence(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		PatchID string `json:"patch_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.PatchID == "" {
		return "", fmt.Errorf("patch_id is required")
	}
	patch, err := s.store.LoadPatch(ctx, params.PatchID)
	if err != nil {
		return "", fmt.Errorf("load patch: %w", err)
	}
	seen := make(map[string]bool)
	var evidence []*schema.EvidenceArtifact
	for _, oblID := range patch.ObligationIDsClaimed {
		evList, err := s.store.LoadEvidenceForObligation(ctx, oblID)
		if err != nil {
			return "", fmt.Errorf("load evidence for obligation %s: %w", oblID, err)
		}
		for _, ev := range evList {
			if !seen[ev.EvidenceID] {
				seen[ev.EvidenceID] = true
				evidence = append(evidence, ev)
			}
		}
	}
	if evidence == nil {
		evidence = []*schema.EvidenceArtifact{}
	}
	return marshalResult(evidence)
}

func (s *Server) toolGetVerifierResultForPatch(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		PatchID string `json:"patch_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.PatchID == "" {
		return "", fmt.Errorf("patch_id is required")
	}
	result, err := s.store.LoadVerifierResultForPatch(ctx, params.PatchID)
	if err != nil {
		return "", fmt.Errorf("load verifier result: %w", err)
	}
	return marshalResult(result)
}

func (s *Server) toolGetBudgetForGoal(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		GoalID string `json:"goal_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.GoalID == "" {
		return "", fmt.Errorf("goal_id is required")
	}
	records, err := s.store.LoadBudgetForGoal(ctx, params.GoalID)
	if err != nil {
		return "", fmt.Errorf("load budget: %w", err)
	}
	if records == nil {
		records = []*schema.BudgetRecord{}
	}
	return marshalResult(records)
}

// mergeReadinessResult is returned by orca_get_merge_readiness.
type mergeReadinessResult struct {
	Status                  string `json:"status"`
	OpenBlockingObligations int    `json:"open_blocking_obligations"`
	PatchID                 string `json:"patch_id,omitempty"`
	PatchStatus             string `json:"patch_status,omitempty"`
	BlockingFailures        int    `json:"blocking_failures,omitempty"`
}

func (s *Server) toolGetMergeReadiness(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		GoalID  string `json:"goal_id"`
		PatchID string `json:"patch_id"`
	}
	if err := unmarshalArgs(args, &params); err != nil {
		return "", err
	}
	if params.GoalID == "" {
		return "", fmt.Errorf("goal_id is required")
	}

	obligations, err := s.store.LoadOpenObligations(ctx, params.GoalID)
	if err != nil {
		return "", fmt.Errorf("load open obligations: %w", err)
	}
	blockingCount := 0
	for _, o := range obligations {
		if o.Blocking {
			blockingCount++
		}
	}

	patchID := params.PatchID
	if patchID == "" {
		patchID, err = s.latestVerifiedPatchID(ctx, params.GoalID)
		if err != nil {
			return "", err
		}
	}

	if patchID == "" {
		return marshalResult(mergeReadinessResult{
			Status:                  "unknown",
			OpenBlockingObligations: blockingCount,
		})
	}

	verifierResult, err := s.store.LoadVerifierResultForPatch(ctx, patchID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("load verifier result for patch %s: %w", patchID, err)
		}
		return marshalResult(mergeReadinessResult{
			Status:                  "unknown",
			OpenBlockingObligations: blockingCount,
			PatchID:                 patchID,
		})
	}

	if blockingCount > 0 || len(verifierResult.BlockingFailures) > 0 {
		return marshalResult(mergeReadinessResult{
			Status:                  "blocked",
			OpenBlockingObligations: blockingCount,
			PatchID:                 patchID,
			BlockingFailures:        len(verifierResult.BlockingFailures),
		})
	}

	patch, err := s.store.LoadPatch(ctx, patchID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("load patch %s: %w", patchID, err)
		}
		return marshalResult(mergeReadinessResult{
			Status:                  "pending_reconciliation",
			OpenBlockingObligations: blockingCount,
			PatchID:                 patchID,
		})
	}

	status := "blocked"
	switch patch.Status {
	case schema.PatchAccepted:
		status = "ready"
	case schema.PatchCandidate:
		status = "pending_reconciliation"
	}
	return marshalResult(mergeReadinessResult{
		Status:                  status,
		OpenBlockingObligations: blockingCount,
		PatchID:                 patchID,
		PatchStatus:             string(patch.Status),
	})
}

// latestVerifiedPatchID scans the event log for the highest-sequence
// verifier_result_created event and returns its PatchID.
func (s *Server) latestVerifiedPatchID(ctx context.Context, goalID string) (string, error) {
	var patchID string
	var latestSeq int64
	var afterSeq int64
	for {
		events, err := s.log.ReadForGoal(ctx, goalID, afterSeq, 200)
		if err != nil {
			return "", fmt.Errorf("read events for verifier results: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Type == schema.EventVerifierResultCreated && ev.SequenceNum > latestSeq {
				var result schema.VerifierResult
				if err := json.Unmarshal(ev.Payload, &result); err == nil && result.PatchID != "" {
					latestSeq = ev.SequenceNum
					patchID = result.PatchID
				}
			}
		}
		afterSeq = events[len(events)-1].SequenceNum
	}
	return patchID, nil
}

func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, dst)
}

func marshalResult(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("mcp: marshal result: %w", err)
	}
	return string(b), nil
}
