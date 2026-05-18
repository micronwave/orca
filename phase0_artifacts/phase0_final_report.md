# Phase 0 Final Report

**Date:** 2026-05-17  
**Spike repo:** `E:\narrative_engine`  
**Goal:** Fix `SentimentAggregator.compute_market_sentiment()` rounding defect  
**Capsule:** `cap-phase0-sentiment-fix-001`

---

## Five Questions

### 1. Did artifact flow work on a real repo?

**Yes.** A real local repo (`E:\narrative_engine`, commit `0d752c5`) was used. A real failing test (`tests/test_sentiment_pct_sum.py`) drove the goal. The artifact pipeline (Goal IR → obligations → capsule contract → projections → agent run → sidecar → transcript extraction → evidence → verifier → proof-carrying patch) was executed end-to-end. All 17 required output files were produced without stubs.

### 2. Could evidence be mapped to obligations?

**Yes.** Every blocking obligation has a concrete evidence artifact with an ID, command, and exit code. `verifier_result.json` maps each obligation to its verdict and evidence reference. No obligation was accepted on agent confidence alone.

### 3. Was context projection smaller than transcript replay?

**Yes.** Raw transcript: 961 estimated tokens. Follow-up context projection: 359 estimated tokens. Savings: 602 tokens (62.6%). The projection preserves all decision-critical content (verified claims, evidence IDs, obligation status, risks) and omits step-by-step narration.

### 4. Did failure output create a useful retry contract?

**Not applicable — no failure occurred.** `failure_fingerprint.json` records `status: no_failure`. The proof-carrying patch includes a retry contract template for completeness, but it was not needed. The architecture supports retry contract generation (the fields are defined in the capsule contract and verifier schema) and would have been exercised if any obligation had returned `failed`.

### 5. Did transcript extraction and sidecar output conform to the same schema?

**Yes.** `transcript_extracted_artifacts.json` was produced independently from `transcript_raw.md`. It contains the same top-level keys as `agent_sidecar.json` with equivalent values. The only addition is a `_schema_diff_vs_sidecar` field recording that no difference was found. No differences required documentation in this file.

---

## Exit Criteria Checklist

| Criterion | Verdict |
|---|---|
| Artifact flow works on a real repo | **pass** |
| Evidence can be mapped to obligations | **pass** |
| Context projection is smaller than transcript replay | **pass** (62.6% reduction) |
| Failure output creates a useful retry contract, or no-failure record explains why not | **pass** (no_failure record present) |
| Transcript extraction produces artifacts conforming to same schema as sidecar output | **pass** |
| Proof-carrying patch format is usable for a human merge/retry decision | **pass** |
| No Phase 1 infrastructure was built during the spike | **pass** |

---

## Plan Deviations

None. All 16 steps were executed in order. All required output files were produced without stubs. No deviation from the execution plan occurred.

---

## Final Recommendation

**`pass`** — all Phase 0 exit criteria are satisfied. Phase 1 infrastructure may begin.

The spike confirmed that:
- The obligation → evidence → verifier chain produces actionable merge/retry decisions without relying on agent confidence
- Context projections meaningfully reduce token cost for follow-up capsules (62.6% reduction on this task)
- Transcript extraction and sidecar output are structurally equivalent and interchangeable as verifier inputs
- The proof-carrying patch format gives a human enough information to evaluate merge readiness independently of the raw transcript
