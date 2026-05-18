# Module Boundary Map

**Phase 3 design artifact. Authoritative until superseded by a Phase 1 ADR.**

---

## Dependency Graph

```
                         ┌──────────────────────────────┐
                         │   cmd/orca  (runtime loop)    │
                         │  the ONLY place that wires    │
                         │  multiple components together │
                         └──┬────┬────┬────┬────┬────┬──┘
                            │    │    │    │    │    │
                    ┌───────┘  ┌─┘  ┌─┘  ┌─┘  ┌─┘  └──────┐
                    ▼          ▼    ▼    ▼    ▼             ▼
                 intent    planner proj runner  verifier  reconciler
                    │          │    │    │       │            │
                    └──────────┴────┴────┴───────┴────────────┘
                                         │
                              ┌──────────▼──────────┐
                              │    artifact_store    │
                              │  (emits events on    │
                              │     every Save)      │
                              └──────────┬───────────┘
                                         │
                                    ┌────▼─────┐
                                    │ eventlog │◄──── budget, gate,
                                    │          │      capsule_runner,
                                    └──────────┘      reconciler (direct)
```

### Direct dependency map

```
intent_compiler    → artifact_store, event_log
obligation_planner → artifact_store               [events via store]
context_compiler   → artifact_store               [events via store]
capsule_runner     → artifact_store, event_log
verifier_engine    → artifact_store               [events via store]
reconciler         → artifact_store, event_log
budget_controller  → event_log                    [read-only]
human_gatekeeper   → artifact_store, event_log

artifact_store     → event_log                    [emits on every Save]
```

---

## The "Store Emits Events" Rule

Three components — obligation_planner, verifier_engine, and context_compiler —
produce artifacts that belong in the event log (obligation_created, capsule_created,
verifier_result_created, context_projection_created) but are not listed as direct
event_log consumers in the dependency map.

**Resolution:** the concrete `ArtifactStore` implementation appends the corresponding
event to the event log on every `Save*` call. This means:

- `SaveObligation` → appends `obligation_created`
- `SaveCapsule` → appends `capsule_created`
- `SaveVerifierResult` → appends `verifier_result_created`
- `SaveProjection` / `SaveHumanSummaryProjection` → appends `context_projection_created`
- `SaveDecision` → appends `decision_record_created`
- etc.

**`topology_selected` exception:** `topology_selected` is a distinct event type
(schema.EventTopologySelected) that the orchestrator (`cmd/orca`) emits directly
to the event log after `ObligationPlanner.Plan` returns, since the orchestrator
has direct event log access and knows it just completed topology selection. The
planner's `SaveDecision` call emits `decision_record_created`; the orchestrator
then emits `topology_selected` pointing to that decision record's ID. This keeps
planner's event log dependency clean without losing the dedicated event type.

Components that need more granular event control (capsule lifecycle state
transitions, patch accepted/rejected, merge applied) write to the event log
directly alongside their store writes.

---

## Package Map

| Import path | Package name | What it defines |
|---|---|---|
| `internal/store` | `store` | `ArtifactStore` interface |
| `internal/eventlog` | `eventlog` | `EventLog` interface |
| `internal/intent` | `intent` | `IntentCompiler` interface |
| `internal/planner` | `planner` | `ObligationPlanner`, `TopologyClassifier`, `PlanResult` |
| `internal/projector` | `projector` | `ContextCompiler` interface |
| `internal/runner` | `runner` | `CapsuleRunner`, `Adapter`, `RunResult`, `ErrNoSidecar`, `ErrInvalidSidecar` |
| `internal/verifier` | `verifier` | `VerifierEngine` interface |
| `internal/reconciler` | `reconciler` | `Reconciler`, `ReconcileResult` |
| `internal/budget` | `budget` | `BudgetController`, `BudgetCheck`, `Spend`, `ROI` |
| `internal/gate` | `gate` | `HumanGatekeeper`, `GateDecision` |
| `internal/schema` | `schema` | all MVP data types (Phase 2, complete) |

`internal/projector` is named "projector" rather than "context" to avoid
shadowing the stdlib `context` package in import declarations.

---

## Per-Component Contracts

### intent_compiler (`internal/intent`)

| | |
|---|---|
| **Reads (store)** | nothing — takes raw user input only |
| **Writes (store)** | `GoalIR` (with embedded `GoalConditions`) via `SaveGoal` |
| **Writes (log)** | `goal_created` directly |
| **Must NOT import** | `internal/planner`, `internal/runner`, `internal/verifier`, `internal/reconciler`, `internal/projector`, `internal/budget`, `internal/gate` |
| **Must NOT create** | Obligations or Capsules (planner's job) |

The compiler is the entry point for one user intent string. It may call a model
to clarify ambiguous goal conditions, but it must reject a second `Compile` call
when a goal is already active (one active goal per repo, MVP constraint).

---

### obligation_planner (`internal/planner`)

| | |
|---|---|
| **Reads (store)** | `GoalIR`, `GoalConditions`, open `Obligations`, `FailureFingerprints` |
| **Writes (store)** | `Obligations`, `ExecutionCapsules`, `DecisionRecord` (topology decision) |
| **Writes (log)** | none directly — store emits `obligation_created`, `capsule_created`, `decision_record_created`; orchestrator emits `topology_selected` after `Plan` returns |
| **Must NOT import** | `internal/runner`, `internal/verifier`, `internal/reconciler`, `internal/projector`, `internal/budget`, `internal/gate` |
| **Must NOT call** | `store.SavePatch`, `store.SaveEvidence`, `store.SaveClaim`, `store.SaveVerifierResult`, `store.SaveBudgetRecord` |
| **Must NOT know about** | how capsules execute, verifier internals, context projection content |

`TopologyClassifier` is an internal dependency of `ObligationPlanner`. Nothing
outside `internal/planner` calls it. It is a pure function: receives obligations
and fingerprints, returns topology + rationale. The planner wraps the decision in
a `DecisionRecord` and persists it.

MVP topologies: `single`, `implementer_reviewer`, `human_gated` only. Do not
implement `parallel`, `test_first`, or `investigate_then_implement` until Phase 2.

Capsules are created with `State = CapsuleStatePending`. The CapsuleRunner owns
the first transition: `pending → worktree_created`. This ensures the stored state
never claims a worktree exists before the runner has allocated one.

---

### context_compiler (`internal/projector`)

| | |
|---|---|
| **Reads (store)** | `GoalIR`, `GoalConditions`, `Obligations` (for capsule), verified `ClaimArtifacts`, `EvidenceArtifacts`, `FailureFingerprints`, `ExecutionCapsule`, `StateSnapshot` |
| **Writes (store)** | `ContextProjection` (executor), `HumanSummaryProjection` |
| **Writes (log)** | none directly — store emits `context_projection_created` |
| **Must NOT import** | `internal/runner`, `internal/verifier`, `internal/reconciler`, `internal/budget`, `internal/gate` |
| **Must NOT inject** | proposed or stale claims as facts; include only with `[proposed]` label |
| **Must NOT merge** | `executor_projection` and `human_summary_projection` into one document |
| **Must NOT include** | raw transcript content, `executor_projection` content in the `human_summary`, or vice versa |

The `human_summary_projection` is emitted **before** the capsule runner launches,
not after. It is a pre-execution design document for the developer, not a post-hoc
diff summary. The `executor_projection` is the agent's minimal working briefing.
These two documents are always separate. Merging them wastes agent tokens or
strips the developer of go/no-go information. orca.md §5.4.

---

### capsule_runner (`internal/runner`)

| | |
|---|---|
| **Reads (store)** | `ExecutionCapsule`, `ContextProjection` |
| **Writes (store)** | `PatchArtifact`, `EvidenceArtifacts`, `ClaimArtifacts`, `FailureFingerprints`, capsule state transitions |
| **Writes (log)** | `capsule_started`, `capsule_completed`, `patch_artifact_created`, `evidence_artifact_created`, `claim_created`, `failure_fingerprint_created` |
| **Must NOT import** | `internal/planner`, `internal/verifier`, `internal/reconciler`, `internal/projector`, `internal/budget`, `internal/gate` |
| **Must NOT call** | `store.SaveGoal`, `store.SaveObligation`, `store.SaveCapsule`, `store.SaveVerifierResult`, `store.SaveBudgetRecord` |
| **Must NOT advance** | Obligation status — that belongs to the Reconciler |

The runner selects an `Adapter` by `capsule.Agent` from a registry wired by the
orchestrator (constructor injection). It does not know which specific adapter
implementations exist.

Adapter selection by `schema.AgentType`:

| AgentType | Adapter | Notes |
|---|---|---|
| `codex` | `internal/runner/adapters/codex` | Phase 1 |
| `claude` | `internal/runner/adapters/claude` | Phase 1 |
| `copilot` | `internal/runner/adapters/copilot` | Phase 1 if low effort |

Both `Execute` (sidecar) and `ExtractFromTranscript` paths must produce
structurally equivalent `AgentSidecarOutput`. Downstream consumers must not be
able to distinguish which path was used. `SidecarUsed` in `RunResult` is for
observability only; it must not drive different downstream logic. orca.md §8.

---

### verifier_engine (`internal/verifier`)

| | |
|---|---|
| **Reads (store)** | `GoalIR`, `GoalConditions` (`ProposeObligations`); `PatchArtifact`, `ExecutionCapsule` (scope), `Obligations`, `EvidenceArtifacts` (`Verify`) |
| **Writes (store)** | `Obligations` via `SaveObligation` (`ProposeObligations`); `VerifierResult` (`Verify`) |
| **Writes (log)** | none directly — store emits `obligation_created` on `SaveObligation`, `verifier_result_created` on `SaveVerifierResult` |
| **Must NOT import** | `internal/planner`, `internal/runner`, `internal/reconciler`, `internal/projector`, `internal/budget`, `internal/gate` |
| **Must NOT call** | `store.SaveCapsule`, `store.SaveBudgetRecord`, `store.UpdateObligationStatus` |
| **Must NOT update** | Obligation status — advancing obligation state belongs exclusively to the Reconciler |
| **Must NOT run** | agent commands or model calls directly; verifier gates invoke user-configured subprocesses |

The verifier has two distinct roles. `ProposeObligations` acts as an obligation
generator: it reads the GoalIR and derives what must be proven before any capsule
runs (orca.md §6 step 3). `Verify` acts as an evidence arbiter: it checks whether
existing evidence satisfies existing obligations after a capsule completes. It does
not create new evidence by running agents.

---

### reconciler (`internal/reconciler`)

| | |
|---|---|
| **Reads (store)** | `VerifierResult`, `PatchArtifact`, `Obligations`, `EvidenceArtifacts`, `FailureFingerprints` via `LoadFailuresForCapsule`, `BudgetRecords` |
| **Writes (store)** | Obligation status, Patch status, new follow-up `Obligations`, `DecisionRecords`, `BudgetRecords`, `StateSnapshot` |
| **Writes (log)** | `patch_accepted`, `patch_rejected`, `obligation_created` (follow-ups), `decision_record_created`, `merge_applied` |
| **Must NOT import** | `internal/runner`, `internal/verifier`, `internal/projector`, `internal/budget`, `internal/gate` |
| **Must NOT create** | new evidence artifacts or run subprocess checks (verifier's job) |
| **Must NOT accept** | a patch without mapping evidence to every blocking obligation |

The reconciler is the component that closes the loop: it reads the verifier's
verdict and translates it into durable state changes. Follow-up obligations
created here are the input to the next `ObligationPlanner.Plan` call.

---

### budget_controller (`internal/budget`)

| | |
|---|---|
| **Reads (log)** | `capsule_created` (for budget limits in payload), `capsule_started`, `capsule_completed` (for spend), `patch_accepted`, evidence events (for reuse) |
| **Writes (log)** | none |
| **Reads (store)** | **none** — budget state is derived entirely from events |
| **Must NOT import** | `internal/planner`, `internal/runner`, `internal/verifier`, `internal/reconciler`, `internal/projector`, `internal/gate` |

Budget limits are read from the `capsule_created` event payload (which includes
`ExecutionCapsule.Budget`). Accumulated spend is read from `capsule_completed`
event payloads. `BudgetRecord` artifacts in the store are written by the
Reconciler, not by BudgetController. BudgetController computes live metrics from
the event stream and enforces limits before execution.

Primary metric: **verified value per 1K tokens**. orca.md §12.

---

### human_gatekeeper (`internal/gate`)

| | |
|---|---|
| **Reads (store)** | `HumanSummaryProjection`, `Obligations`, `VerifierResult`, `FailureFingerprints`, `DecisionRecords` |
| **Writes (store)** | `DecisionRecord` (human approval/rejection) |
| **Writes (log)** | `decision_record_created` |
| **Must NOT import** | `internal/planner`, `internal/runner`, `internal/verifier`, `internal/reconciler`, `internal/projector`, `internal/budget` |
| **Called by** | orchestrator only at defined gate points |

Gate activation rules (orca.md §5.4.2, §15):

| Topology | Risk | Gate | `reviewWindow` |
|---|---|---|---|
| `human_gated` | any | `ReviewProjection` blocks before capsule runs | 0 (block indefinitely) |
| `implementer_reviewer` | medium or high | `ReviewProjection` blocks before implementer capsule | 0 (block indefinitely) |
| `single` | low | display projection; auto-proceed after window unless blocked | 30s (configurable) |
| any | high-risk patch merge | `ReviewMerge` blocks before merge | N/A |
| any | blocking obligation cannot be satisfied | `ReviewWaiver` required | N/A |

`ReviewProjection` returns `GateDecision{Approved: true, Proceeded: true}` when the
review window expires without a developer response. `Proceeded` is for observability
only — the orchestrator must not treat it differently from an explicit approval for
the purposes of execution. orca.md §5.4.2.

---

## What the Orchestrator (`cmd/orca`) Does

The orchestrator (`cmd/orca`) is the **only** component that knows about multiple
runtime components. It wires them together via constructor injection and runs the
control loop. It does not contain business logic — it sequences calls.

Control loop (orca.md §6):

```
1.  intent_compiler.Compile(rawIntent)
2.  verifier_engine.ProposeObligations(goalID)               [orca.md §6 step 3]
3.  obligation_planner.Plan(goalID)          → PlanResult{CapsuleIDs, Topology, DecisionID}
    orchestrator emits topology_selected(PlanResult.Topology, PlanResult.DecisionID)
4.  projector.CompileHumanSummary(capsuleID) → HumanSummaryProjection
5.  gate.ReviewProjection(capsuleID, reviewWindow) → GateDecision  [if gate required]
6.  budget_controller.CheckCapsuleBudget(capsuleID)          [capsule ID now exists]
7.  projector.CompileExecutor(capsuleID)     → ContextProjection
8.  capsule_runner.Run(capsuleID)            → RunResult
9.  verifier_engine.Verify(patchID)          → VerifierResult
10. reconciler.Reconcile(patchID)            → ReconcileResult
11. budget_controller.ComputeROI(goalID)
12. if ReconcileResult.MergeReady && ReconcileResult.HumanGateRequired:
      gate.ReviewMerge(patchID)              → GateDecision
    elif !ReconcileResult.MergeReady && ReconcileResult.FollowUpObligationIDs non-empty:
      goto step 3
```

**No component other than `cmd/orca` may import two or more of the listed
internal packages.** If you find yourself importing `internal/verifier` inside
`internal/reconciler`, something has crossed a boundary.

---

## Isolation Rules Summary

1. **Planner never knows how capsules execute.** It creates the contract; the runner
   executes it.

2. **Runner never knows obligations exist.** It executes a capsule and produces
   artifacts. The reconciler maps them to obligations.

3. **Verifier never updates obligation status.** It renders verdicts. The
   reconciler applies them.

4. **Reconciler never runs verifier stages.** It reads an existing VerifierResult.

5. **Budget controller never reads the artifact store.** It derives all budget
   state from the event log.

6. **ContextCompiler never knows what runs next.** It compiles from the artifact
   graph state; it does not direct execution.

7. **Human gatekeeper is the only blocking surface.** No other component pauses
   the control loop for human input.

8. **The store emits events on save.** Components without a direct event_log
   dependency still produce a complete event history via this mechanism.

9. **Both adapter paths produce the same schema.** `SidecarUsed` in `RunResult`
   is for observability only; no downstream component may branch on it.

10. **Only `cmd/orca` wires multiple components together.** All other packages
    have at most two runtime dependencies: `store` and/or `eventlog`.
