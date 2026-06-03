package store_test

// Tests for the open-obligations in-memory index that accelerates
// LoadOpenObligations from O(all_obligations) to O(open_obligations).
//
// Run with:
//
//	go test ./internal/store/... -run TestOpenObligationsIndex -v

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// TestOpenObligationsIndex_Accuracy verifies that LoadOpenObligations returns
// exactly the obligations whose current status is Open and none of the others.
func TestOpenObligationsIndex_Accuracy(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-IDX", "GC-A", "GC-B")

	e.seedObligation(t, "OB-1", "GC-A", schema.ObligationOpen)
	e.seedObligation(t, "OB-2", "GC-A", schema.ObligationOpen)
	e.seedObligation(t, "OB-3", "GC-B", schema.ObligationOpen)
	e.seedObligation(t, "OB-4", "GC-A", schema.ObligationSatisfied)
	e.seedObligation(t, "OB-5", "GC-B", schema.ObligationFailed)

	got, err := e.st.LoadOpenObligations(e.ctx, "G-IDX")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d open obligations, want 3", len(got))
	}
	ids := make(map[string]bool, len(got))
	for _, o := range got {
		ids[o.ObligationID] = true
	}
	for _, want := range []string{"OB-1", "OB-2", "OB-3"} {
		if !ids[want] {
			t.Errorf("missing open obligation %s", want)
		}
	}
	for _, notWant := range []string{"OB-4", "OB-5"} {
		if ids[notWant] {
			t.Errorf("unexpected non-open obligation %s in result", notWant)
		}
	}
}

// TestOpenObligationsIndex_UpdatedOnStatusChange verifies that the index is
// kept consistent when UpdateObligationStatus transitions an obligation away
// from or back to Open.
func TestOpenObligationsIndex_UpdatedOnStatusChange(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-UPD", "GC-U")
	e.seedObligation(t, "OB-U1", "GC-U", schema.ObligationOpen)
	e.seedObligation(t, "OB-U2", "GC-U", schema.ObligationOpen)

	if err := e.st.UpdateObligationStatus(e.ctx, "OB-U1", schema.ObligationSatisfied, nil); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}

	got, err := e.st.LoadOpenObligations(e.ctx, "G-UPD")
	if err != nil {
		t.Fatalf("LoadOpenObligations after satisfy: %v", err)
	}
	if len(got) != 1 || got[0].ObligationID != "OB-U2" {
		t.Fatalf("after satisfying OB-U1: want [OB-U2], got %v", got)
	}

	if err := e.st.UpdateObligationStatus(e.ctx, "OB-U2", schema.ObligationFailed, nil); err != nil {
		t.Fatalf("UpdateObligationStatus fail: %v", err)
	}

	got, err = e.st.LoadOpenObligations(e.ctx, "G-UPD")
	if err != nil {
		t.Fatalf("LoadOpenObligations after fail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after failing all obligations: want [], got %d items", len(got))
	}
}

// TestOpenObligationsIndex_MultiGoalIsolation verifies that open obligations
// from goal A do not appear when querying goal B and vice versa.
func TestOpenObligationsIndex_MultiGoalIsolation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-ISO-A", "GC-ISO-A")
	e.seedCompletedGoal(t, "G-ISO-B", "GC-ISO-B")

	e.seedObligation(t, "OB-ISO-A", "GC-ISO-A", schema.ObligationOpen)
	e.seedObligation(t, "OB-ISO-B", "GC-ISO-B", schema.ObligationOpen)

	gotA, err := e.st.LoadOpenObligations(e.ctx, "G-ISO-A")
	if err != nil {
		t.Fatalf("LoadOpenObligations G-ISO-A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ObligationID != "OB-ISO-A" {
		t.Fatalf("G-ISO-A: want [OB-ISO-A], got %v", gotA)
	}

	gotB, err := e.st.LoadOpenObligations(e.ctx, "G-ISO-B")
	if err != nil {
		t.Fatalf("LoadOpenObligations G-ISO-B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].ObligationID != "OB-ISO-B" {
		t.Fatalf("G-ISO-B: want [OB-ISO-B], got %v", gotB)
	}
}

// TestOpenObligationsIndex_EmptyGoal verifies that querying a goal with no
// obligations returns an empty result rather than an error.
func TestOpenObligationsIndex_EmptyGoal(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-EMPTY-IDX", "GC-E1")

	got, err := e.st.LoadOpenObligations(e.ctx, "G-EMPTY-IDX")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 open obligations, got %d", len(got))
	}
}

func TestOpenObligationsIndex_UnknownGoal(t *testing.T) {
	e := newEnv(t)

	_, err := e.st.LoadOpenObligations(e.ctx, "G-MISSING-IDX")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadOpenObligations unknown goal error = %v, want ErrNotFound", err)
	}
}

// TestOpenObligationsIndex_PartiallyMetNotOpen verifies that obligations in
// the partially_met state are not returned by LoadOpenObligations.
func TestOpenObligationsIndex_PartiallyMetNotOpen(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-PM-IDX", "GC-PM")
	e.seedObligation(t, "OB-PM1", "GC-PM", schema.ObligationOpen)

	if err := e.st.UpdateObligationStatus(e.ctx, "OB-PM1", schema.ObligationStatusPartiallyMet, nil); err != nil {
		t.Fatalf("UpdateObligationStatus partially_met: %v", err)
	}

	got, err := e.st.LoadOpenObligations(e.ctx, "G-PM-IDX")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("partially_met obligation must not appear as open; got %d items", len(got))
	}
}

// TestOpenObligationsIndex_ReplayConsistency verifies that the index is rebuilt
// correctly during Replay: obligations closed by subsequent status-update events
// are absent from the index after replay.
func TestOpenObligationsIndex_ReplayConsistency(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-RPL", "GC-RPL")
	e.seedObligation(t, "OB-RPL-1", "GC-RPL", schema.ObligationOpen)
	e.seedObligation(t, "OB-RPL-2", "GC-RPL", schema.ObligationOpen)
	if err := e.st.UpdateObligationStatus(e.ctx, "OB-RPL-1", schema.ObligationSatisfied, nil); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}

	// Replay the same log into a fresh store directory.
	dir2 := t.TempDir()
	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log for replay: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })

	st2, err := store.New(dir2, l2)
	if err != nil {
		t.Fatalf("store.New for replay: %v", err)
	}
	if err := store.Replay(context.Background(), l2, st2, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	got, err := st2.LoadOpenObligations(context.Background(), "G-RPL")
	if err != nil {
		t.Fatalf("LoadOpenObligations post-replay: %v", err)
	}
	if len(got) != 1 || got[0].ObligationID != "OB-RPL-2" {
		t.Fatalf("post-replay: want [OB-RPL-2], got %v", got)
	}
}

func TestOpenObligationsIndex_ReplayRejectsUnknownCondition(t *testing.T) {
	e := newEnv(t)
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:       schema.EventGoalCreated,
		GoalID:     "G-BAD-RPL",
		ArtifactID: "G-BAD-RPL",
		Payload: marshalJSON(t, &schema.GoalIR{
			GoalID:         "G-BAD-RPL",
			OriginalIntent: "replay bad obligation linkage",
			Status:         schema.GoalStatusActive,
			GoalConditions: []schema.GoalCondition{{ID: "GC-REAL", Description: "real"}},
		}),
	}); err != nil {
		t.Fatalf("Append goal_created: %v", err)
	}
	if _, err := e.log.Append(e.ctx, schema.Event{
		Type:       schema.EventObligationCreated,
		GoalID:     "G-BAD-RPL",
		ArtifactID: "OB-BAD-RPL",
		Payload: marshalJSON(t, &schema.Obligation{
			ObligationID:    "OB-BAD-RPL",
			GoalConditionID: "GC-MISSING",
			Status:          schema.ObligationOpen,
		}),
	}); err != nil {
		t.Fatalf("Append obligation_created: %v", err)
	}

	wipeArtifacts(t, e)
	if err := store.Replay(e.ctx, e.log, e.st, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Replay unknown-condition error = %v, want ErrNotFound", err)
	}
}

// TestOpenObligationsIndex_InitFromDisk verifies that a store opened on an
// already-populated directory builds the index correctly from the on-disk
// artifact files without needing a replay.
func TestOpenObligationsIndex_InitFromDisk(t *testing.T) {
	// First store: create a goal + two open obligations, close one.
	e := newEnv(t)
	e.seedGoal(t, "G-DISK", "GC-DISK")
	e.seedObligation(t, "OB-DISK-1", "GC-DISK", schema.ObligationOpen)
	e.seedObligation(t, "OB-DISK-2", "GC-DISK", schema.ObligationOpen)
	if err := e.st.UpdateObligationStatus(e.ctx, "OB-DISK-1", schema.ObligationSatisfied, nil); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}

	// Second store opened on the same root directory (no replay — just New()).
	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	st2, err := store.New(e.root, l2)
	if err != nil {
		t.Fatalf("store.New on existing root: %v", err)
	}

	got, err := st2.LoadOpenObligations(context.Background(), "G-DISK")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(got) != 1 || got[0].ObligationID != "OB-DISK-2" {
		t.Fatalf("init from disk: want [OB-DISK-2], got %v", got)
	}
}

// TestOpenObligationsIndex_Stress creates many obligations with varied statuses
// and verifies that LoadOpenObligations returns only the open ones. The implicit
// performance assertion is that the test does not time out on a large N.
func TestOpenObligationsIndex_Stress(t *testing.T) {
	const (
		total       = 1000
		openCount   = 80
		condPerGoal = 10
	)

	e := newEnv(t)
	condIDs := make([]string, condPerGoal)
	for i := range condIDs {
		condIDs[i] = fmt.Sprintf("GC-ST-%03d", i)
	}
	e.seedGoal(t, "G-STRESS-IDX", condIDs...)

	for i := 1; i <= total; i++ {
		oblID := fmt.Sprintf("OB-ST-%05d", i)
		condID := condIDs[i%condPerGoal]
		status := schema.ObligationSatisfied
		if i <= openCount {
			status = schema.ObligationOpen
		}
		e.seedObligation(t, oblID, condID, status)
	}

	start := time.Now()
	got, err := e.st.LoadOpenObligations(e.ctx, "G-STRESS-IDX")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(got) != openCount {
		t.Fatalf("got %d open obligations, want %d", len(got), openCount)
	}
	t.Logf("LoadOpenObligations(%d total, %d open) completed in %v", total, openCount, elapsed.Round(time.Microsecond))
}
