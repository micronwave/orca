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

func marshalJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}

func TestReconcileRejectsBlockingObligationWithoutEvidenceIDs(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "NOEVID",
		evidenceIDs:  nil,
		saveEvidence: false,
	})

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, patchID)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

func TestReconcileClaimVerificationSetsLastValidatedAgainst(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "CLAIMVAL",
		evidenceIDs:  []string{"EV-CLAIMVAL"},
		saveEvidence: true,
	})
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-CLAIMVAL",
		GoalID:      ids.goalID,
		EventID:     "EVT-CLAIMVAL",
		SequenceNum: 10,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-CLAIMVAL",
		Text:            "claim with evidence",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimProposed,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	claim, err := env.st.LoadClaim(env.ctx, "CL-CLAIMVAL")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status != schema.ClaimVerified || claim.LastValidatedAgainst != "SNAP-CLAIMVAL" {
		t.Fatalf("claim validation = status %s snapshot %q, want verified SNAP-CLAIMVAL", claim.Status, claim.LastValidatedAgainst)
	}
	events, err := env.log.ReadByType(env.ctx, schema.EventClaimStatusUpdated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType claim_status_updated: %v", err)
	}
	var sawValidation bool
	for _, event := range events {
		var payload schema.ClaimStatusPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("Unmarshal claim_status_updated payload: %v", err)
		}
		if payload.ClaimID == "CL-CLAIMVAL" && payload.Status == schema.ClaimVerified && payload.LastValidatedAgainst == "SNAP-CLAIMVAL" {
			sawValidation = true
		}
	}
	if !sawValidation {
		t.Fatal("missing claim_status_updated payload with LastValidatedAgainst")
	}
}

func TestReconcileExplicitContradictionMarksBothClaimsContested(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "CONTEST",
		evidenceIDs:  []string{"EV-CONTEST"},
		saveEvidence: true,
	})
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-CONTEST-OLD",
		Text:            "old verified claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-CONTEST-NEW",
		Text:            "new verified claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimProposed,
		EvidenceIDs:     []string{ids.evidenceID},
		Contradicts:     []string{"CL-CONTEST-OLD"},
	}); err != nil {
		t.Fatalf("SaveClaim new: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	oldClaim, err := env.st.LoadClaim(env.ctx, "CL-CONTEST-OLD")
	if err != nil {
		t.Fatalf("LoadClaim old: %v", err)
	}
	newClaim, err := env.st.LoadClaim(env.ctx, "CL-CONTEST-NEW")
	if err != nil {
		t.Fatalf("LoadClaim new: %v", err)
	}
	if oldClaim.Status != schema.ClaimContested || newClaim.Status != schema.ClaimContested {
		t.Fatalf("claim statuses old=%s new=%s, want both contested", oldClaim.Status, newClaim.Status)
	}
	if len(oldClaim.ContradictedBy) != 1 || oldClaim.ContradictedBy[0] != "CL-CONTEST-NEW" {
		t.Fatalf("old ContradictedBy = %v", oldClaim.ContradictedBy)
	}
	if len(newClaim.ContradictedBy) != 1 || newClaim.ContradictedBy[0] != "CL-CONTEST-OLD" {
		t.Fatalf("new ContradictedBy = %v", newClaim.ContradictedBy)
	}
}

func TestReconcileExplicitInvalidationMarksOnlyTargetInvalidated(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "INVALIDATE",
		evidenceIDs:  []string{"EV-INVALIDATE"},
		saveEvidence: true,
	})
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-INVALIDATE-OLD",
		Text:            "old verified claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-INVALIDATE-NEW",
		Text:            "new verified claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimProposed,
		EvidenceIDs:     []string{ids.evidenceID},
		Invalidates:     []string{"CL-INVALIDATE-OLD"},
	}); err != nil {
		t.Fatalf("SaveClaim new: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	oldClaim, err := env.st.LoadClaim(env.ctx, "CL-INVALIDATE-OLD")
	if err != nil {
		t.Fatalf("LoadClaim old: %v", err)
	}
	newClaim, err := env.st.LoadClaim(env.ctx, "CL-INVALIDATE-NEW")
	if err != nil {
		t.Fatalf("LoadClaim new: %v", err)
	}
	if oldClaim.Status != schema.ClaimInvalidated {
		t.Fatalf("old status = %s, want invalidated", oldClaim.Status)
	}
	if newClaim.Status != schema.ClaimVerified {
		t.Fatalf("new status = %s, want verified", newClaim.Status)
	}
	if len(oldClaim.InvalidatedBy) != 1 || oldClaim.InvalidatedBy[0] != "CL-INVALIDATE-NEW" {
		t.Fatalf("old InvalidatedBy = %v", oldClaim.InvalidatedBy)
	}
}

// TestReconcileVerifierResultInvalidatesTargetClaim verifies the vr.Invalidates
// path in detectClaimDisputes: a VerifierResult may explicitly invalidate a
// verified claim by listing its ID in the Invalidates field.
func TestReconcileVerifierResultInvalidatesTargetClaim(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:              "VRINVAL",
		evidenceIDs:         []string{"EV-VRINVAL"},
		saveEvidence:        true,
		verifierInvalidates: []string{"CL-VRINVAL-TARGET"},
	})
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-VRINVAL-TARGET",
		Text:            "claim explicitly invalidated by verifier result",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim target: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	target, err := env.st.LoadClaim(env.ctx, "CL-VRINVAL-TARGET")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if target.Status != schema.ClaimInvalidated {
		t.Fatalf("target status = %s, want invalidated", target.Status)
	}
	if len(target.InvalidatedBy) != 1 || target.InvalidatedBy[0] != ids.verifierResultID {
		t.Fatalf("target InvalidatedBy = %v, want [%s]", target.InvalidatedBy, ids.verifierResultID)
	}
}

// TestReconcileDecisionInvalidatesTargetClaim verifies the decisionInvalidations
// path in detectClaimDisputes: a DecisionRecord may explicitly invalidate a
// verified claim by listing its ID in the Invalidates field.
func TestReconcileDecisionInvalidatesTargetClaim(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "DECINVAL",
		evidenceIDs:  []string{"EV-DECINVAL"},
		saveEvidence: true,
	})
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-DECINVAL-TARGET",
		Text:            "claim explicitly invalidated by decision record",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/reconciler/reconciler.go"},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim target: %v", err)
	}
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID:  "DEC-DECINVAL",
		Context:     "claim_invalidation",
		Decision:    "invalidate stale architectural claim",
		Rationale:   "claim no longer applies after refactor",
		MadeBy:      "human",
		RelatedIDs:  []string{ids.goalID},
		Invalidates: []string{"CL-DECINVAL-TARGET"},
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	target, err := env.st.LoadClaim(env.ctx, "CL-DECINVAL-TARGET")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if target.Status != schema.ClaimInvalidated {
		t.Fatalf("target status = %s, want invalidated", target.Status)
	}
	if len(target.InvalidatedBy) != 1 || target.InvalidatedBy[0] != "DEC-DECINVAL" {
		t.Fatalf("target InvalidatedBy = %v, want [DEC-DECINVAL]", target.InvalidatedBy)
	}
}

// TestReconcileVerifiedClaimWithEmptyLVAGetsSnapshotSet verifies the plan
// requirement: "Do not leave permanent verified facts without a freshness base."
// A claim already saved as verified with no LastValidatedAgainst must have its
// snapshot ID set the next time Reconcile processes the owning capsule.
func TestReconcileVerifiedClaimWithEmptyLVAGetsSnapshotSet(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "EMPTYSNAP",
		evidenceIDs:  []string{"EV-EMPTYSNAP"},
		saveEvidence: true,
	})
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-EMPTYSNAP",
		GoalID:      ids.goalID,
		EventID:     "EVT-EMPTYSNAP",
		SequenceNum: 10,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	// Claim is already verified but has no LastValidatedAgainst — simulating a
	// claim created before snapshot tracking was introduced or by an adapter
	// that did not populate the field.
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:              "CL-EMPTYSNAP",
		Text:                 "already verified, no snapshot",
		ClaimType:            schema.ClaimInvariant,
		SourceCapsuleID:      ids.capsuleID,
		AffectedFiles:        []string{"internal/reconciler/reconciler.go"},
		Status:               schema.ClaimVerified,
		LastValidatedAgainst: "",
		EvidenceIDs:          []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	claim, err := env.st.LoadClaim(env.ctx, "CL-EMPTYSNAP")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.LastValidatedAgainst != "SNAP-EMPTYSNAP" {
		t.Fatalf("LastValidatedAgainst = %q, want SNAP-EMPTYSNAP", claim.LastValidatedAgainst)
	}
	if claim.Status != schema.ClaimVerified {
		t.Fatalf("status = %s, want verified", claim.Status)
	}
}

func TestReconcileOverlapAloneDoesNotContestClaims(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "NOCONTEST",
		evidenceIDs:  []string{"EV-NOCONTEST"},
		saveEvidence: true,
		changedFiles: []string{"internal/foo/service.go"},
	})
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:         "CL-NOCONTEST-OLD",
		Text:            "old verified claim",
		ClaimType:       schema.ClaimInvariant,
		SourceCapsuleID: ids.capsuleID,
		AffectedFiles:   []string{"internal/foo/service.go"},
		Status:          schema.ClaimVerified,
		EvidenceIDs:     []string{ids.evidenceID},
	}); err != nil {
		t.Fatalf("SaveClaim old: %v", err)
	}
	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	claim, err := env.st.LoadClaim(env.ctx, "CL-NOCONTEST-OLD")
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	if claim.Status == schema.ClaimContested {
		t.Fatal("overlapping files alone marked claim contested")
	}
}

func TestFreshnessCheckMarksClaimsStaleAfterInterveningAcceptedPatch(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "FRESH",
		evidenceIDs:  []string{"EV-FRESH"},
		saveEvidence: true,
		changedFiles: []string{"internal/fresh/service.go"},
	})
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-FRESH-OLD",
		GoalID:      ids.goalID,
		EventID:     "EVT-FRESH-OLD",
		SequenceNum: 1,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot old: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:              "CL-FRESH-STALE",
		Text:                 "validated before patch",
		ClaimType:            schema.ClaimInvariant,
		SourceCapsuleID:      ids.capsuleID,
		AffectedFiles:        []string{`internal\fresh\service.go`},
		Status:               schema.ClaimVerified,
		EvidenceIDs:          []string{ids.evidenceID},
		LastValidatedAgainst: "SNAP-FRESH-OLD",
	}); err != nil {
		t.Fatalf("SaveClaim stale: %v", err)
	}
	if err := env.st.SaveClaim(env.ctx, &schema.ClaimArtifact{
		ClaimID:              "CL-FRESH-KEEP",
		Text:                 "unrelated validated claim",
		ClaimType:            schema.ClaimInvariant,
		SourceCapsuleID:      ids.capsuleID,
		AffectedFiles:        []string{"internal/other/file.go"},
		Status:               schema.ClaimVerified,
		EvidenceIDs:          []string{ids.evidenceID},
		LastValidatedAgainst: "SNAP-FRESH-OLD",
	}); err != nil {
		t.Fatalf("SaveClaim keep: %v", err)
	}
	if _, err := env.log.Append(env.ctx, schema.Event{
		Type:       schema.EventPatchAccepted,
		GoalID:     ids.goalID,
		ArtifactID: ids.patchID,
		Payload:    marshalJSON(t, schema.PatchStatusPayload{PatchID: ids.patchID}),
	}); err != nil {
		t.Fatalf("append patch accepted: %v", err)
	}
	if err := env.st.SaveSnapshot(env.ctx, &schema.StateSnapshot{
		SnapshotID:  "SNAP-FRESH-CURRENT",
		GoalID:      ids.goalID,
		EventID:     "EVT-FRESH-CURRENT",
		SequenceNum: 100,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot current: %v", err)
	}

	if err := New(env.st, env.log, Config{}).FreshnessCheck(env.ctx, ids.goalID); err != nil {
		t.Fatalf("FreshnessCheck: %v", err)
	}
	stale, err := env.st.LoadClaim(env.ctx, "CL-FRESH-STALE")
	if err != nil {
		t.Fatalf("LoadClaim stale: %v", err)
	}
	keep, err := env.st.LoadClaim(env.ctx, "CL-FRESH-KEEP")
	if err != nil {
		t.Fatalf("LoadClaim keep: %v", err)
	}
	if stale.Status != schema.ClaimStale {
		t.Fatalf("stale claim status = %s, want stale", stale.Status)
	}
	if keep.Status != schema.ClaimVerified {
		t.Fatalf("unrelated claim status = %s, want verified", keep.Status)
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
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

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
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

func TestReconcile_DeduplicatesFollowUpsByNormalizedSignatureAndUsesRecommendation(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "SIGFOLLOW",
		evidenceIDs:  []string{"EV-SIGFOLLOW-MISSING"},
		saveEvidence: false,
	})
	signature := "go test ./...\nfailure"
	for _, failure := range []*schema.FailureFingerprint{
		{
			FailureID:             "FAIL-SIGFOLLOW-1",
			SourceCapsuleID:       ids.capsuleID,
			FailureType:           schema.FailureTest,
			Summary:               "test gate failed first",
			ErrorSignature:        signature,
			RecommendedNextAction: "rerun the targeted test after fixing the regression",
		},
		{
			FailureID:             "FAIL-SIGFOLLOW-2",
			SourceCapsuleID:       ids.capsuleID,
			FailureType:           schema.FailureTest,
			Summary:               "test gate failed second",
			ErrorSignature:        " GO TEST ./...\n\nFAILURE ",
			RecommendedNextAction: "rerun the targeted test after fixing the regression",
		},
	} {
		if err := env.st.SaveFailure(env.ctx, failure); err != nil {
			t.Fatalf("SaveFailure %s: %v", failure.FailureID, err)
		}
	}

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("PatchAccepted=true, want rejection")
	}
	wantID := "OB-FOLLOWUP-SIG-" + shortSignatureHash(signature)
	if len(result.FollowUpObligationIDs) != 1 || result.FollowUpObligationIDs[0] != wantID {
		t.Fatalf("FollowUpObligationIDs = %v, want [%s]", result.FollowUpObligationIDs, wantID)
	}
	followUp, err := env.st.LoadObligation(env.ctx, wantID)
	if err != nil {
		t.Fatalf("LoadObligation follow-up: %v", err)
	}
	if !strings.Contains(followUp.Description, "rerun the targeted test") {
		t.Fatalf("follow-up Description = %q, want recommended next action", followUp.Description)
	}
	if len(followUp.EvidenceRequired) != 1 || followUp.EvidenceRequired[0] != string(schema.EvidenceTestResult) {
		t.Fatalf("follow-up EvidenceRequired = %v, want [%s]", followUp.EvidenceRequired, string(schema.EvidenceTestResult))
	}
	decision, err := env.st.LoadDecision(env.ctx, result.DecisionID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if !strings.Contains(decision.Rationale, "rerun the targeted test") {
		t.Fatalf("decision Rationale = %q, want recommended next action", decision.Rationale)
	}
}

func TestReconcile_WritesPerObligationBudgetRecord(t *testing.T) {
	env := newTestEnv(t)
	ids := saveReconcileScenario(t, env, scenarioOptions{
		suffix:       "BUDOBL",
		evidenceIDs:  []string{"EV-BUDOBL"},
		saveEvidence: true,
	})

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
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

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, ids.patchID); err != nil {
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
		goalID    = "G-BUDTOKENS"
		condID    = "GC-BUDTOKENS"
		capsID    = "CAP-BUDTOKENS"
		patchID   = "PATCH-BUDTOKENS"
		vrID      = "VR-BUDTOKENS"
		totalTok  = 5
		totalWall = 7.5
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
		WallTimeSeconds:      totalWall,
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

	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, patchID); err != nil {
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
	if summary.WallTimeSeconds != totalWall {
		t.Fatalf("summary WallTimeSeconds = %.2f, want %.2f", summary.WallTimeSeconds, totalWall)
	}

	var perObligationTotal int
	var perObligationWallTotal float64
	for _, obligationID := range obligationIDs {
		recordID := "BUD-" + capsID + "-" + obligationID
		record := recordsByID[recordID]
		if record == nil {
			t.Fatalf("missing per-obligation record %s", recordID)
		}
		perObligationTotal += record.TokensSpent
		perObligationWallTotal += record.WallTimeSeconds
	}
	if perObligationTotal != totalTok {
		t.Fatalf("per-obligation token total = %d, want %d", perObligationTotal, totalTok)
	}
	if diff := perObligationWallTotal - totalWall; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("per-obligation wall total = %.6f, want %.6f", perObligationWallTotal, totalWall)
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
	verifierInvalidates  []string
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
		Invalidates:             opts.verifierInvalidates,
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

func TestReconcile_SavesTopologyOutcomeOnAcceptedPatch(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID  = "G-TOPOUT-ACCEPT"
		condID  = "GC-TOPOUT-ACCEPT"
		oblID   = "OB-TOPOUT-ACCEPT"
		capsID  = "CAP-TOPOUT-ACCEPT"
		patchID = "PATCH-TOPOUT-ACCEPT"
		vrID    = "VR-TOPOUT-ACCEPT"
		evID    = "EV-TOPOUT-ACCEPT"
		decID   = "DEC-TOPOUT-ACCEPT"
	)
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "topology outcome acceptance test",
		GoalConditions: []schema.GoalCondition{{
			ID:                   condID,
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: now,
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(env.ctx, &schema.Obligation{
		ObligationID:     oblID,
		GoalConditionID:  condID,
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskMedium,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologyImplementerReviewer),
		Rationale:  "medium risk -> implementer_reviewer",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: decID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: evID,
		Type:       schema.EvidenceTestResult,
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ChangedFiles:         []string{"internal/foo/service.go"},
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
		TokensUsed:           150,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evID},
		}},
		RecommendedAction:       schema.ActionAccept,
		RecommendationRationale: "tests passed",
		CreatedAt:               now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.PatchAccepted {
		t.Fatalf("PatchAccepted=false: %s", result.BlockingReason)
	}

	outcomes, err := env.st.LoadTopologyOutcomesForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("topology outcomes = %d, want 1", len(outcomes))
	}
	o := outcomes[0]
	if o.Topology != schema.TopologyImplementerReviewer {
		t.Errorf("Topology = %s, want %s", o.Topology, schema.TopologyImplementerReviewer)
	}
	if !o.PatchAccepted {
		t.Errorf("PatchAccepted = false, want true")
	}
	if o.ObligationsMet != 1 {
		t.Errorf("ObligationsMet = %d, want 1", o.ObligationsMet)
	}
	if o.TokensSpent != 150 {
		t.Errorf("TokensSpent = %d, want 150", o.TokensSpent)
	}
	if o.MaxRiskLevel != schema.RiskMedium {
		t.Errorf("MaxRiskLevel = %s, want %s", o.MaxRiskLevel, schema.RiskMedium)
	}
	if o.ObligationCount != 1 {
		t.Errorf("ObligationCount = %d, want 1", o.ObligationCount)
	}
	if len(o.AffectedFiles) != 1 || o.AffectedFiles[0] != "internal/foo/service.go" {
		t.Errorf("AffectedFiles = %v, want [internal/foo/service.go]", o.AffectedFiles)
	}
	if o.GoalID != goalID {
		t.Errorf("GoalID = %s, want %s", o.GoalID, goalID)
	}
}

func TestReconcile_SavesTopologyOutcomeOnRejectedPatch(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID  = "G-TOPOUT-REJECT"
		condID  = "GC-TOPOUT-REJECT"
		oblID   = "OB-TOPOUT-REJECT"
		capsID  = "CAP-TOPOUT-REJECT"
		patchID = "PATCH-TOPOUT-REJECT"
		vrID    = "VR-TOPOUT-REJECT"
		decID   = "DEC-TOPOUT-REJECT"
	)
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "topology outcome rejection test",
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
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "low risk -> single",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: decID,
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
	// no evidence ID — will trigger rejection
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  nil,
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	result, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, patchID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.PatchAccepted {
		t.Fatal("expected patch rejection (no evidence IDs)")
	}

	outcomes, err := env.st.LoadTopologyOutcomesForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("topology outcomes = %d, want 1", len(outcomes))
	}
	o := outcomes[0]
	if o.Topology != schema.TopologySingle {
		t.Errorf("Topology = %s, want %s", o.Topology, schema.TopologySingle)
	}
	if o.PatchAccepted {
		t.Error("PatchAccepted = true, want false")
	}
}

func TestReconcile_NoLearningSkipsTopologyOutcome(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID  = "G-TOPOUT-NOLEARN"
		condID  = "GC-TOPOUT-NOLEARN"
		oblID   = "OB-TOPOUT-NOLEARN"
		capsID  = "CAP-TOPOUT-NOLEARN"
		patchID = "PATCH-TOPOUT-NOLEARN"
		vrID    = "VR-TOPOUT-NOLEARN"
		evID    = "EV-TOPOUT-NOLEARN"
		decID   = "DEC-TOPOUT-NOLEARN"
	)
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "topology outcome no-learning test",
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
		Description:      "run tests",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveDecision(env.ctx, &schema.DecisionRecord{
		DecisionID: decID,
		Context:    "topology_selection",
		Decision:   string(schema.TopologySingle),
		Rationale:  "low risk -> single",
		MadeBy:     "system",
		RelatedIDs: []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: decID,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: evID,
		Type:       schema.EvidenceTestResult,
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evID},
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	if _, err := New(env.st, env.log, Config{NoLearning: true}).Reconcile(env.ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	outcomes, err := env.st.LoadTopologyOutcomesForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("topology outcomes = %d, want 0 when NoLearning=true", len(outcomes))
	}
}

// TestReconcile_TopologyOutcomeSkippedWhenNoTopologyDecisionID verifies that
// saveTopologyOutcome is a safe no-op when the capsule has no TopologyDecisionID
// (i.e. the goal was not routed through the planner's topology decision path).
// This happens in direct-run scenarios where a capsule is created without
// a prior topology decision record.
func TestReconcile_TopologyOutcomeSkippedWhenNoTopologyDecisionID(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now().UTC()
	const (
		goalID  = "G-TOPOUT-NODEC"
		condID  = "GC-TOPOUT-NODEC"
		oblID   = "OB-TOPOUT-NODEC"
		capsID  = "CAP-TOPOUT-NODEC"
		patchID = "PATCH-TOPOUT-NODEC"
		vrID    = "VR-TOPOUT-NODEC"
		evID    = "EV-TOPOUT-NODEC"
	)
	if err := env.st.SaveGoal(env.ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "topology outcome no-decision-id skip test",
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
		Description:      "implement feature",
		EvidenceRequired: []string{string(schema.EvidenceTestResult)},
		Blocking:         true,
		RiskLevel:        schema.RiskLow,
		Status:           schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	// Capsule has no TopologyDecisionID — not routed via topology planning.
	if err := env.st.SaveCapsule(env.ctx, &schema.ExecutionCapsule{
		CapsuleID:          capsID,
		ObligationIDs:      []string{oblID},
		Agent:              schema.AgentCodex,
		Role:               schema.RoleExecutor,
		State:              schema.CapsuleStateCompleted,
		TopologyDecisionID: "",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(env.ctx, &schema.EvidenceArtifact{
		EvidenceID: evID,
		Type:       schema.EvidenceTestResult,
		Command:    "go test ./...",
		ExitCode:   0,
		Supports:   []string{oblID},
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SavePatch(env.ctx, &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsID,
		ObligationIDsClaimed: []string{oblID},
		ChangedFiles:         []string{"internal/foo/foo.go"},
		Status:               schema.PatchCandidate,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}
	if err := env.st.SaveVerifierResult(env.ctx, &schema.VerifierResult{
		VerifierResultID: vrID,
		PatchID:          patchID,
		CapsuleID:        capsID,
		ObligationResults: []schema.ObligationVerdict{{
			ObligationID: oblID,
			Verdict:      schema.VerdictSatisfied,
			EvidenceIDs:  []string{evID},
		}},
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         now,
	}); err != nil {
		t.Fatalf("SaveVerifierResult: %v", err)
	}

	// Reconcile must succeed — the missing TopologyDecisionID causes a silent
	// skip of topology outcome recording, not an error.
	if _, err := New(env.st, env.log, Config{}).Reconcile(env.ctx, patchID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	outcomes, err := env.st.LoadTopologyOutcomesForGoal(env.ctx, goalID)
	if err != nil {
		t.Fatalf("LoadTopologyOutcomesForGoal: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("topology outcomes = %d, want 0 when capsule has no TopologyDecisionID", len(outcomes))
	}
}
