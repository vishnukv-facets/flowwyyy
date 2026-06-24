package productdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// ThreadState is the steerer's persistent per-thread "running understanding"
// (task steerer-thread-memory, spec §4). Unlike a FeedItem — which the
// coalescing upsert overwrites on every new event — this record ACCUMULATES
// across events for one thread_key, so a resumed triage doesn't start from
// scratch. It lives in its own table (attention_thread_state) so it outlives any
// single card and dismissal/re-surface cycles. Downstream tasks
// (steerer-context-assembly, steerer-operator-reply-learning) read and enrich
// it; this task only persists it and its slots.
type ThreadState struct {
	ThreadKey           string
	Source              string
	CurrentAction       string
	CurrentConfidence   float64
	CurrentReason       string
	Summary             string
	OperatorActions     []ThreadOperatorAction
	OperatorReplies     []ThreadOperatorReply
	OperatorCorrections []ThreadOperatorCorrection
	EventCount          int
	LastSeenTS          string
	FirstSeenAt         string
	UpdatedAt           string
}

// ThreadOperatorAction is one operator/autonomous resolution recorded against a
// thread (make_task, forward, dismiss, confirm_handoff, send_reply). Appended by
// the steering action layer's single feedback chokepoint.
type ThreadOperatorAction struct {
	At         string `json:"at"`
	Action     string `json:"action"`
	Outcome    string `json:"outcome,omitempty"`
	LinkedTask string `json:"linked_task,omitempty"`
}

// ThreadOperatorReply is one operator-authored reply seen on a thread. This task
// only persists the slot; routing it into a learn path is
// steerer-operator-reply-learning.
type ThreadOperatorReply struct {
	At     string `json:"at"`
	TS     string `json:"ts,omitempty"`
	Author string `json:"author,omitempty"`
	Text   string `json:"text"`
}

// ThreadOperatorCorrection is one authoritative context correction the operator
// supplied for a thread via the "correct the steerer" button. Unlike a reply
// (something said in the conversation), this is the operator telling the steerer
// what the thread actually means; deep triage treats it as ground truth.
type ThreadOperatorCorrection struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// ThreadDecision is the input to RecordThreadDecision: the latest verdict the
// cascade reached for a thread, plus the source ts anchor and the write time.
type ThreadDecision struct {
	ThreadKey  string
	Source     string
	Action     string
	Confidence float64
	Reason     string
	Summary    string
	LastSeenTS string
	At         string // RFC3339
}

// ThreadCursor is the minimal (thread_key, last_seen_ts) pair the steerer backfill
// needs to gap-recover a watched thread that has no slack-reply task. last_seen_ts
// is the newest message ts the steerer recorded for the thread — the floor to
// fetch replies after.
type ThreadCursor struct {
	ThreadKey  string
	LastSeenTS string
}

// ListRecentSlackThreadCursors returns up to `limit` most-recently-updated Slack
// threads the steerer has tracked, each with its last_seen_ts recovery floor.
// This is how the backfill finds steerer-watched threads with no slack-reply task
// (closing the live-routing-vs-gap-recovery coverage gap that lost messages over
// a laptop sleep). Bounded by limit so it can't fan out to every thread ever seen.
func ListRecentSlackThreadCursors(db *sql.DB, limit int) ([]ThreadCursor, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT thread_key, last_seen_ts FROM attention_thread_state
		 WHERE source = 'slack' AND last_seen_ts <> ''
		 ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadCursor
	for rows.Next() {
		var tc ThreadCursor
		if err := rows.Scan(&tc.ThreadKey, &tc.LastSeenTS); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// GetThreadState loads the running understanding for a thread. The bool is false
// (with a zero ThreadState and nil error) when no row exists yet.
func GetThreadState(db *sql.DB, threadKey string) (ThreadState, bool, error) {
	var s ThreadState
	var action, reason, lastSeen sql.NullString
	var actionsJSON, repliesJSON, correctionsJSON string
	err := db.QueryRow(
		`SELECT thread_key, source, current_action, current_confidence, current_reason,
		        summary, operator_actions, operator_replies, operator_corrections,
		        event_count, last_seen_ts, first_seen_at, updated_at
		 FROM attention_thread_state WHERE thread_key = ?`, threadKey,
	).Scan(
		&s.ThreadKey, &s.Source, &action, &s.CurrentConfidence, &reason,
		&s.Summary, &actionsJSON, &repliesJSON, &correctionsJSON, &s.EventCount, &lastSeen,
		&s.FirstSeenAt, &s.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return ThreadState{}, false, nil
	}
	if err != nil {
		return ThreadState{}, false, fmt.Errorf("productdb: get thread state %q: %w", threadKey, err)
	}
	s.CurrentAction = action.String
	s.CurrentReason = reason.String
	s.LastSeenTS = lastSeen.String
	if err := json.Unmarshal([]byte(actionsJSON), &s.OperatorActions); err != nil {
		return ThreadState{}, false, fmt.Errorf("productdb: thread state %q operator_actions: %w", threadKey, err)
	}
	if err := json.Unmarshal([]byte(repliesJSON), &s.OperatorReplies); err != nil {
		return ThreadState{}, false, fmt.Errorf("productdb: thread state %q operator_replies: %w", threadKey, err)
	}
	if err := json.Unmarshal([]byte(correctionsJSON), &s.OperatorCorrections); err != nil {
		return ThreadState{}, false, fmt.Errorf("productdb: thread state %q operator_corrections: %w", threadKey, err)
	}
	return s, true, nil
}

// RecordThreadDecision upserts the latest decision for a thread keyed by
// thread_key. First event inserts the row (event_count=1, first_seen_at=At);
// subsequent events overwrite current_* with the fresh decision, bump
// event_count, and advance last_seen_ts/updated_at — without touching
// first_seen_at or the accumulated operator_actions/operator_replies. A blank
// summary carries the prior one forward (a later event's summary may be empty;
// don't blank out a good one). Atomic via SQLite UPSERT — no read-then-write race.
func RecordThreadDecision(db *sql.DB, d ThreadDecision) error {
	if d.ThreadKey == "" {
		return fmt.Errorf("productdb: thread decision requires a thread_key")
	}
	_, err := db.Exec(
		`INSERT INTO attention_thread_state (
		   thread_key, source, current_action, current_confidence, current_reason,
		   summary, operator_actions, operator_replies, event_count, last_seen_ts,
		   first_seen_at, updated_at
		 ) VALUES (?,?,?,?,?,?,'[]','[]',1,?,?,?)
		 ON CONFLICT(thread_key) DO UPDATE SET
		   source = CASE WHEN excluded.source <> '' THEN excluded.source ELSE attention_thread_state.source END,
		   current_action = excluded.current_action,
		   current_confidence = excluded.current_confidence,
		   current_reason = excluded.current_reason,
		   summary = CASE WHEN excluded.summary <> '' THEN excluded.summary ELSE attention_thread_state.summary END,
		   event_count = attention_thread_state.event_count + 1,
		   last_seen_ts = excluded.last_seen_ts,
		   updated_at = excluded.updated_at`,
		d.ThreadKey, d.Source, NullIfEmpty(d.Action), d.Confidence, NullIfEmpty(d.Reason),
		d.Summary, NullIfEmpty(d.LastSeenTS), d.At, d.At,
	)
	if err != nil {
		return fmt.Errorf("productdb: record thread decision %q: %w", d.ThreadKey, err)
	}
	return nil
}

// AppendThreadOperatorAction appends an operator/autonomous action to a thread's
// running understanding. Creates a minimal state row first if none exists (an
// action can land on a thread the cascade never carded), so the slot is never
// silently dropped.
func AppendThreadOperatorAction(db *sql.DB, threadKey string, a ThreadOperatorAction) error {
	return appendThreadJSON(db, threadKey, "operator_actions", a, a.At)
}

// AppendThreadOperatorReply appends an operator-authored reply to a thread's
// running understanding (same create-if-missing semantics as the action append).
func AppendThreadOperatorReply(db *sql.DB, threadKey string, r ThreadOperatorReply) error {
	return appendThreadJSON(db, threadKey, "operator_replies", r, r.At)
}

// AppendThreadOperatorCorrection appends an operator correction (authoritative
// context for "the steerer got this wrong") to a thread's running understanding,
// same create-if-missing semantics as the action/reply appends.
func AppendThreadOperatorCorrection(db *sql.DB, threadKey string, corr ThreadOperatorCorrection) error {
	return appendThreadJSON(db, threadKey, "operator_corrections", corr, corr.At)
}

// appendThreadJSON read-modify-writes one JSON-array column on a thread-state
// row. column is an internal constant (never user input), validated against a
// whitelist so the dynamic column name can't smuggle SQL. at stamps both the
// defensive insert and updated_at.
func appendThreadJSON(db *sql.DB, threadKey, column string, entry any, at string) error {
	if threadKey == "" {
		return fmt.Errorf("productdb: append thread %s requires a thread_key", column)
	}
	if column != "operator_actions" && column != "operator_replies" && column != "operator_corrections" {
		return fmt.Errorf("productdb: append thread: unknown column %q", column)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO attention_thread_state (thread_key, first_seen_at, updated_at) VALUES (?,?,?)`,
		threadKey, at, at,
	); err != nil {
		return fmt.Errorf("productdb: ensure thread state %q: %w", threadKey, err)
	}
	var raw string
	if err := db.QueryRow(`SELECT `+column+` FROM attention_thread_state WHERE thread_key = ?`, threadKey).Scan(&raw); err != nil {
		return fmt.Errorf("productdb: load thread %s for %q: %w", column, threadKey, err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return fmt.Errorf("productdb: decode thread %s for %q: %w", column, threadKey, err)
	}
	enc, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("productdb: encode thread %s entry: %w", column, err)
	}
	arr = append(arr, enc)
	out, err := json.Marshal(arr)
	if err != nil {
		return fmt.Errorf("productdb: encode thread %s for %q: %w", column, threadKey, err)
	}
	if _, err := db.Exec(
		`UPDATE attention_thread_state SET `+column+` = ?, updated_at = ? WHERE thread_key = ?`,
		string(out), at, threadKey,
	); err != nil {
		return fmt.Errorf("productdb: append thread %s for %q: %w", column, threadKey, err)
	}
	return nil
}
