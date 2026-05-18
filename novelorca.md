Assumption: the emdash baseline here is the architecture described in your prompt. `emdash.md` is empty; `orca.md` is the current project scope. So the comparison is: prompt-described emdash constraints vs the Orca scope in [orca.md](/E:/orca/orca.md).

## 1. Distilled Problem Framing

### Emdash as architectural constraints

Emdash is constrained by these primitives:

- **Terminal session as execution substrate**: agents are opaque CLI processes.
- **Transcript/output parsing as integration layer**: system state is inferred from terminal output, hooks, and side effects.
- **Provider adapter as boundary**: each agent runtime is special-cased.
- **Git worktree/branch as isolation unit**: parallelism maps to filesystem branches.
- **Conversation/task UI as orchestration surface**: the human-visible session is close to the system's operational record.
- **Prompt assembly as control plane**: behavior is steered by what text gets injected.
- **Diff as primary artifact**: work is judged mostly after code appears.
- **Local desktop as supervisor shell**: orchestration is framed as managing visible coding-agent sessions.

This is not just an implementation choice. It defines the ontology. The system sees agents as terminals that eventually emit diffs.

### Current Orca scope

Orca already rejects "just a wrapper" and introduces serious mechanisms:

- Semantic Goal State.
- Residual Planner.
- Capability Posterior Router with Thompson Sampling.
- EpistemicResidual handoff.
- Knowledge Graph and Conflict Graph.
- Strategy Memory.
- MAVEN adversarial verification.
- Mutation coverage loop closure.
- Causal DAG failure archaeology.
- Symbol-level Semantic Snapshot Isolation.
- MCP exposure.
- Event log plus materialized JSON state.
- PTY/worktree/provider adapters retained underneath.

The core thesis is: keep the terminal/worktree substrate, but build a learning epistemic supervisor above it.

### Key mismatch

Your scope is bold at the supervisor layer but conservative at the substrate layer.

It adds epistemic machinery around the same old execution primitive: opaque CLI agents in worktrees, steered through prompts, interpreted through stdout and diffs. That may improve orchestration, but it does not fully escape emdash's hard ceiling: the system still learns indirectly from artifacts produced by external conversational processes.

The architectural leap is to make the **artifact/state/control contract** primary and make terminal agents merely one kind of executor.

### 5 highest-leverage emdash bottlenecks

1. **Opaque execution**: the orchestrator cannot know intent, intermediate reasoning, tool semantics, or state transitions except by inference.
2. **Transcript gravity**: history becomes state, state becomes context, context becomes cost.
3. **Weak task semantics**: a "task" is too close to a natural-language request, not a typed transformation with preconditions, effects, evidence, and rollback semantics.
4. **Branch-bound isolation**: worktree isolation protects files, but not semantic dependencies, proof obligations, or shared beliefs.
5. **Post-hoc verification**: verification arrives after patch generation, instead of shaping decomposition, execution, and context.

### 5 missing ideas in Orca scope

1. **Orchestration IR**: Orca has structured records, but not a compiler-like intermediate representation for goals, plans, effects, evidence, and context projections.
2. **Execution contracts**: agents should not just receive prompts; they should accept typed capsules with declared preconditions, allowed effects, expected evidence, and required proof objects.
3. **Artifact graph as primary state**: the durable object should not be transcript, goal JSON, or branch. It should be a graph of claims, patches, tests, proofs, observations, failures, and state transitions.
4. **Verifier-first planning**: the plan should be generated backward from what can be checked, not forward from what seems useful.
5. **Context compiler**: token efficiency needs a real compilation pipeline from state graph to model-specific context projection, not entropy-gated text injection.

---

## 2. First-Principles Critique Of Current Assumptions

### Terminal-first orchestration

Why it exists: current agents are CLIs; PTYs preserve their native behavior.

Where it helps: fast compatibility, low integration burden, lets existing tools run unchanged.

Hard ceiling: terminal sessions are lossy, interactive, provider-shaped streams. They are bad APIs. They collapse commands, reasoning, tool use, edits, errors, and progress into text.

Verdict: **weaken**. Keep PTY as compatibility mode, not the core runtime.

### Text transcript as coordination medium

Why it exists: LLMs consume text; users trust readable transcripts.

Where it helps: debuggability and quick handoff.

Hard ceiling: transcripts are verbose, order-sensitive, hard to validate, and expensive to replay. They mix facts, speculation, decisions, and stale assumptions.

Verdict: **remove as system of record**. Preserve transcript as view, not state.

### Provider-specific imperative adapters

Why it exists: each CLI behaves differently.

Where it helps: unavoidable for legacy agents.

Hard ceiling: provider behavior leaks upward; orchestration becomes a compatibility matrix.

Verdict: **invert**. Define a provider-neutral execution contract; adapters translate into it when possible.

### Worktree-per-agent isolation

Why it exists: Git gives cheap filesystem isolation and merge semantics.

Where it helps: pragmatic parallel coding.

Hard ceiling: Git branches do not represent semantic conflicts, test obligations, belief conflicts, or partial artifacts well.

Verdict: **preserve but subordinate**. Worktrees are storage sandboxes, not units of work.

### Prompt replay as state transfer

Why it exists: models need context and providers expose conversation interfaces.

Where it helps: simple, universal.

Hard ceiling: replay cost grows linearly or worse; causality becomes vague; stale context contaminates execution.

Verdict: **remove** as primary transfer. Replace with compiled context projections from structured state.

### UI shell as primary control layer

Why it exists: users need to see and steer many sessions.

Where it helps: operational usability.

Hard ceiling: it biases architecture toward session management instead of state convergence.

Verdict: **weaken**. UI should visualize state machine, artifact graph, budgets, and gates, not terminals first.

### Human-readable logs as observability artifact

Why it exists: logs are easy.

Where it helps: debugging.

Hard ceiling: logs are not queryable causal state. You cannot reliably compute "why did this invariant enter the retry?" from prose.

Verdict: **replace** with event-sourced typed telemetry; render logs from events.

### Planning, execution, verification in same context stream

Why it exists: agent CLIs present a unified chat-like interaction.

Where it helps: simplicity.

Hard ceiling: planning contaminates execution; verification gets anchored by implementation narrative; all roles pay for irrelevant context.

Verdict: **split hard**. Planning context, execution context, verification context, and reconciliation context should be separately compiled.

### Agents coordinate via natural language

Why it exists: LLMs are language-native.

Where it helps: flexible transfer of ambiguous discoveries.

Hard ceiling: natural language is weak for invariants, dependencies, effects, and proof obligations.

Verdict: **weaken**. Use text at the boundary; use typed claims/evidence internally.

---

## 3. Rewrite Theses

### 1. Replace transcript-driven orchestration with artifact-state orchestration

Old primitive: transcript/session.

New primitive: typed artifact graph containing goals, tasks, patches, claims, tests, failures, proofs, observations, and state transitions.

Quality: agents operate on verified, scoped facts instead of accumulated chat.

Efficiency: context is projected from the graph, not replayed.

Token cost: repeated reasoning becomes memoized artifacts.

Reliability: every claim has provenance, confidence, invalidation rules, and consumers.

Techniques: event sourcing, artifact graph, provenance DAG, structural diffs, typed evidence.

Risks: graph schema design can become bloated.

Practicality: **near-term practical**.

Rank: **1**.

### 2. Replace terminal sessions with contract-bound execution capsules

Old primitive: PTY process.

New primitive: execution capsule: task IR + allowed effects + sandbox + required outputs + verification obligations.

Quality: agents are constrained to produce checkable work.

Efficiency: no need to parse unbounded stdout as the main result.

Token cost: capsule includes only the projected context required for its contract.

Reliability: failures classify cleanly as contract violations, tool failures, environment failures, or verification failures.

Techniques: JSON schema contracts, sandbox policies, tool manifests, sidecar outputs, effect logs.

Risks: legacy CLIs may not comply; adapters become complex.

Practicality: **near-term for wrappers, medium-term for native agents**.

Rank: **2**.

### 3. Replace prompt-centric control with a plan/effect IR compiler

Old primitive: prompt assembly.

New primitive: orchestration IR compiled into model-specific prompts, tool permissions, expected effects, and verifier gates.

Quality: plans become analyzable before execution.

Efficiency: reusable plan fragments and context projections.

Token cost: eliminate role-irrelevant context.

Reliability: static checks catch invalid plans before agents run.

Techniques: compiler passes, type checking, dependency analysis, context projection, verifier synthesis.

Risks: too much formalism can slow iteration.

Practicality: **medium-term**.

Rank: **3**.

### 4. Replace branch isolation with semantic snapshot isolation

Old primitive: git branch/worktree.

New primitive: semantic work state: filesystem snapshot plus symbol footprints, API contracts, claim dependencies, test obligations, and merge preconditions.

Quality: catches cross-file semantic interference.

Efficiency: parallelism can be more aggressive where semantics permit.

Token cost: agents receive changed semantic neighborhoods, not whole files or transcripts.

Reliability: conflict detection shifts from file overlap to symbol/effect overlap.

Techniques: LSP, tree-sitter, code property graph, semantic deltas, SSI.

Risks: language support is uneven.

Practicality: **near-term partial, medium-term robust**.

Rank: **4**.

### 5. Replace planner-led execution with verifier-steered convergence

Old primitive: planner decomposes, workers implement, verifiers check.

New primitive: verifier-generated obligations drive decomposition backward from observable success conditions.

Quality: reduces fake progress.

Efficiency: agents work only on gaps that can be checked.

Token cost: fewer speculative implementation loops.

Reliability: every task is born with a falsification path.

Techniques: proof obligations, mutation testing, property tests, differential tests, test synthesis, failure classifiers.

Risks: hard for UI/UX, exploratory refactors, design work.

Practicality: **medium-term**.

Rank: **5**.

---

## 4. Novel Architecture Candidates

## Candidate 1: Compiler-Orchestrator

### A. Overview

The system is a compiler for software-change intent.

Core abstraction: **Goal IR -> Plan IR -> Execution IR -> Patch IR -> Evidence IR**.

Difference from emdash: agents are codegen backends, not terminal sessions.

### B. Execution Model

Unit: typed transformation pass.

Tasks decompose through compiler passes:

- intent normalization
- dependency analysis
- verifier synthesis
- patch generation
- evidence production
- reconciliation

Scheduling uses dependency graph plus semantic footprint analysis.

Agents communicate by emitting typed artifacts, not chat.

Explicit state: IR nodes, preconditions, effects, proofs, semantic deltas.

Implicit state: only model-internal reasoning, discarded unless converted to artifact.

### C. Context Model

Context is compiled.

Persistent:

- goal IR
- code map
- symbol graph
- artifact graph
- proof/evidence objects

Summarized:

- natural-language rationale
- historical attempts
- low-value transcript

Cached:

- symbol summaries
- call graph neighborhoods
- prior failure explanations
- test-gap objects

Structured instead of text:

- semantic deltas
- read/write sets
- obligation lists
- API contracts
- verifier outputs

Intermediate representation: yes, central.

### D. Memory Model

Short-term: current pass state.

Long-term: artifact graph and learned pass outcomes.

Shared memory: verified claims and reusable evidence.

Per-agent memory: capability statistics and adapter quirks.

Avoids bloat by storing artifacts, not chat.

Compaction: stale claims invalidated by affected symbols/tests.

### E. Coordination Model

Compiler pipeline plus DAG scheduler.

Planner emits IR, not prose tasks.

Executor agents are backends.

Reviewer/verifier agents are analysis passes.

### F. Cost-Efficiency Model

Cheap models run classification, retrieval, formatting.

Expensive models handle ambiguous decomposition and difficult synthesis.

Context compiler emits minimal projections per pass.

Reasoning products are memoized as IR nodes.

### G. Verification Model

Every execution IR node has proof obligations.

Checks include tests, static analysis, mutation operators, contract checks, and model critique where tools cannot decide.

False progress is detected when artifacts do not discharge obligations.

### H. Failure Model

Failed pass returns structured diagnostics.

Resume from last valid IR node.

Rollback by dropping/replacing patch nodes.

Disagreements become competing claims with evidence weights.

### I. Human Interaction Model

User sees a build graph of intent convergence:

- open obligations
- patches
- evidence
- blocked nodes
- cost
- risk

Transcript becomes drill-down, not primary UI.

### J. Why This Could Win

It improves quality by forcing checkable transformations.

It improves token use by compiling context.

It creates a novel category: agent orchestration as a compiler/runtime, not a terminal multiplexer.

Best metrics:

- fewer retries
- lower prompt tokens per accepted patch
- higher verifier pass rate
- fewer semantic merge conflicts

---

## Candidate 2: Distributed Systems Runtime

### A. Overview

The system is a local distributed runtime for agent work.

Core abstraction: **durable actors operating on an artifact log**.

Difference from emdash: agents are workers in a fault-tolerant state machine.

### B. Execution Model

Unit: actor command.

Actors:

- PlannerActor
- ContextActor
- ExecutorActor
- VerifierActor
- ReconcilerActor
- BudgetActor
- HumanApprovalActor

Tasks decompose into commands and events.

Scheduling uses queues, leases, backpressure, priorities, and retries.

Agents communicate through event log and artifact bus.

Explicit state: event-sourced.

Implicit state: actor-local cache, reconstructable.

### C. Context Model

Context is a materialized view.

Persistent:

- command log
- artifact store
- snapshots
- leases
- task states

Summarized:

- human explanations

Cached:

- retrieval indexes
- code maps
- verifier outputs

Structured:

- event types
- artifact schemas
- state snapshots
- semantic deltas

IR: yes, but less compiler-like; more command/event protocol.

### D. Memory Model

Short-term: actor mailbox and current lease.

Long-term: event log plus artifact store.

Shared: blackboard/artifact bus.

Per-agent: capability and failure records.

Transcript bloat avoided because messages are typed events.

Compaction through snapshots and log segmenting.

### E. Coordination Model

Actor model plus event sourcing.

Could use blackboard architecture for shared unresolved facts.

DAG scheduler handles dependencies.

### F. Cost-Efficiency Model

BudgetActor enforces token and dollar quotas per goal.

Speculative work requires expected value.

Repeated model calls deduped by artifact hash.

Cheap workers maintain indexes and projections.

### G. Verification Model

VerifierActor subscribes to patch artifacts.

Reconciler cannot merge without verifier-signed evidence.

False progress detected by missing required events.

### H. Failure Model

Crashes recover from log.

Partial work preserved as artifacts.

Remote failure becomes lease timeout.

Model disagreement creates conflict events.

Tool failure is typed and routeable.

### I. Human Interaction Model

User sees runtime state:

- command graph
- actors
- leases
- blocked approvals
- failure classes
- merge candidates

### J. Why This Could Win

It wins on reliability, observability, and resumability.

It is less novel intellectually than Compiler-Orchestrator, but more buildable.

Best metrics:

- crash recovery success
- fewer lost partial runs
- lower operator confusion
- higher parallel throughput

---

## Candidate 3: Verifier-First Control System

### A. Overview

The system is a closed-loop controller that minimizes distance between current code state and verified goal state.

Core abstraction: **error signal**.

Difference from emdash: planning is not "make tasks"; planning is "reduce measured residual error."

### B. Execution Model

Unit: control action.

Tasks decompose from unsatisfied measurable conditions.

Scheduler chooses actions with highest expected error reduction per cost.

Agents communicate through state estimates and residual errors.

Explicit state:

- goal vector
- current state estimate
- uncertainty
- control actions
- error history

Implicit state: model reasoning, discarded unless it updates estimate.

### C. Context Model

Context built from current residual:

- what condition is unsatisfied
- what evidence exists
- what uncertainty remains
- what action is expected to reduce it

Persistent:

- state estimates
- error traces
- action outcomes

Structured:

- condition vectors
- confidence intervals
- verifier measurements
- causal links

IR: control-action schema.

### D. Memory Model

Short-term: active residual and action plan.

Long-term: action/outcome model.

Shared: system state estimate.

Per-agent: action effectiveness posterior.

Avoids bloat by never replaying history unless causal debugging requires it.

Compaction through state estimation.

### E. Coordination Model

Model predictive control / operations research.

Planner chooses next best action under budget.

Verifier updates observed state.

Router learns action effectiveness.

### F. Cost-Efficiency Model

Expected value per token is first-class.

Expensive calls only when uncertainty blocks action selection or verification.

Cheap verifiers update state continuously.

### G. Verification Model

Verification is measurement.

Tests, static analysis, mutation, and model review all emit observations against goal dimensions.

False progress is detected when apparent changes do not reduce measured residual.

### H. Failure Model

Failures are disturbances.

Recovery recomputes state estimate.

Disagreement increases uncertainty and triggers information-gathering actions.

### I. Human Interaction Model

User sees convergence:

- residual map
- uncertainty
- proposed high-value next actions
- cost-to-close estimates

Chat is not primary.

### J. Why This Could Win

It could outperform on cost and focus. It makes the system stop doing plausible but low-value work.

Best metrics:

- cost per discharged condition
- fewer unnecessary tasks
- lower retry loops
- more accurate "done" detection

Risk: very hard to define good measurable state for broad coding work.

---

## Candidate 4: Artifact Blackboard / Scientific Workflow Engine

### A. Overview

The system is a scientific workflow engine for code changes.

Core abstraction: **claims and evidence on a shared blackboard**.

Difference from emdash: agents do not coordinate by conversation; they publish hypotheses, patches, tests, and refutations.

### B. Execution Model

Unit: experiment.

Tasks decompose into hypotheses:

- "This invariant holds."
- "This patch satisfies condition X."
- "This test distinguishes intended behavior."
- "This failure is caused by Y."

Scheduler selects experiments that resolve high-impact uncertainty.

Agents communicate by adding/refuting blackboard entries.

Explicit state: hypotheses, evidence, confidence, provenance.

### C. Context Model

Context is built around a hypothesis.

Persistent:

- claim graph
- evidence graph
- experiment results
- patch artifacts

Summarized:

- experiment rationale

Cached:

- reusable tests
- prior refutations
- code region summaries

Structured:

- claim types
- evidence weights
- invalidation scopes
- dependency links

IR: claim/evidence schema.

### D. Memory Model

Short-term: active hypothesis set.

Long-term: verified and refuted claims.

Shared: blackboard.

Per-agent: credibility per claim type.

Bloat avoided by retiring claims after invalidation or supersession.

### E. Coordination Model

Blackboard architecture plus active learning.

Agents bid to resolve claims they are likely to handle.

Verifier agents attack high-confidence claims.

### F. Cost-Efficiency Model

Spend tokens on uncertainty reduction.

Claim reuse prevents repeated rediscovery.

Cheap models classify evidence; expensive models handle ambiguous claims.

### G. Verification Model

Claims require evidence thresholds before they influence planning.

False progress detected by patches without supporting evidence claims.

### H. Failure Model

Contradictions create contested zones.

Rollback means demoting claims and dependent artifacts.

Tool failure marks experiment invalid, not claim false.

### I. Human Interaction Model

User sees:

- accepted claims
- contested claims
- open uncertainties
- evidence behind merge readiness

### J. Why This Could Win

It creates unusually strong epistemic hygiene.

Best metrics:

- fewer hallucinated invariants
- better cross-agent knowledge transfer
- lower repeated discovery cost

Risk: too much overhead for small tasks.

---

## Candidate 5: Patch Market With Proof-Carrying Bids

### A. Overview

The system is a market where agents bid patches against obligations.

Core abstraction: **bid = patch proposal + cost + expected value + proof object**.

Difference from emdash: agents compete/cooperate through explicit offers, not assigned sessions.

### B. Execution Model

Unit: bid.

Planner posts work orders.

Agents bid:

- expected files/symbols
- confidence
- cost estimate
- patch strategy
- proof plan

Scheduler allocates based on expected verified value.

Communication occurs through bid artifacts and counterexamples.

### C. Context Model

Context generated per work order.

Persistent:

- work orders
- bids
- accepted patches
- rejected bids
- proof objects

Structured:

- cost estimates
- obligations
- expected effects
- confidence

### D. Memory Model

Long-term: bid accuracy history.

Per-agent: calibration curves.

Shared: market ledger.

Avoids bloat by preserving bid/evidence summaries, not full attempts.

### E. Coordination Model

Auction/mechanism design.

Can use VCG-like scoring in spirit, though not monetary.

### F. Cost-Efficiency Model

Agents must estimate cost and confidence.

Badly calibrated agents lose future allocations.

Speculative execution only allowed when upside exceeds budget.

### G. Verification Model

Proof-carrying bids are checked before merge.

False progress penalizes future bid credibility.

### H. Failure Model

Failed bid becomes training data.

Partial patch can be rebid by another agent.

Disagreement resolved by verifier or higher-value experiment.

### I. Human Interaction Model

User sees competing options and why one was chosen.

### J. Why This Could Win

It may improve routing and calibration.

But it risks overfitting to agent self-reports unless bids are mostly derived from observed history.

Best as a subsystem, not the whole architecture.

---

## 5. Cross-Domain Methods Worth Importing

### Compiler IR and optimization passes

Original: compilers lower high-level code into analyzable IR, optimize, then emit target code.

Analog: lower user intent into goal/plan/effect/verifier IR.

Implementation: define typed nodes for conditions, effects, context requirements, proof obligations, patch artifacts.

Why uncommon: agent systems grew out of chat products, not program transformation systems.

Fit: **excellent**. This is the strongest import.

### Database query planning

Original: choose execution plans based on cost estimates and statistics.

Analog: choose task decomposition and model routing based on expected cost, uncertainty, and success probability.

Implementation: maintain statistics per task type, file area, agent, verifier, and context size.

Why uncommon: most orchestrators do not have enough structured telemetry.

Fit: **excellent**.

### Event sourcing

Original: store all state changes as append-only events; materialize views.

Analog: orchestration state is an event log; UI, DAGs, metrics, and recovery are derived views.

Implementation: typed event log with snapshots and schema evolution.

Why uncommon: terminal tools treat logs as text output, not state.

Fit: **excellent**.

### Semantic snapshot isolation

Original: databases detect conflicting concurrent transactions.

Analog: detect concurrent patch conflicts by read/write symbols, API contracts, claims, and tests.

Implementation: read/write footprints, semantic deltas, verifier obligations, revalidation on merge.

Why uncommon: language support is messy.

Fit: **high**.

### Blackboard systems

Original: AI systems where specialists contribute to shared problem state.

Analog: agents publish claims, tests, patches, counterexamples.

Implementation: artifact blackboard with claim lifecycle.

Why uncommon: chat UX hides the shared state behind conversation.

Fit: **high**, especially for multi-agent coordination.

### Model predictive control

Original: repeatedly choose actions that minimize future error under constraints.

Analog: choose next agent action to minimize residual goal uncertainty under budget.

Implementation: residual score, action outcome model, cost-to-close estimates.

Why uncommon: hard to define state/error for software tasks.

Fit: **medium-high**, best for planning policy.

### HTN / partial-order planning

Original: decompose abstract goals into partially ordered tasks.

Analog: decompose software goals into tasks with dependency constraints.

Implementation: typed plan operators and dependency graph.

Why uncommon: LLM planners are easier to call than formal planners.

Fit: **medium**. Useful, but do not overformalize.

### Scientific workflow engines

Original: reproducible pipelines with artifacts, provenance, and cached results.

Analog: agent runs are experiments producing evidence and patches.

Implementation: content-addressed artifacts, provenance graph, reproducible verifier runs.

Why uncommon: coding-agent tools prioritize interactive speed.

Fit: **excellent**.

### Auction mechanisms

Original: allocate scarce resources to bidders based on value/cost.

Analog: allocate tasks to agents based on expected verified value per cost.

Implementation: observed calibration, confidence scoring, bid comparison.

Why uncommon: agents are bad at honest self-estimation.

Fit: **medium**. Use observed bids, not self-reported ones.

### Formal methods

Original: prove properties or check models.

Analog: generate lightweight proof obligations, contracts, invariants, and property tests.

Implementation: property tests, type checks, static contracts, bounded model checks where available.

Why uncommon: hard to generalize across repos.

Fit: **selective but valuable**.

### Collaborative editing CRDT/OT

Original: reconcile concurrent document edits.

Analog: merge concurrent code transformations.

Implementation: AST-level operation logs and patch lattices.

Why uncommon: code semantics are harder than text.

Fit: **low-medium**. Useful for narrow structured files; not general architecture.

---

## 6. Token-Efficiency Redesign

The token strategy should be structural: stop treating model context as a transcript window.

### Mechanisms

**Semantic deltas**

Store changes as symbol-level and behavior-level deltas:

- symbols added/changed/removed
- call edges changed
- API contracts changed
- tests added
- obligations discharged

Agents receive deltas relevant to their task, not raw prior transcripts.

**Plan IR**

Represent decomposition as typed nodes:

```text
GoalCondition
  -> Obligation
  -> WorkOrder
  -> ExecutionCapsule
  -> PatchArtifact
  -> EvidenceArtifact
```

Planning context and execution context separate cleanly.

**Artifact-first memory**

Persist:

- claims
- evidence
- patches
- verifier outputs
- failure diagnoses
- context projections
- model decisions

Do not persist full conversations as reusable memory except as debug material.

**Event-sourced state**

Each context projection is reproducible from event log plus artifact graph.

This makes "why was this included?" answerable.

**Code-property-graph retrieval**

Build a code graph:

- symbols
- files
- references
- imports
- tests
- call edges
- ownership of obligations

Context retrieval becomes graph neighborhood selection.

**Contract-based tool execution**

Agents receive allowed tools and expected outputs.

Tool outputs are stored as artifacts.

No repeated "go inspect the same files" loops when a prior artifact is still valid.

**Speculative execution**

Run cheap probes before expensive implementation:

- static footprint estimation
- test discovery
- dependency risk
- likely conflict
- missing fixture risk

Only escalate when expected value is positive.

**Reusable proof objects**

A proof object can be:

- passing test run
- mutation killed
- static check result
- generated adversarial test
- invariant corroboration
- trace explaining failure

Future agents consume proof objects instead of re-deriving facts.

**Context projections**

For every model call, compile one of:

- planner projection
- executor projection
- verifier projection
- reconciler projection
- human summary projection

Each has token budget and allowed artifact types.

**Tiered model routing**

Cheap model:

- classify task
- compress artifacts
- generate schema-constrained summaries
- route retrieval

Mid model:

- local code reasoning
- test synthesis
- failure triage

Expensive model:

- ambiguous architecture planning
- cross-module reasoning
- high-risk review

**Memoized reasoning products**

Hash inputs to planning/verifier calls.

If same goal condition + code state + evidence set appears, reuse the prior reasoning artifact.

### Token ROI measurement

Track per stage:

- tokens spent
- artifacts produced
- obligations discharged
- failures prevented
- retries avoided
- merge accepted/rejected
- downstream reuse count

Metric: **verified value per 1K tokens**, not just total token spend.

---

## 7. New Primitives For A Frontier System

### 1. Execution Capsule

Definition: a sealed unit containing task IR, context projection, sandbox policy, allowed effects, required outputs, verifier gates, budget, and timeout.

Why existing tools lack it: CLIs accept prompts, not contracts.

Solves: opaque execution and sloppy task boundaries.

Fits: all recommended architectures.

Difficulty: medium.

Payoff: very high.

### 2. Evidence Artifact

Definition: durable object proving or weakening a claim: test result, static finding, mutation survivor, trace, review finding, typecheck output.

Why absent: current systems treat verification as log text.

Solves: repeated reasoning and unverifiable progress.

Difficulty: medium.

Payoff: very high.

### 3. Context Projection

Definition: deterministic compilation of artifact graph state into model-specific prompt/tool context for a role.

Why absent: most systems concatenate history.

Solves: transcript bloat and stale context.

Difficulty: high.

Payoff: very high.

### 4. Semantic Work State

Definition: a work unit's filesystem snapshot plus read/write symbols, claim dependencies, obligations, evidence, and merge preconditions.

Why absent: Git branches are easier.

Solves: branch-level isolation being too weak.

Difficulty: high.

Payoff: high.

### 5. Proof-Carrying Patch

Definition: patch plus machine-checkable and model-checkable evidence bundle satisfying declared obligations.

Why absent: agents emit diffs, not proof bundles.

Solves: false progress.

Difficulty: medium-high.

Payoff: very high.

### 6. Claim Ledger

Definition: provenance-tracked graph of codebase claims with status: proposed, corroborated, contested, invalidated, expired.

Why absent: transcripts blur claims and rationale.

Solves: hallucinated shared memory.

Difficulty: medium.

Payoff: high.

### 7. Budgeted Obligation

Definition: verifier requirement with expected cost, risk reduction, and blocking status.

Why absent: verification is usually a fixed checklist.

Solves: runaway verification costs.

Difficulty: medium.

Payoff: high.

### 8. Reconciliation Loop

Definition: deterministic process that reconciles patches, claims, obligations, and current repo state after each merge or failure.

Why absent: systems treat merge as the end.

Solves: stale memory and semantic drift.

Difficulty: high.

Payoff: high.

### 9. Failure Fingerprint

Definition: normalized representation of failure type, affected symbols, error trace, prior attempts, and likely cause.

Why absent: failures remain stderr blobs.

Solves: repeated bad retries.

Difficulty: medium.

Payoff: high.

### 10. Artifact Bus

Definition: publish/subscribe layer where agents and internal services consume typed artifacts.

Why absent: terminal output is the default bus.

Solves: coordination via text.

Difficulty: medium.

Payoff: high.

---

## 8. Brutal Evaluation Of Current Scope

### Too incremental

- Keeping PTY/worktree/adapters as the durable foundation.
- Post-hoc LLM extraction of EpistemicResidual from full stdout.
- Wails desktop app in MVP.
- Full MVP containing every novel mechanism at once.
- JSON flat-file state with migration later.
- Treating MCP compliance as central early.
- Mutation coverage, MAVEN, causal DAG, KG, strategy memory, router, UI, adapters, merge sequencer all in MVP.

### Misplaced effort

The scope spends too much effort making opaque agents look structured after the fact. Post-hoc residual extraction is a tax caused by the wrong boundary. The better move is to force structured sidecar artifacts through execution capsules, with stdout extraction as degraded compatibility.

The UI should not be first-order until the runtime ontology is proven.

MCP is useful, but it is not the hard problem. It should expose the substrate after the substrate is worth exposing.

### Won't matter much even if done well

- Adapter polish beyond two providers.
- Fancy capability posterior dimensions before you have enough data.
- Rich desktop visualizations before artifact state is validated.
- Local model recommendations before the cost architecture is clear.
- Thompson Sampling if task/evidence schemas are noisy.

### Deprioritize

- Full Wails app.
- Copilot adapter.
- Full mutation coverage loop in MVP.
- Full MAVEN four-dimension suite in MVP.
- MCP registration.
- Strategy Memory promotion to agent tendency injection.

### Add for step-function improvement

- Orchestration IR.
- Execution capsules.
- Context compiler.
- Artifact graph.
- Proof-carrying patches.
- Verifier-first obligations.
- Artifact-level state snapshots.
- Role-specific context projections.
- Token ROI instrumentation.

### Reframe as experiments

- EpistemicResiduals: experiment, not foundation.
- Entropy-delta gating: experiment; embedding entropy may be fake rigor.
- MAVEN dimensions: experiment; measure precision.
- Capability Posterior Router: experiment; may need hundreds of samples.
- Causal contribution score: experiment; causal inference from injected text may be weak.
- Mutation loop closure: experiment; useful but potentially expensive/noisy.

### Sequence to maximize learning

1. Build artifact graph and execution capsule.
2. Run one agent through capsule with sidecar output.
3. Build context compiler for planner/executor/verifier.
4. Require proof-carrying patch for one task class.
5. Compare against plain terminal baseline.
6. Only then add routing, memory, mutation, and UI.

---

## 9. Recommended Architecture

## A. Core Idea

Rewrite emdash as an **artifact-state orchestration runtime with a compiler-style context/control layer**.

The system is not a terminal supervisor. It is a runtime that transforms user intent into typed obligations, executes contract-bound capsules, stores artifacts and evidence, and converges through verifier-gated reconciliation.

## B. Core Runtime

Components:

- **Intent Compiler**: converts user request into Goal IR and obligations.
- **Artifact Store**: content-addressed patches, claims, tests, logs, evidence, context projections.
- **Event Log**: authoritative append-only state transitions.
- **State Projector**: materialized views for UI, scheduling, memory, and metrics.
- **Context Compiler**: builds role-specific context projections.
- **Capsule Runner**: launches local/remote/PTY/native executors under contract.
- **Verifier Engine**: checks proof obligations and emits evidence artifacts.
- **Reconciler**: merges accepted patches, invalidates stale artifacts, detects conflicts.
- **Budget Router**: selects model/agent/tool based on expected value per cost.
- **Human Gatekeeper**: approval and steering interface.

## C. Core Data Model

Store:

- `GoalCondition`
- `Obligation`
- `WorkOrder`
- `ExecutionCapsule`
- `ContextProjection`
- `PatchArtifact`
- `EvidenceArtifact`
- `Claim`
- `FailureFingerprint`
- `SemanticDelta`
- `StateSnapshot`
- `BudgetRecord`
- `DecisionRecord`

Flows:

```text
Intent
  -> Goal IR
  -> Obligations
  -> Work orders
  -> Execution capsules
  -> Patch artifacts
  -> Evidence artifacts
  -> Reconciliation
  -> Updated state snapshot
```

## D. Core Control Loop

1. User enters goal.
2. Intent Compiler creates goal conditions and initial obligations.
3. Verifier Engine determines what would prove progress.
4. Planner creates work orders only for unmet obligations.
5. Context Compiler emits minimal capsule context.
6. Runner executes agent/tool.
7. Agent returns patch plus sidecar artifacts.
8. Verifier checks obligations.
9. Reconciler merges or returns structured failure.
10. State Projector updates artifact graph.
11. Loop continues until obligations are discharged or blocked.

## E. Core Interfaces / Contracts

Stable internal contracts:

- `ExecutionCapsuleInput`
- `ExecutionCapsuleOutput`
- `PatchArtifact`
- `EvidenceArtifact`
- `ClaimArtifact`
- `ContextProjection`
- `VerifierResult`
- `SemanticDelta`
- `FailureFingerprint`

Adapters translate legacy CLI behavior into these contracts. Native future agents can implement them directly.

## F. Local And Remote Execution

Local:

- worktree/sandbox per capsule
- tool allowlist
- local verifier commands
- local artifact store

Remote:

- capsule serialized and sent to worker
- remote worker returns artifacts
- no remote worker can mutate authoritative state directly

Hybrid:

- expensive model planning remote
- code execution local
- verification local unless configured otherwise

## G. Human Oversight

The operator sees:

- goal conditions
- open obligations
- current patches
- verifier evidence
- contested claims
- cost by stage
- blocked decisions
- risk summary

Transcript view exists but is secondary.

Trust comes from provenance and replayable evidence, not chat readability.

## H. Token And Cost Architecture

Default behavior:

- no transcript replay
- every model call gets a compiled context projection
- every projection has a token budget
- artifacts are reusable
- expensive calls require expected-value justification
- verifier/tool checks run before model escalation where possible
- token ROI is measured by discharged obligations and accepted patches

## I. Differentiators

This would be genuinely different because it makes agent orchestration:

- artifact-native
- verifier-gated
- context-compiled
- event-sourced
- contract-bound
- budget-aware
- substrate-agnostic

That is a stronger foundation than "multi-agent terminal manager with learning."

---

## 10. Research And Build Roadmap

## Phase 0 - Falsification / Research Spikes

Objectives:

- prove artifact/capsule model beats terminal transcript baseline
- test whether context projections reduce tokens without quality loss
- test whether proof-carrying patches are practical

Experiments:

- one repo, one agent, one task class
- baseline: normal CLI prompt
- treatment: execution capsule + context projection + evidence artifact

Success metrics:

- 30%+ lower prompt tokens
- equal or better patch acceptance
- fewer retries
- verifier evidence reusable downstream

Failure modes:

- capsules too rigid
- sidecar outputs ignored
- context projection misses crucial facts

Kill criteria:

- agents cannot reliably comply with output contracts
- context projection causes more failures than it saves
- evidence artifacts are too expensive to produce

Instrument:

- token spend
- retries
- verifier pass/fail
- artifact reuse
- missing-context failures

## Phase 1 - Minimal Orchestration Kernel

Objectives:

- build event log, artifact store, capsule runner, verifier result model

Experiments:

- support one provider via PTY adapter
- require structured sidecar output
- support basic patch/evidence artifacts

Success metrics:

- crash-resumable runs
- deterministic state reconstruction
- accepted patch has attached evidence bundle

Failure modes:

- adapter fragility
- state schema churn
- artifact graph overcomplexity

Kill criteria:

- system cannot reconstruct state after interruption
- artifact model fails to represent common tasks

Instrument:

- event replay time
- artifact sizes
- schema validation failures
- contract violations

## Phase 2 - Differentiated Coordination

Objectives:

- add context compiler and verifier-first obligations
- support multi-agent work through artifact graph, not chat

Experiments:

- planner creates obligations before execution
- verifier generates missing-test tasks
- agents consume prior evidence artifacts

Success metrics:

- fewer repeated file inspections
- lower retry rate
- better detection of incomplete work

Failure modes:

- verifier obligations too noisy
- agents overfit to narrow checks

Kill criteria:

- obligation generation has poor precision
- users cannot understand why tasks exist

Instrument:

- obligation precision
- evidence reuse
- false-block rate
- missed-regression rate

## Phase 3 - Token / Cost Optimization

Objectives:

- make efficiency structural and measurable

Experiments:

- role-specific projections
- model tier routing
- memoized reasoning products
- expected-value budget gate

Success metrics:

- 40%+ lower total tokens per accepted patch
- expensive model calls reduced without quality loss
- measurable artifact reuse

Failure modes:

- cheap model misroutes
- memoized reasoning goes stale

Kill criteria:

- cost savings disappear after verifier overhead
- stale cached reasoning causes regressions

Instrument:

- token ROI
- cache hit rate
- stale artifact invalidations
- cost per obligation discharged

## Phase 4 - Advanced Reliability And Memory

Objectives:

- add claim ledger, semantic deltas, reconciliation, failure fingerprints

Experiments:

- semantic snapshot isolation
- claim invalidation by affected symbols
- failure fingerprint retry routing

Success metrics:

- fewer semantic merge conflicts
- fewer repeated failures
- better restart/resume fidelity

Failure modes:

- language graph inaccuracies
- claim ledger too noisy

Kill criteria:

- semantic conflict detector blocks too much
- memory requires constant human cleanup

Instrument:

- conflict precision/recall
- invalidation accuracy
- repeated-failure rate
- human override rate

## Phase 5 - Productization

Objectives:

- UI, security, packaging, migration

Experiments:

- artifact graph UI
- transcript as derived view
- policy presets
- local/remote capsule workers

Success metrics:

- user can explain why a patch is merge-ready
- secure default-deny execution
- migration from terminal workflows is tolerable

Failure modes:

- UI becomes terminal dashboard again
- policy friction blocks useful work

Kill criteria:

- users bypass artifact model to use raw terminals
- trust model is not understandable

Instrument:

- approval latency
- manual override rate
- security prompts
- abandoned runs

---

## 11. Hard Truths

Exciting but probably weak:

- Entropy-delta gating over embeddings. It sounds rigorous, but semantic embedding entropy may not map to truth or usefulness.
- Four named MAVEN dimensions. Useful framing, but risks becoming taxonomy theater unless precision is measured.
- Causal contribution scores. Causality from injected text to task outcome is very hard; treat as heuristic.
- Market-based bidding. Interesting, but agent self-estimated confidence is unreliable.
- Full mutation coverage everywhere. Valuable, but cost/noise can dominate.

Real bottlenecks remain:

- models still misunderstand code
- tests still under-specify behavior
- language tooling is uneven
- agents still resist strict output contracts
- human goals are often ambiguous
- verifier generation can hallucinate too

Complexity traps:

- knowledge graph lifecycle
- schema evolution
- semantic invalidation
- UI for contested claims
- multi-agent reconciliation
- local/remote security policy
- routing models without enough data

Tradeoffs you cannot escape:

- stronger contracts reduce flexibility
- richer verification costs time
- better memory requires invalidation machinery
- more parallelism increases reconciliation burden
- less transcript means less casual debuggability unless artifact views are excellent

Novelty cosplay:

- "swarm" without shared state
- "market" without calibrated incentives
- "memory" without invalidation
- "DAG" where nodes are just prompts
- "knowledge graph" full of unverified prose
- "AI compiler" without typed IR and passes

Keep boring:

- event log
- content-addressed artifacts
- SQLite/Postgres eventually
- JSON schema contracts
- deterministic replay
- sandbox policy
- normal test runners
- boring UI controls
- explicit approvals

---

## 12. Final Answer

Best rewrite thesis:

**Replace transcript-driven terminal orchestration with artifact-state orchestration: execution capsules produce proof-carrying patches against verifier-generated obligations, and every model context is compiled from structured state.**

Recommended end-state architecture:

**A compiler-style, event-sourced artifact runtime for agent work.** Terminal CLIs become compatibility executors. The durable system is the artifact graph, obligation graph, evidence ledger, and reconciliation loop.

Top 5 technical bets:

1. Execution capsules.
2. Context compiler.
3. Proof-carrying patches.
4. Artifact/evidence graph.
5. Verifier-first obligations.

Top 5 experiments to run immediately:

1. Capsule vs raw CLI on the same task.
2. Context projection vs transcript replay.
3. Patch plus evidence bundle acceptance rate.
4. Verifier-generated obligations before execution.
5. Artifact reuse reducing downstream token spend.

Top 5 mistakes to avoid:

1. Building the desktop app before proving the runtime.
2. Treating PTY adapters as the foundation.
3. Shipping the whole Orca scope as MVP.
4. Believing knowledge memory without invalidation.
5. Confusing named epistemic mechanisms with measured quality gains.

Shortest statement:

**This rewrite beats emdash if it stops managing agent conversations and starts compiling, executing, verifying, and reconciling typed software-change artifacts.**
