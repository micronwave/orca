package eventlog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// helpers

func openLog(t *testing.T) *eventlog.FileLog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func openLogAt(t *testing.T, path string) *eventlog.FileLog {
	t.Helper()
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func append1(t *testing.T, l *eventlog.FileLog, typ schema.EventType, goalID string) schema.Event {
	t.Helper()
	e := schema.Event{Type: typ, GoalID: goalID}
	appended, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return appended
}

// --- Open ---

func TestOpen_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpen_CreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orca", "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("Open with nested dirs: %v", err)
	}
	_ = l.Close()
}

func TestOpen_RepairsTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	first, _ := json.Marshal(schema.Event{
		EventID:     "EV-1",
		Type:        schema.EventGoalCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 1,
	})
	if err := os.WriteFile(path, append(append([]byte{}, first...), []byte("\n{\"event_id\"")...), 0o644); err != nil {
		t.Fatalf("seed partial log: %v", err)
	}

	l := openLogAt(t, path)
	defer l.Close()
	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter repaired log: %v", err)
	}
	if len(events) != 1 || events[0].SequenceNum != 1 {
		t.Fatalf("events after repair = %+v, want only seq=1", events)
	}
	next := append1(t, l, schema.EventObligationCreated, "G-1")
	if next.SequenceNum != 2 {
		t.Fatalf("next SequenceNum = %d, want 2", next.SequenceNum)
	}
}

func TestOpen_RepairsValidUntermunatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	first, _ := json.Marshal(schema.Event{
		EventID:     "EV-1",
		Type:        schema.EventGoalCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 1,
	})
	second, _ := json.Marshal(schema.Event{
		EventID:     "EV-2",
		Type:        schema.EventObligationCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 2,
	})
	// second is valid JSON but has no trailing '\n'
	data := append(append(first, '\n'), second...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	l := openLogAt(t, path)
	defer l.Close()
	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 1 || events[0].SequenceNum != 1 {
		t.Fatalf("events after repair = %+v, want only seq=1", events)
	}
	next := append1(t, l, schema.EventCapsuleCreated, "G-1")
	if next.SequenceNum != 2 {
		t.Fatalf("next SequenceNum = %d, want 2", next.SequenceNum)
	}
}

func TestOpen_RejectsCorruptCompletedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	first, _ := json.Marshal(schema.Event{
		EventID:     "EV-1",
		Type:        schema.EventGoalCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 1,
	})
	data := append(append([]byte{}, first...), []byte("\n{\"event_id\"\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("seed corrupt log: %v", err)
	}
	if _, err := eventlog.Open(path); err == nil {
		t.Fatal("Open succeeded on corrupt completed line, want error")
	}
}

func TestOpen_RejectsOutOfOrderSequenceNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	first, _ := json.Marshal(schema.Event{
		EventID:     "EV-1",
		Type:        schema.EventGoalCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 1,
	})
	// seq=3 directly after seq=1 — gap/ordering violation
	third, _ := json.Marshal(schema.Event{
		EventID:     "EV-3",
		Type:        schema.EventObligationCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 3,
	})
	data := append(append(first, '\n'), append(third, '\n')...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if _, err := eventlog.Open(path); err == nil {
		t.Fatal("Open succeeded on out-of-order sequence numbers, want error")
	}
}

// --- ReadAfter ---

func TestReadAfter_RejectsOutOfOrderSequenceNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l := openLogAt(t, path)

	append1(t, l, schema.EventGoalCreated, "G-1")       // seq=1
	append1(t, l, schema.EventObligationCreated, "G-1") // seq=2

	// Inject an event with seq=5 (gap: should be 3) directly into the file,
	// bypassing the FileLog writer so Open's scan does not reject it.
	bad, _ := json.Marshal(schema.Event{
		EventID:     "EV-bad",
		Type:        schema.EventCapsuleCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 5,
	})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open file for injection: %v", err)
	}
	if _, err := f.Write(append(bad, '\n')); err != nil {
		t.Fatalf("inject bad event: %v", err)
	}
	_ = f.Close()

	if _, err := l.ReadAfter(context.Background(), 0, 0); err == nil {
		t.Fatal("ReadAfter succeeded on out-of-order log, want error")
	}
}

func TestReadAfter_RejectsFirstSequenceNumberGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l := openLogAt(t, path)

	bad, _ := json.Marshal(schema.Event{
		EventID:     "EV-bad",
		Type:        schema.EventGoalCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 2,
	})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open file for injection: %v", err)
	}
	if _, err := f.Write(append(bad, '\n')); err != nil {
		t.Fatalf("inject bad event: %v", err)
	}
	_ = f.Close()

	if _, err := l.ReadAfter(context.Background(), 0, 0); err == nil {
		t.Fatal("ReadAfter succeeded when first event seq was not 1, want error")
	}
}

func TestReadAfter_SkipsUnterminatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l := openLogAt(t, path)

	append1(t, l, schema.EventGoalCreated, "G-1") // seq=1

	// Inject a valid-JSON event without a trailing '\n', simulating a partial
	// write by an external process while this FileLog is open.
	partial, _ := json.Marshal(schema.Event{
		EventID:     "EV-partial",
		Type:        schema.EventObligationCreated,
		GoalID:      "G-1",
		CreatedAt:   time.Now().UTC(),
		SequenceNum: 2,
	})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open file for injection: %v", err)
	}
	if _, err := f.Write(partial); err != nil { // no trailing '\n'
		t.Fatalf("inject partial event: %v", err)
	}
	_ = f.Close()

	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 1 || events[0].SequenceNum != 1 {
		t.Fatalf("events = %+v, want only seq=1", events)
	}
}

// --- Append ---

func TestAppend_ReturnsErrClosedAfterClose(t *testing.T) {
	l := openLog(t)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := l.Append(context.Background(), schema.Event{Type: schema.EventGoalCreated, GoalID: "G-1"})
	if !errors.Is(err, eventlog.ErrClosed) {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	l := openLog(t)
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClose_ReadsReturnErrClosedAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l := openLogAt(t, path)
	append1(t, l, schema.EventGoalCreated, "G-1")
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := l.ReadAfter(context.Background(), 0, 0); !errors.Is(err, eventlog.ErrClosed) {
		t.Errorf("ReadAfter after Close = %v, want ErrClosed", err)
	}
	if _, err := l.ReadByType(context.Background(), schema.EventGoalCreated, 0, 0); !errors.Is(err, eventlog.ErrClosed) {
		t.Errorf("ReadByType after Close = %v, want ErrClosed", err)
	}
	if _, err := l.ReadForGoal(context.Background(), "G-1", 0, 0); !errors.Is(err, eventlog.ErrClosed) {
		t.Errorf("ReadForGoal after Close = %v, want ErrClosed", err)
	}
}

func TestAppend_AssignsSequenceNumbers(t *testing.T) {
	l := openLog(t)
	for i := 1; i <= 5; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	for i, e := range events {
		want := int64(i + 1)
		if e.SequenceNum != want {
			t.Errorf("event[%d].SequenceNum = %d, want %d", i, e.SequenceNum, want)
		}
	}
}

func TestAppend_GeneratesEventID(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	if events[0].EventID == "" {
		t.Error("EventID should be non-empty")
	}
}

func TestAppend_SetsCreatedAt(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	after := time.Now().UTC().Add(time.Second)
	if events[0].CreatedAt.Before(before) || events[0].CreatedAt.After(after) {
		t.Errorf("CreatedAt %v out of expected window [%v, %v]", events[0].CreatedAt, before, after)
	}
}

func TestAppend_PreservesCallerCreatedAt(t *testing.T) {
	fixed := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	l := openLog(t)
	e := schema.Event{Type: schema.EventGoalCreated, GoalID: "G-1", CreatedAt: fixed}
	appended, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if appended.EventID == "" || appended.SequenceNum == 0 || appended.CreatedAt.IsZero() {
		t.Fatalf("Append returned incomplete assigned event: %+v", appended)
	}
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	if !events[0].CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", events[0].CreatedAt, fixed)
	}
}

func TestAppend_PreservesPayload(t *testing.T) {
	l := openLog(t)
	type sample struct{ X int }
	payload, _ := json.Marshal(sample{X: 42})
	e := schema.Event{Type: schema.EventGoalCreated, GoalID: "G-1", Payload: payload}
	if _, err := l.Append(context.Background(), e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	var got sample
	if err := json.Unmarshal(events[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.X != 42 {
		t.Errorf("payload.X = %d, want 42", got.X)
	}
}

func TestAppend_ReadsLargePayload(t *testing.T) {
	l := openLog(t)
	payload, err := json.Marshal(strings.Repeat("x", 2<<20))
	if err != nil {
		t.Fatalf("marshal large payload: %v", err)
	}
	if _, err := l.Append(context.Background(), schema.Event{
		Type:    schema.EventGoalCreated,
		GoalID:  "G-1",
		Payload: payload,
	}); err != nil {
		t.Fatalf("Append large payload: %v", err)
	}
	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter large payload: %v", err)
	}
	if len(events) != 1 || len(events[0].Payload) != len(payload) {
		t.Fatalf("large payload length = %d, want %d", len(events[0].Payload), len(payload))
	}
}

// --- ReadAfter ---

func TestReadAfter_EmptyLog(t *testing.T) {
	l := openLog(t)
	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter on empty log: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestReadAfter_SeqFilter(t *testing.T) {
	l := openLog(t)
	for i := 0; i < 5; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	// afterSeq=3 → should return events 4 and 5
	events, err := l.ReadAfter(context.Background(), 3, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].SequenceNum != 4 || events[1].SequenceNum != 5 {
		t.Errorf("unexpected sequence numbers: %v", []int64{events[0].SequenceNum, events[1].SequenceNum})
	}
}

func TestReadAfter_Limit(t *testing.T) {
	l := openLog(t)
	for i := 0; i < 10; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	events, err := l.ReadAfter(context.Background(), 0, 3)
	if err != nil {
		t.Fatalf("ReadAfter with limit: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}
}

func TestReadAfter_ZeroLimitMeansAll(t *testing.T) {
	l := openLog(t)
	for i := 0; i < 7; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	if len(events) != 7 {
		t.Errorf("expected 7 events, got %d", len(events))
	}
}

func TestReadAfter_Ascending(t *testing.T) {
	l := openLog(t)
	for i := 0; i < 5; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	events, _ := l.ReadAfter(context.Background(), 0, 0)
	for i := 1; i < len(events); i++ {
		if events[i].SequenceNum <= events[i-1].SequenceNum {
			t.Errorf("events not ascending at index %d: %d <= %d",
				i, events[i].SequenceNum, events[i-1].SequenceNum)
		}
	}
}

// --- ReadByType ---

func TestReadByType_Filters(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")
	append1(t, l, schema.EventObligationCreated, "G-1")
	append1(t, l, schema.EventGoalCreated, "G-1")
	append1(t, l, schema.EventCapsuleCreated, "G-1")

	events, err := l.ReadByType(context.Background(), schema.EventGoalCreated, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 goal_created events, got %d", len(events))
	}
	for _, e := range events {
		if e.Type != schema.EventGoalCreated {
			t.Errorf("unexpected event type: %s", e.Type)
		}
	}
}

func TestReadByType_AfterSeq(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1") // seq=1
	append1(t, l, schema.EventGoalCreated, "G-1") // seq=2
	append1(t, l, schema.EventGoalCreated, "G-1") // seq=3

	events, _ := l.ReadByType(context.Background(), schema.EventGoalCreated, 1, 0)
	if len(events) != 2 {
		t.Errorf("expected 2 events after seq=1, got %d", len(events))
	}
}

func TestReadByType_NoMatch(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")
	events, err := l.ReadByType(context.Background(), schema.EventMergeApplied, 0, 0)
	if err != nil {
		t.Fatalf("ReadByType: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

// --- ReadForGoal ---

func TestReadForGoal_Filters(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")
	append1(t, l, schema.EventGoalCreated, "G-2")
	append1(t, l, schema.EventObligationCreated, "G-1")
	append1(t, l, schema.EventObligationCreated, "G-2")

	events, err := l.ReadForGoal(context.Background(), "G-1", 0, 0)
	if err != nil {
		t.Fatalf("ReadForGoal: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for G-1, got %d", len(events))
	}
	for _, e := range events {
		if e.GoalID != "G-1" {
			t.Errorf("unexpected GoalID: %s", e.GoalID)
		}
	}
}

func TestReadForGoal_AfterSeq(t *testing.T) {
	l := openLog(t)
	append1(t, l, schema.EventGoalCreated, "G-1")       // seq=1
	append1(t, l, schema.EventObligationCreated, "G-1") // seq=2
	append1(t, l, schema.EventCapsuleCreated, "G-1")    // seq=3

	events, _ := l.ReadForGoal(context.Background(), "G-1", 2, 0)
	if len(events) != 1 || events[0].SequenceNum != 3 {
		t.Errorf("expected 1 event at seq=3, got %v", events)
	}
}

// --- Durability ---

func TestFileLog_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")

	// Write 5 events in one session.
	l := openLogAt(t, path)
	for i := 0; i < 5; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and verify events are all present.
	l2 := openLogAt(t, path)
	defer l2.Close()
	events, err := l2.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter after reopen: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 events after reopen, got %d", len(events))
	}
}

func TestFileLog_ResumesSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")

	// First session: write 3 events.
	l := openLogAt(t, path)
	for i := 0; i < 3; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second session: write 2 more events.
	l2 := openLogAt(t, path)
	for i := 0; i < 2; i++ {
		append1(t, l2, schema.EventObligationCreated, "G-1")
	}
	defer l2.Close()

	events, _ := l2.ReadAfter(context.Background(), 0, 0)
	if len(events) != 5 {
		t.Fatalf("expected 5 total events, got %d", len(events))
	}
	// Sequence numbers must be 1-5 without gaps or duplicates.
	for i, e := range events {
		if e.SequenceNum != int64(i+1) {
			t.Errorf("events[%d].SequenceNum = %d, want %d", i, e.SequenceNum, i+1)
		}
	}
}

// --- Deterministic replay property ---
//
// Replay verifies the invariant: if we read all events from seq=0 and apply
// them in order, we reconstruct the same list of events (order and content
// preserved). The event log itself is idempotent to read; this test checks
// that every appended event is retrievable in its original form.
func TestFileLog_DeterministicReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	l := openLogAt(t, path)

	type entry struct {
		typ    schema.EventType
		goalID string
	}
	want := []entry{
		{schema.EventGoalCreated, "G-1"},
		{schema.EventObligationCreated, "G-1"},
		{schema.EventCapsuleCreated, "G-1"},
		{schema.EventGoalCreated, "G-2"},
		{schema.EventObligationCreated, "G-2"},
	}
	for _, w := range want {
		append1(t, l, w.typ, w.goalID)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Replay from seq=0 on a fresh handle.
	l2 := openLogAt(t, path)
	defer l2.Close()

	const batchSize = 2
	var got []schema.Event
	var afterSeq int64
	for {
		batch, err := l2.ReadAfter(context.Background(), afterSeq, batchSize)
		if err != nil {
			t.Fatalf("ReadAfter seq=%d: %v", afterSeq, err)
		}
		if len(batch) == 0 {
			break
		}
		got = append(got, batch...)
		afterSeq = batch[len(batch)-1].SequenceNum
	}

	if len(got) != len(want) {
		t.Fatalf("replay: got %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.Type != want[i].typ || e.GoalID != want[i].goalID {
			t.Errorf("replay[%d]: got {%s, %s}, want {%s, %s}",
				i, e.Type, e.GoalID, want[i].typ, want[i].goalID)
		}
		if e.SequenceNum != int64(i+1) {
			t.Errorf("replay[%d].SequenceNum = %d, want %d", i, e.SequenceNum, i+1)
		}
	}
}

// --- Snapshot support ---
//
// Demonstrates reading only the events after a known snapshot sequence number,
// which is the mechanism context projections and claims use for freshness checks.
func TestFileLog_ReadFromSnapshot(t *testing.T) {
	l := openLog(t)
	for i := 0; i < 10; i++ {
		append1(t, l, schema.EventObligationCreated, "G-1")
	}
	// Snapshot was taken at seq=5.
	snapshotSeq := int64(5)
	events, err := l.ReadAfter(context.Background(), snapshotSeq, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 5 {
		t.Errorf("expected 5 events after snapshot seq=%d, got %d", snapshotSeq, len(events))
	}
	if events[0].SequenceNum != 6 {
		t.Errorf("first event seq = %d, want 6", events[0].SequenceNum)
	}
}

// --- Concurrent appends ---

func TestFileLog_ConcurrentAppends(t *testing.T) {
	l := openLog(t)

	const goroutines = 8
	const perGoroutine = 25

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			goalID := fmt.Sprintf("goal-%d", id)
			for j := 0; j < perGoroutine; j++ {
				if _, err := l.Append(context.Background(), schema.Event{
					Type:   schema.EventGoalCreated,
					GoalID: goalID,
				}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	events, err := l.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	total := goroutines * perGoroutine
	if len(events) != total {
		t.Errorf("expected %d events, got %d", total, len(events))
	}

	// All sequence numbers unique and ascending.
	seen := make(map[int64]bool, total)
	for i, e := range events {
		if seen[e.SequenceNum] {
			t.Errorf("duplicate sequence number %d", e.SequenceNum)
		}
		seen[e.SequenceNum] = true
		if i > 0 && e.SequenceNum <= events[i-1].SequenceNum {
			t.Errorf("events not ascending at index %d: %d <= %d",
				i, e.SequenceNum, events[i-1].SequenceNum)
		}
	}
}

func TestFileLog_ConcurrentReadWrite(t *testing.T) {
	const writes = 50
	l := openLog(t)

	// Writer goroutine: collect append errors.
	writeErrs := make(chan error, writes)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			if _, err := l.Append(context.Background(), schema.Event{
				Type:   schema.EventGoalCreated,
				GoalID: "G-1",
			}); err != nil {
				writeErrs <- err
			}
		}
	}()

	// Concurrent readers: collect read errors.
	var wg sync.WaitGroup
	readErrs := make(chan error, 4*writes)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					if _, err := l.ReadAfter(context.Background(), 0, 10); err != nil {
						readErrs <- err
					}
				}
			}
		}()
	}
	<-done
	wg.Wait()
	close(writeErrs)
	close(readErrs)

	for err := range writeErrs {
		t.Errorf("concurrent write error: %v", err)
	}
	for err := range readErrs {
		t.Errorf("concurrent read error: %v", err)
	}

	// All 50 events must be durably present after concurrent access.
	events, err := l.ReadAfter(context.Background(), 0, writes+1)
	if err != nil {
		t.Fatalf("final ReadAfter: %v", err)
	}
	if len(events) != writes {
		t.Errorf("final event count = %d, want %d", len(events), writes)
	}
}

// --- Seek-index correctness ---
//
// These tests verify that the byte-offset index built during Append and Open
// produces identical results to a full scan from byte 0 for every afterSeq value.

// TestReadAfter_SeekIndex_AppendPath verifies that the index built incrementally
// by Append lets ReadAfter seek correctly to any mid-log position.
func TestReadAfter_SeekIndex_AppendPath(t *testing.T) {
	const n = 100
	l := openLog(t)
	for i := 0; i < n; i++ {
		append1(t, l, schema.EventGoalCreated, "G-1")
	}

	cases := []struct {
		afterSeq int64
		wantLen  int
		wantFirst int64
	}{
		{0, n, 1},
		{1, n - 1, 2},
		{50, 50, 51},
		{99, 1, 100},
		{100, 0, 0}, // past the last event
		{200, 0, 0}, // well past the end
	}
	for _, tc := range cases {
		events, err := l.ReadAfter(context.Background(), tc.afterSeq, 0)
		if err != nil {
			t.Fatalf("ReadAfter(%d): %v", tc.afterSeq, err)
		}
		if len(events) != tc.wantLen {
			t.Errorf("ReadAfter(%d): got %d events, want %d", tc.afterSeq, len(events), tc.wantLen)
			continue
		}
		if tc.wantLen > 0 && events[0].SequenceNum != tc.wantFirst {
			t.Errorf("ReadAfter(%d): first seq=%d, want %d", tc.afterSeq, events[0].SequenceNum, tc.wantFirst)
		}
		if tc.wantLen > 0 && events[len(events)-1].SequenceNum != int64(n) {
			t.Errorf("ReadAfter(%d): last seq=%d, want %d", tc.afterSeq, events[len(events)-1].SequenceNum, n)
		}
	}
}

// TestReadAfter_SeekIndex_ScanPath verifies that the index built from the
// initial file scan by Open (not by incremental Append) also produces correct
// seek behaviour. seedLog writes events directly to disk, then Open rescans.
func TestReadAfter_SeekIndex_ScanPath(t *testing.T) {
	const n = 500
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	seedLog(t, path, n)

	l := openLogAt(t, path)

	// Tail read: seek deep into a log whose index was built by Open's scan.
	const tail = 10
	events, err := l.ReadAfter(context.Background(), int64(n-tail), 0)
	if err != nil {
		t.Fatalf("ReadAfter tail: %v", err)
	}
	if len(events) != tail {
		t.Fatalf("ReadAfter tail: got %d events, want %d", len(events), tail)
	}
	if events[0].SequenceNum != int64(n-tail+1) {
		t.Errorf("first seq=%d, want %d", events[0].SequenceNum, n-tail+1)
	}
	if events[len(events)-1].SequenceNum != int64(n) {
		t.Errorf("last seq=%d, want %d", events[len(events)-1].SequenceNum, n)
	}
}

// TestReadAfter_SeekIndex_SeekMatchesFullScan checks that seek and full-scan
// results are identical for a representative set of afterSeq values.
func TestReadAfter_SeekIndex_SeekMatchesFullScan(t *testing.T) {
	const n = 50
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")

	// First session: write events.
	l := openLogAt(t, path)
	for i := 0; i < n; i++ {
		append1(t, l, schema.EventObligationCreated, "G-cmp")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second session: reopen so the index is built by scanMaxSeq.
	l2 := openLogAt(t, path)

	// For each afterSeq, compare seek-based result with full-scan slice.
	fullScan, err := l2.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("full scan: %v", err)
	}

	for afterSeq := int64(0); afterSeq <= int64(n); afterSeq++ {
		got, err := l2.ReadAfter(context.Background(), afterSeq, 0)
		if err != nil {
			t.Fatalf("ReadAfter(%d): %v", afterSeq, err)
		}
		want := fullScan[afterSeq:]
		if len(got) != len(want) {
			t.Errorf("ReadAfter(%d): len=%d, want %d", afterSeq, len(got), len(want))
			continue
		}
		for i := range got {
			if got[i].SequenceNum != want[i].SequenceNum {
				t.Errorf("ReadAfter(%d)[%d]: seq=%d, want %d", afterSeq, i, got[i].SequenceNum, want[i].SequenceNum)
			}
		}
	}
}

// TestReadAfter_SeekIndex_ReadByType verifies that ReadByType also benefits
// from the offset index when afterSeq > 0.
func TestReadAfter_SeekIndex_ReadByType(t *testing.T) {
	const n = 40
	l := openLog(t)
	for i := 0; i < n; i++ {
		typ := schema.EventGoalCreated
		if i%2 == 1 {
			typ = schema.EventObligationCreated
		}
		append1(t, l, typ, "G-1")
	}

	// afterSeq=20 means events 21..40; half of those are obligation_created.
	events, err := l.ReadByType(context.Background(), schema.EventObligationCreated, 20, 0)
	if err != nil {
		t.Fatalf("ReadByType: %v", err)
	}
	// Events 21..40: odd indices (1-based) among [21..40] are obligation_created.
	// seq 21=goal, 22=obl, 23=goal, 24=obl, ... 40=obl → 10 obligation events.
	if len(events) != 10 {
		t.Errorf("ReadByType after afterSeq=20: got %d events, want 10", len(events))
	}
	for _, e := range events {
		if e.Type != schema.EventObligationCreated {
			t.Errorf("unexpected event type: %s", e.Type)
		}
		if e.SequenceNum <= 20 {
			t.Errorf("event seq=%d should be > 20", e.SequenceNum)
		}
	}
}
