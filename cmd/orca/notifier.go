package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// EventKind identifies the lifecycle stage being reported.
type EventKind string

const (
	// Setup lifecycle.
	EventKindSetupStarted EventKind = "setup.started"
	EventKindSetupReady   EventKind = "setup.ready"
	EventKindSetupBlocked EventKind = "setup.blocked"

	// Goal compilation.
	EventKindGoalCompiling EventKind = "goal.compiling"
	EventKindGoalPlanning  EventKind = "goal.planning"

	// Topology selection.
	EventKindTopologySelected EventKind = "topology.selected"

	// Capsule lifecycle.
	EventKindCapsuleCreated        EventKind = "capsule.created"
	EventKindCapsuleWaitingForGate EventKind = "capsule.waiting_for_gate"
	EventKindCapsuleRunning        EventKind = "capsule.running"
	EventKindCapsuleCompleted      EventKind = "capsule.completed"
	EventKindCapsuleFailed         EventKind = "capsule.failed"

	// Verifier.
	EventKindVerifierRunning EventKind = "verifier.running"
	EventKindVerifierPassed  EventKind = "verifier.passed"
	EventKindVerifierFailed  EventKind = "verifier.failed"

	// Reconciliation.
	EventKindReconcileRunning  EventKind = "reconcile.running"
	EventKindReconcileAccepted EventKind = "reconcile.accepted"
	EventKindReconcileFollowUp EventKind = "reconcile.follow_up"
	EventKindReconcileBlocked  EventKind = "reconcile.blocked"

	// Merge and PR.
	EventKindMergeReady   EventKind = "merge.ready"
	EventKindMergeApplied EventKind = "merge.applied"
	EventKindPRCreated    EventKind = "pr.created"
)

// UIEvent carries lifecycle information from the runtime to the presentation layer.
type UIEvent struct {
	Kind      EventKind         `json:"kind"`
	GoalID    string            `json:"goal_id,omitempty"`
	CapsuleID string            `json:"capsule_id,omitempty"`
	PatchID   string            `json:"patch_id,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Status    string            `json:"status,omitempty"`
	Severity  string            `json:"severity,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	At        time.Time         `json:"at"`
}

// Notifier is the presentation boundary for lifecycle events emitted by the runtime.
type Notifier interface {
	Step(ctx context.Context, event UIEvent)
}

// noopNotifier discards all events. Zero value is usable.
type noopNotifier struct{}

func (noopNotifier) Step(_ context.Context, _ UIEvent) {}

// plainNotifier writes human-readable progress lines to out.
// When verbose is true, the Detail field is appended if non-empty.
// Output format matches the previous [orca] stderr lines exactly.
type plainNotifier struct {
	out     io.Writer
	verbose bool
}

func newPlainNotifier(out io.Writer, verbose bool) Notifier {
	return &plainNotifier{out: out, verbose: verbose}
}

func (n *plainNotifier) Step(_ context.Context, ev UIEvent) {
	msg := ev.Summary
	if msg == "" {
		msg = string(ev.Kind)
	}
	if n.verbose && ev.Detail != "" {
		fmt.Fprintf(n.out, "[orca] %s (%s)\n", msg, ev.Detail)
	} else {
		fmt.Fprintf(n.out, "[orca] %s\n", msg)
	}
}

// jsonNotifier writes newline-delimited JSON event objects to out.
type jsonNotifier struct {
	out io.Writer
}

func newJSONNotifier(out io.Writer) Notifier {
	return &jsonNotifier{out: out}
}

func (n *jsonNotifier) Step(_ context.Context, ev UIEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintln(n.out, string(data))
}

// recorderNotifier appends events in call order. Used in tests.
type recorderNotifier struct {
	events []UIEvent
}

func (r *recorderNotifier) Step(_ context.Context, ev UIEvent) {
	r.events = append(r.events, ev)
}
