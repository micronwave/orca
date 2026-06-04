package store_test

// Tests for the four in-memory caches added in speed-up Phase 3:
//   B7  condToGoal    — findGoalIDForCondition O(1) map lookup
//   B8  latestSnap    — LoadLatestSnapshot O(1) map lookup
//   B12 budgetsByGoal — LoadBudgetForGoal O(1) map lookup
//   B13 knownGoals    — goalExists O(1) map lookup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── B7: condToGoal cache ──────────────────────────────────────────────────────

// TestCondToGoalCache_ObligationSave verifies that SaveObligation succeeds for a
// condition that was registered via SaveGoal (cache hit path).
func TestCondToGoalCache_ObligationSave(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-CGT", "GC-CGT-A", "GC-CGT-B")
	// Both conditions must resolve without disk scan.
	e.seedObligation(t, "OB-CGT-1", "GC-CGT-A", schema.ObligationOpen)
	e.seedObligation(t, "OB-CGT-2", "GC-CGT-B", schema.ObligationOpen)
}

// TestCondToGoalCache_InitFromDisk verifies that after re-opening the store on
// an existing directory the condToGoal cache is rebuilt from disk and
// SaveObligation (which calls findGoalIDForCondition) still works.
func TestCondToGoalCache_InitFromDisk(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-CGT2", "GC-CGT2")
	e.seedObligation(t, "OB-CGT2-1", "GC-CGT2", schema.ObligationOpen)

	// Open a second store on the same root.
	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	st2, err := store.New(e.root, l2)
	if err != nil {
		t.Fatalf("store.New on existing root: %v", err)
	}

	// Adding a new obligation for the existing condition must succeed (cache hit).
	o := &schema.Obligation{
		ObligationID:    "OB-CGT2-2",
		GoalConditionID: "GC-CGT2",
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}
	if err := st2.SaveObligation(context.Background(), o); err != nil {
		t.Fatalf("SaveObligation after init-from-disk: %v", err)
	}
}

// ── B8: latestSnap cache ──────────────────────────────────────────────────────

// TestSnapshotCache_LoadLatestErrNotFound verifies that LoadLatestSnapshot
// returns ErrNotFound (not nil, not a zero-value struct) for a goalID with
// no snapshots.
func TestSnapshotCache_LoadLatestErrNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.st.LoadLatestSnapshot(e.ctx, "G-no-snaps")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadLatestSnapshot no-snapshot error = %v, want ErrNotFound", err)
	}
}

// TestSnapshotCache_UpdateOnSave verifies that SaveSnapshot updates the cache
// so that the next LoadLatestSnapshot is served from memory.
func TestSnapshotCache_UpdateOnSave(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-SC1", "GC-SC1")

	snaps := []*schema.StateSnapshot{
		{SnapshotID: "SNAP-SC-1", GoalID: "G-SC1", SequenceNum: 3, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-SC-2", GoalID: "G-SC1", SequenceNum: 7, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-SC-3", GoalID: "G-SC1", SequenceNum: 5, CreatedAt: time.Now().UTC()},
	}
	for _, s := range snaps {
		if err := e.st.SaveSnapshot(e.ctx, s); err != nil {
			t.Fatalf("SaveSnapshot %s: %v", s.SnapshotID, err)
		}
	}

	latest, err := e.st.LoadLatestSnapshot(e.ctx, "G-SC1")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	if latest.SnapshotID != "SNAP-SC-2" || latest.SequenceNum != 7 {
		t.Errorf("latest = %+v, want SNAP-SC-2/7", latest)
	}
}

// TestSnapshotCache_SaveDoesNotAliasCaller verifies that mutating the caller's
// snapshot after SaveSnapshot does not change the cached latest snapshot.
func TestSnapshotCache_SaveDoesNotAliasCaller(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-SC-ALIAS", "GC-SC-ALIAS")

	snap := &schema.StateSnapshot{
		SnapshotID:  "SNAP-SC-ALIAS-1",
		GoalID:      "G-SC-ALIAS",
		SequenceNum: 4,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.st.SaveSnapshot(e.ctx, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	snap.SequenceNum = 999

	latest, err := e.st.LoadLatestSnapshot(e.ctx, "G-SC-ALIAS")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	if latest.SequenceNum != 4 {
		t.Fatalf("latest.SequenceNum = %d, want 4", latest.SequenceNum)
	}
}

// TestSnapshotCache_MultiGoalIsolation verifies that snapshots from different
// goals do not appear when querying another goal.
func TestSnapshotCache_MultiGoalIsolation(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-SC-A", "GC-SC-A")
	e.seedCompletedGoal(t, "G-SC-B", "GC-SC-B")

	for _, s := range []*schema.StateSnapshot{
		{SnapshotID: "SNAP-A-1", GoalID: "G-SC-A", SequenceNum: 10, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-B-1", GoalID: "G-SC-B", SequenceNum: 20, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveSnapshot(e.ctx, s); err != nil {
			t.Fatalf("SaveSnapshot %s: %v", s.SnapshotID, err)
		}
	}

	latA, err := e.st.LoadLatestSnapshot(e.ctx, "G-SC-A")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot G-SC-A: %v", err)
	}
	if latA.SnapshotID != "SNAP-A-1" {
		t.Errorf("G-SC-A latest = %s, want SNAP-A-1", latA.SnapshotID)
	}

	latB, err := e.st.LoadLatestSnapshot(e.ctx, "G-SC-B")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot G-SC-B: %v", err)
	}
	if latB.SnapshotID != "SNAP-B-1" {
		t.Errorf("G-SC-B latest = %s, want SNAP-B-1", latB.SnapshotID)
	}
}

// TestSnapshotCache_InitFromDisk verifies that opening a store on a directory
// that already contains snapshot files rebuilds the latestSnap cache correctly.
func TestSnapshotCache_InitFromDisk(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-SDISK", "GC-SDISK")
	for _, s := range []*schema.StateSnapshot{
		{SnapshotID: "SNAP-DISK-1", GoalID: "G-SDISK", SequenceNum: 2, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-DISK-2", GoalID: "G-SDISK", SequenceNum: 9, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-DISK-3", GoalID: "G-SDISK", SequenceNum: 5, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveSnapshot(e.ctx, s); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}

	// Open a second store on the same root (no replay — just New).
	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	st2, err := store.New(e.root, l2)
	if err != nil {
		t.Fatalf("store.New on existing root: %v", err)
	}

	got, err := st2.LoadLatestSnapshot(context.Background(), "G-SDISK")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot: %v", err)
	}
	if got.SnapshotID != "SNAP-DISK-2" || got.SequenceNum != 9 {
		t.Errorf("init-from-disk latest = %+v, want SNAP-DISK-2/9", got)
	}
}

// TestSnapshotCache_ReplayConsistency verifies that after replaying events into
// a fresh store, LoadLatestSnapshot reflects the replayed snapshots.
func TestSnapshotCache_ReplayConsistency(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-SRPL", "GC-SRPL")
	for _, s := range []*schema.StateSnapshot{
		{SnapshotID: "SNAP-RPL-1", GoalID: "G-SRPL", SequenceNum: 4, CreatedAt: time.Now().UTC()},
		{SnapshotID: "SNAP-RPL-2", GoalID: "G-SRPL", SequenceNum: 11, CreatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveSnapshot(e.ctx, s); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}

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

	got, err := st2.LoadLatestSnapshot(context.Background(), "G-SRPL")
	if err != nil {
		t.Fatalf("LoadLatestSnapshot post-replay: %v", err)
	}
	if got.SnapshotID != "SNAP-RPL-2" || got.SequenceNum != 11 {
		t.Errorf("post-replay latest = %+v, want SNAP-RPL-2/11", got)
	}
}

// ── B12: budgetsByGoal cache ──────────────────────────────────────────────────

// TestBudgetCache_LoadForGoalFromCache verifies that LoadBudgetForGoal returns
// only the records for the requested goal and returns an independent slice
// (not the live cache slice).
func TestBudgetCache_LoadForGoalFromCache(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-BC1", "GC-BC1")
	e.seedCompletedGoal(t, "G-BC2", "GC-BC2")
	for _, b := range []*schema.BudgetRecord{
		{BudgetID: "BUD-BC-1", GoalID: "G-BC1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{BudgetID: "BUD-BC-2", GoalID: "G-BC1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{BudgetID: "BUD-BC-3", GoalID: "G-BC2", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
			t.Fatalf("SaveBudgetRecord %s: %v", b.BudgetID, err)
		}
	}

	out, err := e.st.LoadBudgetForGoal(e.ctx, "G-BC1")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("LoadBudgetForGoal G-BC1 = %d records, want 2", len(out))
	}
	for _, r := range out {
		if r.GoalID != "G-BC1" {
			t.Errorf("unexpected GoalID %q in result", r.GoalID)
		}
	}

	out[0].TokensSpent = 999

	again, err := e.st.LoadBudgetForGoal(e.ctx, "G-BC1")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal second read: %v", err)
	}
	if again[0].TokensSpent == 999 {
		t.Fatalf("LoadBudgetForGoal returned live cached record; second read = %+v", again[0])
	}
}

// TestBudgetCache_UpdateReflectsInLoad verifies that after UpdateBudgetRecord
// the updated value is visible via LoadBudgetForGoal.
func TestBudgetCache_UpdateReflectsInLoad(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-BUP", "GC-BUP")
	b := &schema.BudgetRecord{
		BudgetID:    "BUD-UPD-1",
		GoalID:      "G-BUP",
		TokensSpent: 100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}

	b.TokensSpent = 999
	if err := e.st.UpdateBudgetRecord(e.ctx, b); err != nil {
		t.Fatalf("UpdateBudgetRecord: %v", err)
	}

	out, err := e.st.LoadBudgetForGoal(e.ctx, "G-BUP")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	if len(out) != 1 || out[0].TokensSpent != 999 {
		t.Fatalf("LoadBudgetForGoal after update = %+v, want single record with TokensSpent=999", out)
	}
}

// TestBudgetCache_SaveDoesNotAliasCaller verifies that mutating a saved
// BudgetRecord after SaveBudgetRecord does not change the cached value.
func TestBudgetCache_SaveDoesNotAliasCaller(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-BALIAS", "GC-BALIAS")

	record := &schema.BudgetRecord{
		BudgetID:    "BUD-BALIAS-1",
		GoalID:      "G-BALIAS",
		TokensSpent: 12,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, record); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}

	record.TokensSpent = 777

	out, err := e.st.LoadBudgetForGoal(e.ctx, "G-BALIAS")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal: %v", err)
	}
	if len(out) != 1 || out[0].TokensSpent != 12 {
		t.Fatalf("LoadBudgetForGoal after caller mutation = %+v, want TokensSpent=12", out)
	}
}

// TestBudgetCache_InitFromDisk verifies that a store opened on an existing
// directory repopulates budgetsByGoal from the artifact files.
func TestBudgetCache_InitFromDisk(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-BDISK", "GC-BDISK")
	for _, b := range []*schema.BudgetRecord{
		{BudgetID: "BUD-DISK-1", GoalID: "G-BDISK", TokensSpent: 10, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		{BudgetID: "BUD-DISK-2", GoalID: "G-BDISK", TokensSpent: 20, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	} {
		if err := e.st.SaveBudgetRecord(e.ctx, b); err != nil {
			t.Fatalf("SaveBudgetRecord: %v", err)
		}
	}

	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	st2, err := store.New(e.root, l2)
	if err != nil {
		t.Fatalf("store.New on existing root: %v", err)
	}

	out, err := st2.LoadBudgetForGoal(context.Background(), "G-BDISK")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal after init-from-disk: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("init-from-disk: got %d budget records, want 2", len(out))
	}
}

// TestBudgetCache_ReplayConsistency verifies that after replaying events into
// a fresh store, LoadBudgetForGoal reflects the replayed records.
func TestBudgetCache_ReplayConsistency(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-BRPL", "GC-BRPL")
	b1 := &schema.BudgetRecord{
		BudgetID:    "BUD-RPL-1",
		GoalID:      "G-BRPL",
		TokensSpent: 50,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := e.st.SaveBudgetRecord(e.ctx, b1); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	b1.TokensSpent = 75
	if err := e.st.UpdateBudgetRecord(e.ctx, b1); err != nil {
		t.Fatalf("UpdateBudgetRecord: %v", err)
	}

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

	out, err := st2.LoadBudgetForGoal(context.Background(), "G-BRPL")
	if err != nil {
		t.Fatalf("LoadBudgetForGoal post-replay: %v", err)
	}
	// Save then Update → still one logical record; after update the cache
	// replays both events so we expect exactly one entry with the final value.
	if len(out) != 1 || out[0].TokensSpent != 75 {
		t.Fatalf("post-replay budget = %+v, want 1 record with TokensSpent=75", out)
	}
}

// ── B13: knownGoals cache ─────────────────────────────────────────────────────

// TestKnownGoalsCache_SaveBudgetUsesCache verifies that after SaveGoal the
// goal existence is cached: SaveBudgetRecord and SaveSnapshot succeed without
// requiring an additional disk read for goal validation.
func TestKnownGoalsCache_SaveBudgetUsesCache(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-KG1", "GC-KG1")

	if err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:  "BUD-KG-1",
		GoalID:    "G-KG1",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveBudgetRecord: %v", err)
	}
	if err := e.st.SaveSnapshot(e.ctx, &schema.StateSnapshot{
		SnapshotID: "SNAP-KG-1",
		GoalID:     "G-KG1",
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
}

// TestKnownGoalsCache_MissingGoalReturnsError verifies that SaveBudgetRecord
// for an unknown goalID still returns an error.
func TestKnownGoalsCache_MissingGoalReturnsError(t *testing.T) {
	e := newEnv(t)
	err := e.st.SaveBudgetRecord(e.ctx, &schema.BudgetRecord{
		BudgetID:  "BUD-KG-MISS",
		GoalID:    "G-DOES-NOT-EXIST",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SaveBudgetRecord missing goal error = %v, want ErrNotFound", err)
	}
}

// TestKnownGoalsCache_InitFromDisk verifies that a store opened on an existing
// directory recognises goals that were saved previously (knownGoals rebuilt).
func TestKnownGoalsCache_InitFromDisk(t *testing.T) {
	e := newEnv(t)
	e.seedGoal(t, "G-KGDISK", "GC-KGDISK")

	l2, err := eventlog.Open(filepath.Join(e.root, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	st2, err := store.New(e.root, l2)
	if err != nil {
		t.Fatalf("store.New on existing root: %v", err)
	}

	// SaveSnapshot requires the goal to exist; must succeed without disk read.
	if err := st2.SaveSnapshot(context.Background(), &schema.StateSnapshot{
		SnapshotID: "SNAP-KGDISK-1",
		GoalID:     "G-KGDISK",
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSnapshot after init-from-disk: %v", err)
	}
}
