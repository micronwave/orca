# Proof-Carrying Patch — cap-phase0-sentiment-fix-001

## Goal

Fix `SentimentAggregator.compute_market_sentiment()` so that `bullish_pct + bearish_pct + neutral_pct == 100.0` for any non-empty ticker list.

## Obligations Addressed

| Obligation | Description | Verdict |
|---|---|---|
| OBL-1 | Reproduce defect | satisfied |
| OBL-2 | Identify file/symbol | satisfied |
| OBL-3 | Implement minimal fix | satisfied |
| OBL-4 | Run targeted verification | satisfied |
| OBL-5 | Prove no unrelated files changed | satisfied |
| OBL-6 | Document unresolved risk | satisfied |

## Patch Summary

Extracted `bullish_pct` and `bearish_pct` as local variables (rounded independently as before). Changed `neutral_pct` from `round(neutral / total * 100, 1)` to `round(100.0 - bullish_pct - bearish_pct, 1)`. This eliminates independent three-way rounding and guarantees the sum is exactly 100.0.

## Files Changed

| File | Lines Removed | Lines Added |
|---|---|---|
| `api/services/sentiment_aggregator.py` | 4 | 8 |

No other files changed.

## Commands Run

```
python -X utf8 tests/test_sentiment_pct_sum.py   # exit 0, 5 passed 0 failed
git diff HEAD -- api/services/sentiment_aggregator.py  # single file confirmed
```

## Evidence Artifacts

| ID | Command | Exit Code | Obligations |
|---|---|---|---|
| ev-test-run | `python -X utf8 tests/test_sentiment_pct_sum.py` | 0 | OBL-1, OBL-4 |
| ev-git-diff-scope | `git diff HEAD` | 0 | OBL-5 |

## Verifier Result

All 6 obligations: **satisfied**. Recommendation: **accept**.

See `verifier_result.json` for per-obligation verdicts with evidence references.

## Assumptions

1. `bullish + bearish` cannot exceed `total` — guaranteed by `neutral = total - bullish - bearish` counting
2. Callers accept `neutral_pct` as a residual derivation (semantically correct; the neutral count IS the remainder)
3. `round()` normalises float subtraction epsilon; no observed drift

## Unresolved Risks

1. If a future upstream bug allows `bullish` or `bearish` > `total`, `neutral_pct` would go negative. Pre-existing risk, not introduced by this fix.
2. Float epsilon from `100.0 - 33.3 - 33.3` normalised by `round(..., 1)` — correctly produces `33.4`.

## Merge Recommendation

**MERGE.** All obligations satisfied, evidence is concrete, scope is limited to one arithmetic change in one method. No follow-up capsule needed.

## Retry Contract

Not applicable — verifier recommends accept. If a retry were ever required: base commit `0d752c5862605043e8a63a2006e92bb87851072d`, re-run `test_sentiment_pct_sum.py`, apply failure fingerprint from `failure_fingerprint.json` (currently no_failure).
