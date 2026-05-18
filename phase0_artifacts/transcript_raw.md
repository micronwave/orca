# Raw Transcript — Capsule cap-phase0-sentiment-fix-001

## Session Summary

Agent: claude-sonnet-4-6  
Date: 2026-05-17  
Task: Fix test_sentiment_pct_sum.py — percentage sum invariant in SentimentAggregator

---

## Step 1 — Read Test File

Read `tests/test_sentiment_pct_sum.py`. Key findings:
- Test documents known rounding defect in `api/services/sentiment_aggregator.py:compute_market_sentiment()`
- Section 1: 3 tickers (1/1/1 split) → each pct rounds independently → 33.3+33.3+33.3 = 99.9 ≠ 100.0
- Section 2: 6 tickers (2/2/2 split) → same problem
- Section 3: empty list → all zeros — baseline sanity check
- Fix suggested by test docstring: "derive neutral_pct as 100.0 - bullish_pct - bearish_pct"

## Step 2 — Read Source File

Read `api/services/sentiment_aggregator.py`. Located `compute_market_sentiment()` at line 152.

Defect confirmed at lines 220–222:
```python
"bullish_pct": round(bullish / total * 100, 1),
"bearish_pct": round(bearish / total * 100, 1),
"neutral_pct": round(neutral / total * 100, 1),  # independent rounding — bug
```

All three are rounded independently within the return dict literal.

## Step 3 — Capture Base Commit

```
git -C E:\narrative_engine rev-parse HEAD
→ 0d752c5862605043e8a63a2006e92bb87851072d
```

## Step 4 — Apply Fix

Edited `api/services/sentiment_aggregator.py` lines 218–226:

**Before:**
```python
return {
    "market_score": market_score,
    "bullish_pct": round(bullish / total * 100, 1),
    "bearish_pct": round(bearish / total * 100, 1),
    "neutral_pct": round(neutral / total * 100, 1),
    ...
}
```

**After:**
```python
bullish_pct = round(bullish / total * 100, 1)
bearish_pct = round(bearish / total * 100, 1)
neutral_pct = round(100.0 - bullish_pct - bearish_pct, 1)

return {
    "market_score": market_score,
    "bullish_pct": bullish_pct,
    "bearish_pct": bearish_pct,
    "neutral_pct": neutral_pct,
    ...
}
```

Rationale: `neutral_pct` is derived as the residual after the other two are fixed. This is mathematically sound because `bullish + bearish + neutral == total` is guaranteed by their definitions.

## Step 5 — Run Verification

Command: `python -X utf8 tests/test_sentiment_pct_sum.py`

Output:
```
============================================================
Section                                   Pass  Fail
------------------------------------------------------------
  1: market_sentiment pct sum equals 100.0 (1 bullish / 1 bearish / 1 neutral)     1     0
  2: market_sentiment pct sum equals 100.0 (2/2/2 even split)     1     0
  3: empty ticker list returns zeroed structure (existing behaviour, sanity check)     3     0
============================================================
  TOTAL: 5 passed, 0 failed out of 5 tests
============================================================
```

Exit code: 0

## Step 6 — Confirm Scope

Command: `git -C E:\narrative_engine diff HEAD -- api/services/sentiment_aggregator.py`

Output confirms only 4 lines removed, 8 lines added (extracting variables), all within `compute_market_sentiment()`. No other files modified.

## Assumptions Made

- `bullish + bearish` will never exceed `total` (guaranteed by the counting logic `neutral = total - bullish - bearish`)
- Round(100.0 - bullish_pct - bearish_pct, 1) is acceptable precision for the neutral bucket; no caller requires all three to be independently rounded
- The test's Section 3 empty-list early-return path is unaffected (returns hardcoded 0.0s)

## Agent-Reported Risks

- Minor: if future code introduces a bug where bullish or bearish counts exceed total, `neutral_pct` would become negative — but this was already a latent correctness issue and is not introduced by this fix
- Negligible: Python's float subtraction `100.0 - 33.3 - 33.3` produces `33.400000000000006`; `round(..., 1)` normalises this to `33.4` correctly
