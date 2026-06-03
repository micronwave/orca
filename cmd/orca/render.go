package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/micronwave/orca/internal/ui"
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

// ─── Live renderer ────────────────────────────────────────────────────────────

type liveRenderer struct {
	mu             sync.Mutex
	out            io.Writer
	state          DashboardState
	currentStep    int
	totalSteps     int
	lastLine       string
	lastLineStep   int // step number of lastLine, used for in-place replacement
	hasHeader      bool
	isInteractive  bool
}

// newLiveRenderer returns a Notifier that writes ANSI-colored lifecycle lines
// to out. It should only be called when out is a TTY.
func newLiveRenderer(out io.Writer) Notifier {
	return &liveRenderer{
		out:           out,
		totalSteps:    4, // Typical flow: Compile, Plan, Run, Verify
		isInteractive: ui.UseColor(out),
	}
}

func (r *liveRenderer) Step(_ context.Context, ev UIEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Apply(ev)

	if !r.hasHeader {
		if ev.GoalID != "" {
			fmt.Fprintf(r.out, "%s %s Goal: %s\n", ui.IconOrca, r.color(ui.OrcaBlue+ui.Bold, "Goal"), r.color(ui.Bold, shortID(ev.GoalID)))
		} else {
			fmt.Fprintf(r.out, "%s %s\n", ui.IconOrca, r.color(ui.OrcaBlue+ui.Bold, "Orca Goal Running"))
		}
		r.hasHeader = true
	}

	step, icon, fallback, done := r.stepInfo(ev)
	msg := summarizedMessage(ev, fallback)
	if step > 0 {
		r.currentStep = step
	}

	status := r.color(ui.Black+ui.Bold, "[Running...]")
	if done {
		status = r.color(ui.Green, "Done")
	}
	if ev.Severity == "error" {
		status = r.color(ui.Red+ui.Bold, "Failed")
	}

	line := fmt.Sprintf("%s %s", icon, msg)
	if step > 0 {
		counter := r.color(ui.Black+ui.Bold, fmt.Sprintf("[%d/%d]", r.currentStep, r.totalSteps))
		line = fmt.Sprintf("%s %s %-30s %s", counter, icon, msg, status)
	}

	if r.isInteractive {
		if step > 0 && r.lastLine != "" && r.currentStep == r.lastLineStep {
			ui.ReplaceLine(r.out, line)
		} else {
			if r.lastLine != "" {
				fmt.Fprintln(r.out)
			}
			fmt.Fprint(r.out, line)
		}
	} else {
		fmt.Fprintln(r.out, line)
	}
	if step > 0 {
		r.lastLine = line
		r.lastLineStep = r.currentStep
	} else {
		r.lastLine = ""
		r.lastLineStep = 0
	}

	// Expand verifier failures so the user sees each one immediately.
	if ev.Kind == EventKindVerifierFailed && ev.Detail != "" {
		fmt.Fprintln(r.out)
		for _, f := range splitDetail(ev.Detail) {
			fmt.Fprintf(r.out, "  %s %s\n", ui.IconCross, r.color(ui.Red, f))
		}
		r.lastLine = "" // Force new line for next event
	}

	// Expand reconcile-blocked failures so the user sees each blocking reason.
	if ev.Kind == EventKindReconcileBlocked && ev.Detail != "" {
		fmt.Fprintln(r.out)
		for _, f := range splitDetail(ev.Detail) {
			fmt.Fprintf(r.out, "  %s %s\n", ui.IconCross, r.color(ui.Red, f))
		}
		r.lastLine = ""
	}

	// Expand follow-up obligation IDs so the user knows what runs next.
	if ev.Kind == EventKindReconcileFollowUp && ev.Detail != "" {
		fmt.Fprintln(r.out)
		for _, id := range strings.Split(ev.Detail, ", ") {
			if id = strings.TrimSpace(id); id != "" {
				fmt.Fprintf(r.out, "  %s follow-up: %s\n", ui.IconStep, r.color(ui.Black+ui.Bold, shortID(id)))
			}
		}
		r.lastLine = ""
	}

	if ev.Kind == EventKindCapsuleWaitingForGate {
		prompt := r.color(ui.Yellow, "ENTER to approve · 'reject' to reject · 'cancel' to abort")
		fmt.Fprintf(r.out, "\n  %s %s\n", r.color(ui.Yellow, ui.IconStep), prompt)
		r.lastLine = ""
	}

	if ev.Kind == EventKindMergeReady {
		fmt.Fprintf(r.out, "\n  %s\n", r.color(ui.Green, ui.IconCheck+" Patch verified and ready to merge."))
		r.lastLine = ""
	}

	if ev.Kind == EventKindMergeApplied {
		fmt.Fprintf(r.out, "\n%s %s\n", r.color(ui.Green, ui.IconCheck), r.color(ui.Green, "Changes applied to working directory."))
		r.lastLine = ""
	}
}

func (r *liveRenderer) stepInfo(ev UIEvent) (step int, icon, fallback string, done bool) {
	switch ev.Kind {
	case EventKindGoalCompiling:
		return 1, ui.IconHammer, "compiling intent", false
	case EventKindGoalPlanning:
		return 2, ui.IconClipboard, "planning", false
	case EventKindCapsuleWaitingForGate:
		return 3, ui.IconRocket, "awaiting projection review", false
	case EventKindCapsuleCreated, EventKindCapsuleRunning:
		return 3, ui.IconRocket, "running capsule", false
	case EventKindCapsuleCompleted:
		return 3, ui.IconRocket, "capsule completed", true
	case EventKindCapsuleFailed:
		return 3, ui.IconCross, "capsule failed", false
	case EventKindVerifierRunning:
		return 4, ui.IconCheck, "verifying patch", false
	case EventKindVerifierPassed:
		return 4, ui.IconCheck, "verification passed", true
	case EventKindVerifierFailed:
		return 4, ui.IconCheck, "verification failed", false
	case EventKindMergeReady:
		return 4, ui.IconCheck, "merge ready", true
	}
	return 0, ui.IconStep, "", false
}

func summarizedMessage(ev UIEvent, fallback string) string {
	msg := strings.TrimSpace(ev.Summary)
	if msg == "" {
		msg = fallback
	}
	if msg == "" {
		msg = string(ev.Kind)
	}

	if ev.GoalID != "" {
		msg = strings.ReplaceAll(msg, ev.GoalID, shortID(ev.GoalID))
	}
	if ev.CapsuleID != "" {
		msg = strings.ReplaceAll(msg, ev.CapsuleID, shortID(ev.CapsuleID))
	}
	if ev.PatchID != "" {
		msg = strings.ReplaceAll(msg, ev.PatchID, shortID(ev.PatchID))
	}
	return msg
}

// color wraps s in the given ANSI code only when the renderer is in
// interactive (TTY + color-enabled) mode. This respects NO_COLOR / TERM=dumb.
func (r *liveRenderer) color(code, s string) string {
	if !r.isInteractive {
		return s
	}
	return code + s + ui.Reset
}

// liveRendererState returns the current DashboardState from a liveRenderer.
// The caller must type-assert the Notifier to *liveRenderer to use this.
func (r *liveRenderer) liveRendererState() DashboardState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}
