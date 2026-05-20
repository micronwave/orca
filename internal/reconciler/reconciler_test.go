package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

type testEnv struct {
	ctx context.Context
	log *eventlog.FileLog
	st  *store.FileStore
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.log"))
	if err != nil {
		t.Fatalf("Open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	st, err := store.New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &testEnv{ctx: context.Background(), log: log, st: st}
}

func TestReconcileRejectsBlockingObligationWithoutEvidenceIDs(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "NOEVID",
		evidenceIDs:  nil,
		saveEvidence: false,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	wantReason := "blocking obligation OB-NOEVID has no evidence IDs"
	if result.BlockingReason != wantReason {
		t.Fatalf("BlockingReason = %q, want %q", result.BlockingReason, wantReason)
	}

	patch, err := env.st.LoadPatch(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("LoadPatch: %v", err)
	}
	if patch.Status != schema.PatchRejected {
		t.Fatalf("patch status = %s, want %s", patch.Status, schema.PatchRejected)
	}
}

func TestReconcileRejectsAbsentEvidenceArtifact(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "GHOST",
		evidenceIDs:  []string{"EV-GHOST"},
		saveEvidence: false,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	if !strings.Contains(result.BlockingReason, "references absent evidence artifact EV-GHOST") {
		t.Fatalf("BlockingReason = %q, want absent evidence reason", result.BlockingReason)
	}
}

func TestReconcileRejectsClaimedBlockingObligationWithoutVerdict(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "MISSVERDICT",
		evidenceIDs:  nil,
		saveEvidence: false,
		omitVerdict:  true,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted = true, want false")
	}
	wantReason := "patch PATCH-MISSVERDICT has no obligation verdicts"
	if result.BlockingReason != wantReason {
		t.Fatalf("BlockingReason = %q, want %q", result.BlockingReason, wantReason)
	}
}

func TestReconcileAcceptsWhenNonBlockingObligationHasGhostEvidence(t *testing.T) {
	// Ghost evidence on a non-blocking obligation must not block acceptance.
	// The invariant "must not accept without evidence for every blocking obligation"
	// applies only to blocking obligations (orca.md §11, module_boundaries.md).
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID  = "G-NBGHOST"
		condID  = "GC-NBGHOST"
		oblID   = "OB-NBGHOST"
		capsID  = "CAP-NBGHOST"
		patchID = "PATCH-NBGHOST"
		vrID    = "VR-NBGHOST"
		ghostID = "EV-NBGHOST-GHOST"
	)
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test non-blocking ghost evidence",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "scope check",
		EvidenceRequired: []string{string(schema.EvidenceDiffRiskReport)},
		Blocking:         false,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsID,
		ObligationIDs: []string{oblID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	// ghostID is listed in the verdict but the artifact is never saved to the store.
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{ghostID},
		}},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "scope check passed per agent report",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	result, err := New(env.st, env.log).Reconcile(env.ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted = false, reason=%q; ghost evidence on a non-blocking "+
			"obligation must not block patch acceptance", result.BlockingReason)
	}

	// The obligation itself must be marked failed (evidence is absent),
	// but the patch is accepted because the obligation is non-blocking.
	obl, err := env.st.LoadObligation(env.ctx, oblID)
	if err != nil {
		t.Fatalf("LoadObligation: %v", err)
	}
	if obl.Status != schema.ObligationFailed {
		t.Fatalf("obligation status = %s, want failed (ghost evidence marks obligation failed even when non-blocking)", obl.Status)
	}
}

func TestReconcileAcceptsPatchAndSnapshotsLastPreSnapshotEvent(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "OK",
		evidenceIDs:  []string{"EV-OK"},
		saveEvidence: true,
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted = false, reason=%q", result.BlockingReason)
	}
	if !result.MergeReady {
		t.Fatalf("MergeReady = false, reason=%q", result.BlockingReason)
	}

	obligation, err := env.st.LoadObligation(env.ctx, ids.obligationID)
	if err != nil {
		t.Fatalf("LoadObligation: %v", err)
	}
	if obligation.Status != schema.ObligationSatisfied {
		t.Fatalf("obligation status = %s, want %s", obligation.Status, schema.ObligationSatisfied)
	}
	if len(obligation.SatisfiedBy) != 1 || obligation.SatisfiedBy[0] != ids.evidenceID {
		t.Fatalf("SatisfiedBy = %v, want [%s]", obligation.SatisfiedBy, ids.evidenceID)
	}

	snapshot, err := env.st.LoadLatestSnapshot(env.ctx, ids.goalID)
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	decisionEvents, err := env.log.ReadByType(env.ctx, schema.EventDecisionRecordCreated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType decision: %v", err)
	}
	if len(decisionEvents) != 1 {
		t.Fatalf("decision events = %d, want 1", len(decisionEvents))
	}
	if snapshot.EventID != decisionEvents[0].EventID || snapshot.SequenceNum != decisionEvents[0].SequenceNum {
		t.Fatalf("snapshot anchored at event (%s,%d), want decision event (%s,%d)",
			snapshot.EventID, snapshot.SequenceNum, decisionEvents[0].EventID, decisionEvents[0].SequenceNum)
	}

	mergeEvents, err := env.log.ReadByType(env.ctx, schema.EventMergeApplied, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType merge: %v", err)
	}
	if len(mergeEvents) != 1 {
		t.Fatalf("merge_applied events = %d, want 1", len(mergeEvents))
	}
}

func TestReconcileAcceptedPatchMarksOverlappingVerifiedClaimsStale(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "STALEFILE",
		evidenceIDs:  []string{"EV-STALEFILE"},
		saveEvidence: true,
		changedFiles: []string{"internal/foo/service.go"},
	})
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-OLD-STALEFILE",
		ObligationIDs: []string{ids.obligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-OLD-STALEFILE",
		Text:            "legacy claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: "CAP-OLD-STALEFILE",
		AffectedFiles:   []string{`internal\foo\service.go`},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-NEW-STALEFILE",
		Text:            "current claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/foo/service.go"},
		Status:          schema.ClaimProposed,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim new: %v", err)
	}

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted=false, reason=%q", result.BlockingReason)
	}
	oldClaim, err := env.st.LoadClaim(env.ctx, "CL-OLD-STALEFILE")
	if err != nil {
		t.Fatalf("LoadClaim old: %v", err)
	}
	if oldClaim.Status != schema.ClaimStale {
		t.Fatalf("old claim status = %s, want %s", oldClaim.Status, schema.ClaimStale)
	}
	newClaim, err := env.st.LoadClaim(env.ctx, "CL-NEW-STALEFILE")
	if err != nil {
		t.Fatalf("LoadClaim new: %v", err)
	}
	if newClaim.Status != schema.ClaimVerified {
		t.Fatalf("new claim status = %s, want %s", newClaim.Status, schema.ClaimVerified)
	}

	events, err := env.log.ReadByType(env.ctx, schema.EventClaimStatusUpdated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType claim_status_updated: %v", err)
	}
	seen := map[string]schema.ClaimStatus{}
	for _, event := range events {
		var payload schema.ClaimStatusPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("Unmarshal claim_status_updated payload: %v", err)
		}
		seen[payload.ClaimID] = payload.Status
	}
	if seen["CL-OLD-STALEFILE"] != schema.ClaimStale {
		t.Fatalf("CL-OLD-STALEFILE event status = %s, want %s", seen["CL-OLD-STALEFILE"], schema.ClaimStale)
	}
	if seen["CL-NEW-STALEFILE"] != schema.ClaimVerified {
		t.Fatalf("CL-NEW-STALEFILE event status = %s, want %s", seen["CL-NEW-STALEFILE"], schema.ClaimVerified)
	}
}

func TestReconcileRejectedPatchDoesNotInvalidateHistoricalClaims(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "NOSTALE",
		evidenceIDs:  []string{"EV-NOSTALE-MISSING"},
		saveEvidence: false,
		changedFiles: []string{"internal/foo/service.go"},
	})
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-OLD-NOSTALE",
		ObligationIDs: []string{ids.obligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-OLD-NOSTALE",
		Text:            "legacy claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: "CAP-OLD-NOSTALE",
		AffectedFiles:   []string{"internal/foo/service.go"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted=true, want false")
	}
	claim, err := env.st.LoadClaim(env.ctx, "CL-OLD-NOSTALE")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status != schema.ClaimVerified {
		t.Fatalf("claim status = %s, want %s", claim.Status, schema.ClaimVerified)
	}
}

func TestReconcileInvalidatesClaimsOnSymbolOverlap(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "STALESYM",
		evidenceIDs:  []string{"EV-STALESYM"},
		saveEvidence: true,
		changedFiles: []string{"internal/new/location.go"},
	})
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     "CAP-OLD-STALESYM",
		ObligationIDs: []string{ids.obligationID},
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-OLD-STALESYM",
		Text:            "legacy symbol claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: "CAP-OLD-STALESYM",
		AffectedFiles:   []string{"internal/other/file.go"},
		AffectedSymbols: []string{"Service.Apply"},
		Status:          schema.ClaimVerified,
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-NEW-STALESYM",
		Text:            "new symbol claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedSymbols: []string{"service.apply"},
		Status:          schema.ClaimProposed,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim new: %v", err)
	}

	if _, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	claim, err := env.st.LoadClaim(env.ctx, "CL-OLD-STALESYM")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status != schema.ClaimStale {
		t.Fatalf("claim status = %s, want %s", claim.Status, schema.ClaimStale)
	}
}

func TestReconcile_RecommendedHumanReviewRequiresGate(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:            "HUMANREVIEW",
		evidenceIDs:       []string{"EV-HUMANREVIEW"},
		saveEvidence:      true,
		recommendedAction: schema.ActionHumanReview,
		recommendation:    "reviewer identified unresolved risk claim",
	})

	result, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted=false, reason=%q", result.BlockingReason)
	}
	if !result.MergeReady {
		t.Fatalf("MergeReady=false, reason=%q", result.BlockingReason)
	}
	if !result.HumanGateRequired {
		t.Fatal("HumanGateRequired=false, want true when verifier recommends human_review")
	}
}

func TestReconcile_WritesPerObligationBudgetRecord(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "BUDOBL",
		evidenceIDs:  []string{"EV-BUDOBL"},
		saveEvidence: true,
	})

	if _, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	records, err := env.st.LoadBudgetForGoal(env.ctx, ids.goalID)
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	var hasSummary bool
	var obligationRecord *schema.BudgetRecord
	for _, record := range records {
		if record.BudgetID == "BUD-"+ids.capsuleID && record.ObligationID == "" {
			hasSummary = true
			continue
		}
		if record.ObligationID == ids.obligationID {
			obligationRecord = record
		}
	}
	if !hasSummary {
		t.Fatal("missing capsule summary budget record")
	}
	if obligationRecord == nil {
		t.Fatalf("missing per-obligation budget record for %s", ids.obligationID)
	}
	if obligationRecord.ToolCalls == 0 {
		t.Fatalf("obligation ToolCalls = %d, want > 0", obligationRecord.ToolCalls)
	}
	if obligationRecord.ObligationsDischarged != 1 {
		t.Fatalf("obligation ObligationsDischarged = %d, want 1", obligationRecord.ObligationsDischarged)
	}
}

func TestReconcile_BudgetCountsReusedEvidenceByReusedFromID(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:               "BUDREUSE",
		evidenceIDs:          []string{"EV-BUDREUSE"},
		saveEvidence:         true,
		evidenceReusedFromID: "EV-BUDREUSE-SOURCE",
	})

	if _, err := New(env.st, env.log).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	records, err := env.st.LoadBudgetForGoal(env.ctx, ids.goalID)
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	var summary *schema.BudgetRecord
	var obligationRecord *schema.BudgetRecord
	for _, record := range records {
		if record.BudgetID == "BUD-"+ids.capsuleID && record.ObligationID == "" {
			summary = record
		}
		if record.ObligationID == ids.obligationID {
			obligationRecord = record
		}
	}
	if summary == nil || obligationRecord == nil {
		t.Fatalf("missing budget records: summary=%v obligation=%v", summary != nil, obligationRecord != nil)
	}
	if summary.EvidenceArtifactsReused != 1 {
		t.Fatalf("summary EvidenceArtifactsReused = %d, want 1", summary.EvidenceArtifactsReused)
	}
	if obligationRecord.EvidenceArtifactsReused != 1 {
		t.Fatalf("obligation EvidenceArtifactsReused = %d, want 1", obligationRecord.EvidenceArtifactsReused)
	}
}

func TestReconcile_DistributesTokensWithoutOvercount(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID   = "G-BUDTOKENS"
		condID   = "GC-BUDTOKENS"
		capsID   = "CAP-BUDTOKENS"
		patchID  = "PATCH-BUDTOKENS"
		vrID     = "VR-BUDTOKENS"
		totalTok = 5
	)
	obligationIDs := []string{"OB-BUDTOKENS-1", "OB-BUDTOKENS-2", "OB-BUDTOKENS-3"}
	evidenceIDs := []string{"EV-BUDTOKENS-1", "EV-BUDTOKENS-2", "EV-BUDTOKENS-3"}

	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test token distribution",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	for i, obligationID := range obligationIDs {
		if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
			ObligationID:     obligationID,
			GoalConditionID:  condID,
			Description:      "run tests",
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			Blocking:         true,
			RiskLevel:        schema.RiskLow,
			Status:           schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("SaveObligation[%d]: %v", i, err)
		}
		if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
			EvidenceID: evidenceIDs[i],
			Type:       schema.EvidenceTestResult,
			Command:    "go test ./...",
			ExitCode:   0,
			Supports:   []string{obligationID},
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("SaveEvidence[%d]: %v", i, err)
		}
	}

	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsID,
		ObligationIDs: append([]string(nil), obligationIDs...),
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: append([]string(nil), obligationIDs...),
		Status:               schema.PatchCandidate,
		TokensUsed:           totalTok,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	verdicts := make([]schema.ObligationVerdict, 0, len(obligationIDs))
	for i, obligationID := range obligationIDs {
		verdicts = append(verdicts, schema.ObligationVerdict{
			ObligationID: obligationID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evidenceIDs[i]},
		})
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID:        vrID,
		PatchID:                 patchID,
		CapsuleID:               capsID,
		ObligationResults:       verdicts,
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "tests passed",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	if _, err := New(env.st, env.log).Reconcile(env.ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	records, err := env.st.LoadBudgetForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	summaryID := "BUD-" + capsID
	recordsByID := make(map[string]*schema.BudgetRecord, len(records))
	for _, record := range records {
		recordsByID[record.BudgetID] = record
	}
	summary := recordsByID[summaryID]
	if summary == nil {
		t.Fatalf("missing summary budget record %s", summaryID)
	}
	if summary.TokensSpent != totalTok {
		t.Fatalf("summary TokensSpent = %d, want %d", summary.TokensSpent, totalTok)
	}

	var perObligationTotal int
	for _, obligationID := range obligationIDs {
		recordID := "BUD-" + capsID + "-" + obligationID
		record := recordsByID[recordID]
		if record == nil {
			t.Fatalf("missing per-obligation record %s", recordID)
		}
		perObligationTotal += record.TokensSpent
	}
	if perObligationTotal != totalTok {
		t.Fatalf("per-obligation token total = %d, want %d", perObligationTotal, totalTok)
	}
}

type scenarioOptions struct {
	suffix               string
	evidenceIDs          []string
	saveEvidence         bool
	evidenceReusedFromID string
	omitVerdict          bool
	changedFiles         []string
	recommendedAction    schema.RecommendedAction
	recommendation       string
}

type scenarioIDs struct {
	goalID, conditionID, obligationID string
	capsuleID, patchID, evidenceID    string
	verifierResultID                  string
}

func saveReconcileScenario(t *testing.T, env *testEnv, opts scenarioOptions) scenarioIDs {
	t.Helper()
	now := time.Now().UTC()
	ids := scenarioIDs{
		goalID:           "G-" + opts.suffix,
		conditionID:      "GC-" + opts.suffix,
		obligationID:     "OB-" + opts.suffix,
		capsuleID:        "CAP-" + opts.suffix,
		patchID:          "PATCH-" + opts.suffix,
		verifierResultID: "VR-" + opts.suffix,
	}
	if len(opts.evidenceIDs) > 0 {
		ids.evidenceID = opts.evidenceIDs[0]
	}
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         ids.goalID,
		OriginalIntent: "test reconcile",
		GoalConditions: []schema.GoalCondition{{
			ID:                   ids.conditionID,
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     ids.obligationID,
		GoalConditionID:  ids.conditionID,
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:     ids.capsuleID,
		ObligationIDs: []string{ids.obligationID},
		Agent:         schema.AgentCodex,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if opts.saveEvidence {
		if ids.evidenceID == "" {
			t.Fatal("saveEvidence requires an evidence ID")
		}
		if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
			EvidenceID:   ids.evidenceID,
			Type:         schema.EvidenceTestResult,
			Command:      "go test ./...",
			ExitCode:     0,
			Supports:     []string{ids.obligationID},
			ReusedFromID: opts.evidenceReusedFromID,
			CreatedAt:    now,
		}); err != nil {
			t.Fatalf("SaveEvidence: %v", err)
		}
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              ids.patchID,
		CapsuleID:            ids.capsuleID,
		ChangedFiles:         append([]string(nil), opts.changedFiles...),
		ObligationIDsClaimed: []string{ids.obligationID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	verdicts := []schema.ObligationVerdict{{
		ObligationID: ids.obligationID,
		Verdict:      schema.VerdictSatisfied,
		EvidenceIDs:  opts.evidenceIDs,
	}}
	if opts.omitVerdict {
		verdicts = nil
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID:        ids.verifierResultID,
		PatchID:                 ids.patchID,
		CapsuleID:               ids.capsuleID,
		ObligationResults:       verdicts,
		RecommendedAction:       pickRecommendedAction(opts.recommendedAction),
		RecommendationRationale: pickRecommendationRationale(opts.recommendation),
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}
	if _, err := env.st.LoadActiveGoal(env.ctx); errors.Is(err, store.ErrNotFound) {
		t.Fatal("scenario must create an active goal")
	}
	return ids
}

func pickRecommendedAction(action schema.RecommendedAction) schema.RecommendedAction {
	if action == "" {
		return schema.ActionAccept
	}
	return action
}

func pickRecommendationRationale(rationale string) string {
	if strings.TrimSpace(rationale) == "" {
		return "tests passed"
	}
	return rationale
}
