package store_test

// Run with:
//
//	go test ./internal/store/... -run TestReplay_Stress -v -timeout 15m

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

const stressEventCount = 100_000

// TestReplay_Stress seeds a 100,000-event log, wipes the artifact directories,
// runs store.Replay from sequence 0, then reports:
//
//   - wall time and events/sec
//   - heap size before and after (to detect memory growth during bulk unmarshal)
//   - total bytes allocated during replay
//   - artifact files created on disk
//
// The log is written without per-event fsync so fixture creation is fast;
// only store.Replay is on the critical path.
func TestReplay_Stress(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")

	t.Log("seeding event log …")
	seedStressLog(t, logPath, stressEventCount)

	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	t.Logf("log file: %.2f MB  (%d bytes)", float64(fi.Size())/1e6, fi.Size())

	l, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("Open log: %v", err)
	}
	defer l.Close()

	st, err := store.New(dir, l)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Force a GC so the before-snapshot is clean.
	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	t.Log("running store.Replay …")
	start := time.Now()

	if err := store.Replay(context.Background(), l, st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	elapsed := time.Since(start)

	// Second GC so HeapInuse reflects live objects only.
	runtime.GC()
	runtime.ReadMemStats(&mAfter)

	fileCount := countArtifactFiles(t, dir)

	evPerSec := float64(stressEventCount) / elapsed.Seconds()
	heapBefore := float64(mBefore.HeapInuse) / 1e6
	heapAfter := float64(mAfter.HeapInuse) / 1e6
	allocated := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1e6

	t.Logf("─────────────────────────────────")
	t.Logf("REPLAY STRESS  n=%d", stressEventCount)
	t.Logf("  duration      %v", elapsed.Round(time.Millisecond))
	t.Logf("  throughput    %.0f events/sec", evPerSec)
	t.Logf("  files created %d", fileCount)
	t.Logf("  heap before   %.1f MB", heapBefore)
	t.Logf("  heap after    %.1f MB", heapAfter)
	t.Logf("  heap delta    %+.1f MB", heapAfter-heapBefore)
	t.Logf("  total alloc   %.1f MB", allocated)
	t.Logf("─────────────────────────────────")

	if fileCount == 0 {
		t.Error("replay produced no artifact files — replay may have silently skipped events")
	}
}

// seedStressLog writes n events in a round-robin of four creation types:
// goal_created, obligation_created, capsule_created, verifier_result_created.
// Each event has a unique artifact ID so every event materialises a new file.
// The file is written without per-line fsync so the fixture can be created quickly.
func seedStressLog(tb testing.TB, path string, n int) {
	tb.Helper()

	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("seedStressLog create: %v", err)
	}
	defer f.Close()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 1; i <= n; i++ {
		var evType schema.EventType
		var payload any

		switch i % 4 {
		case 1:
			id := fmt.Sprintf("G-%07d", i)
			evType = schema.EventGoalCreated
			payload = &schema.GoalIR{
				GoalID:         id,
				OriginalIntent: "stress test goal",
				Status:         schema.GoalStatusActive,
				RiskLevel:      schema.RiskLow,
				CreatedAt:      ts,
			}
		case 2:
			id := fmt.Sprintf("OB-%07d", i)
			evType = schema.EventObligationCreated
			payload = &schema.Obligation{
				ObligationID: id,
				Description:  "stress test obligation",
				Status:       schema.ObligationOpen,
				RiskLevel:    schema.RiskLow,
			}
		case 3:
			id := fmt.Sprintf("CAP-%07d", i)
			evType = schema.EventCapsuleCreated
			payload = &schema.ExecutionCapsule{
				CapsuleID: id,
				Agent:     schema.AgentMock,
				Role:      schema.RoleExecutor,
				State:     schema.CapsuleStatePending,
			}
		default: // 0
			id := fmt.Sprintf("VR-%07d", i)
			evType = schema.EventVerifierResultCreated
			payload = &schema.VerifierResult{
				VerifierResultID:  id,
				RecommendedAction: schema.ActionAccept,
				CreatedAt:         ts,
			}
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			tb.Fatalf("marshal payload i=%d: %v", i, err)
		}

		line, err := json.Marshal(schema.Event{
			EventID:     fmt.Sprintf("ev-%010d", i),
			Type:        evType,
			GoalID:      "G-stress",
			Payload:     json.RawMessage(payloadBytes),
			CreatedAt:   ts,
			SequenceNum: int64(i),
		})
		if err != nil {
			tb.Fatalf("marshal event i=%d: %v", i, err)
		}

		line = append(line, '\n')
		if _, err := f.Write(line); err != nil {
			tb.Fatalf("write event i=%d: %v", i, err)
		}
	}
}

// countArtifactFiles walks dir and counts every .json file.
func countArtifactFiles(tb testing.TB, dir string) int {
	tb.Helper()
	var n int
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(d.Name()) == ".json" {
			n++
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("countArtifactFiles: %v", err)
	}
	return n
}
