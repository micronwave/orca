// Package eventlog provides the append-only authoritative history of all
// runtime operations. The artifact graph (ArtifactStore) is the materialized
// state; the event log is the ground truth from which it can be replayed.
// orca.md §9.
//
// Components with a direct FileLog dependency:
//   - Write consumers (append lifecycle events): intent_compiler, capsule_runner,
//     reconciler, human_gatekeeper — for transitions not covered by artifact saves,
//     e.g. capsule_started, patch_accepted, merge_applied.
//   - Read-only consumers: budget_controller — reads the event stream to compute
//     token spend and enforce budget limits without appending events directly.
//
// Components without a direct FileLog dependency (obligation_planner,
// verifier_engine, context_compiler) rely on the ArtifactStore concrete
// implementation to emit artifact-creation events on their behalf.
package eventlog
