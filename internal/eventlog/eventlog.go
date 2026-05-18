// Package eventlog defines the EventLog interface — the append-only authoritative
// history of all runtime operations. The artifact graph (ArtifactStore) is the
// materialized state; the event log is the ground truth from which it can be
// replayed. orca.md §9.
//
// Components with a direct EventLog dependency:
//   - Write consumers (append lifecycle events): intent_compiler, capsule_runner,
//     reconciler, human_gatekeeper — for transitions not covered by artifact saves,
//     e.g. capsule_started, patch_accepted, merge_applied.
//   - Read-only consumers: budget_controller — reads the event stream to compute
//     token spend and enforce budget limits without appending events directly.
//
// Components without a direct EventLog dependency (obligation_planner,
// verifier_engine, context_compiler) rely on the ArtifactStore concrete
// implementation to emit artifact-creation events on their behalf.
package eventlog

import (
	"context"
	"io"

	"github.com/micronwave/orca/internal/schema"
)

// EventLog is the append-only authoritative event history.
// All Append calls must be durable before returning; no write-behind buffering.
type EventLog interface {
	io.Closer

	// Append adds one event to the log. The implementation assigns SequenceNum;
	// callers must leave SequenceNum at zero. The returned Event is the durable
	// record, including the assigned EventID, SequenceNum, and CreatedAt.
	Append(ctx context.Context, e schema.Event) (schema.Event, error)

	// ReadAfter returns up to limit events with SequenceNum > afterSeq,
	// ordered ascending. Pass afterSeq=0 to read from the beginning.
	ReadAfter(ctx context.Context, afterSeq int64, limit int) ([]schema.Event, error)

	// ReadByType returns up to limit events of the given type with SequenceNum > afterSeq.
	ReadByType(ctx context.Context, eventType schema.EventType, afterSeq int64, limit int) ([]schema.Event, error)

	// ReadForGoal returns up to limit events for goalID with SequenceNum > afterSeq.
	ReadForGoal(ctx context.Context, goalID string, afterSeq int64, limit int) ([]schema.Event, error)
}
