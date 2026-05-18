# Orca

**A local proof runtime for coding agents.**

Orca turns a coding goal into checkable obligations, runs coding agents inside bounded execution capsules, requires every accepted patch to carry evidence, compiles only the context each agent needs, and reconciles work through a durable artifact graph.

Orca is not primarily a terminal dashboard. It is the reliability layer above Claude Code, Codex, Copilot, and future coding agents.

The product promise:

> Give Orca a coding goal. Orca breaks it into provable steps, runs the right agents under controlled contracts, verifies their work, tracks evidence, avoids repeated context waste, and recommends merge only when the patch is backed by proof.

---

## 1. Problem

Coding agents are good at producing plausible diffs. They are weak at proving that those diffs satisfy the user's real intent.

Current orchestration tools mostly help users:

- launch many agents
- isolate them in worktrees
- monitor terminals
- compare diffs
- create pull requests
- run checks

That is useful, but it leaves the hard parts to the human:

- deciding what would prove the work is done
- preventing agents from duplicating context and work
- knowing which claims are verified and which are agent chatter
- comparing patches against the same acceptance criteria
- detecting when parallelism is hurting more than helping
- preserving useful evidence for future runs

Orca's job is to make agent work provable, cheaper to continue, and safer to merge.

---

## 2. Product Thesis

The unit of orchestration is not a terminal session, chat transcript, branch, or task.

The unit of orchestration is:

> a checkable obligation discharged by a proof-carrying patch inside a bounded execution capsule.

This changes the system from "run agents and inspect their output" to "define what must be proven, run agents under contract, and accept only evidence-backed results."

Core principles:

- **Obligations before tasks:** Orca defines what evidence would prove progress before it assigns work.
- **Artifacts over transcripts:** durable state is typed artifacts, not chat history.
- **Capsules over prompts:** each agent run has allowed effects, required outputs, budget, timeout, and gates.
- **Proof over confidence:** patches are accepted because evidence satisfies obligations, not because an agent sounds certain.
- **Compiled context over replay:** agents receive role-specific context projections, not previous transcripts.
- **Verification steers execution:** verifier results create new obligations, retry contracts, or merge decisions.
- **Learning only from evidence:** Orca learns from accepted artifacts, failures, and verified claims, not raw agent chatter.
- **Parallelism is conditional:** Orca runs multiple agents only when task topology justifies it.

---

## 3. How Orca Differs From Emdash

Emdash is a polished multi-agent workbench. It launches coding agents, isolates work in worktrees, shows terminals and diffs, integrates issues and PR flow, and helps humans compare results.

Orca should not clone that cockpit.

Orca should become the verification and coordination runtime:

| Area | Emdash-style cockpit | Orca |
| --- | --- | --- |
| Primary object | task/session/worktree | obligation/artifact/capsule |
| Agent input | prompt or issue | execution contract |
| Agent output | diff and transcript | proof-carrying patch |
| State | sessions, branches, logs | artifact graph and event log |
| Context | terminal/chat history | compiled context projection |
| Parallelism | launch multiple agents | classify topology first |
| Review | human compares diffs | verifier checks obligations |
| Memory | session history | verified claims, evidence, failures |
| Cost control | user/provider-level | per-obligation token ROI |

Emdash helps run agents. Orca decides what agent work must prove.

---

## 4. Core Architecture

```text
User Goal
  -> Goal IR
  -> Obligations
  -> Execution Capsules
  -> Patch Artifacts
  -> Evidence Artifacts
  -> Verifier Results
  -> Reconciliation
  -> Updated Artifact Graph
  -> Merge Recommendation or New Obligations
```

Main components:

- **Intent Compiler:** converts user intent into Goal IR and initial obligations.
- **Obligation Planner:** creates capsules for unmet obligations; invokes the topology classifier to select execution shape before capsule creation.
- **Task Topology Classifier:** chooses single-agent, implementer-reviewer, or human-gated flow (MVP). Additional topologies deferred to Phase 2.
- **Context Compiler:** builds minimal role-specific context projections from artifacts.
- **Capsule Runner:** launches agents/tools inside bounded execution capsules.
- **Artifact Store:** stores patches, claims, evidence, projections, logs, failures, and decisions.
- **Verifier Engine:** checks whether evidence satisfies obligations.
- **Reconciler:** accepts or rejects patches, creates follow-up obligations.
- **Budget Controller:** tracks token, time, tool, and coordination cost against verified value.
- **Human Gatekeeper:** surfaces approvals, blocked decisions, risks, and merge readiness.

---

## 5. Core Primitives

### 5.1 Goal IR

Goal IR is the durable representation of the user's objective.

It replaces the old "Semantic Goal State" name but keeps the useful idea: Orca tracks the desired end state, not a fixed task list.

Fields:

```json
{
  "goal_id": "string",
  "original_intent": "string",
  "goal_conditions": [],
  "scope_constraints": [],
  "risk_level": "low|medium|high",
  "created_at": "timestamp",
  "status": "active|blocked|complete|cancelled"
}
```

Each goal condition keeps both original and effective wording:

```json
{
  "id": "GC-1",
  "description": "Original user-facing condition",
  "effective_description": "Approved clarified condition",
  "status": "unmet|partially_met|met|blocked",
  "refinements": []
}
```

Refinements may clarify or narrow a condition. They may not silently broaden scope. User approval is required for any material change.

### 5.2 Obligation

An obligation is a checkable requirement that must be discharged before Orca can say progress was made.

Examples:

- reproduce the failing behavior
- identify affected files and symbols
- add a regression test
- preserve a public API contract
- run targeted tests
- run lint/typecheck
- document an unresolved risk
- prove no unrelated files changed

Fields:

```json
{
  "obligation_id": "OB-1",
  "goal_condition_id": "GC-1",
  "description": "What must be proven",
  "evidence_required": ["test_result", "static_check", "patch_review"],
  "blocking": true,
  "risk_level": "low|medium|high",
  "status": "open|satisfied|failed|waived",
  "satisfied_by": []
}
```

Obligations replace the old habit of generating tasks directly from a semantic gap. Capsules exist only to satisfy obligations.

### 5.3 Execution Capsule

An execution capsule is the contract for one agent or tool run.

It is the most important primitive in Orca.

Fields:

```json
{
  "capsule_id": "CAP-1",
  "obligation_ids": ["OB-1"],
  "agent": "codex|claude|copilot|tool",
  "role": "executor|reviewer|tester|investigator",
  "context_projection_id": "CTX-1",
  "allowed_paths": [],
  "forbidden_paths": [],
  "allowed_tools": [],
  "forbidden_actions": [],
  "required_outputs": [],
  "verifier_gates": [],
  "budget": {
    "max_tokens": 0,
    "max_wall_time_seconds": 0,
    "max_retries": 0
  },
  "sandbox": {
    "worktree_path": "string",
    "network": "deny|allowlist|allow",
    "write_scope": "worktree_only"
  }
}
```

Capsule lifecycle states: `pending` → `worktree_created` → `workspace_attached` → `setup_run` → `agent_running` → `completed|failed`. The `pending` state is set by the Obligation Planner when it creates the capsule contract; the capsule runner transitions it to `worktree_created` as its first action. The capsule runner must track and expose all states; partial failures must leave no ambiguous intermediate state.

Capsules replace loose prompts. A legacy CLI can still run inside a capsule, but terminal execution is an adapter detail.

### 5.4 Context Projection

A context projection is a deterministic, role-specific briefing compiled from the artifact graph.

Agents do not receive raw transcripts by default.

MVP projection types:

- `executor_projection`
- `human_summary_projection`

Additional projection types (`planner`, `reviewer`, `tester`, `verifier`, `reconciler`) are deferred to Phase 2. Add a type only when a distinct role requires materially different context content.

Fields:

```json
{
  "context_projection_id": "CTX-1",
  "role": "executor",
  "source_artifact_ids": [],
  "included_sections": [],
  "omitted_sections": [],
  "token_budget": 0,
  "freshness_base": "state_snapshot_id",
  "created_at": "timestamp"
}
```

Context projections are the main token-cost reduction mechanism.

They should include:

- relevant goal conditions
- obligations assigned to this capsule
- allowed scope
- verified claims only when needed
- current failure fingerprints
- relevant prior evidence
- exact expected outputs

They should avoid:

- full prior transcripts
- irrelevant terminal logs
- unverified agent claims unless labeled
- broad repo summaries when a file-level view is enough

The two MVP projection types serve opposite audiences and must never be the same document.

### 5.4.1 executor_projection

The executor_projection is the agent's working briefing. It is minimal and constraint-focused. The agent needs to know what to do, what the boundaries are, and what outputs are required — nothing else.

Content:

- relevant goal conditions
- obligations assigned to this capsule
- allowed paths and forbidden paths
- allowed tools and forbidden actions
- verified claims only when directly needed
- current failure fingerprints for affected areas
- relevant prior evidence artifacts
- exact required outputs and output schema

The executor_projection should contain the fewest tokens that allow the agent to do its work correctly. It is not a design document.

### 5.4.2 human_summary_projection

The human_summary_projection is the developer-facing implementation briefing. It is the primary surface where a developer sees what is about to be built. It is emitted before the capsule runner launches the agent, not after.

It is not a status display. It is not a post-hoc diff summary. It is a pre-execution design document that gives a developer enough information to make a meaningful go/no-go decision before an agent runs.

Fields specific to human_summary_projection (in addition to the shared context projection fields above):

```json
{
  "goal_plain": "string",
  "conditions_addressed": [
    {
      "condition_id": "GC-1",
      "description": "original user-facing condition wording"
    }
  ],
  "obligations_addressed": [
    {
      "obligation_id": "OB-1",
      "description": "developer-readable description of what must be proven",
      "risk_level": "low|medium|high"
    }
  ],
  "implementation_approach": "string",
  "expected_file_scope": {
    "to_read": [],
    "to_write": [],
    "to_create": []
  },
  "explicit_exclusions": [],
  "topology": {
    "selected": "single|implementer_reviewer|human_gated",
    "rationale": "string"
  },
  "design_decisions": [
    {
      "decision": "string",
      "alternatives_considered": [],
      "reason": "string"
    }
  ],
  "pre_execution_risks": [
    {
      "description": "string",
      "source": "obligation_risk|failure_fingerprint|scope|claim"
    }
  ],
  "evidence_plan": {
    "verifier_gates": [],
    "tests_to_run": [],
    "static_checks": []
  },
  "budget": {
    "max_tokens": 0,
    "max_wall_time_seconds": 0
  },
  "required_approvals": []
}
```

Content requirements:

- **Goal and conditions addressed**: the goal in plain language and which goal conditions this capsule serves, using the original user-facing condition wording — not internal IDs as primary identifiers.
- **Implementation approach**: what the agent intends to implement or change, stated as intended behavior and affected components. This is the "what are we building" field. It must be written in terms a developer can evaluate, not framed as evidence requirements.
- **Expected file and symbol scope**: files expected to be read, written, or created; symbols expected to be modified where known. Derived from obligations and capsule allowed_paths. If the scope is uncertain, say so explicitly rather than omitting the field.
- **Explicit exclusions**: files and areas the capsule will not touch, especially areas a developer might reasonably assume are in scope given the goal.
- **Topology and rationale**: which topology was selected and the specific reason — not just the label, but the classifier's actual inputs (risk level, obligation count, estimated file overlap, known failure fingerprints in area).
- **Design decisions**: any architectural or implementation choices the Context Compiler can infer from the obligation set, codebase state, and verified claims. Include alternatives considered and not taken. If the agent will make a choice that is not obvious from the goal, it belongs here.
- **Pre-execution risks**: risks known before the agent runs, derived from obligation risk levels, failure fingerprints in the affected file area, and scope. These are things the developer should know about before authorizing the run, not things discovered after.
- **Evidence plan**: what the verifier will check after the capsule completes — which tests will run, which static gates apply, what scope constraints will be enforced. The developer should be able to read this and understand what "done" means before the capsule starts.
- **Required approvals**: what human gates are triggered before or after this capsule, so the developer knows where they will be asked to intervene again.

Execution gating rules:

- `human_gated` topology: developer must explicitly approve the human_summary_projection before execution proceeds.
- `implementer_reviewer` topology with medium or high risk obligations: developer must explicitly approve before the implementer capsule runs.
- `single` topology with low-risk obligations: projection is displayed and execution proceeds after a configurable review window unless the developer blocks it. Default window is 30 seconds.

The human_summary_projection is compiled from:

- Goal IR and goal conditions
- Obligations assigned to this capsule
- Topology classifier decision record
- Capsule contract (allowed paths, tools, budget)
- Known failure fingerprints in the affected file area
- Verified claim artifacts for affected files and symbols

It must not include:

- Internal artifact IDs as primary identifiers (include as secondary reference only)
- Raw transcript content
- Executor context projection content
- Unverified claims presented as facts (include with a `[proposed]` label if relevant to a design decision or risk)

Relationship to executor_projection:

The executor_projection and human_summary_projection are compiled from the same artifact graph state but serve opposite audiences. Merging them would either bloat the agent's context — wasting tokens and degrading agent focus — or leave the developer without the design rationale needed to make a meaningful go/no-go decision. They are always two separate documents.

### 5.5 Patch Artifact

A patch artifact is the actual code change plus metadata.

Fields:

```json
{
  "patch_id": "PATCH-1",
  "capsule_id": "CAP-1",
  "base_commit": "string",
  "changed_files": [],
  "diff_path": "string",
  "summary": "string",
  "obligation_ids_claimed": [],
  "risk_notes": [],
  "status": "candidate|accepted|rejected|superseded"
}
```

### 5.6 Evidence Artifact

An evidence artifact proves, weakens, or contextualizes a claim or patch.

MVP types:

- `test_result`
- `lint_result`
- `typecheck_result`
- `diff_risk_report`

Additional types (`static_analysis_result`, `manual_review`, `agent_review`, `runtime_trace`, `reproduction_log`, `mutation_survivor`, `security_scan`) are deferred. Add a type when a verification gate requires it.

Fields:

```json
{
  "evidence_id": "EV-1",
  "type": "test_result",
  "source": "pytest",
  "command": "string",
  "exit_code": 0,
  "summary": "string",
  "raw_log_path": "string",
  "supports": [],
  "weakens": [],
  "created_at": "timestamp"
}
```

### 5.7 Proof-Carrying Patch

A proof-carrying patch is a patch artifact plus the evidence bundle needed to satisfy its obligations.

Required contents:

- patch artifact
- obligations addressed
- commands run
- evidence artifacts
- files changed
- assumptions
- unresolved risks
- verifier result
- merge recommendation

A patch is not complete until the verifier can map evidence to obligations.

### 5.8 Claim Artifact

Claim artifacts replace the old EpistemicResidual as the durable memory unit.

Agents may still report assumptions, discoveries, exclusions, and open questions, but they become typed claims with status and provenance.

Fields:

```json
{
  "claim_id": "CL-1",
  "text": "string",
  "claim_type": "assumption|invariant|exclusion|open_question|risk|test_gap",
  "source_capsule_id": "CAP-1",
  "affected_files": [],
  "affected_symbols": [],
  "status": "proposed|verified|stale",
  "evidence_ids": [],
  "last_validated_against": "state_snapshot_id"
}
```

MVP claim statuses: `proposed`, `verified`, `stale`. The `contested` and `invalidated` statuses are deferred to Phase 3 when claim-to-claim resolution infrastructure is available.

Claim artifacts must be labeled. Proposed claims are not treated as facts.

### 5.9 Failure Fingerprint

A failure fingerprint is a normalized failure record used to avoid repeated bad retries.

Fields:

```json
{
  "failure_id": "FAIL-1",
  "source_capsule_id": "CAP-1",
  "failure_type": "test|lint|typecheck|runtime|merge|policy|infra|agent",
  "summary": "string",
  "affected_files": [],
  "affected_symbols": [],
  "error_signature": "string"
}
```

`source_capsule_id` is required. It links a fingerprint to the capsule that produced it, which allows the store to satisfy capsule-scoped failure lookup (`LoadFailuresForCapsule`) without scanning all fingerprints.

Prior attempt history, likely cause inference, and recommended action generation are deferred to Phase 3. The MVP fingerprint records what failed and where; the orchestrator creates follow-up obligations mechanically from that record.

### 5.10 Decision Record

Every important orchestration decision is recorded.

Examples:

- why a topology was chosen
- why an agent was selected
- why a tool was allowed
- why a patch was accepted or rejected
- why an obligation was waived
- why multi-agent mode was collapsed to single-agent mode

---

## 6. Control Loop

1. User enters a goal.
2. Intent Compiler creates Goal IR.
3. Verifier Engine proposes initial obligations.
4. Obligation Planner selects topology, then creates capsules for open obligations.
5. Context Compiler emits minimal projections.
6. Context Compiler emits human_summary_projection. Developer reviews what is about to be built: goal conditions addressed, implementation approach, expected file scope, topology rationale, pre-execution risks, and evidence plan. For human-gated topology or medium/high-risk obligations, developer must explicitly approve before execution continues. For low-risk single capsules, execution proceeds after the configured review window unless the developer blocks it.
7. Capsule Runner executes agents/tools under contract.
8. Agents return sidecar outputs or transcripts; adapter normalizes both into the same artifact schema.
9. Verifier maps evidence to obligations.
10. Reconciler accepts, rejects, or creates follow-up obligations.
11. Budget Controller updates cost and token ROI.
12. Human Gatekeeper approves high-risk actions and merge decisions.
13. Loop continues until obligations are satisfied, waived, or blocked.

---

## 7. Task Topology Classifier

Orca should not assume multiple agents are always better.

Before creating capsules, classify topology:

| Topology | Use When | Avoid When |
| --- | --- | --- |
| `single` | work is small, sequential, or high-overlap | independent pieces are obvious |
| `implementer_reviewer` | patch risk is medium/high | task is trivial |
| `human_gated` | destructive, broad, security-sensitive, dependency changes | low-risk local edit |

Deferred to Phase 2: `parallel`, `test_first`, `investigate_then_implement`. Do not implement these until the MVP topology baseline is validated on real tasks.

Classifier inputs:

- obligation count and risk
- expected file overlap
- changed-file limits
- known failure fingerprints
- required tools
- whether reproduction is needed
- whether tests already exist
- user approval policy
- cost budget

If coordination cost exceeds expected value, Orca collapses to a simpler topology.

---

## 8. Agent Adapter Layer

Adapters translate legacy coding tools into Orca's contracts.

Initial adapters:

- Codex CLI
- Claude Code
- GitHub Copilot CLI

Adapter responsibilities:

- preflight authentication and version checks
- create or attach to capsule worktree
- launch CLI or tool process
- inject context projection
- enforce allowed paths and tool policy where possible
- collect diff and file changes
- read structured sidecar output
- validate schema
- fall back to transcript extraction when sidecar output is missing or malformed
- normalize errors into failure fingerprints
- preserve raw logs as debug artifacts, subject to retention policy

Required sidecar output schema:

```json
{
  "obligations_addressed": [],
  "files_changed": [],
  "commands_run": [],
  "assumptions": [],
  "claims": [],
  "risks": [],
  "follow_up_needed": [],
  "evidence_paths": []
}
```

Transcript extraction and sidecar output are alternate collection paths. Both must produce artifacts that conform to the same schema. Transcript extraction is not a degraded mode that produces different or lesser artifacts. An executor that receives transcript-extracted outputs must not be able to distinguish them from sidecar outputs. Until agent CLIs emit reliable sidecar output, transcript extraction will be the primary path in practice; the adapter layer must be designed accordingly.

---

## 9. Artifact Graph And Event Log

The event log is the authoritative history. The artifact graph is the materialized state.

Event examples:

- `goal_created`
- `obligation_created`
- `topology_selected`
- `context_projection_created`
- `capsule_started`
- `capsule_completed`
- `patch_artifact_created`
- `evidence_artifact_created`
- `claim_created`
- `verifier_result_created`
- `failure_fingerprint_created`
- `decision_record_created`
- `patch_accepted`
- `patch_rejected`
- `merge_applied`
- `artifact_invalidated`

State layout:

```text
.orca/
  config.yaml
  events.log
  state/
    goal.json
    obligations.json
    artifact_graph.json
    budget.json
    decisions.json
  artifacts/
    patches/
    evidence/
    claims/
    contexts/
    failures/
    logs/
  capsules/
    CAP-*/
```

No raw transcript is reusable memory by default. Raw logs are debug artifacts.

---

## 10. Verifier Engine

Verification is the orchestrator, not the final checkbox.

The verifier has two jobs:

1. define what evidence is needed
2. decide whether available evidence satisfies obligations

Default verifier stages:

- **Preflight:** repo status, auth, configured gates, allowed commands, clean base.
- **Scope check:** changed files and LOC within capsule contract.
- **Static checks:** lint, typecheck, formatting, security checks as configured.
- **Targeted tests:** tests relevant to changed files or obligations.
- **Regression checks:** bugfix/security obligations require reproduction or regression evidence.
- **Patch review:** model or human review for risk, assumptions, and obligation fit.
- **Merge readiness:** all blocking obligations satisfied or explicitly waived.

Verifier result:

```json
{
  "verifier_result_id": "VR-1",
  "patch_id": "PATCH-1",
  "obligation_results": [],
  "blocking_failures": [],
  "warnings": [],
  "recommended_action": "accept|retry|split|reject|human_review"
}
```

---

## 11. Reconciliation

Reconciliation happens after every capsule completion, verifier failure, and merge.

MVP responsibilities:

- map evidence to obligations
- accept or reject obligation satisfaction
- create follow-up obligations from failure fingerprints and evidence gaps
- decide whether a patch can merge
- update budget records (token spend per obligation)
- create decision records

Deferred to Phase 2: stale claim invalidation after code changes, file and symbol overlap detection.

Merge policy:

- no merge recommendation while blocking obligations are open
- high-risk obligations require human approval
- scope violations require human approval or capsule retry
- failed static gates block merge unless explicitly waived
- unverified claims may not justify merge
- accepted patches retain evidence bundle for audit

---

## 12. Token And Cost Architecture

Token savings come from structure, not prompt tricks.

Mechanisms:

- no transcript replay by default
- role-specific context projections
- artifact reuse
- hash-based reuse of unchanged projections
- cheap tool checks before expensive model calls
- sidecar structured outputs before transcript extraction
- test/static evidence before model review
- coordination-cost budget before parallel fan-out

Track per stage:

- tokens spent
- wall time
- tool calls
- retries
- duplicated file reads
- overlapping edits
- obligations discharged
- patches accepted
- patches rejected
- evidence artifacts reused
- human interventions

Primary metric:

> verified value per 1K tokens

Verified value is based on satisfied obligations, accepted patches, avoided retries, and reused evidence.

---

## 13. Learning Layer

The learning layer is important, but it is not the MVP foundation.

Orca learns only from structured artifacts:

- accepted patches
- rejected patches
- verifier results
- failure fingerprints
- verified claims
- invalidated claims
- topology decisions
- cost records
- human approvals and rejections

Initial learning should be simple:

- remember recurring failure fingerprints
- remember file areas with repeated verifier failures
- remember which topology worked for similar obligation sets
- remember which evidence artifacts are reusable
- remember which claims became stale after code changes

Do not start with complex agent-performance inference.

### `--no-learning` Mode

`--no-learning` is a comparison mode, not the default product experience.

When enabled, Orca still runs the proof runtime:

- Goal IR
- obligations
- capsules
- context projections
- verifier gates
- proof-carrying patches
- reconciliation

It disables adaptive reuse:

- prior failure fingerprints
- claim reuse
- topology outcome memory
- historical routing hints
- reusable evidence suggestions

This lets Orca compare proof-runtime value against proof-runtime plus learning. If the learning layer does not reduce retries, token cost, or verifier failures, it should not be promoted.

---

## 14. Claim Trust Model

This replaces entropy-delta gating.

The old Shannon-entropy-over-embeddings framing is removed. It is too hard to defend and risks fake rigor.

Claims are ranked by operational trust signals:

- direct tool evidence
- independent source
- human confirmation
- freshness against current code state
- specificity
- contradiction status
- downstream reuse without failure
- implication in prior failures

MVP claim statuses:

- `proposed`: agent reported it, not verified
- `verified`: supported by tool, independent agent, or human
- `stale`: affected code changed since validation

`contested` and `invalidated` are deferred to Phase 3. Add them only when claim-to-claim resolution proves necessary.

Only verified claims can be injected as facts. Proposed and stale claims may be included only with labels.

---

## 15. Coordination Safety

### Tool Scope

Each capsule gets explicit tool permissions.

Default policy:

- write only inside capsule worktree
- no destructive commands outside worktree
- network denied unless allowed
- secrets never written to artifacts
- high-risk commands require approval

### Parallel Safety

Parallel work requires:

- low expected file overlap, or
- clear one-writer/many-reviewer split, or
- separate obligations with independent merge paths

Shared files are serialized by default:

- lock files
- migrations
- generated files
- global config
- dependency manifests
- broad public API files

### Human Gates

Human approval required for:

- pre-execution implementation brief (human_summary_projection) for human-gated topology or medium/high-risk obligations — developer must confirm what is about to be built before the agent runs
- destructive operations
- dependency upgrades
- security-sensitive changes
- large diffs over configured threshold
- failed gate waivers
- scope expansion
- merge of high-risk patches

---

## 16. MVP Scope

The MVP must prove the new architecture, not implement every advanced idea.

**Phase 0 falsification spikes must pass their exit criteria before Phase 1 infrastructure begins.** If context projections do not reduce tokens on real repos, or obligation mapping does not produce useful merge decisions, revise the architecture before building infrastructure.

### In MVP

- CLI-first local runtime
- one active goal per repo
- Goal IR
- obligation generation
- task topology classifier v0 (single, implementer-reviewer, human-gated only)
- execution capsules
- Codex CLI and Claude Code adapters
- optional Copilot CLI adapter if low effort
- worktree per capsule
- event log
- artifact store
- executor and human-summary context projections
- structured sidecar output schema
- transcript extraction path producing identical artifact schema
- static verifier gates
- targeted test gates
- proof-carrying patch schema
- failure fingerprints (type, signature, affected files only)
- reconciliation loop (map evidence, accept/reject, follow-up obligations)
- merge recommendation
- human approval gates
- budget and token accounting
- minimal claim ledger (proposed, verified, stale)
- `--no-learning` comparison mode

### Not In MVP

- Work Orders as a first-class primitive
- extended topology types (parallel, test-first, investigate-then-implement)
- extended context projection types (reviewer, tester, planner, verifier, reconciler)
- extended claim states (contested, invalidated)
- reconciler stale-claim invalidation and overlap detection
- failure fingerprint prior-attempt history and recommended-action inference
- Wails desktop app
- mobile interface
- MCP self-registration
- full Codebase Knowledge Graph
- Strategy Memory as a broad subsystem
- Capability Posterior Router
- Thompson Sampling
- MAVEN full four-dimension suite
- default mutation coverage loop
- causal DAG failure archaeology
- causal contribution scoring
- symbol-level Semantic Snapshot Isolation
- composition review
- agent tendency injection
- entropy-delta gating
- local GPU/model recommendation in the core product
- multi-repo orchestration
- live context refresh into running agents

### MVP Success Criteria

The MVP succeeds if Orca can:

- turn a real coding goal into obligations
- run an agent inside a capsule
- produce a patch artifact
- attach evidence artifacts
- verify obligation satisfaction
- avoid replaying transcripts into later agents
- create a useful failure fingerprint on failure
- recommend merge or retry with clear reasons
- show token/cost spent per obligation
- demonstrate that the executor context projection contains fewer tokens than a naive transcript replay for the same follow-up capsule

---

## 17. Deferred Advanced Systems

These ideas remain valuable, but they should be built only after the artifact/capsule/proof runtime works.

### 17.1 Work Orders

Work Orders are deferred from the MVP.

In MVP, the Obligation Planner creates capsules directly from obligations. Introduce Work Orders in Phase 2 if multi-capsule obligation fulfillment, parallel fan-out scheduling, or retry queuing requires a named coordination unit between an obligation and its capsules. Do not add the abstraction speculatively.

### 17.2 Extended Topology Types

`parallel`, `test_first`, and `investigate_then_implement` topology types are deferred to Phase 2.

Build these only after the three MVP topologies are validated on real tasks. Add `test_first` when bugfix and regression obligations are common enough to justify it. Add `parallel` when file-overlap detection is reliable. Add `investigate_then_implement` when scope-unclear obligations recur.

### 17.3 Extended Context Projection Types

`planner_projection`, `reviewer_projection`, `tester_projection`, `verifier_projection`, and `reconciler_projection` are deferred to Phase 2.

Add a projection type only when a distinct agent role requires materially different context content and the difference is measurable in token savings or verifier outcomes.

### 17.4 Extended Claim States

`contested` and `invalidated` claim states are deferred to Phase 3.

Add them only when claim-to-claim resolution infrastructure is available and the distinction from `stale` produces measurable value in verifier decisions.

### 17.5 Reconciler Scope Expansion

Stale claim invalidation after code changes and file/symbol overlap detection are Phase 2 additions to the reconciler.

Add them when the claim ledger is large enough that stale injection causes verifier failures, or when parallel capsule conflicts become frequent.

### 17.6 Failure Fingerprint Intelligence

Prior attempt history, likely cause inference, and recommended action generation are Phase 3 additions to failure fingerprints.

Add them only when recurring fingerprints are common enough that machine-suggested next actions would be acted on.

### 17.7 MAVEN Red-Team Review

Keep as a later verifier mode.

Use factual, logical, causal, and assumption probes as a review rubric. Measure precision before making MAVEN a default gate.

Do not route agents based on MAVEN dimensions until there is enough data.

### 17.8 Targeted Mutation Testing

Keep as high-risk verification mode.

Use for:

- bugfixes
- security changes
- core business logic
- flaky tests
- user-requested high-confidence runs
- files with repeated test gaps

Mutation survivors should become `test_gap` claim artifacts.

Do not run mutation coverage by default in MVP.

### 17.9 Capability Posterior Router

Defer until Orca has enough comparable outcomes.

Early routing should use:

- task topology
- user preference
- adapter availability
- tool requirements
- simple historical failure counts

Later routing can use Bayesian posteriors over agent, topology, obligation type, and verifier outcome.

### 17.10 Strategy Memory

Defer broad strategy memory.

Start with concrete memories:

- failure fingerprints
- topology outcomes
- recurring risky file areas
- accepted/rejected patch stats

Promote to Strategy Memory only when patterns are measurable.

### 17.11 Codebase Knowledge Graph

Defer full KG.

Start with Claim Ledger plus optional symbol extraction.

Later add:

- symbol graph
- call graph
- test ownership
- API contract tracking
- file-to-obligation mapping

### 17.12 Causal DAG Failure Archaeology

Defer and soften claims.

Future version may track information flow between artifacts and capsules to identify candidate causal contributors.

Do not claim deterministic root cause from embeddings or text similarity.

### 17.13 Semantic Snapshot Isolation

Defer symbol-level SSI.

MVP conflict detection:

- file overlap
- protected path overlap
- dependency manifest overlap
- public API file overlap
- verifier-detected regression

Later add LSP-derived symbol footprints.

### 17.14 MCP Compliance

Defer as integration layer.

Internal Orca contracts must not depend on MCP. MCP can later expose tools, artifacts, and agent registration.

### 17.15 Desktop UI

Defer until CLI runtime works.

When built, UI should show:

- goal
- obligations
- capsules
- patches
- evidence
- failures
- blocked decisions
- merge readiness
- cost

Terminal transcript view is drill-down, not the main surface.

---

## 18. Replacement Map From Old Orca Plan

| Old Item | New Treatment |
| --- | --- |
| Semantic Goal State | Keep concept, rename to Goal IR and Goal Conditions |
| Residual Planner | Replace with Obligation Planner |
| Task-first planning | Replace with obligations-first planning |
| Work Order | Removed from MVP; Obligation Planner creates capsules directly. Reintroduce in Phase 2 if multi-capsule scheduling requires a coordination unit. |
| Agent Adapter Layer | Keep, but adapters execute capsules and validate artifacts; transcript extraction and sidecar output produce the same artifact schema |
| EpistemicResidual | Replace with ClaimArtifact plus structured sidecar output |
| Post-hoc stdout extraction as primary | Both sidecar and transcript extraction are valid collection paths producing identical schemas |
| Knowledge Bus | Replace with Artifact Graph and Context Compiler |
| Entropy-Delta Gating | Replace with Claim Trust Model |
| Independent corroboration | Keep as claim verification signal |
| Capability Posterior Router | Defer; start with topology classifier and simple history |
| Thompson Sampling | Defer until enough comparable outcomes exist |
| MAVEN full red-team | Defer; keep as optional verifier rubric |
| Mutation coverage loop | Defer; keep targeted high-risk mode |
| Mutation survivors as test gaps | Keep as future `test_gap` claim artifacts |
| Causal DAG archaeology | Defer; later use for candidate contributors only |
| Causal contribution score | Remove from core plan |
| Codebase Knowledge Graph | Defer; start with Claim Ledger |
| Conflict Graph | Defer; claim status handles contested claims in MVP |
| Strategy Memory | Defer; start with concrete failure/topology memory |
| Symbol-level SSI | Defer; start with file/protected-path conflict checks |
| Composition Review | Defer until merge/reconciliation is stable |
| MCP Compliance Layer | Defer as integration layer |
| Wails desktop app | Defer; CLI first |
| Mobile monitoring | Defer |
| Recommended local GPU/model | Move to setup docs or appendix, not core architecture |
| Full architecture MVP | Replace with narrow proof-runtime MVP |

---

## 19. Data Model Summary

Required MVP objects:

- `GoalIR`
- `GoalCondition`
- `Obligation`
- `ExecutionCapsule`
- `ContextProjection`
- `PatchArtifact`
- `EvidenceArtifact`
- `ClaimArtifact`
- `FailureFingerprint`
- `VerifierResult`
- `DecisionRecord`
- `BudgetRecord`
- `StateSnapshot`

Minimal relationships:

```text
GoalIR
  has many GoalConditions
GoalCondition
  has many Obligations
Obligation
  has many ExecutionCapsules
ExecutionCapsule
  creates PatchArtifacts, EvidenceArtifacts, ClaimArtifacts, FailureFingerprints
VerifierResult
  evaluates PatchArtifact against Obligations using EvidenceArtifacts
Reconciler
  updates Obligations, Claims, BudgetRecords, Decisions, and StateSnapshot
```

---

## 20. Technical Stack

Preferred implementation shape:

- Go supervisor runtime
- CLI-first interface
- JSONL event log
- JSON materialized state for MVP
- file-backed artifact store
- git worktrees for capsule isolation
- schema validation for all artifacts
- user-configured verifier commands
- adapter interface for coding CLIs

Database:

- MVP: file-backed JSON/JSONL
- Later: SQLite if event replay or query volume becomes painful

UI:

- MVP: CLI and local artifact files
- Later: desktop app if runtime proves useful

Model configuration:

- provider-agnostic
- no hardware-specific model in core plan
- separate setup docs may recommend local models

---

## 21. Observability

Orca should make decisions inspectable.

Show:

- pre-execution implementation brief (human_summary_projection): what is about to be built, which files are in scope, what approach the agent will take, what topology was selected and why, what evidence will verify the work — surfaced before the capsule runner starts, not after
- current goal conditions
- open obligations
- active capsules
- selected topology and reason
- context projection token counts
- patch evidence status
- verifier failures
- failure fingerprints
- blocked human decisions
- budget spent per obligation
- merge readiness

Warnings:

- `SCOPE_VIOLATION`
- `MISSING_EVIDENCE`
- `UNVERIFIED_CLAIM_USED`
- `CONTEXT_PROJECTION_STALE`
- `COORDINATION_COST_EXCEEDED`
- `CAPSULE_TIMEOUT`
- `VERIFIER_GATE_FAILED`
- `PATCH_TOO_LARGE`
- `CLAIM_STALE`
- `TRANSCRIPT_EXTRACTION_FALLBACK_USED`

---

## 22. Build Roadmap

### Phase 0 - Falsification Spikes

Goal: prove the architecture is worth building before committing to infrastructure.

Spikes:

- run one coding goal through manual obligations
- execute one capsule with Codex or Claude
- produce sidecar output; also run transcript extraction on the same session and confirm both produce identical artifact schema
- attach static/test evidence
- compile a second context projection without transcript replay
- compare token use against raw transcript continuation
- test whether proof-carrying patch format is usable

Exit criteria:

- artifact flow works on a real repo
- evidence can be mapped to obligations
- context projection is smaller than transcript replay for the same follow-up capsule
- failure output creates a useful retry contract
- transcript extraction adapter produces artifacts conforming to the same schema as sidecar output

**Do not begin Phase 1 until all Phase 0 exit criteria are met.** If projections do not reduce tokens or obligation mapping does not produce useful merge decisions on real codebases, revise the architecture before building Phase 1 infrastructure.

### Phase 1 - Minimal Proof Runtime

Build:

- CLI
- Goal IR
- obligations
- capsules
- artifact store
- event log
- Codex and Claude adapters (both sidecar and transcript extraction paths)
- static/test gates
- proof-carrying patch schema
- reconciliation (map evidence, accept/reject, follow-up obligations)
- budget records

Exit criteria:

- one real goal can go from intent to merge recommendation
- failed work produces structured retry reason
- no transcript replay required for follow-up capsule

### Phase 2 - Topology And Cost Control

Build:

- topology classifier (confirm single and implementer-reviewer; add parallel, test-first, investigate-then-implement only if validated)
- Work Orders as coordination unit if multi-capsule scheduling requires it
- implementer-reviewer capsule flow
- coordination-cost budget
- simple historical routing hints
- protected path serialization
- extended context projection types (reviewer, tester)
- reconciler stale-claim invalidation and overlap detection
- extended claim states (contested, invalidated) if needed

Exit criteria:

- Orca selects topology correctly for single and implementer-reviewer cases
- reviewer capsules improve merge confidence on real tasks
- cost reports show value per obligation

### Phase 3 - Evidence Memory

Build:

- stronger Claim Ledger
- claim freshness checks
- reusable evidence artifacts
- recurring failure fingerprints with prior-attempt history
- topology outcome memory
- contested and invalidated claim states if not done in Phase 2

Exit criteria:

- future capsules reuse verified artifacts
- stale claims are detected after code changes
- repeated failures are avoided or shortened

### Phase 4 - Advanced Verification

Build selectively:

- MAVEN verifier mode
- targeted mutation testing
- adversarial test generation
- optional model reviewer diversity
- test-gap artifacts

Exit criteria:

- advanced checks find real issues often enough to justify cost
- false-positive rate is measured
- high-risk mode is useful without being default

### Phase 5 - Integrations And Product Shell

Build:

- MCP integration
- desktop UI
- issue tracker intake
- PR creation
- CI status
- remote execution

Only build these after the runtime is clearly better than a terminal/worktree cockpit.

---

## 23. Hard Rules

- Do not make raw transcripts durable memory.
- Do not accept a patch without mapping evidence to obligations.
- Do not treat agent claims as facts without verification.
- Do not default to multi-agent execution when topology is sequential.
- Do not make speculative learning systems MVP dependencies.
- Do not use model confidence as proof.
- Do not hide coordination cost.
- Do not let integrations dictate internal architecture.
- Do not lead with UI before the runtime works.
- Do not promote Work Orders to a first-class primitive until multi-capsule obligation fulfillment proves necessary.
- Do not treat transcript extraction as producing inferior artifacts. Both collection paths must conform to the same schema; downstream components must not distinguish between them.
- Do not begin Phase 1 infrastructure before Phase 0 exit criteria are met.
- Do not implement extended topology types, projection types, or claim states before the MVP baseline is validated on real tasks.

---

## 24. Final Product Definition

Orca is a local proof runtime for coding agents.

It converts a coding goal into obligations, runs agents inside execution capsules, stores patches and evidence as artifacts, verifies whether obligations are satisfied, compiles minimal context for follow-up work, tracks coordination cost, and recommends merge only when the work is evidence-backed.

The differentiator is not that Orca runs more agents.

The differentiator is that Orca makes agent output provable.
