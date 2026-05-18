# Executor Projection — Capsule cap-phase0-sentiment-fix-001

## Assigned Obligations

- OBL-1: Reproduce defect (confirm test fails pre-fix)
- OBL-2: Identify file/symbol (api/services/sentiment_aggregator.py, compute_market_sentiment)
- OBL-3: Implement minimal fix (neutral_pct as residual)
- OBL-4: Run targeted verification (test exits 0)
- OBL-5: Prove no unrelated files changed (git diff scope)
- OBL-6: Document unresolved risk (floating-point edge note)

## Allowed Paths

- `api/services/sentiment_aggregator.py` — write allowed

## Forbidden Paths

- `tests/**` — read-only
- All other `api/services/*.py`
- `repository.py`, `pipeline.py`, `settings.py`, `.env`, `*.db`

## Allowed Tools

- `Read` (any file for inspection)
- `Edit` / `Write` (only `api/services/sentiment_aggregator.py`)
- `Bash` for: `git diff`, `python -X utf8 tests/test_sentiment_pct_sum.py`

## Forbidden Actions

- `git commit`, `git push`, `pip install`
- Write to any file except `api/services/sentiment_aggregator.py`

## Failure Fingerprints

None — this is the first capsule run.

## Required Outputs

1. Modified `api/services/sentiment_aggregator.py`
2. `patch_artifact.json` — changed files, diff path, obligation IDs
3. `agent_sidecar.json` — structured claims, evidence refs, risks
4. `evidence_artifacts.json` — test command, exit code, summary

## Verification Commands

```
cd E:\narrative_engine
python -X utf8 tests/test_sentiment_pct_sum.py
git diff HEAD -- api/services/sentiment_aggregator.py
```

Expected: 5 passed, 0 failed; exit code 0; diff shows only sentiment_aggregator.py.

## Schema Reference

`agent_sidecar.json` must contain:
`capsule_id`, `obligations_claimed`, `changed_files`, `commands_run`, `evidence_references`, `assumptions`, `unresolved_risks`, `claims`, `follow_up_needed`, `summary`
