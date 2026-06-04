package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

type countingContext struct {
	context.Context
	allowErrCalls int32
	errCalls      atomic.Int32
}

func (c *countingContext) Err() error {
	call := c.errCalls.Add(1)
	if call <= c.allowErrCalls {
		return nil
	}
	return context.Canceled
}

func TestWriteFile_LateContextCancellationStillSucceeds(t *testing.T) {
	st := &FileStore{}
	path := filepath.Join(t.TempDir(), "artifact.json")

	ctx := &countingContext{Context: context.Background(), allowErrCalls: 1}
	if err := st.writeFile(ctx, path, map[string]string{"id": "A-1"}); err != nil {
		t.Fatalf("writeFile late-cancel error = %v, want nil after committed rename", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["id"] != "A-1" {
		t.Fatalf("written payload = %#v, want id A-1", got)
	}
}

func TestWriteFile_CompactJSONAndReplayIndented(t *testing.T) {
	st := &FileStore{}
	dir := t.TempDir()
	compactPath := filepath.Join(dir, "compact.json")
	replayPath := filepath.Join(dir, "replay.json")
	payload := struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{
		ID:   "A-1",
		Name: "artifact",
	}

	if err := st.writeFile(context.Background(), compactPath, payload); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := st.writeFileReplay(context.Background(), replayPath, payload); err != nil {
		t.Fatalf("writeFileReplay: %v", err)
	}

	compact, err := os.ReadFile(compactPath)
	if err != nil {
		t.Fatalf("ReadFile compact: %v", err)
	}
	replay, err := os.ReadFile(replayPath)
	if err != nil {
		t.Fatalf("ReadFile replay: %v", err)
	}

	if string(compact) != `{"id":"A-1","name":"artifact"}` {
		t.Fatalf("compact JSON = %q, want single-line marshaled JSON", string(compact))
	}
	if string(replay) != "{\n  \"id\": \"A-1\",\n  \"name\": \"artifact\"\n}" {
		t.Fatalf("replay JSON = %q, want indented JSON", string(replay))
	}
}

// TestScanDir_BoundedSemaphoreNoDeadlock verifies that scanDir correctly handles
// directories with more than the semaphore bound (16) of files without deadlocking,
// and returns all files.
func TestScanDir_BoundedSemaphoreNoDeadlock(t *testing.T) {
	const fileCount = 30
	dir := t.TempDir()
	type item struct {
		ID    string `json:"id"`
		Value int    `json:"value"`
	}
	for i := range fileCount {
		data, err := json.Marshal(item{ID: fmt.Sprintf("item-%02d", i), Value: i})
		if err != nil {
			t.Fatalf("marshal item %d: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("item-%02d.json", i)), data, 0o644); err != nil {
			t.Fatalf("WriteFile item %d: %v", i, err)
		}
	}
	got, err := scanDir[item](context.Background(), dir)
	if err != nil {
		t.Fatalf("scanDir: %v", err)
	}
	if len(got) != fileCount {
		t.Fatalf("got %d items, want %d", len(got), fileCount)
	}
}

func TestSaveObligation_LateContextCancellationStillUpdatesOpenIndex(t *testing.T) {
	root := t.TempDir()
	log, err := eventlog.Open(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer log.Close()

	st, err := New(root, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := st.SaveGoal(context.Background(), &schema.GoalIR{
		GoalID:         "G-1",
		OriginalIntent: "test",
		GoalConditions: []schema.GoalCondition{{
			ID:                   "GC-1",
			Description:          "condition",
			EffectiveDescription: "condition",
			Status:               schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusComplete,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	// SaveObligation performs seven ctx.Err checks before the committed write
	// returns. Canceling on the next check reproduces the late-cancel edge case
	// without racing the filesystem.
	lateCancelCtx := &countingContext{Context: context.Background(), allowErrCalls: 7}
	if err := st.SaveObligation(lateCancelCtx, &schema.Obligation{
		ObligationID:    "OB-1",
		GoalConditionID: "GC-1",
		Description:     "prove it",
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation late-cancel error = %v, want nil after committed write", err)
	}

	open, err := st.LoadOpenObligations(context.Background(), "G-1")
	if err != nil {
		t.Fatalf("LoadOpenObligations: %v", err)
	}
	if len(open) != 1 || open[0].ObligationID != "OB-1" {
		t.Fatalf("open obligations = %#v, want OB-1 indexed as open", open)
	}
}
