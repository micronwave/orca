package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestSupervisor_DetailsCommandShowsGoalInfo verifies that /details calls
// printStatus and produces output that includes the active goal's intent.
// /details is the verbose status command; /status calls printStatusConcise.
func TestSupervisor_DetailsCommandShowsGoalInfo(t *testing.T) {
	orcaDir := seedOrcaDir(t, true) // active goal with capsule
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	var out bytes.Buffer
	sup.out = &out

	if err := sup.handleLine(context.Background(), "/details"); err != nil {
		t.Fatalf("handleLine(/details): %v", err)
	}

	got := out.String()
	if got == "" {
		t.Fatal("/details produced no output")
	}

	// Must contain the seeded goal's intent string.
	const wantIntent = "fix the auth middleware rounding defect"
	if !strings.Contains(got, wantIntent) {
		t.Errorf("/details output missing goal intent %q:\n%s", wantIntent, got)
	}
}

// TestSupervisor_DetailsCommandDoesNotStartGoal verifies that /details never
// starts a new goal run. The seeded dir has an active goal but no running capsule.
func TestSupervisor_DetailsCommandDoesNotStartGoal(t *testing.T) {
	orcaDir := seedOrcaDir(t, false) // active goal in store, no capsule seeded
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	if err := sup.handleLine(context.Background(), "/details"); err != nil {
		t.Fatalf("handleLine(/details): %v", err)
	}
	if sup.goalActive.Load() {
		t.Fatal("/details started a goal")
	}
}

// TestSupervisor_DetailsCommandIsSupervisorCommand verifies that /details is
// recognised as a supervisor command and is therefore routed to the command
// loop rather than the gate pipe while a gate is waiting.
// All slash-prefixed commands qualify via isSupervisorCommand's prefix rule.
func TestSupervisor_DetailsCommandIsSupervisorCommand(t *testing.T) {
	if !isSupervisorCommand("/details") {
		t.Fatal("/details must be a supervisor command; gate pipe must not consume it during gate-wait")
	}
}
