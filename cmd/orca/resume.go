package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// CheckpointKind identifies the pipeline stage from which resume should continue.
// It is derived purely from durable artifacts and events; no explicit checkpoint
// events are written.
type CheckpointKind string

const (
	// CheckpointPlanFromStart: no capsules yet (or all capsules failed with no
	// patch). Resume by running the full plan loop.
	CheckpointPlanFromStart CheckpointKind = "plan_from_start"

	// CheckpointRunCapsules: one or more capsules are in a pending/active state
	// and have not produced a patch. Resume by running those capsules.
	CheckpointRunCapsules CheckpointKind = "run_capsules"

	// CheckpointVerifyPatches: at least one patch exists but has no verifier
	// result. Resume by running the verifier.
	CheckpointVerifyPatches CheckpointKind = "verify_patches"

	// CheckpointReconcile: all patches have verifier results but at least one
	// patch is still in candidate status (not accepted/rejected). Resume by
	// running the reconciler.
	CheckpointReconcile CheckpointKind = "reconcile"

	// CheckpointMergeGate: at least one patch was accepted and no merge_applied
	// event exists. Resume by running the merge gate.
	CheckpointMergeGate CheckpointKind = "merge_gate"

	// CheckpointFinalizeMerge: merge_applied events exist but the goal is still
	// active. Resume by idempotently applying accepted patches and marking the
	// goal complete without re-running gates or PR creation.
	CheckpointFinalizeMerge CheckpointKind = "finalize_merge"
)

// Checkpoint is the derived resume state for an active goal. It carries
// everything the resume path needs to continue without re-querying the store.
type Checkpoint struct {
	Kind   CheckpointKind
	GoalID string

	// CapsuleIDs: pending capsules that need to be run (CheckpointRunCapsules).
	CapsuleIDs []string
	// AbandonedCapsuleIDs: capsules in an active state with no patch.
	// The resume path marks these failed before re-running.
	AbandonedCapsuleIDs []string

	// PatchIDs: patches to verify/reconcile (CheckpointVerifyPatches / CheckpointReconcile).
	PatchIDs []string
	// AcceptedPatchIDs: patches in accepted status (CheckpointMergeGate).
	AcceptedPatchIDs []string

	// LastStep describes the last durably completed step (for display).
	LastStep string
	// NextStep describes what resume will do (for display).
	NextStep string
}

// deriveCheckpoint inspects the artifact graph and event log for goal and returns
// the earliest safe resume point. It is a pure read; it writes nothing.
func (rt *runtime) deriveCheckpoint(ctx context.Context, goal *schema.GoalIR) (Checkpoint, error) {
	cp := Checkpoint{GoalID: goal.GoalID}

	// Collect all capsule IDs ever created for this goal.
	capsuleIDs, err := rt.capsuleIDsForGoal(ctx, goal.GoalID)
	if err != nil {
		return cp, fmt.Errorf("deriveCheckpoint: capsule IDs for goal %s: %w", goal.GoalID, err)
	}

	if len(capsuleIDs) == 0 {
		cp.Kind = CheckpointPlanFromStart
		cp.LastStep = "obligations proposed"
		cp.NextStep = "plan topology and create capsules"
		return cp, nil
	}

	var (
		pendingCapsuleIDs   []string
		abandonedCapsuleIDs []string
		patchIDs            []string
	)

	for _, capsuleID := range capsuleIDs {
		capsule, loadErr := rt.store.LoadCapsule(ctx, capsuleID)
		if loadErr != nil {
			return cp, fmt.Errorf("deriveCheckpoint: load capsule %s: %w", capsuleID, loadErr)
		}

		// Only executor capsules produce patches. Non-executor capsules
		// (reviewer, tester) that completed without a patch are fine.
		if capsule.Role != schema.RoleExecutor {
			continue
		}

		switch {
		case capsule.State == schema.CapsuleStateCompleted:
			patches, pErr := rt.store.LoadPatchesForCapsule(ctx, capsuleID)
			if pErr != nil {
				return cp, fmt.Errorf("deriveCheckpoint: load patches for capsule %s: %w", capsuleID, pErr)
			}
			for _, p := range patches {
				patchIDs = append(patchIDs, p.PatchID)
			}

		case capsule.State == schema.CapsuleStatePending:
			pendingCapsuleIDs = append(pendingCapsuleIDs, capsuleID)

		case isActiveCapsule(capsule.State):
			// Active but not completed: check whether a patch was saved before the crash.
			patches, pErr := rt.store.LoadPatchesForCapsule(ctx, capsuleID)
			if pErr != nil {
				return cp, fmt.Errorf("deriveCheckpoint: load patches for capsule %s: %w", capsuleID, pErr)
			}
			if len(patches) > 0 {
				// Patch was saved; treat capsule as effectively completed.
				for _, p := range patches {
					patchIDs = append(patchIDs, p.PatchID)
				}
			} else {
				abandonedCapsuleIDs = append(abandonedCapsuleIDs, capsuleID)
			}

		case capsule.State == schema.CapsuleStateFailed:
			// Previously failed/abandoned; check for a patch anyway.
			patches, pErr := rt.store.LoadPatchesForCapsule(ctx, capsuleID)
			if pErr != nil {
				return cp, fmt.Errorf("deriveCheckpoint: load patches for capsule %s: %w", capsuleID, pErr)
			}
			for _, p := range patches {
				patchIDs = append(patchIDs, p.PatchID)
			}
		}
	}

	cp.AbandonedCapsuleIDs = abandonedCapsuleIDs

	// Case: capsules exist but none produced a patch yet.
	if len(patchIDs) == 0 {
		if len(pendingCapsuleIDs) > 0 || len(abandonedCapsuleIDs) > 0 {
			cp.Kind = CheckpointRunCapsules
			cp.CapsuleIDs = pendingCapsuleIDs
			cp.LastStep = "capsules created"
			cp.NextStep = fmt.Sprintf("run %d capsule(s) (abandon %d)", len(pendingCapsuleIDs)+len(abandonedCapsuleIDs), len(abandonedCapsuleIDs))
			return cp, nil
		}
		// All capsules failed with no patches; re-plan.
		cp.Kind = CheckpointPlanFromStart
		cp.LastStep = "all capsules failed with no patch"
		cp.NextStep = "re-plan and create new capsules"
		return cp, nil
	}

	// De-duplicate patch IDs (multiple capsules can claim the same obligations
	// in unusual scenarios; use the first occurrence).
	patchIDs = dedupStrings(patchIDs)
	cp.PatchIDs = patchIDs

	// Check verification status.
	var unverifiedPatchIDs []string
	for _, patchID := range patchIDs {
		_, vrErr := rt.store.LoadVerifierResultForPatch(ctx, patchID)
		if errors.Is(vrErr, store.ErrNotFound) {
			unverifiedPatchIDs = append(unverifiedPatchIDs, patchID)
		} else if vrErr != nil {
			return cp, fmt.Errorf("deriveCheckpoint: load verifier result for patch %s: %w", patchID, vrErr)
		}
	}

	if len(unverifiedPatchIDs) > 0 {
		cp.Kind = CheckpointVerifyPatches
		cp.PatchIDs = patchIDs
		cp.LastStep = "patch(es) created"
		cp.NextStep = fmt.Sprintf("verify %d patch(es)", len(unverifiedPatchIDs))
		return cp, nil
	}

	// All patches verified. Check reconciliation status.
	var acceptedPatchIDs []string
	var candidatePatchIDs []string
	var unreconciled bool
	for _, patchID := range patchIDs {
		patch, pErr := rt.store.LoadPatch(ctx, patchID)
		if pErr != nil {
			return cp, fmt.Errorf("deriveCheckpoint: load patch %s: %w", patchID, pErr)
		}
		switch patch.Status {
		case schema.PatchAccepted:
			acceptedPatchIDs = append(acceptedPatchIDs, patchID)
		case schema.PatchCandidate:
			candidatePatchIDs = append(candidatePatchIDs, patchID)
			unreconciled = true
		}
	}
	cp.AcceptedPatchIDs = acceptedPatchIDs

	if unreconciled {
		cp.Kind = CheckpointReconcile
		cp.PatchIDs = append(append([]string(nil), acceptedPatchIDs...), candidatePatchIDs...)
		cp.LastStep = "verification complete"
		cp.NextStep = "reconcile patches"
		return cp, nil
	}

	if len(acceptedPatchIDs) > 0 {
		// Check if merge was already applied.
		for _, patchID := range acceptedPatchIDs {
			applied, mErr := rt.hasMergeAppliedEvent(ctx, goal.GoalID, patchID)
			if mErr != nil {
				return cp, mErr
			}
			if !applied {
				cp.Kind = CheckpointMergeGate
				cp.AcceptedPatchIDs = acceptedPatchIDs
				cp.LastStep = "patch(es) accepted"
				cp.NextStep = "run merge gate"
				return cp, nil
			}
		}
		// Merge already applied; goal should have been marked complete.
		cp.Kind = CheckpointFinalizeMerge
		cp.AcceptedPatchIDs = acceptedPatchIDs
		cp.LastStep = "merge applied (goal status not updated)"
		cp.NextStep = "finalize merge and update goal status"
		return cp, nil
	}

	// Patches all rejected or unexpected state — let the plan loop re-plan.
	cp.Kind = CheckpointPlanFromStart
	cp.LastStep = "patches rejected or unknown status"
	cp.NextStep = "re-plan with follow-up obligations"
	return cp, nil
}

// capsuleIDsForGoal scans the event log for capsule_created events and returns
// all capsule IDs associated with the goal in creation order.
func (rt *runtime) capsuleIDsForGoal(ctx context.Context, goalID string) ([]string, error) {
	var ids []string
	seen := make(map[string]bool)
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return nil, fmt.Errorf("capsuleIDsForGoal: read events: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Type == schema.EventCapsuleCreated && ev.ArtifactID != "" && !seen[ev.ArtifactID] {
				seen[ev.ArtifactID] = true
				ids = append(ids, ev.ArtifactID)
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
	return ids, nil
}

// hasMergeAppliedEvent returns true if a merge_applied event exists in the log
// for the given patchID under goalID.
func (rt *runtime) hasMergeAppliedEvent(ctx context.Context, goalID, patchID string) (bool, error) {
	var seq int64
	for {
		events, err := rt.eventLog.ReadForGoal(ctx, goalID, seq, 200)
		if err != nil {
			return false, fmt.Errorf("hasMergeAppliedEvent: read events: %w", err)
		}
		if len(events) == 0 {
			return false, nil
		}
		for _, ev := range events {
			if ev.Type == schema.EventMergeApplied && ev.ArtifactID == patchID {
				return true, nil
			}
		}
		seq = events[len(events)-1].SequenceNum
	}
}

// markCapsulesAbandoned transitions each capsule ID to CapsuleStateFailed and
// appends a capsule_state_updated event so replay reconstructs the failed state.
func (rt *runtime) markCapsulesAbandoned(ctx context.Context, goalID string, capsuleIDs []string) error {
	for _, capsuleID := range capsuleIDs {
		payload, err := json.Marshal(schema.CapsuleTransitionPayload{
			CapsuleID: capsuleID,
			State:     schema.CapsuleStateFailed,
		})
		if err != nil {
			return fmt.Errorf("markCapsulesAbandoned: marshal payload for %s: %w", capsuleID, err)
		}
		if _, err := rt.eventLog.Append(ctx, schema.Event{
			Type:       schema.EventCapsuleStateUpdated,
			GoalID:     goalID,
			ArtifactID: capsuleID,
			Payload:    payload,
		}); err != nil {
			return fmt.Errorf("markCapsulesAbandoned: append event for %s: %w", capsuleID, err)
		}
		if err := rt.store.UpdateCapsuleState(ctx, capsuleID, schema.CapsuleStateFailed); err != nil {
			return fmt.Errorf("markCapsulesAbandoned: update state for %s: %w", capsuleID, err)
		}
	}
	return nil
}

// resumeFromCheckpoint re-enters the pipeline at the checkpoint derived from
// the active goal's artifact graph.
func (rt *runtime) resumeFromCheckpoint(ctx context.Context, cp Checkpoint) error {
	goal, err := rt.store.LoadGoal(ctx, cp.GoalID)
	if err != nil {
		return fmt.Errorf("resume: load goal %s: %w", cp.GoalID, err)
	}

	rt.emit(ctx, UIEvent{
		Kind:    EventKindSetupReady,
		GoalID:  cp.GoalID,
		Summary: fmt.Sprintf("resume: %s → %s", cp.LastStep, cp.NextStep),
	})

	switch cp.Kind {
	case CheckpointPlanFromStart:
		// Mark any abandoned capsules failed so the planner creates fresh ones.
		if err := rt.markCapsulesAbandoned(ctx, cp.GoalID, cp.AbandonedCapsuleIDs); err != nil {
			return err
		}
		return rt.runPlanLoop(ctx, goal.GoalID)

	case CheckpointRunCapsules:
		// Mark abandoned capsules failed; run remaining pending capsules.
		if err := rt.markCapsulesAbandoned(ctx, cp.GoalID, cp.AbandonedCapsuleIDs); err != nil {
			return err
		}
		if len(cp.CapsuleIDs) == 0 {
			// All capsules were abandoned; fall back to full re-plan.
			return rt.runPlanLoop(ctx, goal.GoalID)
		}
		return rt.runExistingCapsules(ctx, goal, cp.CapsuleIDs)

	case CheckpointVerifyPatches:
		result, err := rt.runVerifyAndMerge(ctx, goal, cp.PatchIDs, nil, nil)
		if err != nil {
			return err
		}
		if result.GoalComplete {
			return nil
		}
		if len(result.FollowUpObligationIDs) > 0 {
			return rt.runPlanLoop(ctx, goal.GoalID)
		}
		return fmt.Errorf("orca resume: reconciliation stopped: %s", result.BlockingReason)

	case CheckpointReconcile:
		result, err := rt.runVerifyAndMerge(ctx, goal, cp.PatchIDs, nil, nil)
		if err != nil {
			return err
		}
		if result.GoalComplete {
			return nil
		}
		if len(result.FollowUpObligationIDs) > 0 {
			return rt.runPlanLoop(ctx, goal.GoalID)
		}
		return fmt.Errorf("orca resume: reconciliation stopped: %s", result.BlockingReason)

	case CheckpointMergeGate:
		return rt.runMergeGateAndApply(ctx, goal, cp.AcceptedPatchIDs)

	case CheckpointFinalizeMerge:
		return rt.finalizeAppliedMerge(ctx, goal, cp.AcceptedPatchIDs)

	default:
		return fmt.Errorf("orca resume: unknown checkpoint kind %q", cp.Kind)
	}
}

// runExistingCapsules runs a set of capsules that already exist in the store
// (created by the planner in a prior interrupted run). It skips projection
// compilation and gate review if those steps already completed. After running,
// it delegates to runVerifyAndMerge.
func (rt *runtime) runExistingCapsules(ctx context.Context, goal *schema.GoalIR, capsuleIDs []string) error {
	// Determine topology from the first capsule's topology decision.
	var topology schema.Topology
	var maxRisk schema.RiskLevel
	if len(capsuleIDs) > 0 {
		c, err := rt.store.LoadCapsule(ctx, capsuleIDs[0])
		if err == nil && c.TopologyDecisionID != "" {
			dec, decErr := rt.store.LoadDecision(ctx, c.TopologyDecisionID)
			if decErr == nil {
				topology = schema.Topology(dec.Decision)
			}
		}
	}
	obligations, _ := rt.store.LoadOpenObligations(ctx, goal.GoalID)
	maxRisk = maxObligationRisk(obligations)

	var patchIDs []string
	var supplementalEvidenceIDs []string
	var supplementalClaimIDs []string

	for _, capsuleID := range capsuleIDs {
		capsule, err := rt.store.LoadCapsule(ctx, capsuleID)
		if err != nil {
			return fmt.Errorf("resume runExistingCapsules: load capsule %s: %w", capsuleID, err)
		}

		// Guard: skip if patch already exists.
		patches, _ := rt.store.LoadPatchesForCapsule(ctx, capsuleID)
		if len(patches) > 0 {
			if capsule.Role == schema.RoleExecutor {
				patchIDs = append(patchIDs, patches[0].PatchID)
			}
			continue
		}

		rt.emit(ctx, UIEvent{Kind: EventKindCapsuleCreated, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": resuming"})

		// Guard: compile human summary only if not already done.
		if _, projErr := rt.store.LoadHumanSummaryProjectionForCapsule(ctx, capsuleID); errors.Is(projErr, store.ErrNotFound) {
			if _, err := rt.projector.CompileHumanSummary(ctx, capsuleID); err != nil {
				return fmt.Errorf("resume: compile human summary for capsule %s: %w", capsuleID, err)
			}
		}

		// Guard: skip projection gate if decision already exists.
		if capsule.Role == schema.RoleExecutor {
			shouldGate := topology != "" && schema.Topology(topology) != schema.TopologySingle ||
				(topology == "" && maxRisk >= schema.RiskMedium)
			// Use the canonical gate check when topology is known.
			if topology != "" {
				shouldGate = isGateRequired(topology, maxRisk)
			}
			if shouldGate {
				alreadyDecided, gErr := rt.hasGateDecision(ctx, goal.GoalID, "projection_review", capsuleID)
				if gErr != nil {
					return fmt.Errorf("resume: check gate decision for capsule %s: %w", capsuleID, gErr)
				}
				if !alreadyDecided {
					reviewWindow := time.Duration(rt.cfg.Gate.ReviewWindowSeconds) * time.Second
					rt.emit(ctx, UIEvent{Kind: EventKindCapsuleWaitingForGate, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": awaiting projection review"})
					decision, gErr := rt.gatekeeper.ReviewProjection(ctx, capsuleID, reviewWindow)
					if gErr != nil {
						return gErr
					}
					if !decision.Approved {
						return fmt.Errorf("orca resume: projection gate rejected capsule %s: %s", capsuleID, decision.Notes)
					}
				}
			}
		}

		// Budget check (idempotent).
		rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": checking budget"})
		check, err := rt.budget.CheckCapsuleBudget(ctx, capsuleID)
		if err != nil {
			return err
		}
		if !check.Allowed {
			return fmt.Errorf("orca resume: budget rejected capsule %s: %s", capsuleID, check.Reason)
		}

		// Guard: compile executor projection only if not already linked.
		if capsule.ContextProjectionID == "" {
			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": compiling agent projection"})
			agentProjection, pErr := rt.compileAgentProjection(ctx, capsule)
			if pErr != nil {
				return fmt.Errorf("resume: compile executor projection for capsule %s: %w", capsuleID, pErr)
			}
			if err := rt.store.UpdateCapsuleProjectionID(ctx, capsuleID, agentProjection.ContextProjectionID); err != nil {
				return fmt.Errorf("resume: link projection for capsule %s: %w", capsuleID, err)
			}
		}

		rt.emit(ctx, UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": running agent"})
		runResult, err := rt.runner.Run(ctx, capsuleID)
		if err != nil {
			rt.emit(ctx, UIEvent{Kind: EventKindCapsuleFailed, GoalID: goal.GoalID, CapsuleID: capsuleID, Summary: "capsule " + capsuleID + ": failed", Detail: err.Error(), Status: "failed", Severity: "error"})
			return err
		}
		rt.emit(ctx, UIEvent{Kind: EventKindCapsuleCompleted, GoalID: goal.GoalID, CapsuleID: capsuleID, PatchID: runResult.PatchID, Summary: "capsule " + capsuleID + ": completed", Status: "completed"})

		if capsule.Role != schema.RoleExecutor {
			supplementalEvidenceIDs = append(supplementalEvidenceIDs, runResult.EvidenceIDs...)
			supplementalClaimIDs = append(supplementalClaimIDs, runResult.ClaimIDs...)
			continue
		}
		if runResult.PatchID != "" {
			patchIDs = append(patchIDs, runResult.PatchID)
		}
	}

	if len(patchIDs) == 0 {
		return fmt.Errorf("orca resume: capsules produced no patch for goal %s", goal.GoalID)
	}

	result, err := rt.runVerifyAndMerge(ctx, goal, patchIDs, supplementalEvidenceIDs, supplementalClaimIDs)
	if err != nil {
		return err
	}
	if result.GoalComplete {
		return nil
	}
	if len(result.FollowUpObligationIDs) > 0 {
		return rt.runPlanLoop(ctx, goal.GoalID)
	}
	return fmt.Errorf("orca resume: reconciliation stopped: %s", result.BlockingReason)
}

// runMergeGateAndApply handles CheckpointMergeGate: presents the merge gate for
// the last accepted patch and applies all accepted patches.
func (rt *runtime) runMergeGateAndApply(ctx context.Context, goal *schema.GoalIR, acceptedPatchIDs []string) error {
	if len(acceptedPatchIDs) == 0 {
		return fmt.Errorf("orca resume: no accepted patches to merge for goal %s", goal.GoalID)
	}
	lastPatchID := acceptedPatchIDs[len(acceptedPatchIDs)-1]

	rt.emit(ctx, UIEvent{Kind: EventKindMergeReady, GoalID: goal.GoalID, PatchID: lastPatchID, Summary: "patch " + lastPatchID + ": merge ready (resumed)", Status: "ready"})

	decision, err := rt.gatekeeper.ReviewMerge(ctx, lastPatchID)
	if err != nil {
		return err
	}
	if !decision.Approved {
		return fmt.Errorf("orca resume: merge gate rejected patch %s: %s", lastPatchID, decision.Notes)
	}
	for _, pid := range acceptedPatchIDs {
		if err := rt.applyPatchToWorkDir(ctx, pid); err != nil {
			return fmt.Errorf("orca resume: apply patch %s: %w", pid, err)
		}
	}
	prExists, err := rt.hasPRCreatedForPatch(ctx, goal.GoalID, lastPatchID)
	if err != nil {
		return err
	}
	if rt.cfg.PR.Enabled && !prExists {
		if err := rt.createAndSavePR(ctx, goal.GoalID, lastPatchID); err != nil {
			return fmt.Errorf("orca resume: create pr for patch %s: %w", lastPatchID, err)
		}
	}
	for _, pid := range acceptedPatchIDs {
		if err := rt.appendMergeApplied(ctx, goal.GoalID, pid); err != nil {
			return err
		}
	}
	return rt.updateGoalStatus(ctx, goal.GoalID, schema.GoalStatusComplete)
}

func (rt *runtime) finalizeAppliedMerge(ctx context.Context, goal *schema.GoalIR, acceptedPatchIDs []string) error {
	if len(acceptedPatchIDs) == 0 {
		return fmt.Errorf("orca resume: no accepted patches to finalize for goal %s", goal.GoalID)
	}
	for _, pid := range acceptedPatchIDs {
		if err := rt.applyPatchToWorkDir(ctx, pid); err != nil {
			return fmt.Errorf("orca resume: finalize apply patch %s: %w", pid, err)
		}
	}
	return rt.updateGoalStatus(ctx, goal.GoalID, schema.GoalStatusComplete)
}

// isGateRequired is the canonical check used by the resume path when the
// topology is known. It mirrors the condition in runPlanLoop.
func isGateRequired(topology schema.Topology, maxRisk schema.RiskLevel) bool {
	switch topology {
	case schema.TopologyHumanGated:
		return true
	case schema.TopologyImplementerReviewer:
		return maxRisk >= schema.RiskMedium
	default:
		return false
	}
}

// showActiveGoalResumePrompt prints a concise summary of the active goal and
// its checkpoint, then writes available options to out. It does not block.
func (rt *runtime) showActiveGoalResumePrompt(_ context.Context, out io.Writer, goal *schema.GoalIR, cp Checkpoint) {
	fmt.Fprintf(out, "\nActive goal detected (status: %s)\n", goal.Status)
	fmt.Fprintf(out, "  Intent: %q\n", goal.OriginalIntent)
	fmt.Fprintf(out, "  Goal ID: %s\n", goal.GoalID)
	fmt.Fprintf(out, "  Last step: %s\n", cp.LastStep)
	fmt.Fprintf(out, "  Resume from: %s\n\n", cp.NextStep)
	if len(cp.AbandonedCapsuleIDs) > 0 {
		fmt.Fprintf(out, "  Abandoned capsules: %s\n", strings.Join(cp.AbandonedCapsuleIDs, ", "))
	}
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  resume   — continue from last checkpoint")
	fmt.Fprintln(out, "  cancel   — cancel this goal")
	fmt.Fprintln(out, "  status   — show detailed status")
	fmt.Fprintln(out, "  exit     — exit without changing goal state")
}

// runResume is the entry point for `orca resume`.
func runResume(args []string) (err error) {
	fs := flag.NewFlagSet("orca resume", flag.ContinueOnError)
	orcaDir := fs.String("orca-dir", "", "path to the .orca directory")
	flagPlain := fs.Bool("plain", false, "use plain text output")
	flagJSON := fs.Bool("json", false, "emit lifecycle events as JSONL to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *orcaDir == "" {
		*orcaDir = filepath.Join(findProjectRoot("."), ".orca")
	}

	rt, closeFn, err := openRuntime(*orcaDir, false)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeFn(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	switch {
	case *flagJSON:
		rt.notifier = newJSONNotifier(os.Stderr)
	case *flagPlain:
		rt.notifier = newPlainNotifier(os.Stderr, false)
	default:
		if isatty(os.Stderr) {
			rt.notifier = newLiveRenderer(os.Stderr)
		} else {
			rt.notifier = newPlainNotifier(os.Stderr, false)
		}
	}

	ctx := context.Background()
	goal, err := rt.store.LoadActiveGoal(ctx)
	if err != nil {
		return fmt.Errorf("orca resume: load active goal: %w", err)
	}
	if goal == nil {
		return fmt.Errorf("orca resume: no active goal to resume")
	}

	cp, err := rt.deriveCheckpoint(ctx, goal)
	if err != nil {
		return fmt.Errorf("orca resume: derive checkpoint: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Resuming goal %s from: %s\n", goal.GoalID, cp.NextStep)
	return rt.resumeFromCheckpoint(ctx, cp)
}

// dedupStrings returns a new slice with duplicates removed, preserving order.
func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
