package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// ─── shortID ─────────────────────────────────────────────────────────────────

func TestShortID_LongID_Truncated(t *testing.T) {
	id := "CAP-1c23f1d8-extra-long-suffix"
	got := shortID(id)
	if len(got) != 12 {
		t.Errorf("shortID(%q) len = %d, want 12", id, len(got))
	}
	if got != id[:12] {
		t.Errorf("shortID(%q) = %q, want %q", id, got, id[:12])
	}
}

func TestShortID_ShortID_Unchanged(t *testing.T) {
	// All IDs in this list are at most 12 characters; shortID must return them unchanged.
	for _, id := range []string{"", "CAP-1", "PATCH-AB", "G-1", "OB-123456789"} {
		got := shortID(id)
		if got != id {
			t.Errorf("shortID(%q) = %q, want unchanged", id, got)
		}
	}
}

func TestShortID_ExactlyTwelve_Unchanged(t *testing.T) {
	id := "CAP-12345678" // exactly 12 chars
	got := shortID(id)
	if got != id {
		t.Errorf("shortID(%q) = %q, want unchanged", id, got)
	}
}

// ─── DashboardState.Apply ────────────────────────────────────────────────────

func TestDashboardState_Apply_SetsGoalID(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindGoalPlanning, GoalID: "G-99"})
	if s.GoalID != "G-99" {
		t.Errorf("GoalID = %q, want G-99", s.GoalID)
	}
}

func TestDashboardState_Apply_SetsTopology(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{
		Kind:   EventKindTopologySelected,
		Fields: map[string]string{"topology": "implementer_reviewer"},
	})
	if s.Topology != "implementer_reviewer" {
		t.Errorf("Topology = %q, want implementer_reviewer", s.Topology)
	}
}

func TestDashboardState_Apply_TopologySelectedWithNoFields_Ignored(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindTopologySelected}) // no Fields
	if s.Topology != "" {
		t.Errorf("Topology = %q, want empty when Fields is nil", s.Topology)
	}
}

func TestDashboardState_Apply_CapsuleStateProgression(t *testing.T) {
	var s DashboardState

	s.Apply(UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: "CAP-A"})
	if s.ActiveCapsuleID != "CAP-A" || s.ActiveCapsuleState != "running" {
		t.Errorf("after running: id=%q state=%q, want CAP-A/running", s.ActiveCapsuleID, s.ActiveCapsuleState)
	}

	s.Apply(UIEvent{Kind: EventKindCapsuleWaitingForGate, CapsuleID: "CAP-A"})
	if s.ActiveCapsuleState != "waiting_gate" || !s.GateWaiting {
		t.Errorf("after waiting_gate: state=%q gate=%v, want waiting_gate/true", s.ActiveCapsuleState, s.GateWaiting)
	}

	s.Apply(UIEvent{Kind: EventKindCapsuleCompleted, CapsuleID: "CAP-A"})
	if s.ActiveCapsuleState != "completed" || s.GateWaiting {
		t.Errorf("after completed: state=%q gate=%v, want completed/false", s.ActiveCapsuleState, s.GateWaiting)
	}
}

func TestDashboardState_Apply_GateWaiting_ClearedOnFailed(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindCapsuleWaitingForGate, CapsuleID: "CAP-A"})
	s.Apply(UIEvent{Kind: EventKindCapsuleFailed, CapsuleID: "CAP-A"})
	if s.GateWaiting {
		t.Error("GateWaiting should be false after capsule failed")
	}
	if s.ActiveCapsuleState != "failed" {
		t.Errorf("state = %q, want failed", s.ActiveCapsuleState)
	}
}

func TestDashboardState_Apply_VerifierPass(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindVerifierPassed, PatchID: "PATCH-1"})
	if !s.VerifyPassed || s.VerifyFailed {
		t.Errorf("VerifyPassed=%v VerifyFailed=%v, want true/false", s.VerifyPassed, s.VerifyFailed)
	}
	if s.VerifyPatchID != "PATCH-1" {
		t.Errorf("VerifyPatchID = %q, want PATCH-1", s.VerifyPatchID)
	}
}

func TestDashboardState_Apply_VerifierFail_SplitsDetail(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{
		Kind:   EventKindVerifierFailed,
		Detail: "go test ./...: exit 1; go vet ./...: exit 1",
	})
	if !s.VerifyFailed {
		t.Error("VerifyFailed should be true")
	}
	if len(s.VerifyFailures) != 2 {
		t.Errorf("VerifyFailures = %v, want 2 entries", s.VerifyFailures)
	}
	if s.VerifyFailures[0] != "go test ./...: exit 1" {
		t.Errorf("VerifyFailures[0] = %q, want gate failure text", s.VerifyFailures[0])
	}
}

func TestDashboardState_Apply_MergeReadiness_Transitions(t *testing.T) {
	var s DashboardState

	s.Apply(UIEvent{Kind: EventKindMergeReady, PatchID: "PATCH-1"})
	if s.MergeReadiness != "ready" {
		t.Errorf("after merge.ready: %q, want ready", s.MergeReadiness)
	}

	s.Apply(UIEvent{Kind: EventKindMergeApplied, PatchID: "PATCH-1"})
	if s.MergeReadiness != "applied" {
		t.Errorf("after merge.applied: %q, want applied", s.MergeReadiness)
	}
}

func TestDashboardState_Apply_ReconcileFollowUp(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindReconcileFollowUp, PatchID: "PATCH-2", Status: "follow_up"})
	if s.MergeReadiness != "follow_up" {
		t.Errorf("MergeReadiness = %q, want follow_up", s.MergeReadiness)
	}
}

func TestDashboardState_Apply_ReconcileBlocked(t *testing.T) {
	var s DashboardState
	s.Apply(UIEvent{Kind: EventKindReconcileBlocked, PatchID: "PATCH-3", Status: "blocked"})
	if s.MergeReadiness != "blocked" {
		t.Errorf("MergeReadiness = %q, want blocked", s.MergeReadiness)
	}
}

func TestDashboardState_Apply_Timeline_OrderPreserved(t *testing.T) {
	var s DashboardState
	kinds := []EventKind{
		EventKindGoalCompiling,
		EventKindGoalPlanning,
		EventKindTopologySelected,
		EventKindCapsuleRunning,
		EventKindVerifierPassed,
		EventKindMergeReady,
	}
	for _, k := range kinds {
		s.Apply(UIEvent{Kind: k, Summary: string(k)})
	}
	if len(s.Timeline) != len(kinds) {
		t.Fatalf("Timeline len = %d, want %d", len(s.Timeline), len(kinds))
	}
	for i, k := range kinds {
		if s.Timeline[i].Kind != k {
			t.Errorf("Timeline[%d].Kind = %s, want %s", i, s.Timeline[i].Kind, k)
		}
	}
}

func TestDashboardState_Apply_StartsAt_SetOnGoalCompiling(t *testing.T) {
	var s DashboardState
	before := time.Now()
	s.Apply(UIEvent{Kind: EventKindGoalCompiling})
	if s.StartedAt.Before(before) || s.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want approximately now", s.StartedAt)
	}
	// Second compiling event must not overwrite StartedAt.
	first := s.StartedAt
	s.Apply(UIEvent{Kind: EventKindGoalCompiling})
	if s.StartedAt != first {
		t.Errorf("StartedAt changed on second compiling event: was %v, now %v", first, s.StartedAt)
	}
}

func TestDashboardState_Elapsed_ReturnsZeroBeforeStart(t *testing.T) {
	var s DashboardState
	if s.Elapsed() != 0 {
		t.Errorf("Elapsed before start = %v, want 0", s.Elapsed())
	}
}

func TestDashboardState_Elapsed_PositiveAfterStart(t *testing.T) {
	var s DashboardState
	now := time.Now()
	s.Apply(UIEvent{Kind: EventKindGoalCompiling, At: now})
	s.Apply(UIEvent{Kind: EventKindGoalPlanning, At: now.Add(3 * time.Second)})
	elapsed := s.Elapsed()
	if elapsed < 3*time.Second || elapsed > 4*time.Second {
		t.Errorf("Elapsed = %v, want ~3s", elapsed)
	}
}

// ─── splitDetail ─────────────────────────────────────────────────────────────

func TestSplitDetail_SplitsOnSemicolonSpace(t *testing.T) {
	got := splitDetail("a; b; c")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitDetail = %v, want [a b c]", got)
	}
}

func TestSplitDetail_DropsEmptySegments(t *testing.T) {
	got := splitDetail("; ; x; ; ")
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("splitDetail = %v, want [x]", got)
	}
}

func TestSplitDetail_EmptyInput_ReturnsNil(t *testing.T) {
	got := splitDetail("")
	if len(got) != 0 {
		t.Errorf("splitDetail(\"\") = %v, want empty", got)
	}
}

// ─── Non-TTY output: no control sequences ────────────────────────────────────

func TestPlainNotifier_NoANSISequences(t *testing.T) {
	var buf bytes.Buffer
	n := newPlainNotifier(&buf, false)
	events := []UIEvent{
		{Kind: EventKindGoalCompiling, Summary: "compiling intent"},
		{Kind: EventKindVerifierFailed, Summary: "failed", Detail: "exit 1", Severity: "error"},
		{Kind: EventKindMergeReady, Summary: "ready"},
	}
	for _, ev := range events {
		n.Step(context.Background(), ev)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("plain notifier output contains ANSI escape sequences:\n%s", buf.String())
	}
}

func TestJSONNotifier_NoANSISequences(t *testing.T) {
	var buf bytes.Buffer
	n := newJSONNotifier(&buf)
	events := []UIEvent{
		{Kind: EventKindCapsuleRunning, CapsuleID: "CAP-1", Summary: "running"},
		{Kind: EventKindVerifierFailed, Summary: "failed", Severity: "error"},
	}
	for _, ev := range events {
		n.Step(context.Background(), ev)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("JSON notifier output contains ANSI escape sequences:\n%s", buf.String())
	}
}

// ─── Live renderer ────────────────────────────────────────────────────────────

func TestLiveRenderer_NonTTY_NoANSISequences(t *testing.T) {
	// newLiveRenderer with a bytes.Buffer (non-TTY) must not emit ANSI escapes,
	// respecting the NO_COLOR convention even when the renderer is explicitly created.
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{Kind: EventKindGoalCompiling, Summary: "compiling intent"})
	got := buf.String()
	if !strings.Contains(got, "compiling intent") {
		t.Errorf("live renderer output missing summary: %s", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("live renderer output to non-TTY buffer must not contain ANSI escapes:\n%s", got)
	}
}

func TestLiveRenderer_Interactive_WritesColoredLines(t *testing.T) {
	// Force isInteractive=true to cover the TTY color path without a real TTY.
	var buf bytes.Buffer
	r := &liveRenderer{out: &buf, isInteractive: true, totalSteps: 4}
	r.Step(context.Background(), UIEvent{Kind: EventKindGoalCompiling, Summary: "compiling intent"})
	got := buf.String()
	if !strings.Contains(got, "compiling intent") {
		t.Errorf("interactive renderer output missing summary: %s", got)
	}
	if !strings.Contains(got, "\x1b") {
		t.Errorf("interactive renderer output has no ANSI escapes (expected color): %s", got)
	}
}

func TestLiveRenderer_VerifierFailure_PrintsEachFailure(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindVerifierFailed,
		Summary: "verification failed",
		Detail:  "go test ./...: exit 1; go vet ./...: exit 2",
	})
	got := buf.String()
	for _, want := range []string{"go test ./...: exit 1", "go vet ./...: exit 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("live renderer verifier failure missing %q:\n%s", want, got)
		}
	}
}

func TestLiveRenderer_ReconcileBlocked_PrintsEachReason(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindReconcileBlocked,
		Summary: "reconcile blocked",
		Detail:  "missing evidence for OBL-1; patch rejected by reviewer",
	})
	got := buf.String()
	for _, want := range []string{"missing evidence for OBL-1", "patch rejected by reviewer"} {
		if !strings.Contains(got, want) {
			t.Errorf("live renderer reconcile blocked missing %q:\n%s", want, got)
		}
	}
}

func TestLiveRenderer_ReconcileFollowUp_PrintsObligationIDs(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindReconcileFollowUp,
		Summary: "follow-up obligations created",
		Detail:  "OBL-0000000000000004, OBL-0000000000000005",
	})
	got := buf.String()
	for _, want := range []string{"follow-up:", "OBL-0000"} {
		if !strings.Contains(got, want) {
			t.Errorf("live renderer reconcile follow-up missing %q:\n%s", want, got)
		}
	}
}

func TestLiveRenderer_GateWaiting_ShowsActionHint(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{
		Kind:      EventKindCapsuleWaitingForGate,
		CapsuleID: "CAP-1",
		Summary:   "awaiting gate",
	})
	got := buf.String()
	// Must tell user how to approve or reject.
	if !strings.Contains(got, "approve") || !strings.Contains(got, "reject") {
		t.Errorf("live renderer gate hint missing approve/reject:\n%s", got)
	}
}

func TestLiveRenderer_MergeReady_ShowsConfirmation(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{Kind: EventKindMergeReady, PatchID: "PATCH-1", Summary: "merge ready"})
	got := buf.String()
	if !strings.Contains(got, "verified") || !strings.Contains(got, "merge") {
		t.Errorf("live renderer merge-ready confirmation missing expected text:\n%s", got)
	}
}

func TestLiveRenderer_MaintainsState(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)

	n.Step(context.Background(), UIEvent{Kind: EventKindGoalPlanning, GoalID: "G-5"})
	n.Step(context.Background(), UIEvent{
		Kind:   EventKindTopologySelected,
		Fields: map[string]string{"topology": "single"},
	})
	n.Step(context.Background(), UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: "CAP-XYZ"})
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindVerifierFailed,
		PatchID: "PATCH-9",
		Detail:  "gate failed: exit 1",
	})

	lr := n.(*liveRenderer)
	state := lr.liveRendererState()

	if state.GoalID != "G-5" {
		t.Errorf("state.GoalID = %q, want G-5", state.GoalID)
	}
	if state.Topology != "single" {
		t.Errorf("state.Topology = %q, want single", state.Topology)
	}
	if state.ActiveCapsuleID != "CAP-XYZ" {
		t.Errorf("state.ActiveCapsuleID = %q, want CAP-XYZ", state.ActiveCapsuleID)
	}
	if !state.VerifyFailed {
		t.Error("state.VerifyFailed should be true")
	}
	if len(state.Timeline) != 4 {
		t.Errorf("state.Timeline len = %d, want 4", len(state.Timeline))
	}
}

func TestLiveRenderer_FallbackToKind_WhenSummaryEmpty(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	n.Step(context.Background(), UIEvent{Kind: EventKindTopologySelected}) // no Summary
	if !strings.Contains(buf.String(), "topology.selected") {
		t.Errorf("live renderer should fall back to kind string when summary is empty:\n%s", buf.String())
	}
}

func TestLiveRenderer_HidesLongArtifactIDsByDefault(t *testing.T) {
	longCapsuleID := "CAP-47b9a3e9-dead-beef-cafe-012345678901"
	longPatchID := "PATCH-1c23f1d8-e0c5-4b2a-9d33-f7a8b9c0d1e2"
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)

	n.Step(context.Background(), UIEvent{
		Kind:      EventKindCapsuleRunning,
		CapsuleID: longCapsuleID,
		Summary:   "capsule " + longCapsuleID + ": running agent",
	})
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindVerifierFailed,
		PatchID: longPatchID,
		Summary: "patch " + longPatchID + ": verification failed",
	})

	got := buf.String()
	for _, absent := range []string{longCapsuleID, longPatchID} {
		if strings.Contains(got, absent) {
			t.Fatalf("live renderer exposed full artifact ID %q:\n%s", absent, got)
		}
	}
	for _, want := range []string{shortID(longCapsuleID), shortID(longPatchID)} {
		if !strings.Contains(got, want) {
			t.Fatalf("live renderer missing short artifact ID %q:\n%s", want, got)
		}
	}
}

// ─── Default summary hides raw paths and long IDs ────────────────────────────

// TestShortID_HidesLongArtifactIDs verifies that shortID produces output
// suitable for the default summary view — raw UUIDs and long hex identifiers
// are truncated so they don't dominate the terminal line.
func TestShortID_HidesLongArtifactIDs(t *testing.T) {
	cases := []struct {
		id      string
		wantLen int
	}{
		{"PATCH-1c23f1d8-e0c5-4b2a-9d33-f7a8b9c0d1e2", 12},
		{"CAP-47b9a3e9-dead-beef-cafe-012345678901", 12},
		{"EV-short", 8}, // short ID: unchanged
	}
	for _, tc := range cases {
		got := shortID(tc.id)
		if len(got) != tc.wantLen {
			t.Errorf("shortID(%q) len = %d, want %d (got %q)", tc.id, len(got), tc.wantLen, got)
		}
		// Must not contain the full ID if it was long.
		if tc.wantLen == 12 && strings.Contains(got, tc.id[12:]) {
			t.Errorf("shortID(%q) still contains suffix: %q", tc.id, got)
		}
	}
}

// ─── Verifier failure summary: command, exit code, message ───────────────────

// TestLiveRenderer_VerifierSummaryIncludesCommandAndExitCode verifies that the
// live renderer output for a failed gate shows the gate command and exit code
// as separate entries — this is how the control loop formats BlockingFailures.
func TestLiveRenderer_VerifierSummaryIncludesCommandAndExitCode(t *testing.T) {
	var buf bytes.Buffer
	n := newLiveRenderer(&buf)
	// The verifier formats BlockingFailures as "gate '<cmd>': exit <N>: <output>".
	n.Step(context.Background(), UIEvent{
		Kind:    EventKindVerifierFailed,
		Summary: "patch PATCH-1: verification failed",
		Detail:  "gate 'go test ./...': exit 1: FAIL github.com/acme/app 0.123s; gate 'go vet ./...': exit 1",
	})
	got := buf.String()
	for _, want := range []string{
		"go test ./...",
		"exit 1",
		"go vet ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verifier failure output missing %q:\n%s", want, got)
		}
	}
}
