# Phase 0 Spike — README

## Repo Details

| Field | Value |
|---|---|
| Repo path | `E:\narrative_engine` |
| Base commit | `0d752c5862605043e8a63a2006e92bb87851072d` |
| Selected goal | Fix the percentage-sum rounding defect in `SentimentAggregator.compute_market_sentiment()` so that `bullish_pct + bearish_pct + neutral_pct == 100.0` |
| Expected affected files | `api/services/sentiment_aggregator.py` (1 file, ~4 line change) |
| Verification command | `python -X utf8 tests/test_sentiment_pct_sum.py` |

## Known Environment Constraints

- Python 3.x required (3.10+ recommended for `match`/type-union syntax elsewhere in repo)
- Test runner: plain Python script (not pytest) — run directly: `python -X utf8 tests/test_sentiment_pct_sum.py`
- No network access required; test uses `unittest.mock` throughout
- No database required; all repo calls are mocked

## Goal Justification

The failing test `tests/test_sentiment_pct_sum.py` documents a known rounding invariant violation. When 3 tickers each land in a different sentiment bucket (bullish / bearish / neutral), each percentage is `1/3 * 100 = 33.333...`. Rounded independently to 1 decimal place each becomes `33.3`, summing to `99.9` instead of `100.0`. The fix is purely arithmetic (1–4 lines); it does not require any new dependencies, schema changes, or broad refactors.

## Risk Level

**Low.** Single arithmetic expression change in one method. No side effects on other callers — the fix only changes how `neutral_pct` is derived (residual vs. independent rounding), which is semantically identical when the other two values are already rounded. Rollback: `git checkout api/services/sentiment_aggregator.py`.

## Rollback Strategy

```
git -C E:\narrative_engine checkout HEAD -- api/services/sentiment_aggregator.py
```
