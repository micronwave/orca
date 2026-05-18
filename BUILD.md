# Orca — Phase 1 Build Plan

## Current State

**Complete and tested:**
- `internal/schema` — all MVP data types (no business logic)
- `internal/eventlog` — EventLog interface + FileLog concrete implementation
- `internal/store` — ArtifactStore interface + FileStore concrete implementation + Replay
- `internal/integration` — 4 Phase 1 acceptance tests (3 failing, 1 passing)
- All other packages — interface contracts and module boundary docs only; no implementations

**Failing integration tests (root cause: Reconciler not implemented):**
- `TestCrashResumableRun` — Phase B (reconcile gate)
- `TestAcceptedPatchRequiresEvidenceBundle`
- `TestAcceptedPatchRequiresPresentEvidenceArtifacts`

**Passing:**
- `TestDeterministicStateReconstruction` — data layer is complete

---

## Repeatable Build Pattern

Every remaining component follows the same pattern — because every interface is already stubbed and the dependency contracts are locked:

1. Implement the concrete type satisfying the existing interface in its package
2. Write unit tests; wire into the integration test where applicable
3. Confirm no forbidden imports are introduced (each package's doc.go lists them)
4. Move to the next step

The import graph is enforced by the module boundary rules. Violating it is the primary architectural mistake risk. When in doubt, check the package's `// Must NOT import:` contract before adding any dependency.

---

## Build Steps

### Step 1 — Reconciler (`internal/reconciler`)

Most immediate build. Fixes 3 failing integration tests and is the structural core of the proof runtime. Depends only on the store (already complete).

**In:** VerifierResult for a patch, Obligations, ClaimArtifacts for the patch's capsule, EvidenceArtifacts, FailureFingerprints from the capsule run, BudgetRecords for the goal.
**Out:** updated Obligation statuses, updated PatchArtifact status, ClaimArtifact status updates (`proposed` → `verified`) for claims from the accepted capsule that are directly supported by evidence in the VerifierResult, follow-up Obligations from blocking failures, BudgetRecord updates, DecisionRecord, StateSnapshot. ReconcileResult returned to the orchestrator.

Hard rule: a patch cannot be accepted without mapping evidence to every blocking obligation. The Reconciler must also verify that each EvidenceID in ObligationResults resolves to an artifact in the store — not just that the IDs list is non-empty. Every `UpdateObligationStatus` call must be preceded by emitting `obligation_status_updated`; every `UpdatePatchStatus` call must be preceded by emitting `patch_accepted` or `patch_rejected`; every `UpdateClaimStatus` call must be preceded by emitting `claim_status_updated`. All three ordering rules are enforced by the `store.go` contract.

Claim verification ownership: orca.md §16 requires a minimal claim ledger (proposed → verified → stale). No existing interface contract assigns this responsibility. The Reconciler owns it: on patch acceptance, load claims for the capsule (patch → capsule_id → claims), and for each claim whose `evidence_ids` all resolve to artifacts in the store, call `UpdateClaimStatus(verified)`. This requires adding `LoadClaimsForCapsule(ctx, capsuleID) ([]*schema.ClaimArtifact, error)` to the `store.go` ArtifactStore interface and updating the reconciler.go dependency contract to include it in reads and `UpdateClaimStatus` in writes.

Milestone: all 4 integration tests pass.

---

### Step 2 — IntentCompiler (`internal/intent`)

Entry point for the control loop. Takes raw user goal text, derives GoalConditions, persists GoalIR, emits `goal_created`. May call a model to parse and structure intent. Should not create Obligations or Capsules — those belong to the VerifierEngine and ObligationPlanner respectively.

**In:** raw intent string.
**Out:** GoalIR with embedded GoalConditions in the store; `goal_created` in the log.

MVP constraint: one active goal per repo. Before creating a new goal, call `store.LoadActiveGoal`; return an error if a non-nil GoalIR is returned.

Spec note: orca.md §4 describes the Intent Compiler as creating "Goal IR and initial obligations." This is stale — §6 (the control loop) is authoritative. IntentCompiler creates Goal IR only; the VerifierEngine proposes the initial obligations in Step 3. Do not add obligation creation here.

---

### Step 3 — VerifierEngine: ProposeObligations (`internal/verifier`)

First of the VerifierEngine's two jobs. After IntentCompiler creates a GoalIR, the verifier derives the initial set of checkable obligations from it — what evidence would prove progress for each goal condition. May call a model to generate obligation descriptions from goal conditions.

**In:** GoalIR and GoalConditions from the store.
**Out:** Obligations persisted via `SaveObligation`; `obligation_created` emitted by the store on each save.

Must not assign obligations to capsules or advance obligation status — both belong to the planner and reconciler respectively. This job is called once by the orchestrator, between IntentCompiler and ObligationPlanner.

---

### Step 4 — TopologyClassifier (`internal/planner`, internal dependency)

Pure logic. No store reads, no I/O, no model calls. Takes Obligations and FailureFingerprints already loaded by the planner; returns one of the three MVP topologies and a rationale string. The easiest component to fully unit test.

**In:** `[]*schema.Obligation`, `[]*schema.FailureFingerprint`.
**Out:** `schema.Topology` (`single` | `implementer_reviewer` | `human_gated`), rationale string.

The rationale must name the specific classifier inputs that drove the decision — obligation risk levels, fingerprint count in affected files, whether reproduction is needed, etc. "High risk" is not enough; say which obligation was high risk and why that triggered the topology. `parallel`, `test_first`, and `investigate_then_implement` are Phase 2 — do not add them here.

---

### Step 5 — ObligationPlanner (`internal/planner`)

Reads open obligations for a goal, calls the TopologyClassifier (Step 4), creates ExecutionCapsules (one per obligation group after topology selection), persists the topology DecisionRecord, returns capsule IDs and topology to the orchestrator.

**In:** open Obligations and FailureFingerprints loaded from the store.
**Out:** ExecutionCapsules (state = `CapsuleStatePending`) and a topology DecisionRecord in the store. PlanResult returned to the orchestrator.

Key constraints:
- Capsules are created with state `pending`. The CapsuleRunner — not the planner — transitions them to `worktree_created` as its first action, so the stored state never claims a worktree exists before the runner creates it.
- Before calling `SaveCapsule`, set `capsule.TopologyDecisionID` to the `DecisionRecord.DecisionID` returned by the preceding `SaveDecision` call. The ContextCompiler (Step 6) reads this field to load the topology rationale via `store.LoadDecision(capsule.TopologyDecisionID)` for `HumanSummaryProjection.Topology`.
- The planner reads open obligations and creates capsules for them — it does not create new obligations. Do not call `store.SaveObligation` in the planner. Initial obligations are created by VerifierEngine (Step 3); follow-up obligations are created by the Reconciler (Step 1).

---

### Step 6 — ContextCompiler (`internal/projector`)

Builds the two MVP projection types for a capsule. These are always two separate artifacts; they must never be merged into one.

**executor_projection** — minimal, constraint-focused agent briefing: goal conditions, assigned obligations, allowed/forbidden paths, allowed tools, verified claims only when directly needed, failure fingerprints for affected files, relevant prior evidence artifacts, required outputs.

**human_summary_projection** — developer-facing pre-execution design document: goal in plain language, conditions addressed, implementation approach, expected file scope, explicit exclusions, topology rationale, design decisions, pre-execution risks, evidence plan, budget, required approvals. Compiled and surfaced before the capsule runner launches the agent.

**In:** GoalIR, GoalConditions, capsule Obligations, ExecutionCapsule (capsule contract: allowed paths, tools, budget), verified ClaimArtifacts for affected files, EvidenceArtifacts for affected obligations, FailureFingerprints for affected files, topology DecisionRecord (loaded via `store.LoadDecision(capsule.TopologyDecisionID)`), latest StateSnapshot.
**Out:** ContextProjection and HumanSummaryProjection persisted via the store.

Proposed and stale claims must not be injected as facts — include only with a `[proposed]` label when relevant to a design decision or risk. Raw transcript content must never appear in either projection.

---

### Step 7 — HumanGatekeeper (`internal/gate`)

Terminal UI component. Presents the HumanSummaryProjection to the developer before a capsule runs and records the response as a DecisionRecord. Also handles merge approval gates and obligation waiver requests.

**In:** HumanSummaryProjection from the store; optionally VerifierResult and DecisionRecords for context.
**Out:** GateDecision (Approved/Rejected/Proceeded), persisted as a DecisionRecord.

Gate activation rules from orca.md §5.4.2: `human_gated` and `implementer_reviewer` with medium/high risk obligations block indefinitely (reviewWindow = 0). `single` topology with low-risk obligations proceeds automatically after a configurable window (default 30s) if no response is given. MVP implementation is a simple CLI prompt — no UI framework required.

---

### Step 8 — BudgetController (`internal/budget`)

Derives capsule budget state from the event log. Does not read the artifact store — budget limits come from `capsule_created` event payloads; spend comes from `budget_record_saved/updated` payloads. Enforced by the orchestrator before handing a capsule to the CapsuleRunner.

**In:** event log for the goal.
**Out:** BudgetCheck (allowed/denied + reason + current spend) and ROI metrics (verified value per 1K tokens, obligations discharged, patches accepted/rejected, evidence reuse counts).

This is a read-only event log projection — no writes anywhere. Enforcement decisions (when budget is exceeded) are recorded as DecisionRecords by the orchestrator, not by BudgetController.

---

### Step 9 — CapsuleRunner (`internal/runner`)

Most complex component. Selects an Adapter by `capsule.Agent` from a registry wired by the orchestrator, drives the capsule state machine, injects the executor context projection, collects sidecar output or falls back to transcript extraction, normalizes output into schema artifacts, and persists everything.

**In:** ExecutionCapsule and ContextProjection from the store; Adapter registry.
**Out:** PatchArtifact, EvidenceArtifacts, ClaimArtifacts, FailureFingerprints in the store; capsule state transitions via UpdateCapsuleState; RunResult returned to the orchestrator.

State machine: `pending → worktree_created → workspace_attached → setup_run → agent_running → completed | failed`. Two transitions emit log events: (1) before transitioning to `worktree_created`, emit `EventCapsuleStarted` (`capsule_started`); (2) before transitioning to `completed` or `failed`, emit `EventCapsuleCompleted` (`capsule_completed`). Intermediate transitions (`worktree_created → workspace_attached → setup_run → agent_running`) write state directly via `UpdateCapsuleState` — no log events. A crash during any intermediate state replays only to `worktree_created`, the last durably logged state.

Fallback rule: on `ErrNoSidecar` or `ErrInvalidSidecar`, fall back to `ExtractFromTranscript`. Both paths must produce structurally identical `AgentSidecarOutput`. Downstream code must not distinguish which path was used. On any error, transition the capsule to `CapsuleStateFailed` and ensure a FailureFingerprint is persisted before returning.

Unit test with a stub adapter; real adapters fill it in next.

---

### Step 10 — Adapters (`internal/runner/codex`, `internal/runner/claude`)

Concrete Adapter implementations for Codex CLI and Claude Code. Each handles its CLI's authentication preflight, worktree creation and attachment, agent launch with injected projection, sidecar output collection, and transcript extraction fallback.

Both implement the `runner.Adapter` interface. Codex and Claude Code are required MVP adapters; Copilot CLI is optional if low effort (add a third sub-package if so).

Transcript extraction is not a degraded mode. The schema produced by both collection paths must be identical — the runner and all downstream components must not be able to distinguish sidecar from transcript-extracted output.

---

### Step 11 — VerifierEngine: Verify (`internal/verifier`)

Second job for the VerifierEngine. Runs verifier stages in sequence for a completed patch and produces a VerifierResult. Invokes user-configured commands via subprocess (not model calls for stages 1–4); model or human review for stage 6 is optional/configurable.

**Stages (in order):**
1. Preflight — repo status, auth, configured gates, clean base
2. Scope check — changed files and LOC within capsule contract
3. Static checks — lint, typecheck, formatting as configured
4. Targeted tests — tests for changed files or obligation-relevant areas
5. Regression checks — reproduction or regression evidence for bugfix/security obligations
6. Patch review — model or human review for risk, assumptions, obligation fit
7. Merge readiness — all blocking obligations satisfied or waived

Stages 1–4: first blocking failure stops the pipeline — remaining stages within 1–4 are skipped, but stages 5–7 still proceed. Stages 5–7 run for their applicable obligation types regardless of any stage 1–4 blocking failure. Verifier does not advance Obligation status — the Reconciler owns that.

Note: `verifier.go`'s doc comment currently reads "later stages are skipped" after a stage 1–4 blocker. That phrase refers only to the remaining stages within 1–4; it is inconsistently worded given the follow-up sentence "Stages 5–7 always run." Treat BUILD.md (stages 5–7 run regardless) as authoritative and correct the `verifier.go` comment before implementing Verify.

**In:** GoalIR, GoalConditions, ExecutionCapsule (for scope contract), PatchArtifact, Obligations, EvidenceArtifacts from the store; user-configured verifier command list from config.
**Out:** VerifierResult persisted via the store; `verifier_result_created` emitted by the store.

---

### Step 12 — CLI Orchestrator + Configuration (`cmd/orca`)

Wires all components into the control loop defined in orca.md §6. Owns the `.orca/` directory layout, `config.yaml`, and CLI commands. This is the only code that imports multiple runtime components and wires them together.

**Minimum viable commands:**
- `orca init` — create `.orca/` layout and `config.yaml`
- `orca run "<goal>"` — execute the full control loop for a goal
- `orca status` — show active goal, open obligations, active capsules, budget spent
- `orca merge` — approve or inspect a pending merge recommendation

**`.orca/` layout:**
```
.orca/
  config.yaml               # verifier commands, agent prefs, review window, budget defaults
  events.log                # JSONL authoritative history
  state/                    # JSON materialized state (goal, obligations, artifact_graph, budget, decisions)
  artifacts/                # patches/, evidence/, claims/, contexts/, failures/, logs/
  capsules/CAP-*/           # per-capsule worktrees
```

`config.yaml` carries: verifier gate commands, agent preference order, review window duration, default token budget, path policies, `--no-learning` flag.

`--no-learning` mode (orca.md §13): when the flag is set, the orchestrator suppresses all adaptive reuse across the control loop. Specifically: prior failure fingerprints are not surfaced to the planner or projector; claim reuse is skipped (LoadVerifiedClaimsForFiles returns nothing to the projector); topology outcome memory is not consulted; historical routing hints are not applied; reusable evidence suggestions are not generated. The full proof runtime still runs — Goal IR, obligations, capsules, projections, verifier gates, reconciliation — but each run is treated as if no prior history exists. This is a comparison mode for measuring proof-runtime value independent of the learning layer.

Events the orchestrator emits directly (not via the store's auto-emit on Save): after `ObligationPlanner.Plan` returns, the orchestrator must emit `topology_selected` (`EventTopologySelected`) to the event log using the `Topology` and `DecisionID` from `PlanResult`. The store does not auto-emit this event. No other package may emit `topology_selected`.

The orchestrator is the only component that may wire together more than one other internal package. All other packages communicate only through their declared interfaces.

---

### Step 13 — End-to-End Smoke Test

Run one real coding goal on a real local repo through the full system. This is the Phase 1 exit gate before Phase 2 begins.

**Must demonstrate all MVP success criteria (orca.md §16):**
- goal text → structured obligations
- agent executed inside a capsule with a real worktree
- patch artifact produced with evidence attached
- verifier maps evidence to obligations
- failed work produces a structured failure fingerprint and retry reason
- a follow-up capsule receives an executor_projection, not a transcript replay
- merge recommendation or retry decision with stated reason
- token/cost report per obligation
- executor_projection token count is lower than a naive transcript replay would be for the same follow-up capsule

If any criterion fails, resolve it before declaring Phase 1 complete.

---

## What Not to Build Now

These are explicitly Phase 2+. Do not add them to Phase 1 code:

- Parallel, test-first, investigate-then-implement topologies
- Work Orders
- Extended projection types (reviewer, tester, planner, verifier, reconciler)
- Contested/invalidated claim states
- Reconciler stale-claim invalidation and overlap detection
- Failure fingerprint prior-attempt history and recommended-action inference
- MAVEN, mutation testing, adversarial test generation
- Codebase Knowledge Graph, Strategy Memory, Capability Posterior Router
- MCP integration, desktop UI, remote execution

See orca.md §17 for the full deferred list and the conditions under which each should be added.
