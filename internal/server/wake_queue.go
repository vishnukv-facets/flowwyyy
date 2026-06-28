package server

import (
	"database/sql"
	"sync"

	"flow/internal/flowdb"
)

// wakeQueue is the persistent buffer of wake prompts that are ready to deliver
// once a session is safe to receive input (AskUserQuestion / permission prompts
// are clear, not_before holds elapsed). Paused-session input first lands in
// paused_session_queue, then moves here when resume starts.
// The only in-memory state is the per-slug flush guard (concurrency control
// within one process); the queue itself is the database.
type wakeQueue struct {
	db       *sql.DB
	mu       sync.Mutex
	flushing map[string]bool
}

func newWakeQueue(db *sql.DB) *wakeQueue {
	return &wakeQueue{db: db, flushing: map[string]bool{}}
}

// push appends a prompt to slug's persistent queue.
func (q *wakeQueue) push(slug, prompt string) error {
	_, err := flowdb.EnqueuePendingWake(q.db, slug, prompt)
	return err
}

// pushAfter appends a prompt that cannot be flushed until notBefore.
func (q *wakeQueue) pushAfter(slug, prompt, notBefore string) error {
	_, err := flowdb.EnqueuePendingWakeAfter(q.db, slug, prompt, notBefore)
	return err
}

// peek returns the oldest buffered wake for slug without removing it. ok=false
// when empty. Non-destructive so a row survives a failed/interrupted delivery
// and is dropped only by ack after a confirmed inject.
func (q *wakeQueue) peek(slug string) (flowdb.PendingWake, bool) {
	pw, ok, err := flowdb.PeekPendingWake(q.db, slug)
	if err != nil {
		return flowdb.PendingWake{}, false
	}
	return pw, ok
}

// ack removes a delivered wake row.
func (q *wakeQueue) ack(id int64) {
	_ = flowdb.AckPendingWake(q.db, id)
}

// has reports whether slug has any buffered wake.
func (q *wakeQueue) has(slug string) bool {
	has, err := flowdb.HasPendingWakes(q.db, slug)
	return err == nil && has
}

// beginFlush marks slug as flushing and returns true if the caller owns the
// flush. A concurrent caller gets false and must not drain (the owner will), so
// buffered pastes are delivered serially and never interleave on the PTY.
func (q *wakeQueue) beginFlush(slug string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.flushing[slug] {
		return false
	}
	q.flushing[slug] = true
	return true
}

// endFlush releases the flush ownership taken by beginFlush.
func (q *wakeQueue) endFlush(slug string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.flushing, slug)
}
