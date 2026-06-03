package reconciler

// Run with:
//
//	go test ./internal/reconciler/... -run TestReconciler_WorkflowComplexityStress -v -timeout 15m
//
// -race requires CGO; on Windows install mingw64 first:
//
//	go test ./internal/reconciler/... -run TestReconciler_WorkflowComplexityStress -v -timeout 15m -race

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

const (
	stressNConditions   = 1_000 // 1,000+ GoalConditions as specified
	stressOblsPerCond   = 5     // 5,000 total obligations
	stressSeqCapsules   = 10    // sequential sample: 10 reconciles, timed individually
	stressConcCapsules  = 20    // concurrent race batch
)

// TestReconciler_WorkflowComplexityStress creates a Mega-Goal with 1,000
// GoalConditions and 5,000 Obligations, then probes two failure modes:
//
//  1. O(N²) CPU: each reconcile calls LoadOpenObligations (a full directory
//     scan).  With 5,000 obligation files this cost is already O(N) per call;
//     if the reconciler has additional O(N)-per-obligation inner loops the
//     per-reconcile time grows quadratically.  The test times ten sequential
//     reconciles individually and reports the per-reconcile cost so you can
//     verify whether it stays flat (O(1) in-memory logic) or grows (O(N²)).
//
//  2. Race conditions: 20 goroutines each reconcile a distinct capsule
//     concurrently.  After all finish the test verifies that each obligation's
//     SatisfiedBy field was written exactly once — no double-trigger.
//     Run with -race (requires CGO/mingw64) to surface unsynchronised access.
func TestReconciler_WorkflowComplexityStress(t *testing.T) {
	env := newTestEnv(t)
	ctx := env.ctx

	const (
		nConds = stressNConditions
		oblPC  = stressOblsPerCond
		nObls  = nConds * oblPC // 5,000
	)

	// ── Phase 1: seed the Mega-Goal (1,000 conditions + 5,000 obligations) ──
	// Capsules and patches are NOT created here; they are created just before
	// each sub-test so we don't inflate the seed file count unnecessarily.
	t.Logf("seeding mega-goal: %d conditions, %d obligations …", nConds, nObls)
	seedStart := time.Now()

	goalID    := "G-MEGA"
	conditions := make([]schema.GoalCondition, nConds)
	for i := range nConds {
		conditions[i] = schema.GoalCondition{
			ID:                   fmt.Sprintf("GC-MEGA-%04d", i),
			Description:          fmt.Sprintf("condition %d", i),
			EffectiveDescription: fmt.Sprintf("condition %d", i),
			Status:               schema.GoalConditionUnmet,
		}
	}
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "workflow complexity stress test",
		GoalConditions: conditions,
		RiskLevel:      schema.RiskLow,
		CreatedAt:      time.Now().UTC(),
		Status:         schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	// Save all 5,000 obligations up front.  These form the "open" pool that
	// LoadOpenObligations scans on every subsequent reconcile call.
	for ci := range nConds {
		condID := fmt.Sprintf("GC-MEGA-%04d", ci)
		for oi := range oblPC {
			if err := env.st.SaveObligation(ctx, &schema.Obligation{
				ObligationID:     fmt.Sprintf("OB-MEGA-%04d-%d", ci, oi),
				GoalConditionID:  condID,
				Description:      fmt.Sprintf("obligation %d.%d", ci, oi),
				EvidenceRequired: []string{string(schema.EvidenceTestResult)},
				Blocking:         true,
				RiskLevel:        schema.RiskLow,
				Status:           schema.ObligationOpen,
			}); err != nil {
				t.Fatalf("SaveObligation ci=%d oi=%d: %v", ci, oi, err)
			}
		}
	}
	t.Logf("  seed complete: %v  (goal + %d obligations written)",
		time.Since(seedStart).Round(time.Millisecond), nObls)

	// ── Phase 2: sequential reconcile — O(N²) CPU check ────────────────────
	// We create stressSeqCapsules capsules and reconcile them one by one,
	// timing each reconcile individually.  The open-obligation pool stays near
	// 5,000 throughout (only stressSeqCapsules × oblPC obligations are
	// satisfied), so the per-reconcile cost reflects the full O(N) scan cost.
	//
	// A flat per-reconcile time confirms the reconciler's in-memory logic is
	// O(oblPC) per call regardless of pool size.  A growing per-reconcile time
	// indicates a hidden O(N) inner loop inside the reconciler (O(N²) overall).
	t.Logf("creating %d sequential capsules …", stressSeqCapsules)
	seqNow := time.Now().UTC()
	seqPatchIDs := make([]string, stressSeqCapsules)

	for i := range stressSeqCapsules {
		capsID  := fmt.Sprintf("CAP-SEQ-%04d", i)
		patchID := fmt.Sprintf("PATCH-SEQ-%04d", i)
		vrID    := fmt.Sprintf("VR-SEQ-%04d", i)

		oblIDs  := make([]string, oblPC)
		verdicts := make([]schema.ObligationVerdict, oblPC)

		for oi := range oblPC {
			oblID := fmt.Sprintf("OB-MEGA-%04d-%d", i, oi)
			evID  := fmt.Sprintf("EV-SEQ-%04d-%d", i, oi)
			oblIDs[oi] = oblID

			if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
				EvidenceID: evID,
				Type:       schema.EvidenceTestResult,
				Command:    "go test ./...",
				ExitCode:   0,
				Supports:   []string{oblID},
				CreatedAt:  seqNow,
			}); err != nil {
				t.Fatalf("SaveEvidence seq i=%d oi=%d: %v", i, oi, err)
			}
			verdicts[oi] = schema.ObligationVerdict{
				ObligationID: oblID,
				Verdict:      schema.VerdictSatisfied,
				EvidenceIDs:  []string{evID},
			}
		}

		if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID:     capsID,
			ObligationIDs: oblIDs,
			Agent:         schema.AgentMock,
			Role:          schema.RoleExecutor,
			State:         schema.CapsuleStateCompleted,
		}); err != nil {
			t.Fatalf("SaveCapsule seq i=%d: %v", i, err)
		}
		if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID:              patchID,
			CapsuleID:            capsID,
			ObligationIDsClaimed: oblIDs,
			Status:               schema.PatchCandidate,
		}); err != nil {
			t.Fatalf("SavePatch seq i=%d: %v", i, err)
		}
		if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
			VerifierResultID:  vrID,
			PatchID:           patchID,
			CapsuleID:         capsID,
			ObligationResults: verdicts,
			RecommendedAction: schema.ActionAccept,
			CreatedAt:         seqNow,
		}); err != nil {
			t.Fatalf("SaveVerifierResult seq i=%d: %v", i, err)
		}
		seqPatchIDs[i] = patchID
	}

	t.Logf("running %d sequential reconciles (open-obligation pool ≈ %d) …",
		stressSeqCapsules, nObls)
	rec := New(env.st, env.log, Config{NoLearning: true})

	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	seqTimes := make([]time.Duration, stressSeqCapsules)
	seqTotal := time.Duration(0)

	for i, pid := range seqPatchIDs {
		t0 := time.Now()
		result, err := rec.Reconcile(ctx, pid)
		seqTimes[i] = time.Since(t0)
		seqTotal += seqTimes[i]

		if err != nil {
			t.Fatalf("Reconcile seq[%d]: %v", i, err)
		}
		if !result.PatchAccepted {
			t.Fatalf("Reconcile seq[%d] not accepted: %s", i, result.BlockingReason)
		}
	}

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	avgSeq := seqTotal / time.Duration(stressSeqCapsules)

	// Separate cold-cache (reconcile[0]) from warm-cache (reconcile[1+]) times.
	// reconcile[0] pays the OS file-cache cold-start cost for the 5,000-file
	// obligation directory; subsequent reconciles run from cache and represent
	// the steady-state per-reconcile cost.
	coldTime := seqTimes[0]
	warmMin, warmMax := seqTimes[1], seqTimes[1]
	warmTotal := time.Duration(0)
	for _, d := range seqTimes[1:] {
		warmTotal += d
		if d < warmMin {
			warmMin = d
		}
		if d > warmMax {
			warmMax = d
		}
	}
	warmAvg := warmTotal / time.Duration(stressSeqCapsules-1)

	t.Logf("─────────────────────────────────────────────────────────")
	t.Logf("WORKFLOW COMPLEXITY STRESS  conditions=%d  obligations=%d", nConds, nObls)
	t.Logf("  sequential batch    n=%d  total=%v  avg=%v",
		stressSeqCapsules, seqTotal.Round(time.Millisecond), avgSeq.Round(time.Millisecond))
	t.Logf("  cold reconcile[00]  %v  (OS file-cache cold start for %d obligation files)",
		coldTime.Round(time.Millisecond), nObls)
	t.Logf("  warm reconciles     n=%d  avg=%v  min=%v  max=%v",
		stressSeqCapsules-1,
		warmAvg.Round(time.Millisecond),
		warmMin.Round(time.Millisecond),
		warmMax.Round(time.Millisecond))
	for i, d := range seqTimes {
		t.Logf("    reconcile[%02d]  %v", i, d.Round(time.Millisecond))
	}

	// O(N²) signal: if warm reconcile times grow significantly as obligations
	// are satisfied, the reconciler has a hidden O(N) inner loop over all
	// obligations (not just the verdicts for this capsule).
	// reconcile[1] has the most open obligations; reconcile[N-1] has the fewest.
	// A growing trend indicates O(N) per reconcile = O(N²) total.
	if stressSeqCapsules > 2 && warmMax > 3*warmMin && warmMin > 0 {
		t.Logf("  WARNING: warm max/min = %.1fx — reconciler cost grows with obligation count (O(N²) signal)",
			float64(warmMax)/float64(warmMin))
	} else {
		t.Logf("  warm reconcile cost stable (max/min = %.1fx) — reconciler logic is O(oblPC) per call",
			float64(warmMax)/float64(warmMin))
	}

	// Known store-layer cost: LoadOpenObligations does a full directory scan on
	// every reconcile.  With N obligations and M capsules total I/O is O(N×M).
	// At warm-cache rate, 1,000 capsules at this obligation pool size would take:
	t.Logf("  store note: LoadOpenObligations scans all %d obligation files per reconcile (O(N×M) total)", nObls)
	t.Logf("  extrapolated cost for 1,000 capsules at warm avg=%v: ~%v",
		warmAvg.Round(time.Millisecond),
		(warmAvg * 1_000).Round(time.Second))

	t.Logf("  heap before         %.1f MB", float64(mBefore.HeapInuse)/1e6)
	t.Logf("  heap after          %.1f MB", float64(mAfter.HeapInuse)/1e6)
	t.Logf("  heap delta          %+.1f MB", float64(int64(mAfter.HeapInuse)-int64(mBefore.HeapInuse))/1e6)
	t.Logf("  total alloc         %.1f MB", float64(mAfter.TotalAlloc-mBefore.TotalAlloc)/1e6)
	t.Logf("─────────────────────────────────────────────────────────")

	// ── Phase 3: concurrent race test ─────────────────────────────────────
	// Each goroutine owns a unique obligation; no two capsules share an ID, so
	// there is no intentional write contention.  The test validates:
	//   (a) no Reconcile call returns an error under concurrent load,
	//   (b) each satisfied obligation has SatisfiedBy set exactly once
	//       (would be empty or doubled if two goroutines raced on the same OB).
	t.Logf("seeding %d concurrent capsules …", stressConcCapsules)
	concNow    := time.Now().UTC()
	concPatches := make([]string, stressConcCapsules)
	concOblIDs  := make([]string, stressConcCapsules)

	for i := range stressConcCapsules {
		// Each concurrent capsule addresses a unique obligation attached to
		// an existing condition in the mega-goal (GC-MEGA-0000…0019).
		oblID   := fmt.Sprintf("OB-CONC-%04d", i)
		condID  := fmt.Sprintf("GC-MEGA-%04d", i)
		capsID  := fmt.Sprintf("CAP-CONC-%04d", i)
		patchID := fmt.Sprintf("PATCH-CONC-%04d", i)
		vrID    := fmt.Sprintf("VR-CONC-%04d", i)
		evID    := fmt.Sprintf("EV-CONC-%04d", i)

		if err := env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     oblID,
			GoalConditionID:  condID,
			Description:      "concurrent test obligation",
			EvidenceRequired: []string{string(schema.EvidenceTestResult)},
			Blocking:         true,
			RiskLevel:        schema.RiskLow,
			Status:           schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("conc SaveObligation i=%d: %v", i, err)
		}
		if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
			EvidenceID: evID,
			Type:       schema.EvidenceTestResult,
			Command:    "go test ./...",
			ExitCode:   0,
			Supports:   []string{oblID},
			CreatedAt:  concNow,
		}); err != nil {
			t.Fatalf("conc SaveEvidence i=%d: %v", i, err)
		}
		if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
			CapsuleID:     capsID,
			ObligationIDs: []string{oblID},
			Agent:         schema.AgentMock,
			Role:          schema.RoleExecutor,
			State:         schema.CapsuleStateCompleted,
		}); err != nil {
			t.Fatalf("conc SaveCapsule i=%d: %v", i, err)
		}
		if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID:              patchID,
			CapsuleID:            capsID,
			ObligationIDsClaimed: []string{oblID},
			Status:               schema.PatchCandidate,
		}); err != nil {
			t.Fatalf("conc SavePatch i=%d: %v", i, err)
		}
		if err := env.st.SaveVerifierResult(ctx, &schema.VerifierResult{
			VerifierResultID: vrID,
			PatchID:          patchID,
			CapsuleID:        capsID,
			ObligationResults: []schema.ObligationVerdict{{
				ObligationID: oblID,
				Verdict:      schema.VerdictSatisfied,
				EvidenceIDs:  []string{evID},
			}},
			RecommendedAction: schema.ActionAccept,
			CreatedAt:         concNow,
		}); err != nil {
			t.Fatalf("conc SaveVerifierResult i=%d: %v", i, err)
		}
		concPatches[i] = patchID
		concOblIDs[i] = oblID
	}

	t.Logf("launching %d concurrent reconciles …", stressConcCapsules)
	concRec := New(env.st, env.log, Config{NoLearning: true})
	errs    := make([]error, stressConcCapsules)

	var wg sync.WaitGroup
	concStart := time.Now()
	for i, pid := range concPatches {
		wg.Add(1)
		go func(idx int, patchID string) {
			defer wg.Done()
			_, err := concRec.Reconcile(ctx, patchID)
			errs[idx] = err
		}(i, pid)
	}
	wg.Wait()
	concDur := time.Since(concStart)

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Reconcile[%d]: %v", i, err)
		}
	}
	t.Logf("  concurrent: %d reconciles in %v  (%.0f/sec)",
		stressConcCapsules, concDur.Round(time.Millisecond),
		float64(stressConcCapsules)/concDur.Seconds())

	// Verify no-double-trigger: each concurrent obligation must be satisfied
	// exactly once.  If two goroutines wrote to the same obligation, SatisfiedBy
	// would have 0 entries (lost write) or 2 entries (duplicate write).
	doubleCount := 0
	for i, oblID := range concOblIDs {
		obl, err := env.st.LoadObligation(ctx, oblID)
		if err != nil {
			t.Errorf("LoadObligation conc[%d] %s: %v", i, oblID, err)
			continue
		}
		if obl.Status != schema.ObligationSatisfied {
			t.Errorf("conc[%d] obligation %s status = %s, want satisfied", i, oblID, obl.Status)
		}
		if len(obl.SatisfiedBy) != 1 {
			t.Errorf("conc[%d] obligation %s SatisfiedBy = %v (len %d), want exactly 1 — possible double-trigger",
				i, oblID, obl.SatisfiedBy, len(obl.SatisfiedBy))
			doubleCount++
		}
	}
	if doubleCount == 0 {
		t.Logf("  no-double-trigger check passed: all %d obligations satisfied exactly once", stressConcCapsules)
	}
}
