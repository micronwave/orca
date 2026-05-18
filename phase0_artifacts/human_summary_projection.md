# Human Summary Projection — Phase 0 Spike

## Goal (Plain Language)

Fix a rounding defect in `SentimentAggregator.compute_market_sentiment()` so that the three output percentage fields (`bullish_pct`, `bearish_pct`, `neutral_pct`) always sum to exactly `100.0`. Currently they are each rounded independently, which causes the sum to be `99.9` whenever a 1/3–1/3–1/3 (or equivalent) split occurs.

## Conditions Addressed

- **GC-1**: Sum equals 100.0 for any non-empty input after fix
- **GC-2**: Fix uses residual derivation for `neutral_pct`, not independent rounding
- **GC-3**: Empty-ticker case unchanged (still returns 0.0 / 0.0 / 0.0)
- **GC-4**: Only `compute_market_sentiment()` in `api/services/sentiment_aggregator.py` is touched
- **GC-5**: Existing tests continue to pass

## Obligations Addressed

All six obligations (OBL-1 through OBL-6) are in scope for this capsule.

## Implementation Approach

Compute `bullish_pct` and `bearish_pct` as before (independently rounded to 1 decimal place), then derive `neutral_pct` as `round(100.0 - bullish_pct - bearish_pct, 1)`. This eliminates the three-way rounding error while keeping the output format unchanged.

## Expected File Scope

| File | Change |
|---|---|
| `api/services/sentiment_aggregator.py` | Lines 218–226: extract variables before `return`, replace `neutral_pct` derivation |

No other files change.

## Explicit Exclusions

- Test files (do not modify)
- Any other service in `api/services/`
- `repository.py`, `pipeline.py`, `settings.py`
- CI/CD, deployment config, requirements.txt

## Selected Topology

Single agent, single capsule. No parallelism needed — the fix is one method in one file.

**Rationale:** The defect is fully localized. A single implementer capsule followed by a verifier read of test output is sufficient. No topology complexity is warranted.

## Design Decisions

1. **Residual derivation (chosen):** `neutral_pct = 100.0 - bullish_pct - bearish_pct`. Simple, guaranteed to sum to 100.0. The small residual amount is correctly absorbed into the neutral bucket which is computed last and is therefore least salient to users.
2. **Alternative rejected:** Carry full float precision and round only at display layer — would require API contract change (callers currently receive pre-rounded values).

## Pre-Execution Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| `bullish + bearish` exceeds 100.0 due to future upstream bug | Very low | Cannot occur given `neutral = total - bullish - bearish` ensures `bullish + bearish <= total` |
| Floating-point subtraction produces -0.0 for edge case | Negligible | `round()` converts -0.0 to 0.0 in Python |

## Evidence Plan

- Run `python -X utf8 tests/test_sentiment_pct_sum.py` before and after fix; capture stdout/stderr and exit code
- Capture `git diff` to prove single-file scope
- Record both in `evidence_artifacts.json`

## Required Approvals

None — change is purely internal arithmetic within one private method. No API schema changes, no database migrations, no new dependencies.

## Note

This document is a pre-execution plan. Claims about test results are expectations, not verified facts, until `evidence_artifacts.json` is populated.
