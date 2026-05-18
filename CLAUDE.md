# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...
go test ./...
go test ./internal/schema/...        # single package
go vet ./...
```

No external build tooling. Standard Go 1.22 toolchain is sufficient.

## Architecture

Orca is a local proof runtime for coding agents. The design document is `orca.md` — treat it as the authoritative spec. Every schema type, field name, and status value must match `orca.md` exactly.

### Core data flow

```
User Goal → Goal IR → Obligations → Execution Capsules
  → Patch Artifacts + Evidence Artifacts
  → Verifier Result → Reconciliation → Merge Recommendation or new Obligations
```

### Current build state

- **Phase 0 (falsification spikes):** complete. Artifacts live in `phase0_artifacts/`. Do not modify them; they are the evidence baseline for the Phase 0 exit criteria.
- **Phase 1 (minimal proof runtime):** not yet started. This is where the CLI, artifact store, event log, adapters, and reconciliation loop will be built.
- **Do not begin Phase 1 work without confirming `phase0_artifacts/phase0_final_report.md` recommends `pass`** (it does, as of 2026-05-17).

### Package structure

`internal/schema` — all MVP data types as plain JSON-tagged structs. **No business logic here.** Constructors, validation, and persistence belong in separate packages (not yet created).

Each file maps directly to an `orca.md` section:

| File | Types | Spec section |
|---|---|---|
| `goal.go` | `GoalIR`, `GoalCondition` | §5.1 |
| `obligation.go` | `Obligation` | §5.2 |
| `capsule.go` | `ExecutionCapsule` | §5.3 |
| `context.go` | `ContextProjection`, `HumanSummaryProjection` | §5.4 |
| `patch.go` | `PatchArtifact`, `ProofCarryingPatch` | §5.5 |
| `evidence.go` | `EvidenceArtifact` | §5.6 |
| `claim.go` | `ClaimArtifact` | §5.8 |
| `failure.go` | `FailureFingerprint` | §5.9 |
| `verifier.go` | `VerifierResult` | §10 |
| `sidecar.go` | `AgentSidecarOutput` | §8 |
| `decision.go` | `DecisionRecord` | §5.10 |
| `event.go` | `Event` | §9 |
| `budget.go` | `BudgetRecord` | §12 |
| `snapshot.go` | `StateSnapshot` | — |
| `common.go` | `RiskLevel`, `Topology` enums | — |

### Runtime state layout (target, not yet implemented)

```
.orca/
  config.yaml
  events.log          # JSONL, authoritative history
  state/              # JSON materialized state
  artifacts/          # patches, evidence, claims, contexts, failures, logs
  capsules/CAP-*/
```

### Key invariants from orca.md §23

- Raw transcripts are never durable memory.
- A patch is not accepted without mapping evidence to obligations.
- Agent claims are not facts until verified (`proposed` → `verified` in `ClaimArtifact`).
- Both sidecar output and transcript extraction must produce artifacts conforming to the same schema; downstream code must not distinguish between them.
- Phase 1 infrastructure must not begin before Phase 0 exit criteria are met (they are).

### MVP topology classifier

Three topologies only (§7): `single`, `implementer_reviewer`, `human_gated`. `parallel`, `test_first`, and `investigate_then_implement` are deferred to Phase 2 — do not implement them.

### MVP context projections

Two types only (§5.4): `executor_projection` and `human_summary_projection`. Additional projection types are Phase 2.
