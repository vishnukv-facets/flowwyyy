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
