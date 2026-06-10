package server

import (
	"strconv"
	"testing"

	"flow/internal/steering"
)

func TestSteeringRunStoreFoldsStagesAndFillsThreadKey(t *testing.T) {
	store := newSteeringRunStore()
	// "received" has no thread key yet (Stage 0 sets it); a later stage carries it.
	store.record(steering.StageEvent{RunID: "r1", Stage: "received", Status: "running", At: "t0"})
	store.record(steering.StageEvent{RunID: "r1", ThreadKey: "C1:1.1", Source: "slack", Stage: "stage0", Status: "passed", At: "t1"})
	last := store.record(steering.StageEvent{RunID: "r1", ThreadKey: "C1:1.1", Stage: "verdict", Status: "surfaced", At: "t2"})

	if !last.Done {
		t.Fatalf("run should be Done after the verdict stage")
	}
	if last.Status != "surfaced" {
		t.Fatalf("Status = %q, want surfaced", last.Status)
	}
	if last.ThreadKey != "C1:1.1" || last.Source != "slack" {
		t.Fatalf("thread key/source not backfilled: %+v", last)
	}
	if len(last.Stages) != 3 {
		t.Fatalf("stage count = %d, want 3", len(last.Stages))
	}
	if last.StartedAt != "t0" || last.UpdatedAt != "t2" {
		t.Fatalf("timestamps = %s..%s, want t0..t2", last.StartedAt, last.UpdatedAt)
	}

	snap := store.snapshot()
	if len(snap) != 1 || snap[0].RunID != "r1" {
		t.Fatalf("snapshot = %+v, want one run r1", snap)
	}
}

func TestSteeringRunStoreFoldsStreamingUpdatesInPlace(t *testing.T) {
	store := newSteeringRunStore()
	store.record(steering.StageEvent{RunID: "r1", ThreadKey: "C1:9.1", Stage: "stage3", Status: "running", At: "t0"})
	// Two streaming deltas for the same stage must update the row in place, not
	// append a row per chunk.
	store.record(steering.StageEvent{RunID: "r1", Stage: "stage3", Status: "running", Stream: "weighing", At: "t1"})
	last := store.record(steering.StageEvent{RunID: "r1", Stage: "stage3", Status: "running", Stream: "weighing the thread…", At: "t2"})

	if len(last.Stages) != 1 {
		t.Fatalf("stage count = %d, want 1 (streaming updates fold in place)", len(last.Stages))
	}
	if last.Stages[0].Stream != "weighing the thread…" {
		t.Fatalf("stream = %q, want the latest accumulated text", last.Stages[0].Stream)
	}
	if last.UpdatedAt != "t2" {
		t.Fatalf("UpdatedAt = %q, want t2", last.UpdatedAt)
	}
	// A subsequent distinct stage still appends.
	after := store.record(steering.StageEvent{RunID: "r1", Stage: "verdict", Status: "surfaced", At: "t3"})
	if len(after.Stages) != 2 || !after.Done {
		t.Fatalf("after verdict: stages=%d done=%v, want 2/true", len(after.Stages), after.Done)
	}
}

func TestSteeringRunStoreEvictsOldestOverCap(t *testing.T) {
	store := newSteeringRunStore()
	total := steeringRunCap + 10
	for i := 0; i < total; i++ {
		store.record(steering.StageEvent{RunID: "run-" + strconv.Itoa(i), Stage: "received", Status: "running", At: "t"})
	}
	snap := store.snapshot()
	if len(snap) != steeringRunCap {
		t.Fatalf("snapshot size = %d, want cap %d", len(snap), steeringRunCap)
	}
	// Newest-first: the most recent run leads, the oldest survivors are gone.
	if snap[0].RunID != "run-"+strconv.Itoa(total-1) {
		t.Fatalf("newest run = %q, want run-%d", snap[0].RunID, total-1)
	}
	for _, r := range snap {
		if r.RunID == "run-0" {
			t.Fatalf("oldest run-0 should have been evicted")
		}
	}
}
