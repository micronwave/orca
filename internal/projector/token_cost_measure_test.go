package projector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/schema"
)

// TestTokenCostMeasurement runs a rich, realistic projection scenario and reports
// concrete byte/token savings per optimization technique across all three phases.
// Run with: go test ./internal/projector/ -run TestTokenCostMeasurement -v
func TestTokenCostMeasurement(t *testing.T) {
	env := newProjectorEnv(t)
	ctx := env.ctx

	const (
		goalID      = "G-tokenmeasure"
		conditionID = "GC-tokenmeasure"
		obID1       = "OB-tokenmeasure-1"
		obID2       = "OB-tokenmeasure-2"
		obID3       = "OB-tokenmeasure-3"
		capsuleID   = "CAP-tokenmeasure"
		snapID      = "SNAP-tokenmeasure"
		maxTokens   = 32000
	)

	// ── Seed goal with 3 obligations ────────────────────────────────────────────
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "add input validation to user handler in internal/schema/goal.go",
		GoalConditions: []schema.GoalCondition{{
			ID:                   conditionID,
			Description:          "validate goal input fields before processing",
			EffectiveDescription: "validate goal input fields before processing",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskMedium,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	for i, obID := range []string{obID1, obID2, obID3} {
		desc := []string{
			"add nil-check for GoalConditions slice",
			"validate OriginalIntent field is non-empty",
			"write unit tests for goal validation logic",
		}[i]
		if err := env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID:     obID,
			GoalConditionID:  conditionID,
			Description:      desc,
			EvidenceRequired: []string{string(schema.EvidenceTestResult), string(schema.EvidenceLintResult)},
			Blocking:         true,
			RiskLevel:        schema.RiskMedium,
			Status:           schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", obID, err)
		}
	}

	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: []string{obID1, obID2, obID3},
		AllowedPaths:  []string{"internal/schema/goal.go", "internal/schema/goal_test.go"},
		RequiredOutputs: []string{
			"patch.diff",
			"test_evidence.json",
			"lint_evidence.json",
		},
		Budget:             schema.CapsuleBudget{MaxTokens: maxTokens, MaxWallTimeSeconds: 300, MaxRetries: 3},
		State:              schema.CapsuleStatePending,
		TopologyDecisionID: "DEC-tkmeasure",
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	// Save a snapshot so FreshnessBase is set.
	if err := env.st.SaveSnapshot(ctx, &schema.StateSnapshot{
		SnapshotID: snapID,
		GoalID:     goalID,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// ── Seed evidence: 5 items per obligation (T11 keeps 3 most recent) ─────────
	baseTime := time.Now().UTC().Add(-10 * time.Minute)
	for i, obID := range []string{obID1, obID2, obID3} {
		for j := 0; j < 5; j++ {
			evType := schema.EvidenceTestResult
			cmd := fmt.Sprintf("go test ./internal/schema/... -run TestGoal_%s", obID)
			if j%2 == 0 {
				evType = schema.EvidenceLintResult
				cmd = "go vet ./internal/schema/..."
			}
			if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
				EvidenceID:       fmt.Sprintf("EV-tkmeasure-%d-%d-01HZABCDEFGHIJ%d%d", i+1, j, i, j),
				Type:             evType,
				Source:           "verifier",
				Command:          cmd,
				ExitCode:         0,
				Summary:          fmt.Sprintf("obligation %s pass %d: all assertions satisfied, no issues found", obID, j),
				Supports:         []string{obID},
				ValidatedAgainst: snapID,
				CreatedAt:        baseTime.Add(time.Duration(j) * time.Minute),
			}); err != nil {
				t.Fatalf("SaveEvidence ob=%s j=%d: %v", obID, j, err)
			}
		}
	}

	// ── Seed claims: mix of short and long (T5 caps at 200 chars) ───────────────
	longClaimText := strings.Repeat("GoalIR validation must reject empty OriginalIntent at the entry boundary before any planner or projector step processes it; this invariant prevents downstream nil-dereference in the obligation planner ", 2)
	claims := []struct {
		id   string
		text string
	}{
		{"CL-tkmeasure-1", "GoalIR.GoalConditions must be non-nil before planning"},
		{"CL-tkmeasure-2", longClaimText},
		{"CL-tkmeasure-3", "OriginalIntent empty string must be rejected at intake with ErrInvalidGoal"},
		{"CL-tkmeasure-4", strings.Repeat("The obligation planner depends on GoalConditions being pre-validated; an empty slice causes a silent no-op plan which produces zero obligations and is indistinguishable from a legitimate empty goal ", 2)},
		{"CL-tkmeasure-5", "RiskLevel defaults to RiskLow when not specified"},
		{"CL-tkmeasure-6", "SaveGoal must persist all GoalConditions atomically"},
		{"CL-tkmeasure-7", strings.Repeat("Validation layer must run synchronously before any store write; async validation introduces TOCTOU window where invalid goal may be partially persisted before rejection ", 2)},
		{"CL-tkmeasure-8", "unit tests must cover nil-conditions and empty-intent edge cases"},
	}
	for _, c := range claims {
		if err := env.st.SaveClaim(ctx, &schema.ClaimArtifact{
			ClaimID:         c.id,
			Text:            c.text,
			ClaimType:       schema.ClaimInvariant,
			SourceCapsuleID: capsuleID,
			AffectedFiles:   []string{"internal/schema/goal.go"},
			Status:          schema.ClaimVerified,
			EvidenceIDs:     []string{"EV-tkmeasure-1-0-01HZABCDEFGHIJ00"},
		}); err != nil {
			t.Fatalf("SaveClaim %s: %v", c.id, err)
		}
	}

	// ── Seed failures: mix of short and long summaries (T6 caps at 150 chars) ───
	longFailSummary := strings.Repeat("TestGoalValidation panicked with nil pointer dereference in obligation_planner.go:47 because GoalConditions was nil at the point where range was attempted; ", 2)
	failures := []struct {
		id      string
		summary string
	}{
		{"FAIL-tkmeasure-1", "TestGoalIR_nilConditions: panic: nil pointer dereference in planner"},
		{"FAIL-tkmeasure-2", longFailSummary},
		{"FAIL-tkmeasure-3", "go vet: possible misuse of sync.Mutex by value in GoalIR struct"},
		{"FAIL-tkmeasure-4", strings.Repeat("lint: field OriginalIntent is accessed without nil guard in 3 call sites across obligation_planner.go and projector/service.go; each site must be wrapped with a non-empty check ", 2)},
	}
	for _, f := range failures {
		if err := env.st.SaveFailure(ctx, &schema.FailureFingerprint{
			FailureID:       f.id,
			SourceCapsuleID: capsuleID,
			FailureType:     schema.FailureTest,
			Summary:         f.summary,
			AffectedFiles:   []string{"internal/schema/goal.go"},
			ErrorSignature:  "sig-" + f.id,
		}); err != nil {
			t.Fatalf("SaveFailure %s: %v", f.id, err)
		}
	}

	// ── Seed patches: 1 superseded + 1 candidate per obligation (T12 keeps only candidate) ──
	for i, obID := range []string{obID1, obID2, obID3} {
		// Older superseded patch (T12 must exclude).
		if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID:              fmt.Sprintf("PATCH-tkmeasure-%d-old", i+1),
			CapsuleID:            capsuleID,
			ObligationIDsClaimed: []string{obID},
			Summary:              fmt.Sprintf("first attempt: %s", obID),
			DiffPath:             "",
			Status:               schema.PatchSuperseded,
			CreatedAt:            time.Now().UTC().Add(-5 * time.Minute),
		}); err != nil {
			t.Fatalf("SavePatch old ob=%s: %v", obID, err)
		}
		// Newer candidate patch (T12 must keep).
		if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
			PatchID:              fmt.Sprintf("PATCH-tkmeasure-%d-new", i+1),
			CapsuleID:            capsuleID,
			ObligationIDsClaimed: []string{obID},
			Summary:              fmt.Sprintf("revised implementation for %s after reviewer feedback", obID),
			DiffPath:             "",
			Status:               schema.PatchCandidate,
			CreatedAt:            time.Now().UTC(),
		}); err != nil {
			t.Fatalf("SavePatch new ob=%s: %v", obID, err)
		}
	}

	// ── Run current projector ────────────────────────────────────────────────────
	compiler := New(env.st, config.VerifierConfig{})
	execProjection, err := compiler.CompileExecutor(ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileExecutor: %v", err)
	}
	revProjection, err := compiler.CompileReviewer(ctx, capsuleID)
	if err != nil {
		t.Fatalf("CompileReviewer: %v", err)
	}

	execBriefing, err := runner.SerializeExecutorProjection(execProjection)
	if err != nil {
		t.Fatalf("SerializeExecutorProjection (executor): %v", err)
	}
	revBriefing, err := runner.SerializeExecutorProjection(revProjection)
	if err != nil {
		t.Fatalf("SerializeExecutorProjection (reviewer): %v", err)
	}

	// ── Compute "old" baseline metrics ──────────────────────────────────────────
	// T1+T2: old code used MaxTokens/2 for budget and ×4 bytes/token.
	oldTokenBudget := maxTokens / 2
	oldByteLimit := oldTokenBudget * 4

	// T2: new budget fraction.
	newTokenBudget := int(float64(maxTokens) * 0.70)
	// T1: new bytes/token.
	newByteLimit := newTokenBudget * 3

	// T5: measure claim text savings.
	totalClaimTextOld := 0
	totalClaimTextNew := 0
	for _, c := range claims {
		totalClaimTextOld += len(c.text)
		capped := c.text
		if len(capped) > 200 {
			capped = capped[:197] + "..."
		}
		totalClaimTextNew += len(capped)
	}

	// T6: measure failure summary savings.
	totalFailOld := 0
	totalFailNew := 0
	for _, f := range failures {
		totalFailOld += len(f.summary)
		capped := f.summary
		if len(capped) > 150 {
			capped = capped[:147] + "..."
		}
		totalFailNew += len(capped)
	}

	// T7: evidence ID abbreviation savings.
	// Old: full ULIDs like "EV-tkmeasure-1-0-01HZABCDEFGHIJ00" (~34 chars).
	// New: last 6 chars of ID.
	evidenceCount := 15 // 3 obligations × 5 items
	sampleFullID := "EV-tkmeasure-1-0-01HZABCDEFGHIJ00"
	oldEvidenceIDBytes := len(sampleFullID) * evidenceCount
	newEvidenceIDBytes := 6 * evidenceCount
	t7Saving := oldEvidenceIDBytes - newEvidenceIDBytes

	// T11: executor keeps 3 most recent per obligation (5 items → 3 = 2 dropped per obligation).
	// Reviewer: drops obligations with no candidate patch (none dropped here) + freshness filter.
	evItemsOld := 5 * 3 // 5 items × 3 obligations
	evItemsNewExec := 3 * 3 // capped at 3 × 3 obligations
	t11ExecSaving := evItemsOld - evItemsNewExec

	// T12: patches loaded per obligation — 2 stored (1 superseded + 1 candidate), 1 kept.
	patchesOld := 2 * 3 // 2 patches × 3 obligations
	patchesNew := 1 * 3 // 1 candidate × 3 obligations
	t12Saving := patchesOld - patchesNew

	// T3+T8+T9+T4: compute old serializer overhead vs new.
	oldOverheadBytes, oldSerializerBreakdown := buildOldSerializerOverhead(execProjection)
	newSerializerBytes := len(execBriefing)
	oldSerializerBytes := newSerializerBytes + oldOverheadBytes

	// ── Print report ─────────────────────────────────────────────────────────────
	sep := strings.Repeat("─", 70)
	t.Logf("\n%s", sep)
	t.Logf("TOKEN COST OPTIMIZATION — REAL-TIME MEASUREMENT REPORT")
	t.Logf("%s", sep)
	t.Logf("Scenario: 3 obligations, 15 evidence items (5/ob), 8 claims, 4 failures, 6 patches (2/ob)")
	t.Logf("Budget:   MaxTokens=%d", maxTokens)
	t.Logf("%s", sep)

	t.Logf("\n── Phase 1: Budget Math & Serializer Overhead ──────────────────────")
	t.Logf("T1+T2  Budget fraction + bytes/token")
	t.Logf("       Old:  budget=%d tokens, byte_limit=%d", oldTokenBudget, oldByteLimit)
	t.Logf("       New:  budget=%d tokens, byte_limit=%d", newTokenBudget, newByteLimit)
	t.Logf("       Delta: +%d tokens available (+%.0f%%), byte_limit %+d",
		newTokenBudget-oldTokenBudget,
		float64(newTokenBudget-oldTokenBudget)/float64(oldTokenBudget)*100,
		newByteLimit-oldByteLimit)
	t.Logf("T3+T8+T9+T4  Serializer header/section stripping")
	t.Logf("       Old briefing size: ~%d bytes (includes removed overhead)", oldSerializerBytes)
	t.Logf("       New briefing size:  %d bytes", newSerializerBytes)
	t.Logf("       Overhead removed:   %d bytes", oldOverheadBytes)
	t.Logf("       Breakdown: %s", oldSerializerBreakdown)

	t.Logf("\n── Phase 2: Content Compaction ─────────────────────────────────────")
	t.Logf("T5  Claim text cap (200 chars)")
	t.Logf("    Total claim text before: %d bytes", totalClaimTextOld)
	t.Logf("    Total claim text after:  %d bytes", totalClaimTextNew)
	t.Logf("    Saved:                   %d bytes (%.0f%%)", totalClaimTextOld-totalClaimTextNew,
		float64(totalClaimTextOld-totalClaimTextNew)/float64(totalClaimTextOld)*100)
	t.Logf("T6  Failure summary cap (150 chars)")
	t.Logf("    Total failure text before: %d bytes", totalFailOld)
	t.Logf("    Total failure text after:  %d bytes", totalFailNew)
	t.Logf("    Saved:                     %d bytes (%.0f%%)", totalFailOld-totalFailNew,
		float64(totalFailOld-totalFailNew)/float64(totalFailOld)*100)
	t.Logf("T7  Evidence ID abbreviation (full ULID → last 6 chars)")
	t.Logf("    Old: %d items × %d chars = %d bytes", evidenceCount, len(sampleFullID), oldEvidenceIDBytes)
	t.Logf("    New: %d items × 6 chars  = %d bytes", evidenceCount, newEvidenceIDBytes)
	t.Logf("    Saved: %d bytes (%.0f%%)", t7Saving, float64(t7Saving)/float64(oldEvidenceIDBytes)*100)

	t.Logf("\n── Phase 3: Structural Changes ─────────────────────────────────────")
	t.Logf("T11  Role-based evidence freshness (executor)")
	t.Logf("     Old: %d evidence items included (all 5 per obligation)", evItemsOld)
	t.Logf("     New: %d evidence items included (3 most recent per obligation)", evItemsNewExec)
	t.Logf("     Dropped: %d items (%.0f%%)", t11ExecSaving, float64(t11ExecSaving)/float64(evItemsOld)*100)
	t.Logf("T12  Candidate-only patch fan-out (per obligation)")
	t.Logf("     Old: %d patches included (all statuses)", patchesOld)
	t.Logf("     New: %d patches included (candidate only, most recent)", patchesNew)
	t.Logf("     Dropped: %d patches (%.0f%%)", t12Saving, float64(t12Saving)/float64(patchesOld)*100)
	t.Logf("T10  Evidence section reuse (sub-hash)")
	t.Logf("     EvidenceHash=%s (reuse candidate when evidence unchanged)", execProjection.EvidenceHash[:16]+"...")

	t.Logf("\n── Actual Projection Output Measurements ───────────────────────────")
	t.Logf("Executor projection:")
	t.Logf("  TokensBefore: %d bytes (pre-budget raw)", execProjection.TokensBefore)
	t.Logf("  TokensAfter:  %d bytes (after budget enforcement)", execProjection.TokensAfter)
	t.Logf("  TokenBudget:  %d tokens / %d bytes", execProjection.TokenBudget, execProjection.TokenBudget*3)
	t.Logf("  Sections included: %d", len(execProjection.IncludedSections))
	t.Logf("  Sections omitted:  %v", execProjection.OmittedSections)
	t.Logf("  Serialized briefing: %d bytes", len(execBriefing))
	t.Logf("Reviewer projection:")
	t.Logf("  TokensBefore: %d bytes (pre-budget raw)", revProjection.TokensBefore)
	t.Logf("  TokensAfter:  %d bytes (after budget enforcement)", revProjection.TokensAfter)
	t.Logf("  TokenBudget:  %d tokens / %d bytes", revProjection.TokenBudget, revProjection.TokenBudget*3)
	t.Logf("  Sections included: %d", len(revProjection.IncludedSections))
	t.Logf("  Sections omitted:  %v", revProjection.OmittedSections)
	t.Logf("  Serialized briefing: %d bytes", len(revBriefing))

	t.Logf("\n── Cumulative Summary ──────────────────────────────────────────────")
	serializerSaving := oldOverheadBytes
	contentSaving := (totalClaimTextOld - totalClaimTextNew) + (totalFailOld - totalFailNew) + t7Saving
	structuralSaving := (t11ExecSaving * 150) + (t12Saving * 300) // ~rough byte equivalent per dropped item
	totalSaving := serializerSaving + contentSaving + structuralSaving
	budgetGain := newTokenBudget - oldTokenBudget
	t.Logf("  Serializer overhead removed (T3+T4+T8+T9): %d bytes", serializerSaving)
	t.Logf("  Content compaction savings  (T5+T6+T7):    %d bytes", contentSaving)
	t.Logf("  Structural item reduction   (T11+T12):     ~%d bytes", structuralSaving)
	t.Logf("  Budget headroom gained      (T1+T2):       +%d tokens (%d tokens → %d tokens)", budgetGain, oldTokenBudget, newTokenBudget)
	t.Logf("  Estimated total per-invocation savings:    ~%d bytes", totalSaving)
	t.Logf("%s\n", sep)
}

// buildOldSerializerOverhead computes bytes that the old serializer emitted but
// have since been stripped (T3, T4, T8, T9). Returns the actual overhead byte
// count and a human-readable breakdown string.
//
// T8 note: in the old design IncludedSections stored section *keys* (not full
// content), so we reproduce that key list from the known section key set.
func buildOldSerializerOverhead(p *schema.ContextProjection) (int, string) {
	// T9: Context Projection ID + Token Budget lines.
	projIDLine := fmt.Sprintf("- Context Projection ID: `%s`\n", p.ContextProjectionID)
	budgetLine := fmt.Sprintf("- Token Budget: `%d`\n", p.TokenBudget)
	t9 := len(projIDLine) + len(budgetLine)

	// T8: Included and Omitted section key lists (old IncludedSections held keys
	// like "goal_conditions", "obligations", etc., not full content).
	allKeys := []string{
		"role_contract", "goal_conditions", "obligations", "scope_constraints",
		"required_outputs", "candidate_patches", "prior_evidence", "claims", "failure_fingerprints",
	}
	omittedSet := make(map[string]bool, len(p.OmittedSections))
	for _, k := range p.OmittedSections {
		omittedSet[k] = true
	}
	includedKeys := make([]string, 0, len(allKeys))
	for _, k := range allKeys {
		if !omittedSet[k] {
			includedKeys = append(includedKeys, k)
		}
	}
	includedLine := "Included Sections: " + strings.Join(includedKeys, ", ") + "\n"
	omittedLine := "Omitted Sections: " + strings.Join(p.OmittedSections, ", ") + "\n"
	if len(p.OmittedSections) == 0 {
		omittedLine = "Omitted Sections: None\n"
	}
	t8 := len(includedLine) + len(omittedLine)

	// T3: Source Artifact IDs list.
	srcLine := "Source Artifact IDs: " + strings.Join(p.SourceArtifactIDs, ", ") + "\n"
	t3 := len(srcLine)

	// T4: Old Required Output Contract block minus the new 2-line version.
	oldOutputContract := "## Required Output Contract\n" +
		"- Return sidecar-equivalent output with these keys:\n" +
		"  - `obligations_satisfied`: list of obligation IDs satisfied\n" +
		"  - `patch_summary`: human-readable summary of changes\n" +
		"  - `evidence_items`: array of evidence artifacts\n" +
		"  - `claims`: array of new or updated claims\n" +
		"  - `risk_notes`: any risks identified\n" +
		"  - `tokens_used`: total token count\n\n"
	newOutputContract := "## Output\nReturn structured sidecar JSON per the provided schema.\n\n"
	t4 := len(oldOutputContract) - len(newOutputContract)
	if t4 < 0 {
		t4 = 0
	}

	total := t9 + t8 + t3 + t4
	breakdown := fmt.Sprintf("[T9:%db T8:%db T3:%db T4:+%db total=%db]", t9, t8, t3, t4, total)
	return total, breakdown
}
