package server

import (
	"context"
	"database/sql"
	"testing"

	"flow/internal/flowdb"
)

type capturedDraftAck struct {
	channel  string
	ts       string
	userID   string
	threadTS string
	emoji    string
	text     string
}

func stubSlackDraftAckRunner(t *testing.T) *[]capturedDraftAck {
	t.Helper()
	old := slackDraftAckRunner
	calls := &[]capturedDraftAck{}
	slackDraftAckRunner = func(_ context.Context, channel, ts, userID, threadTS, emoji, text string) error {
		*calls = append(*calls, capturedDraftAck{
			channel: channel, ts: ts, userID: userID, threadTS: threadTS, emoji: emoji, text: text,
		})
		return nil
	}
	t.Cleanup(func() { slackDraftAckRunner = old })
	return calls
}

// buildSlackMonitorEvent fabricates a flowdb.MonitorEvent shaped exactly
// like Slack Socket Mode ingest produces. Avoids spinning up the full
// poller so the draft-ack test exercises only the ack path.
func buildSlackMonitorEvent(channel, ts, threadTS string) flowdb.MonitorEvent {
	raw := `{"conversation":{"id":"` + channel + `"},"message":{"ts":"` + ts + `","user":"U_ORIGIN"`
	if threadTS != "" {
		raw += `,"thread_ts":"` + threadTS + `"`
	}
	raw += `}}`
	return flowdb.MonitorEvent{
		ID:       flowdb.MonitorEventID("slack", channel+":"+ts),
		Source:   "slack",
		Kind:     "dm",
		SourceID: channel + ":" + ts,
		Title:    "test",
		URL:      sql.NullString{String: "https://slack.example/p1", Valid: true},
		Severity: "medium",
		Status:   "new",
		RawJSON:  sql.NullString{String: raw, Valid: true},
	}
}

func insertSlackMonitorEvent(t *testing.T, db *sql.DB, event flowdb.MonitorEvent) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO monitor_events (id, source, kind, source_id, title, body, url, severity, status, first_seen_at, last_seen_at, last_seq, raw_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		event.ID, event.Source, event.Kind, event.SourceID, event.Title, event.Body, event.URL, event.Severity, event.Status, now, now, event.RawJSON,
	); err != nil {
		t.Fatalf("insert slack monitor event: %v", err)
	}
}

func TestPostSlackDraftAckHappyPath(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	s := New(Config{DB: db, Version: "test"})
	event := buildSlackMonitorEvent("D9", "1710000005.0001", "1710000000.0001")
	insertSlackMonitorEvent(t, db, event)

	s.postSlackDraftAck(event, "drafted-task")

	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.channel != "D9" || got.ts != "1710000005.0001" {
		t.Errorf("reaction target wrong: channel=%q ts=%q", got.channel, got.ts)
	}
	if got.threadTS != "1710000000.0001" {
		t.Errorf("threadTS = %q, want parent thread ts", got.threadTS)
	}
	if got.emoji != "eyes" {
		t.Errorf("emoji = %q, want eyes (default)", got.emoji)
	}
	if got.userID != "U_FLOW" {
		t.Errorf("userID = %q, want U_FLOW", got.userID)
	}
	if !contains(got.text, "`drafted-task`") || !contains(got.text, "http://flow.example/tasks/drafted-task") {
		t.Errorf("text missing expected substrings: %s", got.text)
	}
	actions, err := flowdb.ListExternalActionsForEvent(db, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].ActionType != "draft_ack" || actions[0].Status != "sent" {
		t.Fatalf("actions = %+v, want sent draft_ack", actions)
	}
}

func TestPostSlackWorkingAckHappyPath(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	s := New(Config{DB: db, Version: "test"})
	event := buildSlackMonitorEvent("C9", "1710000005.0001", "1710000000.0001")
	insertSlackMonitorEvent(t, db, event)

	s.postSlackWorkingAck(event, "working-task")

	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.channel != "C9" || got.ts != "1710000005.0001" || got.threadTS != "1710000000.0001" {
		t.Fatalf("call target = %+v", got)
	}
	if got.emoji != "eyes" {
		t.Errorf("emoji = %q, want eyes", got.emoji)
	}
	if got.userID != "U_FLOW" {
		t.Errorf("userID = %q, want U_FLOW", got.userID)
	}
	if !contains(got.text, "working on this") || !contains(got.text, "http://flow.example/tasks/working-task") {
		t.Errorf("text missing expected substrings: %s", got.text)
	}
	actions, err := flowdb.ListExternalActionsForEvent(db, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].ActionType != "working_ack" || actions[0].Status != "sent" {
		t.Fatalf("actions = %+v, want sent working_ack", actions)
	}
}

func TestStartAgentForNotificationPostsWorkingAckForExistingSlackDraft(t *testing.T) {
	root, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('approved-slack-task', 'Approved Slack task', 'backlog', 'medium', ?, ?, ?)`,
		root, now, now,
	); err != nil {
		t.Fatal(err)
	}
	event := buildSlackMonitorEvent("C9", "1710000006.0001", "")
	if _, err := db.Exec(
		`INSERT INTO monitor_events (id, source, kind, source_id, title, body, url, severity, status, first_seen_at, last_seen_at, last_seq, raw_json)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, 0, ?)`,
		event.ID, event.Source, event.Kind, event.SourceID, event.Title, event.Body, event.Severity, event.Status, now, now, event.RawJSON,
	); err != nil {
		t.Fatal(err)
	}
	if err := flowdb.CreateNotificationForEvent(db, event, "approval"); err != nil {
		t.Fatal(err)
	}
	if err := flowdb.RecordMonitorEventAction(db, event.ID, "draft", "approved-slack-task", "rule mode auto_task"); err != nil {
		t.Fatal(err)
	}
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})

	resp, status := s.startAgentForNotification(actionRequest{EventID: event.ID})
	if status != 200 || !resp.OK {
		t.Fatalf("status = %d resp = %+v", status, resp)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want working ack", len(*calls))
	}
	got := (*calls)[0]
	if got.threadTS != "1710000006.0001" || !contains(got.text, "working on this") {
		t.Fatalf("working ack = %+v", got)
	}
}

func TestPostSlackDraftAckIsNoOpForNonSlackEvent(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	s := New(Config{DB: db, Version: "test"})
	event := flowdb.MonitorEvent{
		Source:   "github",
		SourceID: "review_requested:acme/flow:48",
	}
	s.postSlackDraftAck(event, "gh-task")
	if len(*calls) != 0 {
		t.Errorf("calls = %d, want 0 for github event", len(*calls))
	}
}

func TestPostSlackDraftAckCustomEmojiFromEnv(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	// FLOW_SLACK_DRAFT_EMOJI takes a Slack short name without colons.
	// We pass the colon-wrapped form to verify the helper normalizes it.
	t.Setenv("FLOW_SLACK_DRAFT_EMOJI", ":inbox_tray:")
	s := New(Config{DB: db, Version: "test"})
	event := buildSlackMonitorEvent("D1", "1.001", "")
	insertSlackMonitorEvent(t, db, event)
	s.postSlackDraftAck(event, "tray-task")
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	if got := (*calls)[0].emoji; got != "inbox_tray" {
		t.Errorf("emoji = %q, want inbox_tray (colons stripped)", got)
	}
}

func TestPostSlackDraftAckLinkOmittedWhenBaseURLAbsent(t *testing.T) {
	_, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", t.TempDir()) // no server.url
	t.Setenv("FLOW_BASE_URL", "")
	calls := stubSlackDraftAckRunner(t)
	s := New(Config{DB: db, Version: "test"})
	event := buildSlackMonitorEvent("D1", "1.001", "")
	insertSlackMonitorEvent(t, db, event)
	s.postSlackDraftAck(event, "linkless")
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	got := (*calls)[0].text
	if contains(got, "http://") {
		t.Errorf("URL present despite no baseURL: %s", got)
	}
	if !contains(got, "`linkless`") {
		t.Errorf("slug not in fallback text: %s", got)
	}
}

func TestPostSlackDraftAckPromotesTopLevelToThread(t *testing.T) {
	// Top-level message (thread_ts empty) — the reply must thread off the
	// original message's ts so the conversation stays in one place rather
	// than fragmenting into a new top-level reply.
	_, db := testRootDB(t)
	calls := stubSlackDraftAckRunner(t)
	s := New(Config{DB: db, Version: "test"})
	event := buildSlackMonitorEvent("D9", "1710000005.0001", "")
	insertSlackMonitorEvent(t, db, event)
	s.postSlackDraftAck(event, "top-level")
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	if got := (*calls)[0].threadTS; got != "1710000005.0001" {
		t.Errorf("threadTS = %q, want promoted ts", got)
	}
}
