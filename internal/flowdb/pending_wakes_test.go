package flowdb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Pending wakes are a persistent FIFO per slug: enqueue appends, peek returns
// the oldest, ack removes it. This is what lets a buffered wake survive a
// `flow ui serve` restart instead of living only in memory.
func TestPendingWakesFIFOAndAck(t *testing.T) {
	db := openTestDB(t)

	if _, ok, err := PeekPendingWake(db, "a"); err != nil || ok {
		t.Fatalf("peek empty: ok=%v err=%v; want false,nil", ok, err)
	}

	id1, err := EnqueuePendingWake(db, "a", "first")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := EnqueuePendingWake(db, "a", "second"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := EnqueuePendingWake(db, "b", "other-slug"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	pw, ok, err := PeekPendingWake(db, "a")
	if err != nil || !ok {
		t.Fatalf("peek a: ok=%v err=%v", ok, err)
	}
	if pw.Prompt != "first" || pw.ID != id1 {
		t.Fatalf("peek a = %q (id %d); want first (id %d)", pw.Prompt, pw.ID, id1)
	}
	if pw.NotBefore != "" {
		t.Fatalf("NotBefore = %q; want empty for immediate wake", pw.NotBefore)
	}

	// Peek is non-destructive — same row until acked.
	if pw2, _, _ := PeekPendingWake(db, "a"); pw2.ID != id1 {
		t.Fatalf("peek should be idempotent until ack; got id %d", pw2.ID)
	}

	if err := AckPendingWake(db, id1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pw, ok, err = PeekPendingWake(db, "a")
	if err != nil || !ok || pw.Prompt != "second" {
		t.Fatalf("after ack, peek a = %q ok=%v err=%v; want second", pw.Prompt, ok, err)
	}

	// Slug isolation preserved.
	if pw, ok, _ := PeekPendingWake(db, "b"); !ok || pw.Prompt != "other-slug" {
		t.Fatalf("peek b = %q ok=%v; want other-slug", pw.Prompt, ok)
	}
}

func TestPendingWakesHasAndSlugs(t *testing.T) {
	db := openTestDB(t)
	if has, _ := HasPendingWakes(db, "a"); has {
		t.Fatal("empty: HasPendingWakes should be false")
	}
	EnqueuePendingWake(db, "a", "x")
	EnqueuePendingWake(db, "b", "y")
	if has, _ := HasPendingWakes(db, "a"); !has {
		t.Fatal("HasPendingWakes(a) should be true")
	}
	slugs, err := PendingWakeSlugs(db)
	if err != nil {
		t.Fatalf("slugs: %v", err)
	}
	if len(slugs) != 2 {
		t.Fatalf("PendingWakeSlugs = %v; want 2 distinct slugs", slugs)
	}
}

func TestPendingWakesNotBefore(t *testing.T) {
	db := openTestDB(t)
	notBefore := "2026-06-25T12:00:00Z"
	id, err := EnqueuePendingWakeAfter(db, "a", "later", notBefore)
	if err != nil {
		t.Fatalf("enqueue after: %v", err)
	}
	pw, ok, err := PeekPendingWake(db, "a")
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	if pw.ID != id || pw.NotBefore != notBefore {
		t.Fatalf("pending wake = %+v; want id %d not_before %q", pw, id, notBefore)
	}
}

func TestRateLimitQueueReadyAndReschedule(t *testing.T) {
	db := openTestDB(t)
	if _, err := EnqueueRateLimitQueue(db, RateLimitQueueSlackEvent, "claude", []byte(`{"kind":"message"}`), "2026-06-25T10:00:00Z"); err != nil {
		t.Fatalf("enqueue slack: %v", err)
	}
	id2, err := EnqueueRateLimitQueue(db, RateLimitQueueOpenTask, "codex", []byte(`{"slug":"demo"}`), "2026-06-25T12:00:00Z")
	if err != nil {
		t.Fatalf("enqueue open: %v", err)
	}
	ready, err := ListReadyRateLimitQueue(db, "2026-06-25T11:00:00Z", 10)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) != 1 || ready[0].Kind != RateLimitQueueSlackEvent {
		t.Fatalf("ready = %+v; want only slack event", ready)
	}
	next, ok, err := NextRateLimitQueueRunAfter(db)
	if err != nil || !ok || next != "2026-06-25T10:00:00Z" {
		t.Fatalf("next = %q ok=%v err=%v", next, ok, err)
	}
	if err := AckRateLimitQueue(db, ready[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := RescheduleRateLimitQueue(db, id2, "2026-06-25T13:00:00Z", "still limited"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	ready, err = ListReadyRateLimitQueue(db, "2026-06-25T12:30:00Z", 10)
	if err != nil {
		t.Fatalf("ready after reschedule: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready after reschedule = %+v; want none", ready)
	}
}
