package server

import (
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

// The queue is persistent: push/peek/ack round-trip through the pending_wakes
// table so a buffered wake survives a process restart.
func TestWakeQueuePersistentRoundTrip(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	q := newWakeQueue(db)

	if q.has("a") {
		t.Fatal("empty queue should not report has")
	}
	if err := q.push("a", "first"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := q.push("a", "second"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !q.has("a") {
		t.Fatal("queue with items should report has")
	}

	pw, ok := q.peek("a")
	if !ok || pw.Prompt != "first" {
		t.Fatalf("peek = %q,%v; want first,true", pw.Prompt, ok)
	}
	q.ack(pw.ID)
	pw, ok = q.peek("a")
	if !ok || pw.Prompt != "second" {
		t.Fatalf("after ack, peek = %q,%v; want second,true", pw.Prompt, ok)
	}
	q.ack(pw.ID)
	if q.has("a") {
		t.Fatal("drained queue should not report has")
	}

	notBefore := "2026-06-25T12:00:00Z"
	if err := q.pushAfter("a", "later", notBefore); err != nil {
		t.Fatalf("pushAfter: %v", err)
	}
	pw, ok = q.peek("a")
	if !ok || pw.NotBefore != notBefore {
		t.Fatalf("pushAfter peek = %+v,%v; want not_before %q", pw, ok, notBefore)
	}
}

// beginFlush is a per-slug mutex gate: only one flush goroutine drains a slug at
// a time, so buffered pastes are delivered serially (never interleaved).
func TestWakeQueueFlushGuard(t *testing.T) {
	q := newWakeQueue(nil) // guard is in-memory; no DB needed
	if !q.beginFlush("a") {
		t.Fatal("first beginFlush should win")
	}
	if q.beginFlush("a") {
		t.Fatal("second beginFlush should be refused while flushing")
	}
	if !q.beginFlush("b") {
		t.Fatal("a different slug should flush independently")
	}
	q.endFlush("a")
	if !q.beginFlush("a") {
		t.Fatal("beginFlush should win again after endFlush")
	}
}
