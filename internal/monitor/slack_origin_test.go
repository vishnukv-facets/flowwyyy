package monitor

import (
	"database/sql"
	"testing"

	"flow/internal/flowdb"
)

// seedMonitorEvent inserts a minimal monitor_events row directly so the test
// doesn't depend on the full poller pipeline. The action row is what
// SlackOriginFor actually joins through.
func seedMonitorEvent(t *testing.T, db *sql.DB, source, sourceID, url, rawJSON string) string {
	t.Helper()
	id := flowdb.MonitorEventID(source, sourceID)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO monitor_events (id, source, kind, source_id, title, body, url, severity, status, first_seen_at, last_seen_at, last_seq, raw_json)
		 VALUES (?, ?, ?, ?, 'seeded', NULL, ?, 'medium', 'new', ?, ?, 0, ?)`,
		id, source, "dm", sourceID, flowdb.NullString(url), now, now, flowdb.NullString(rawJSON),
	); err != nil {
		t.Fatalf("seed monitor event: %v", err)
	}
	return id
}

// seedMonitorAction binds an event to a task via the routing action row.
func seedMonitorAction(t *testing.T, db *sql.DB, eventID, taskSlug, action string) {
	t.Helper()
	if err := flowdb.RecordMonitorEventAction(db, eventID, action, taskSlug, ""); err != nil {
		t.Fatalf("record event action: %v", err)
	}
}

// seedTask creates the bare-minimum task row tests need to join against.
func seedTask(t *testing.T, db *sql.DB, slug string) {
	t.Helper()
	now := flowdb.NowISO()
	// status=backlog satisfies the tasks table CHECK constraint without
	// needing to fabricate a session_id; the helper under test only joins
	// against monitor_event_actions.task_slug, which doesn't care about
	// task status.
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'medium', ?, ?, ?)`,
		slug, slug+" task", t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func TestSlackOriginForResolvesThreadFromRawJSON(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "slack-task")
	raw := `{"conversation":{"id":"C123"},"message":{"ts":"1710000001.000001","thread_ts":"1709999999.000099"}}`
	id := seedMonitorEvent(t, db, "slack", "C123:1710000001.000001", "https://slack.example/archives/C123/p1", raw)
	seedMonitorAction(t, db, id, "slack-task", "spawn")

	origin, ok, err := SlackOriginFor(db, "slack-task")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if origin.Channel != "C123" {
		t.Errorf("channel = %q, want C123", origin.Channel)
	}
	if origin.TS != "1710000001.000001" {
		t.Errorf("ts = %q", origin.TS)
	}
	if origin.ThreadTS != "1709999999.000099" {
		t.Errorf("thread_ts = %q, want parent thread ts", origin.ThreadTS)
	}
	if origin.Permalink != "https://slack.example/archives/C123/p1" {
		t.Errorf("permalink = %q", origin.Permalink)
	}

	// PostTarget for an existing thread reply should preserve the parent ts.
	channel, threadTS := origin.PostTarget()
	if channel != "C123" || threadTS != "1709999999.000099" {
		t.Errorf("PostTarget = (%q,%q)", channel, threadTS)
	}
}

func TestSlackOriginForResolvesThreadFromSocketModeRawJSON(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "socket-task")
	raw := `{"event":{"channel":"D123","ts":"1710000001.000001","thread_ts":"1709999999.000099","user":"U123"}}`
	id := seedMonitorEvent(t, db, "slack", "D123:1710000001.000001", "", raw)
	seedMonitorAction(t, db, id, "socket-task", "spawn")

	origin, ok, err := SlackOriginFor(db, "socket-task")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if origin.Channel != "D123" || origin.TS != "1710000001.000001" || origin.ThreadTS != "1709999999.000099" || origin.UserID != "U123" {
		t.Fatalf("origin = %+v", origin)
	}
}

func TestSlackOriginForTopLevelMessagePromotesTSToThread(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "top-level")
	// thread_ts is absent — the original message is top-level. PostTarget
	// must turn TS into the thread parent so replies thread off the original
	// rather than fragmenting into a new top-level message.
	raw := `{"conversation":{"id":"D9"},"message":{"ts":"1710000002.000002"}}`
	id := seedMonitorEvent(t, db, "slack", "D9:1710000002.000002", "", raw)
	seedMonitorAction(t, db, id, "top-level", "spawn")

	origin, ok, err := SlackOriginFor(db, "top-level")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if origin.ThreadTS != "" {
		t.Errorf("thread_ts = %q, want empty for top-level", origin.ThreadTS)
	}
	channel, threadTS := origin.PostTarget()
	if channel != "D9" || threadTS != "1710000002.000002" {
		t.Errorf("PostTarget = (%q,%q), want (D9, 1710000002.000002)", channel, threadTS)
	}
}

func TestSlackOriginForReturnsOkFalseForNonSlackTask(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "gh-task")
	// A GitHub-origin task: action row exists, but source != slack.
	id := seedMonitorEvent(t, db, "github", "review_requested:acme/flow:48", "https://github.com/acme/flow/pull/48", `{}`)
	seedMonitorAction(t, db, id, "gh-task", "spawn")

	origin, ok, err := SlackOriginFor(db, "gh-task")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Fatalf("ok = true for non-slack origin: %+v", origin)
	}
}

func TestSlackOriginForReturnsOkFalseForTaskWithoutAction(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "orphan")
	// No monitor_event_actions row exists at all.
	origin, ok, err := SlackOriginFor(db, "orphan")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Fatalf("ok = true for taskless origin: %+v", origin)
	}
}

func TestSlackOriginForFallsBackToSourceIDWhenRawJSONMalformed(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "broken-raw")
	// raw_json is unparseable; helper must still recover channel/ts from
	// source_id. thread_ts is unknowable so it stays empty; PostTarget
	// then promotes TS to thread parent.
	id := seedMonitorEvent(t, db, "slack", "CABC:1710000003.000003", "", "{not json")
	seedMonitorAction(t, db, id, "broken-raw", "draft")

	origin, ok, err := SlackOriginFor(db, "broken-raw")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true with source_id fallback")
	}
	if origin.Channel != "CABC" || origin.TS != "1710000003.000003" {
		t.Errorf("channel/ts = %q/%q", origin.Channel, origin.TS)
	}
	if origin.ThreadTS != "" {
		t.Errorf("thread_ts = %q, want empty (raw_json was unparseable)", origin.ThreadTS)
	}
}

func TestSlackOriginForReturnsOkFalseForEmptyTaskSlug(t *testing.T) {
	db := openMonitorTestDB(t)
	origin, ok, err := SlackOriginFor(db, "")
	if err != nil || ok {
		t.Fatalf("expected (false, nil) for empty slug; got ok=%v err=%v origin=%+v", ok, err, origin)
	}
}

func TestSlackOriginForPicksMostRecentActionWhenMultiple(t *testing.T) {
	db := openMonitorTestDB(t)
	seedTask(t, db, "rebound")
	earlyID := seedMonitorEvent(t, db, "slack", "C1:1700000000.000001",
		"https://slack.example/archives/C1/p_early",
		`{"conversation":{"id":"C1"},"message":{"ts":"1700000000.000001"}}`)
	lateID := seedMonitorEvent(t, db, "slack", "C2:1710000000.000001",
		"https://slack.example/archives/C2/p_late",
		`{"conversation":{"id":"C2"},"message":{"ts":"1710000000.000001"}}`)
	// Insert action rows directly so we can pin distinct created_at
	// timestamps. RecordMonitorEventAction uses NowISO() (RFC3339 = 1s
	// precision); two back-to-back calls in a test land in the same second
	// and SQLite's tiebreaker for an ORDER BY on a single tied column is
	// implementation-defined. Production isn't affected — Slack messages
	// arrive seconds apart — but the test fabricates simultaneity.
	if _, err := db.Exec(
		`INSERT INTO monitor_event_actions (event_id, action, task_slug, note, created_at)
		 VALUES (?, 'ping', 'rebound', NULL, '2026-01-01T00:00:00Z')`,
		earlyID,
	); err != nil {
		t.Fatalf("insert early action: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO monitor_event_actions (event_id, action, task_slug, note, created_at)
		 VALUES (?, 'spawn', 'rebound', NULL, '2026-05-01T00:00:00Z')`,
		lateID,
	); err != nil {
		t.Fatalf("insert late action: %v", err)
	}

	origin, ok, err := SlackOriginFor(db, "rebound")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if origin.Channel != "C2" {
		t.Errorf("channel = %q, want C2 (most recent action)", origin.Channel)
	}
}
