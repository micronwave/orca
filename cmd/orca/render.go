package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// shortID returns at most 12 characters of an artifact ID. The full ID is
// available in --verbose and --raw modes. IDs shorter than 12 characters are
// returned unchanged.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// ─── Dashboard state model ────────────────────────────────────────────────────

// DashboardState is the live display model built by applying UIEvent sequences.
// Tests can Apply event sequences and assert on the resulting state without
// constructing a real renderer.
type DashboardState struct {
	GoalID    string
	StartedAt time.Time
	UpdatedAt time.Time

	// Topology is the selected topology name from the topology.selected event.
	Topology string

	// Active capsule state.
	ActiveCapsuleID    string
	ActiveCapsuleState string // "running" | "waiting_gate" | "completed" | "failed"

	// Verifier state from the most recent verifier event.
	VerifyPassed   bool
	VerifyFailed   bool
	VerifyPatchID  string
	VerifyFailures []string // concise failure messages split from Detail

	// MergeReadiness is derived from reconcile/merge events.
	// One of: "ready" | "applied" | "accepted" | "follow_up" | "blocked"
	MergeReadiness string

	// GateWaiting is true while the runtime is blocking on a human gate.
	GateWaiting bool

	// Timeline holds the ordered sequence of lifecycle steps received so far.
	Timeline []timelineEntry
}

// timelineEntry is a compact record of one lifecycle step.
type timelineEntry struct {
	At      time.Time
	Kind    EventKind
	Summary string
	Status  string
}

// Apply updates s from a single UIEvent. It is safe to call from tests.
func (s *DashboardState) Apply(ev UIEvent) {
	ts := ev.At
	if ts.IsZero() {
		ts = time.Now()
	}
	s.UpdatedAt = ts
	s.Timeline = append(s.Timeline, timelineEntry{
		At:      ts,
		Kind:    ev.Kind,
		Summary: ev.Summary,
		Status:  ev.Status,
	})

	switch ev.Kind {
	case EventKindGoalCompiling:
		if s.StartedAt.IsZero() {
			s.StartedAt = ts
		}
	case EventKindGoalPlanning, EventKindSetupReady:
		if ev.GoalID != "" {
			s.GoalID = ev.GoalID
		}
	case EventKindTopologySelected:
		if ev.Fields != nil {
			s.Topology = ev.Fields["topology"]
		}
	case EventKindCapsuleCreated, EventKindCapsuleRunning:
		if ev.CapsuleID != "" {
			s.ActiveCapsuleID = ev.CapsuleID
			s.ActiveCapsuleState = "running"
		}
	case EventKindCapsuleWaitingForGate:
		if ev.CapsuleID != "" {
			s.ActiveCapsuleID = ev.CapsuleID
		}
		s.ActiveCapsuleState = "waiting_gate"
		s.GateWaiting = true
	case EventKindCapsuleCompleted:
		s.ActiveCapsuleState = "completed"
		s.GateWaiting = false
	case EventKindCapsuleFailed:
		s.ActiveCapsuleState = "failed"
		s.GateWaiting = false
	case EventKindVerifierPassed:
		s.VerifyPassed = true
		s.VerifyFailed = false
		s.VerifyPatchID = ev.PatchID
		s.VerifyFailures = nil
	case EventKindVerifierFailed:
		s.VerifyFailed = true
		s.VerifyPassed = false
		s.VerifyPatchID = ev.PatchID
		if ev.Detail != "" {
			s.VerifyFailures = splitDetail(ev.Detail)
		}
	case EventKindMergeReady:
		s.MergeReadiness = "ready"
		s.GateWaiting = false
	case EventKindMergeApplied:
		s.MergeReadiness = "applied"
	case EventKindReconcileAccepted:
		if s.MergeReadiness == "" {
			s.MergeReadiness = "accepted"
		}
	case EventKindReconcileFollowUp:
		s.MergeReadiness = "follow_up"
	case EventKindReconcileBlocked:
		s.MergeReadiness = "blocked"
	}
}

// Elapsed returns the wall-clock time between goal compilation and the last
// received event, rounded to the nearest second.
func (s *DashboardState) Elapsed() time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	end := s.UpdatedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(s.StartedAt).Round(time.Second)
}

// splitDetail splits a semicolon-separated failure string into individual
// messages, trimming whitespace and dropping empty segments.
func splitDetail(detail string) []string {
	parts := strings.Split(detail, "; ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ─── ANSI color helpers ───────────────────────────────────────────────────────
// These constants are used only by liveRenderer and are never written to
// non-TTY outputs.

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// eventColorCode returns the ANSI escape prefix appropriate for ev's kind
// and severity.
func eventColorCode(ev UIEvent) string {
	if ev.Severity == "error" {
		return ansiRed + ansiBold
	}
	switch ev.Kind {
	case EventKindVerifierPassed, EventKindMergeApplied, EventKindReconcileAccepted,
		EventKindMergeReady:
		return ansiGreen
	case EventKindVerifierFailed, EventKindCapsuleFailed, EventKindReconcileBlocked:
		return ansiRed
	case EventKindCapsuleWaitingForGate:
		return ansiYellow
	case EventKindGoalCompiling, EventKindGoalPlanning, EventKindTopologySelected:
		return ansiBold
	default:
		return ansiCyan
	}
}

// ─── Live renderer ────────────────────────────────────────────────────────────

// liveRenderer is a TTY-aware Notifier that adds ANSI color and concise
// structure to lifecycle events. It is isolated from all core pipeline packages:
// it depends only on UIEvent. The renderer updates an internal DashboardState
// on every Step call.
//
// The renderer does not redraw lines (no cursor movement). Each event produces
// one or more formatted lines, which is sufficient for a compact live display
// without requiring an external TUI library.
type liveRenderer struct {
	mu    sync.Mutex
	out   io.Writer
	state DashboardState
}

// newLiveRenderer returns a Notifier that writes ANSI-colored lifecycle lines
// to out. It should only be called when out is a TTY.
func newLiveRenderer(out io.Writer) Notifier {
	return &liveRenderer{out: out}
}

func (r *liveRenderer) Step(_ context.Context, ev UIEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Apply(ev)

	msg := liveEventMessage(ev)
	code := eventColorCode(ev)
	fmt.Fprintf(r.out, "%s[orca]%s %s\n", code, ansiReset, msg)

	// Expand verifier failures so the user sees each one immediately.
	if ev.Kind == EventKindVerifierFailed && ev.Detail != "" {
		for _, f := range splitDetail(ev.Detail) {
			fmt.Fprintf(r.out, "  %s✗ %s%s\n", ansiRed, f, ansiReset)
		}
	}
	// Gate waiting: show inline action hints.
	if ev.Kind == EventKindCapsuleWaitingForGate {
		fmt.Fprintf(r.out, "  %s→ ENTER to approve · 'reject' to reject · 'cancel' to abort%s\n",
			ansiYellow, ansiReset)
	}
	// Merge ready: confirmation line.
	if ev.Kind == EventKindMergeReady {
		fmt.Fprintf(r.out, "  %s✓ Patch verified and ready to merge.%s\n", ansiGreen, ansiReset)
	}
	// Merge applied: applied confirmation.
	if ev.Kind == EventKindMergeApplied {
		fmt.Fprintf(r.out, "  %s✓ Changes applied to working directory.%s\n", ansiGreen, ansiReset)
	}
}

func liveEventMessage(ev UIEvent) string {
	switch ev.Kind {
	case EventKindGoalCompiling:
		return fallbackSummary(ev, "compiling intent")
	case EventKindGoalPlanning:
		if ev.GoalID != "" {
			return fmt.Sprintf("goal %s: planning", shortID(ev.GoalID))
		}
	case EventKindSetupStarted, EventKindSetupReady, EventKindSetupBlocked:
		return fallbackSummary(ev, string(ev.Kind))
	case EventKindTopologySelected:
		topology := ""
		if ev.Fields != nil {
			topology = ev.Fields["topology"]
		}
		if ev.GoalID != "" && topology != "" {
			return fmt.Sprintf("goal %s: selected topology %s", shortID(ev.GoalID), topology)
		}
		if topology != "" {
			return "selected topology " + topology
		}
	case EventKindCapsuleCreated:
		if ev.CapsuleID != "" {
			return fmt.Sprintf("capsule %s: compiling projections", shortID(ev.CapsuleID))
		}
	case EventKindCapsuleWaitingForGate:
		if ev.CapsuleID != "" {
			return fmt.Sprintf("capsule %s: awaiting projection review", shortID(ev.CapsuleID))
		}
	case EventKindCapsuleRunning:
		if ev.CapsuleID != "" {
			return fmt.Sprintf("capsule %s: running", shortID(ev.CapsuleID))
		}
	case EventKindCapsuleCompleted:
		if ev.CapsuleID != "" {
			return fmt.Sprintf("capsule %s: completed", shortID(ev.CapsuleID))
		}
	case EventKindCapsuleFailed:
		if ev.CapsuleID != "" {
			return fmt.Sprintf("capsule %s: failed", shortID(ev.CapsuleID))
		}
	case EventKindVerifierRunning:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: verifying", shortID(ev.PatchID))
		}
	case EventKindVerifierPassed:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: verification passed", shortID(ev.PatchID))
		}
	case EventKindVerifierFailed:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: verification failed", shortID(ev.PatchID))
		}
	case EventKindReconcileRunning:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: reconciling proof", shortID(ev.PatchID))
		}
	case EventKindReconcileAccepted:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: accepted", shortID(ev.PatchID))
		}
	case EventKindReconcileFollowUp:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: follow-up required", shortID(ev.PatchID))
		}
	case EventKindReconcileBlocked:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: blocked", shortID(ev.PatchID))
		}
	case EventKindMergeReady:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: merge ready", shortID(ev.PatchID))
		}
	case EventKindMergeApplied:
		if ev.PatchID != "" {
			return fmt.Sprintf("patch %s: applied to working directory", shortID(ev.PatchID))
		}
	case EventKindPRCreated:
		return fallbackSummary(ev, "pr created")
	}
	return fallbackSummary(ev, string(ev.Kind))
}

func fallbackSummary(ev UIEvent, fallback string) string {
	if ev.Summary != "" {
		return ev.Summary
	}
	return fallback
}

// liveRendererState returns the current DashboardState from a liveRenderer.
// The caller must type-assert the Notifier to *liveRenderer to use this.
func (r *liveRenderer) liveRendererState() DashboardState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}
