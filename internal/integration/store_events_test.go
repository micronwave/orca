package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// readGoalEvents reads all events for goalID from log, returning them in order.
func readGoalEvents(t *testing.T, log *eventlog.FileLog, goalID string) []schema.Event {
	t.Helper()
	var all []schema.Event
	var seq int64
	for {
		batch, err := log.ReadForGoal(context.Background(), goalID, seq, 200)
		if err != nil {
			t.Fatalf("ReadForGoal: %v", err)
		}
		if len(batch) == 0 {
			return all
		}
		all = append(all, batch...)
		seq = batch[len(batch)-1].SequenceNum
	}
}

// TestStore_SaveMethodsEmitExactlyOneEvent is a behavioral replacement for the
// source-text heuristic that counted s.appendEvent calls in filestore_external.go.
// It verifies that each Save* method emits exactly one event with the correct
// type, goal ID, and artifact ID — proving event count, type, and ID semantics
// rather than relying on text scanning.
func TestStore_SaveMethodsEmitExactlyOneEvent(t *testing.T) {
	dir := t.TempDir()
	log, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer log.Close()

	st, err := store.New(dir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	ctx := context.Background()
	goalID := "G-EVT-TEST"
	now := time.Now().UTC()

	goal := &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "event emission test",
		Status:         schema.GoalStatusActive,
		RiskLevel:      schema.RiskLow,
		CreatedAt:      now,
	}
	if err := st.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	cases := []struct {
		name      string
		artifactID string
		wantType  schema.EventType
		save      func() error
	}{
		{
			name:       "SavePRRecord",
			artifactID: "PR-EVT-1",
			wantType:   schema.EventPRCreated,
			save: func() error {
				return st.SavePRRecord(ctx, goalID, &schema.PRRecord{
					PRID:      "PR-EVT-1",
					GoalID:    goalID,
					PatchID:   "PATCH-EVT-1",
					PRURL:     "https://example.test/pr/1",
					CreatedAt: now,
				})
			},
		},
		{
			name:       "SaveCIStatusRecord",
			artifactID: "CI-EVT-1",
			wantType:   schema.EventCIStatusReceived,
			save: func() error {
				return st.SaveCIStatusRecord(ctx, goalID, &schema.CIStatusRecord{
					RecordID:   "CI-EVT-1",
					GoalID:     goalID,
					CapsuleID:  "CAP-EVT-1",
					Provider:   "github",
					Branch:     "test-branch",
					Status:     "success",
					RecordedAt: now,
				})
			},
		},
		{
			name:       "SaveIntakeRecord",
			artifactID: "INTAKE-EVT-1",
			wantType:   schema.EventIntakeIssueIngested,
			save: func() error {
				return st.SaveIntakeRecord(ctx, goalID, &schema.IntakeRecord{
					RecordID:   "INTAKE-EVT-1",
					GoalID:     goalID,
					Source:     "github",
					ExternalID: "issue-42",
					IngestedAt: now,
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := readGoalEvents(t, log, goalID)

			if err := tc.save(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			after := readGoalEvents(t, log, goalID)
			newEvents := after[len(before):]
			if len(newEvents) != 1 {
				t.Fatalf("%s: emitted %d events, want exactly 1", tc.name, len(newEvents))
			}
			ev := newEvents[0]
			if ev.Type != tc.wantType {
				t.Errorf("%s: event type = %q, want %q", tc.name, ev.Type, tc.wantType)
			}
			if ev.GoalID != goalID {
				t.Errorf("%s: event goal_id = %q, want %q", tc.name, ev.GoalID, goalID)
			}
			if ev.ArtifactID != tc.artifactID {
				t.Errorf("%s: event artifact_id = %q, want %q", tc.name, ev.ArtifactID, tc.artifactID)
			}
		})
	}
}
