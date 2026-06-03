package eventlog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// BenchmarkAppend_Sequential is the single-goroutine baseline.
// The dominant cost is f.Sync() — one fsync per Append call.
func BenchmarkAppend_Sequential(b *testing.B) {
	path := filepath.Join(b.TempDir(), "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer l.Close()

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Append(ctx, schema.Event{
			Type:   schema.EventGoalCreated,
			GoalID: "G-bench",
		}); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

// BenchmarkAppend_Concurrent distributes b.N appends across exactly 50
// goroutines. Because Append holds a write mutex, goroutines queue behind
// each fsync. time/op reflects fsync latency plus mutex contention overhead.
func BenchmarkAppend_Concurrent(b *testing.B) {
	const goroutines = 50

	path := filepath.Join(b.TempDir(), "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer l.Close()

	ctx := context.Background()

	// Pre-fill work channel before starting timer so goroutine-startup
	// overhead and channel allocation are excluded from the measurement.
	work := make(chan int, b.N)
	for i := range b.N {
		work <- i
	}
	close(work)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstErr error
	)

	b.ResetTimer()

	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			goalID := fmt.Sprintf("goal-%d", id)
			for range work {
				if _, err := l.Append(ctx, schema.Event{
					Type:   schema.EventGoalCreated,
					GoalID: goalID,
				}); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		}(i)
	}
	wg.Wait()
	b.StopTimer()

	if firstErr != nil {
		b.Fatalf("Append error: %v", firstErr)
	}
}

// seedLog writes n correctly sequenced JSONL events directly to path without
// calling fsync, so large fixtures can be created quickly for read benchmarks.
func seedLog(tb testing.TB, path string, n int) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("seedLog create: %v", err)
	}
	defer f.Close()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(schema.Event{
			EventID:     fmt.Sprintf("ev-%010d", i),
			Type:        schema.EventGoalCreated,
			GoalID:      "G-bench",
			CreatedAt:   ts,
			SequenceNum: int64(i),
		})
		if err != nil {
			tb.Fatalf("seedLog marshal i=%d: %v", i, err)
		}
		line = append(line, '\n')
		if _, err := f.Write(line); err != nil {
			tb.Fatalf("seedLog write i=%d: %v", i, err)
		}
	}
}

// BenchmarkReadAfter_LogGrowth measures ReadAfter scan cost as the log grows.
// ReadAfter scans from byte 0 of the file on every call, so latency is O(n)
// in the number of events. Sub-benchmarks at 1 K, 10 K, 100 K, and 1 M events
// make the linear scaling directly observable.
//
// Note: the 1 M sub-benchmark produces a ~160 MB log file and each ReadAfter
// call takes several seconds; the Go test framework will run it only once
// (b.N=1). Use -benchtime=1x to force a single iteration for smaller sizes too.
func BenchmarkReadAfter_LogGrowth(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "events.log")
			seedLog(b, path, n)

			l, err := eventlog.Open(path)
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			defer l.Close()

			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				events, err := l.ReadAfter(ctx, 0, 0)
				if err != nil {
					b.Fatalf("ReadAfter: %v", err)
				}
				if len(events) != n {
					b.Fatalf("ReadAfter returned %d events, want %d", len(events), n)
				}
			}
		})
	}
}

// BenchmarkReadAfter_AfterSeq demonstrates the seek-index optimisation.
// All three sub-benchmarks read a 100 K-event log but request progressively
// smaller tail windows. Because ReadAfter now seeks to the first event after
// afterSeq using a byte-offset index, tail-1k is ~100× faster than scan-all
// instead of the near-identical times seen before the optimisation.
func BenchmarkReadAfter_AfterSeq(b *testing.B) {
	const total = 100_000

	cases := []struct {
		name     string
		afterSeq int64
		want     int
	}{
		{"tail-1k", int64(total - 1_000), 1_000},
		{"tail-10k", int64(total - 10_000), 10_000},
		{"scan-all", 0, total},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "events.log")
			seedLog(b, path, total)

			l, err := eventlog.Open(path)
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			defer l.Close()

			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				events, err := l.ReadAfter(ctx, tc.afterSeq, 0)
				if err != nil {
					b.Fatalf("ReadAfter: %v", err)
				}
				if len(events) != tc.want {
					b.Fatalf("ReadAfter returned %d events, want %d", len(events), tc.want)
				}
			}
		})
	}
}
