package server

import (
	"context"
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

func TestSteeringRunStoreFoldsAndStripsOrigin(t *testing.T) {
	store := newSteeringRunStore()
	// The "received" stage carries the full origin (newTrace sets it before any
	// stage runs), so even a run that drops at Stage 0 has a channel/author.
	last := store.record(steering.StageEvent{
		RunID: "r1", Stage: "received", Status: "running", At: "t0",
		Source: "slack", ThreadKey: "C1:1.1",
		Channel: "C1", ChannelType: "channel", Author: "U1",
		TS: "1.1", TeamID: "T1", URL: "https://x",
	})
	if last.Channel != "C1" || last.ChannelType != "channel" || last.Author != "U1" {
		t.Fatalf("origin not folded onto run: %+v", last)
	}
	if last.TS != "1.1" || last.TeamID != "T1" || last.URL != "https://x" {
		t.Fatalf("resolver inputs not folded onto run: ts=%q team=%q url=%q", last.TS, last.TeamID, last.URL)
	}
	// The per-stage copy must NOT repeat the origin — it lives on the run.
	if len(last.Stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(last.Stages))
	}
	st := last.Stages[0]
	if st.Channel != "" || st.ChannelType != "" || st.Author != "" || st.TS != "" || st.TeamID != "" || st.URL != "" {
		t.Fatalf("origin not stripped from per-stage copy: %+v", st)
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

func TestSteeringRunOriginResolution(t *testing.T) {
	s := &Server{} // nil nameResolver — exercises github + derivation + DM-fallback paths

	// GitHub fields are already human; the item URL is the canonical permalink.
	// This is the fix for the live view's "untracked event" — a GitHub run that
	// drops at Stage 0 still carries owner/repo.
	gh := steeringRun{Source: "github", Channel: "owner/repo", Author: "octocat", URL: "https://github.com/owner/repo/pull/5"}
	s.resolveSteeringRunOrigin(context.Background(), &gh)
	if gh.ChannelName != "owner/repo" || gh.AuthorName != "octocat" || gh.Permalink != "https://github.com/owner/repo/pull/5" {
		t.Fatalf("github resolution = %+v", gh)
	}

	// Slack DM with no resolver available: falls back to a human label rather
	// than a raw D-channel id.
	dm := steeringRun{Source: "slack", Channel: "D1", ChannelType: "im", ThreadKey: "D1:1.1", TS: "1.1"}
	s.resolveSteeringRunOrigin(context.Background(), &dm)
	if dm.ChannelName != "Direct message" {
		t.Fatalf("DM channel name = %q, want %q", dm.ChannelName, "Direct message")
	}

	// A run carrying only a thread_key: the channel is derived from it so the
	// UI's channel || channel_name fallback isn't blank.
	tk := steeringRun{Source: "slack", ThreadKey: "C9:2.2"}
	s.resolveSteeringRunOrigin(context.Background(), &tk)
	if tk.Channel != "C9" {
		t.Fatalf("derived channel = %q, want C9", tk.Channel)
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
