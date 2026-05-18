# Phase 0 Execution Plan

> Agent completion rule: any agent completing this plan must update the status markers in this file as work finishes. Mark each checklist item as `[x]`, add dates/evidence paths where requested, and record any change from this plan in the "Plan Deviations" section before continuing. If the plan changes materially, explain why, what changed, and whether the Phase 0 exit criteria are still valid.

## Status

- Overall phase status: `complete`
- Current owner: `claude-sonnet-4-6`
- Last updated: `2026-05-17`
- Phase 0 decision: `pass`

Allowed phase decisions:

- `pass`: all exit criteria passed; Phase 1 may begin.
- `revise_architecture`: at least one exit criterion failed in a way that challenges the architecture.
- `blocked`: external tool, repo, or environment issue prevents completion.

## Purpose

Phase 0 is a falsification spike for Orca. The goal is to prove that the proof-runtime architecture in `orca.md` works on a real coding task before building Phase 1 infrastructure.

This plan must answer five questions with evidence:

1. Can a real coding goal be converted into useful obligations?
2. Can an agent run be treated as an execution capsule with bounded scope and required outputs?
3. Can sidecar output and transcript extraction produce the same artifact schema?
4. Can evidence be mapped back to obligations strongly enough to support a merge or retry decision?
5. Can a follow-up context projection be smaller than raw transcript replay without losing required context?

Do not begin Phase 1 infrastructure unless every Phase 0 exit criterion is satisfied.

## Non-Goals

- Do not build the Orca CLI.
- Do not build the artifact store as production infrastructure.
- Do not build adapters beyond the minimum manual transcript and sidecar normalization needed for the spike.
- Do not introduce Work Orders.
- Do not create a desktop UI.
- Do not add learning, routing, mutation testing, or advanced topology support.

## Required Output Files

Create these under a new root directory named `phase0_artifacts/`.

- `phase0_artifacts/README.md`
- `phase0_artifacts/goal_ir.json`
- `phase0_artifacts/obligations.json`
- `phase0_artifacts/capsule_contract.json`
- `phase0_artifacts/human_summary_projection.md`
- `phase0_artifacts/executor_projection.md`
- `phase0_artifacts/agent_sidecar.json`
- `phase0_artifacts/transcript_raw.md`
- `phase0_artifacts/transcript_extracted_artifacts.json`
- `phase0_artifacts/patch_artifact.json`
- `phase0_artifacts/evidence_artifacts.json`
- `phase0_artifacts/verifier_result.json`
- `phase0_artifacts/failure_fingerprint.json`
- `phase0_artifacts/followup_context_projection.md`
- `phase0_artifacts/token_comparison.json`
- `phase0_artifacts/proof_carrying_patch.md`
- `phase0_artifacts/phase0_final_report.md`

If a file is intentionally not produced, keep a stub with an explanation and mark the relevant exit criterion failed or blocked.

## Spike Repo Selection

Use a real local repo with a small, low-risk coding task. Prefer a repo where tests can run locally and the expected change touches one to three files.

Selection criteria:

- [x] Real repo selected.
- [x] Baseline git status captured.
- [x] Small coding goal chosen.
- [x] Test or static verification command identified.
- [x] Rollback strategy documented.

Record in `phase0_artifacts/README.md`:

- repo path
- base commit
- selected goal
- expected affected files
- verification commands
- known environment constraints

Avoid tasks that require new dependencies, network access, secrets, broad refactors, or destructive commands.

## Step 1: Create Manual Goal IR

Status: `[x]`

Convert the selected coding goal into `goal_ir.json`.

Minimum fields:

- `goal_id`
- `original_intent`
- `goal_conditions`
- `scope_constraints`
- `risk_level`
- `created_at`
- `status`

Acceptance checks:

- [x] Goal conditions are written in user-facing language.
- [x] Scope constraints include files or areas that must not be touched.
- [x] Risk level is justified in `phase0_artifacts/README.md`.

Evidence to record:

- `phase0_artifacts/goal_ir.json`
- supporting note in `phase0_artifacts/README.md`

## Step 2: Derive Obligations Before Tasks

Status: `[x]`

Create `obligations.json` from the Goal IR before defining implementation tasks.

Minimum obligations:

- reproduce or inspect the current behavior
- identify affected files or symbols
- implement the minimal change
- run targeted verification
- prove no unrelated files changed
- document unresolved risk or test gap

Acceptance checks:

- [x] Every obligation maps to at least one goal condition.
- [x] Every blocking obligation has explicit evidence requirements.
- [x] At least one obligation is capable of failing.
- [x] The obligation set is useful enough to decide merge, retry, or reject.

Evidence to record:

- `phase0_artifacts/obligations.json`

## Step 3: Define One Execution Capsule

Status: `[x]`

Create `capsule_contract.json` for one Codex or Claude run.

Minimum fields:

- `capsule_id`
- `obligation_ids`
- `agent`
- `role`
- `context_projection_id` (reference to executor_projection.md for this spike)
- `allowed_paths`
- `forbidden_paths`
- `allowed_tools`
- `forbidden_actions`
- `required_outputs`
- `verifier_gates`
- `budget`
- `sandbox`

Acceptance checks:

- [x] Capsule has bounded write scope.
- [x] Required outputs include patch, evidence notes, assumptions, and risks.
- [x] Verifier gates match the obligations.
- [x] Human-gated actions are identified if applicable.

Evidence to record:

- `phase0_artifacts/capsule_contract.json`

## Step 4: Compile Human Summary Projection

Status: `[x]`

Create `human_summary_projection.md` before running the agent.

It must include:

- goal in plain language
- conditions addressed
- obligations addressed
- implementation approach
- expected file scope
- explicit exclusions
- selected topology and rationale
- design decisions
- pre-execution risks
- evidence plan
- required approvals

Acceptance checks:

- [x] A developer can make a go/no-go decision from this document.
- [x] It does not include raw transcript content.
- [x] It does not present proposed claims as verified facts.
- [x] It is distinct from the executor projection.

Evidence to record:

- `phase0_artifacts/human_summary_projection.md`

## Step 5: Compile Executor Projection

Status: `[x]`

Create `executor_projection.md` as the actual agent briefing.

It must include only what the agent needs:

- relevant goal conditions
- assigned obligations
- allowed paths and forbidden paths
- allowed tools and forbidden actions
- current failure fingerprints, if any
- required outputs and schema
- verification commands

Acceptance checks:

- [x] Executor projection is shorter and more constraint-focused than the human summary.
- [x] It excludes broad product explanation that is not needed for the task.
- [x] It does not include raw prior transcript content.

Evidence to record:

- `phase0_artifacts/executor_projection.md`

## Step 6: Run One Agent Capsule

Status: `[x]`

Run Codex or Claude manually using the executor projection as the prompt.

During the run, capture:

- raw transcript or terminal session notes
- files changed
- commands run
- agent-reported assumptions
- agent-reported unresolved risks
- final diff

Acceptance checks:

- [x] Agent stayed inside allowed scope, or scope violations were recorded.
- [x] Required outputs were produced, or missing outputs were recorded.
- [x] No destructive action occurred outside the capsule scope.
- [x] Raw transcript was saved without becoming durable memory beyond this Phase 0 test artifact.

Evidence to record:

- `phase0_artifacts/transcript_raw.md`
- changed files and diff path in `phase0_artifacts/patch_artifact.json`

## Step 7: Produce Sidecar Output

Status: `[x]`

Normalize the agent result into `agent_sidecar.json`.

Minimum contents:

- capsule id
- obligations claimed
- changed files
- commands run
- evidence references
- assumptions
- unresolved risks
- claims
- follow_up_needed
- summary

Acceptance checks:

- [x] Sidecar output is structured JSON.
- [x] It references obligations by ID.
- [x] It separates verified evidence from agent claims.
- [x] It can be consumed without reading the raw transcript.

Evidence to record:

- `phase0_artifacts/agent_sidecar.json`

## Step 8: Extract Artifacts From Transcript

Status: `[x]`

Independently extract the same artifact schema from `transcript_raw.md` into `transcript_extracted_artifacts.json`.

Acceptance checks:

- [x] Transcript extraction produces the same top-level schema as `agent_sidecar.json`.
- [x] Any differences are listed explicitly in `phase0_artifacts/phase0_final_report.md`.
- [x] Downstream verifier inputs do not depend on whether artifacts came from sidecar output or transcript extraction.

Evidence to record:

- `phase0_artifacts/transcript_extracted_artifacts.json`
- schema comparison note in `phase0_artifacts/phase0_final_report.md`

## Step 9: Attach Evidence Artifacts

Status: `[x]`

Run targeted verification commands and convert the results into `evidence_artifacts.json`.

At minimum, capture:

- command
- exit code
- summary
- raw log path or inline short output
- obligations supported
- obligations weakened
- timestamp

Acceptance checks:

- [x] Every blocking obligation has supporting evidence or an explicit failure.
- [x] Test, lint, typecheck, or static checks are recorded with exit codes.
- [x] Evidence is not replaced by agent confidence.

Evidence to record:

- `phase0_artifacts/evidence_artifacts.json`
- raw command logs if useful

## Step 10: Create Patch Artifact

Status: `[x]`

Create `patch_artifact.json`.

Minimum contents:

- `patch_id`
- `capsule_id`
- `base_commit`
- `changed_files`
- `diff_path`
- `summary`
- `obligation_ids_claimed`
- `risk_notes`
- `status`

Acceptance checks:

- [x] Changed files match the actual git diff.
- [x] Any unrelated file change is recorded as a scope violation.
- [x] Patch status remains `candidate` until verifier result is complete.

Evidence to record:

- `phase0_artifacts/patch_artifact.json`

## Step 11: Verify Obligation Satisfaction

Status: `[x]`

Create `verifier_result.json` by mapping evidence to obligations.

Minimum verdicts:

- `satisfied`
- `failed`
- `waived`
- `blocked`

Acceptance checks:

- [x] Each obligation has a verdict.
- [x] Each satisfied obligation references concrete evidence.
- [x] Failed obligations produce a retry reason.
- [x] Waived obligations include human approval or a reason they are non-blocking.
- [x] Verifier can recommend accept, reject, or retry.

Evidence to record:

- `phase0_artifacts/verifier_result.json`

## Step 12: Create Failure Fingerprint Or Explicit No-Failure Record

Status: `[x]`

If any verification failed, create a useful `failure_fingerprint.json`.

Minimum fields:

- `failure_id`
- `failure_type`
- `summary`
- `affected_files`
- `affected_symbols`
- `error_signature`

If nothing failed, create `failure_fingerprint.json` with:

- `failure_id: null`
- `status: "no_failure"`
- `summary`

Acceptance checks:

- [x] A failed run produces enough information to create a retry capsule.
- [x] A passing run still records that no failure fingerprint was needed.

Evidence to record:

- `phase0_artifacts/failure_fingerprint.json`

## Step 13: Compile Follow-Up Context Projection

Status: `[x]`

Create `followup_context_projection.md` for a hypothetical next capsule.

It must include:

- current goal status
- remaining open obligations
- accepted evidence
- verified claims only
- failed obligations and failure fingerprint, if any
- next recommended action
- required verifier gates

Acceptance checks:

- [x] Projection avoids raw transcript replay.
- [x] Projection contains enough context for a follow-up agent to act.
- [x] Proposed or stale claims are labeled and not presented as facts.

Evidence to record:

- `phase0_artifacts/followup_context_projection.md`

## Step 14: Compare Token Use Against Transcript Replay

Status: `[x]`

Create `token_comparison.json`.

Compare:

- raw transcript continuation token estimate
- follow-up context projection token estimate
- absolute token savings
- percent token savings
- any important context lost

Use a consistent estimator. A simple approximation is acceptable for Phase 0 if documented, such as words divided by 0.75 or characters divided by 4.

Acceptance checks:

- [x] The estimator is documented.
- [x] Follow-up projection is smaller than raw transcript replay.
- [x] The report states whether the projection lost context needed for the next capsule.

Evidence to record:

- `phase0_artifacts/token_comparison.json`

## Step 15: Assemble Proof-Carrying Patch

Status: `[x]`

Create `proof_carrying_patch.md`.

It must include:

- goal
- obligations addressed
- patch summary
- files changed
- commands run
- evidence artifacts
- verifier result
- assumptions
- unresolved risks
- merge recommendation
- retry contract if not mergeable

Acceptance checks:

- [x] A human can evaluate merge readiness without reading the raw transcript.
- [x] The proof maps evidence to obligations.
- [x] Merge recommendation follows from verifier result, not agent confidence.

Evidence to record:

- `phase0_artifacts/proof_carrying_patch.md`

## Step 16: Final Phase 0 Report

Status: `[x]`

Create `phase0_final_report.md`.

It must answer:

- Did artifact flow work on a real repo?
- Could evidence be mapped to obligations?
- Was context projection smaller than transcript replay?
- Did failure output create a useful retry contract?
- Did transcript extraction and sidecar output conform to the same schema?
- Should Phase 1 begin, or should the architecture be revised first?

Acceptance checks:

- [x] Every Phase 0 exit criterion has a pass/fail/blocked verdict.
- [x] The final recommendation is one of `pass`, `revise_architecture`, or `blocked`.
- [x] Any plan deviations are copied into the "Plan Deviations" section below.

Evidence to record:

- `phase0_artifacts/phase0_final_report.md`

## Exit Criteria Checklist

Do not mark Phase 0 as passed until all are checked.

- [x] Artifact flow works on a real repo.
- [x] Evidence can be mapped to obligations.
- [x] Context projection is smaller than transcript replay for the same follow-up capsule.
- [x] Failure output creates a useful retry contract, or a no-failure record explains why no retry contract was needed.
- [x] Transcript extraction adapter produces artifacts conforming to the same schema as sidecar output.
- [x] Proof-carrying patch format is usable for a human merge/retry decision.
- [x] No Phase 1 infrastructure was built during the spike.

## Plan Deviations

Record all deviations here before continuing.

| Date | Agent | Planned Step | Change | Reason | Exit Criteria Impact |
| --- | --- | --- | --- | --- | --- |
| 2026-05-17 | claude-sonnet-4-6 | none | none | No deviations. All 16 steps completed in order with no stubs. | Exit criteria unaffected. |

## Completion Log

Add a row whenever work is completed.

| Date | Agent | Completed Item | Evidence Path | Notes |
| --- | --- | --- | --- | --- |
| 2026-05-17 | claude-sonnet-4-6 | All 16 steps + all 17 artifact files + fix applied | phase0_artifacts/ (17 files), E:\narrative_engine\api\services\sentiment_aggregator.py | Test passes: 5/5. Token reduction: 62.6%. Verifier: accept. Decision: pass. |

## Phase 1 Gate

Phase 1 may begin only when:

- `Overall phase status` is `complete`
- `Phase 0 decision` is `pass`
- all exit criteria are checked
- `phase0_artifacts/phase0_final_report.md` recommends proceeding

If any exit criterion fails, stop and revise `orca.md` before creating Phase 1 implementation plans.
