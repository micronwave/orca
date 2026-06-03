package eventlog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// appendN appends n events to l and returns the assigned events.
func appendN(t *testing.T, l *eventlog.FileLog, n int) []schema.Event {
	t.Helper()
	ctx := context.Background()
	out := make([]schema.Event, n)
	for i := range n {
		e, err := l.Append(ctx, schema.Event{
			Type:   schema.EventGoalCreated,
			GoalID: fmt.Sprintf("G-%03d", i+1),
		})
		if err != nil {
			t.Fatalf("Append i=%d: %v", i, err)
		}
		out[i] = e
	}
	return out
}

// collectScanner drains sc and returns all events.
func collectScanner(t *testing.T, sc *eventlog.Scanner) []schema.Event {
	t.Helper()
	ctx := context.Background()
	var out []schema.Event
	for {
		e, ok, err := sc.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		out = append(out, e)
	}
	return out
}

// ── unit tests ────────────────────────────────────────────────────────────────

func TestScanner_Empty(t *testing.T) {
	l := openLog(t)
	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	defer sc.Close()

	_, ok, err := sc.Next(context.Background())
	if err != nil {
		t.Fatalf("Next on empty log: %v", err)
	}
	if ok {
		t.Fatal("Next on empty log returned ok=true, want false")
	}
}

func TestScanner_All(t *testing.T) {
	l := openLog(t)
	want := appendN(t, l, 10)

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	defer sc.Close()

	got := collectScanner(t, sc)
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.SequenceNum != want[i].SequenceNum {
			t.Errorf("event[%d] seq=%d, want %d", i, e.SequenceNum, want[i].SequenceNum)
		}
		if e.GoalID != want[i].GoalID {
			t.Errorf("event[%d] goalID=%q, want %q", i, e.GoalID, want[i].GoalID)
		}
	}
}

func TestScanner_AfterSeq(t *testing.T) {
	l := openLog(t)
	all := appendN(t, l, 20)

	for _, afterSeq := range []int64{0, 5, 10, 19, 20} {
		t.Run(fmt.Sprintf("after=%d", afterSeq), func(t *testing.T) {
			sc, err := l.NewScanner(afterSeq)
			if err != nil {
				t.Fatalf("NewScanner(%d): %v", afterSeq, err)
			}
			defer sc.Close()

			got := collectScanner(t, sc)
			wantCount := int(int64(len(all)) - afterSeq)
			if wantCount < 0 {
				wantCount = 0
			}
			if len(got) != wantCount {
				t.Fatalf("got %d events after seq=%d, want %d", len(got), afterSeq, wantCount)
			}
			for i, e := range got {
				wantSeq := afterSeq + int64(i) + 1
				if e.SequenceNum != wantSeq {
					t.Errorf("event[%d] seq=%d, want %d", i, e.SequenceNum, wantSeq)
				}
			}
		})
	}
}

// TestScanner_StopsAtMaxSeq verifies that events appended AFTER the scanner
// is opened are not visible to it.
func TestScanner_StopsAtMaxSeq(t *testing.T) {
	l := openLog(t)
	appendN(t, l, 5)

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	defer sc.Close()

	// Append 5 more events after the scanner is open.
	appendN(t, l, 5)

	got := collectScanner(t, sc)
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5 (the 5 appended before scanner-open)", len(got))
	}
}

// TestScanner_CloseIdempotent verifies Close may be called multiple times.
func TestScanner_CloseIdempotent(t *testing.T) {
	l := openLog(t)
	appendN(t, l, 3)

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}

	if err := sc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestScanner_NextAfterClose returns (_, false, nil) after Close, not an error.
func TestScanner_NextAfterClose(t *testing.T) {
	l := openLog(t)
	appendN(t, l, 3)

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	sc.Close()

	// After Close the FD is released; Next must not panic or return an I/O error.
	_, ok, err := sc.Next(context.Background())
	if err != nil {
		t.Fatalf("Next after Close returned error: %v", err)
	}
	if ok {
		t.Fatal("Next after Close returned ok=true, want false")
	}
}

// TestScanner_ContextCancellation verifies Next honours ctx cancellation.
func TestScanner_ContextCancellation(t *testing.T) {
	l := openLog(t)
	appendN(t, l, 3)

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	defer sc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = sc.Next(ctx)
	if err == nil {
		t.Fatal("Next with cancelled context returned nil error, want context.Canceled")
	}
}

// TestScanner_ClosedLog verifies NewScanner returns ErrClosed on a closed log.
func TestScanner_ClosedLog(t *testing.T) {
	l := openLog(t)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := l.NewScanner(0)
	if err == nil {
		t.Fatal("NewScanner on closed log returned nil error, want ErrClosed")
	}
}

// TestScanner_ExistingLog opens a log that was pre-written without fsync
// (same pattern as replay stress) and reads all events via Scanner.
func TestScanner_ExistingLog(t *testing.T) {
	const n = 1_000
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")

	// Write log without fsync (mirrors seedStressLog).
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(schema.Event{
			EventID:     fmt.Sprintf("ev-%06d", i),
			Type:        schema.EventGoalCreated,
			GoalID:      "G-scanner",
			CreatedAt:   ts,
			SequenceNum: int64(i),
		})
		if err != nil {
			t.Fatalf("marshal i=%d: %v", i, err)
		}
		line = append(line, '\n')
		if _, err := f.Write(line); err != nil {
			t.Fatalf("write i=%d: %v", i, err)
		}
	}
	f.Close()

	l, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	sc, err := l.NewScanner(0)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	defer sc.Close()

	got := collectScanner(t, sc)
	if len(got) != n {
		t.Fatalf("got %d events, want %d", len(got), n)
	}
	for i, e := range got {
		if e.SequenceNum != int64(i+1) {
			t.Errorf("event[%d] seq=%d, want %d", i, e.SequenceNum, i+1)
		}
	}
}
