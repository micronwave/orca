package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
)

func TestNoopNotifier_DoesNotPanic(t *testing.T) {
	n := noopNotifier{}
	// All of these must not panic, regardless of input.
	n.Step(context.Background(), UIEvent{Kind: EventKindGoalCompiling, Summary: "compiling intent"})
	n.Step(context.Background(), UIEvent{})
	n.Step(context.Background(), UIEvent{Kind: EventKindCapsuleRunning, CapsuleID: "CAP-1"})
	n.Step(context.Background(), UIEvent{Kind: EventKindMergeApplied, PatchID: "PATCH-1", Summary: "applied"})
}

func TestNoopNotifier_DoesNotRequireTerminalState(t *testing.T) {
	// An empty context and event must not panic — no terminal I/O.
	n := noopNotifier{}
	n.Step(context.Background(), UIEvent{})
}

func TestPlainNotifier_OutputContainsLifecycleInfo(t *testing.T) {
	var buf bytes.Buffer
	n := newPlainNotifier(&buf, false)

	events := []UIEvent{
		{Kind: EventKindGoalCompiling, Summary: "compiling intent"},
		{Kind: EventKindGoalPlanning, GoalID: "G-1", Summary: "goal G-1: planning"},
		{Kind: EventKindCapsuleRunning, CapsuleID: "CAP-1", Summary: "capsule CAP-1: running agent"},
		{Kind: EventKindVerifierRunning, PatchID: "PATCH-1", Summary: "patch PATCH-1: verifying"},
		{Kind: EventKindMergeApplied, PatchID: "PATCH-1", Summary: "patch PATCH-1: applied to working directory"},
	}
	for _, ev := range events {
		n.Step(context.Background(), ev)
	}

	got := buf.String()
	for _, want := range []string{
		"[orca] compiling intent",
		"[orca] goal G-1: planning",
		"[orca] capsule CAP-1: running agent",
		"[orca] patch PATCH-1: verifying",
		"[orca] patch PATCH-1: applied to working directory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain notifier output missing %q:\n%s", want, got)
		}
	}
}

func TestPlainNotifier_Verbose_AppendsDetail(t *testing.T) {
	var buf bytes.Buffer
	n := newPlainNotifier(&buf, true)
	n.Step(context.Background(), UIEvent{Kind: EventKindCapsuleRunning, Summary: "capsule running", Detail: "role=executor"})
	got := buf.String()
	if !strings.Contains(got, "(role=executor)") {
		t.Errorf("verbose output missing detail: %s", got)
	}
}

func TestPlainNotifier_NonVerbose_OmitsDetail(t *testing.T) {
	var buf bytes.Buffer
	n := newPlainNotifier(&buf, false)
	n.Step(context.Background(), UIEvent{Kind: EventKindCapsuleRunning, Summary: "capsule running", Detail: "role=executor"})
	got := buf.String()
	if strings.Contains(got, "role=executor") {
		t.Errorf("non-verbose output contains detail field: %s", got)
	}
}

func TestPlainNotifier_FallsBackToKind_WhenSummaryEmpty(t *testing.T) {
	var buf bytes.Buffer
	n := newPlainNotifier(&buf, false)
	n.Step(context.Background(), UIEvent{Kind: EventKindTopologySelected})
	got := buf.String()
	if !strings.Contains(got, "[orca] topology.selected") {
		t.Errorf("output = %q, want kind as fallback summary", got)
	}
}

func TestJSONNotifier_WritesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	n := newJSONNotifier(&buf)
	n.Step(context.Background(), UIEvent{
		Kind:      EventKindCapsuleRunning,
		CapsuleID: "CAP-1",
		GoalID:    "G-1",
		Summary:   "capsule CAP-1: running agent",
	})

	line := strings.TrimSpace(buf.String())
	var ev UIEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("JSON notifier output is not valid JSON: %v\noutput: %s", err, line)
	}
	if ev.Kind != EventKindCapsuleRunning {
		t.Errorf("Kind = %q, want capsule.running", ev.Kind)
	}
	if ev.CapsuleID != "CAP-1" {
		t.Errorf("CapsuleID = %q, want CAP-1", ev.CapsuleID)
	}
	if ev.At.IsZero() {
		t.Error("At timestamp is zero, want non-zero")
	}
}

func TestJSONNotifier_OmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	n := newJSONNotifier(&buf)
	n.Step(context.Background(), UIEvent{Kind: EventKindGoalCompiling, Summary: "compiling intent"})
	line := strings.TrimSpace(buf.String())
	// Empty optional fields must be omitted from JSON.
	for _, absent := range []string{`"capsule_id"`, `"patch_id"`, `"detail"`} {
		if strings.Contains(line, absent) {
			t.Errorf("JSON output contains empty field %s: %s", absent, line)
		}
	}
}

func TestRecorderNotifier_PreservesEventOrder(t *testing.T) {
	rec := &recorderNotifier{}
	kinds := []EventKind{
		EventKindGoalCompiling,
		EventKindGoalPlanning,
		EventKindTopologySelected,
		EventKindCapsuleCreated,
		EventKindCapsuleRunning,
		EventKindVerifierRunning,
		EventKindReconcileRunning,
		EventKindMergeApplied,
	}
	for _, k := range kinds {
		rec.Step(context.Background(), UIEvent{Kind: k})
	}
	if len(rec.events) != len(kinds) {
		t.Fatalf("recorded %d events, want %d", len(rec.events), len(kinds))
	}
	for i, want := range kinds {
		if rec.events[i].Kind != want {
			t.Errorf("event[%d].Kind = %s, want %s", i, rec.events[i].Kind, want)
		}
	}
}

func TestVerifierUIEvent_UsesPassedAndFailedOutcomes(t *testing.T) {
	passed := verifierUIEvent("G-1", &schema.VerifierResult{
		PatchID:           "PATCH-1",
		RecommendedAction: schema.ActionAccept,
		ObligationResults: []schema.ObligationVerdict{
			{ObligationID: "OB-1", Verdict: schema.VerdictSatisfied, EvidenceIDs: []string{"EV-1"}},
		},
	})
	if passed.Kind != EventKindVerifierPassed {
		t.Fatalf("passed.Kind = %s, want %s", passed.Kind, EventKindVerifierPassed)
	}
	if passed.Status != "passed" {
		t.Fatalf("passed.Status = %q, want passed", passed.Status)
	}

	failed := verifierUIEvent("G-1", &schema.VerifierResult{
		PatchID:           "PATCH-2",
		RecommendedAction: schema.ActionRetry,
		BlockingFailures:  []string{"OB-1 failed"},
		ObligationResults: []schema.ObligationVerdict{
			{ObligationID: "OB-1", Verdict: schema.VerdictFailed},
		},
	})
	if failed.Kind != EventKindVerifierFailed {
		t.Fatalf("failed.Kind = %s, want %s", failed.Kind, EventKindVerifierFailed)
	}
	if failed.Status != "failed" || failed.Severity != "error" {
		t.Fatalf("failed status/severity = %q/%q, want failed/error", failed.Status, failed.Severity)
	}
}

func TestReconcileUIEvent_UsesDurableOutcomeKinds(t *testing.T) {
	accepted := reconcileUIEvent("G-1", "PATCH-1", reconciler.ReconcileResult{
		PatchAccepted: true,
		MergeReady:    true,
		DecisionID:    "DEC-1",
	})
	if accepted.Kind != EventKindReconcileAccepted {
		t.Fatalf("accepted.Kind = %s, want %s", accepted.Kind, EventKindReconcileAccepted)
	}
	if accepted.Fields["decision_id"] != "DEC-1" {
		t.Fatalf("accepted decision_id field = %q, want DEC-1", accepted.Fields["decision_id"])
	}

	followUp := reconcileUIEvent("G-1", "PATCH-2", reconciler.ReconcileResult{
		FollowUpObligationIDs: []string{"OB-2"},
		BlockingReason:        "needs retry",
	})
	if followUp.Kind != EventKindReconcileFollowUp {
		t.Fatalf("followUp.Kind = %s, want %s", followUp.Kind, EventKindReconcileFollowUp)
	}
	if followUp.Status != "follow_up" {
		t.Fatalf("followUp.Status = %q, want follow_up", followUp.Status)
	}

	blocked := reconcileUIEvent("G-1", "PATCH-3", reconciler.ReconcileResult{
		BlockingReason: "missing evidence",
	})
	if blocked.Kind != EventKindReconcileBlocked {
		t.Fatalf("blocked.Kind = %s, want %s", blocked.Kind, EventKindReconcileBlocked)
	}
	if blocked.Detail != "missing evidence" {
		t.Fatalf("blocked.Detail = %q, want missing evidence", blocked.Detail)
	}
}

func TestRecorderNotifier_ControlLoopEmitsExpectedEvents(t *testing.T) {
	// Verify that compileGoal emits goal.compiling then goal.planning via
	// the notifier, in that order. Uses a real intent compiler and verifier
	// against a temp orcaDir with no pre-existing goal.
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	rec := &recorderNotifier{}
	rt.notifier = rec

	if _, err := rt.compileGoal(context.Background(), "add unit tests for the parser"); err != nil {
		t.Fatalf("compileGoal: %v", err)
	}

	if len(rec.events) < 2 {
		t.Fatalf("compileGoal emitted %d events, want at least 2", len(rec.events))
	}
	if rec.events[0].Kind != EventKindGoalCompiling {
		t.Errorf("event[0].Kind = %s, want goal.compiling", rec.events[0].Kind)
	}
	if rec.events[1].Kind != EventKindGoalPlanning {
		t.Errorf("event[1].Kind = %s, want goal.planning", rec.events[1].Kind)
	}
}

func TestRunStatus_Raw_ShowsDetailedDump(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	if err := rt.store.AppendRuntimeEvent(context.Background(), &schema.CapsuleRuntimeEvent{
		CapsuleID: "CAP-1",
		GoalID:    "G-1",
		Source:    "runner",
		Status:    schema.RuntimeStatusAgentRunning,
	}); err != nil {
		t.Fatalf("AppendRuntimeEvent: %v", err)
	}

	var out bytes.Buffer
	if err := rt.printStatus(context.Background(), &out); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()
	// Raw/detailed fields must be present.
	for _, want := range []string{
		"Active goal: G-1",
		"Open obligations: 1",
		"Active capsules: 1",
		"runtime_status=agent_running",
		"Budget totals:",
		"MCP server:",
		"Remote execution:",
		"Latest PR:",
		"CI:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("raw status missing %q:\n%s", want, got)
		}
	}
}

func TestRunStatus_Concise_HidesImplementationDetails(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	var out bytes.Buffer
	if err := rt.printStatusConcise(context.Background(), &out); err != nil {
		t.Fatalf("printStatusConcise: %v", err)
	}
	got := out.String()

	// Essential info must be present.
	for _, want := range []string{
		"fix the auth middleware rounding defect",
		"active",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("concise status missing %q:\n%s", want, got)
		}
	}

	// Implementation details must be hidden.
	for _, absent := range []string{
		"Budget totals:",
		"MCP server:",
		"Remote execution:",
		"Advanced checks:",
		"Latest PR:",
		// Detailed obligation list format (with raw ID and risk level brackets).
		"- OB-1 [",
		// Detailed capsule list format.
		"- CAP-1 [pending] agent=codex",
	} {
		if strings.Contains(got, absent) {
			t.Errorf("concise status contains detail %q:\n%s", absent, got)
		}
	}
}

func TestRunStatus_Concise_NoActiveGoal(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	var out bytes.Buffer
	if err := rt.printStatusConcise(context.Background(), &out); err != nil {
		t.Fatalf("printStatusConcise: %v", err)
	}
	if !strings.Contains(out.String(), "No active goal.") {
		t.Errorf("concise status = %q, want 'No active goal.'", out.String())
	}
}

func TestRuntimeEmit_NilNotifier_DoesNotPanic(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	// notifier is nil by default from openRuntime; emit must be a no-op.
	rt.emit(context.Background(), UIEvent{Kind: EventKindGoalCompiling, Summary: "test"})
}
