# Phase 2 decision: claim status scope

## Decision

Phase 2 intentionally supports exactly three durable `ClaimStatus` values:

- `proposed`
- `verified`
- `stale`

`contested` and `invalidated` remain deferred to Phase 3.

## Rationale

Phase 2 reconciliation logic verifies claims by artifact presence and invalidates
previously verified claims when accepted patches overlap affected files/symbols.
That flow requires only:

1. "not yet trusted" (`proposed`),
2. "trusted with evidence" (`verified`), and
3. "formerly trusted but code moved" (`stale`).

Adding adversarial or provenance-conflict states in Phase 2 would increase merge
policy and projection complexity without changing current acceptance behavior.
Phase 3 will introduce `contested`/`invalidated` together with explicit dispute
workflows and downstream handling rules.

