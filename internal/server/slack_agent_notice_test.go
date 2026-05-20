package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"flow/internal/flowdb"
)

type capturedAgentNotice struct {
	channel  string
	userID   string
	threadTS string
	text     string
}

func stubSlackAgentNoticeRunner(t *testing.T, retErr error) *[]capturedAgentNotice {
	t.Helper()
	old := slackAgentNoticeRunner
	calls := &[]capturedAgentNotice{}
	slackAgentNoticeRunner = func(_ context.Context, channel, userID, threadTS, text string) error {
		*calls = append(*calls, capturedAgentNotice{channel: channel, userID: userID, threadTS: threadTS, text: text})
		return retErr
	}
	t.Cleanup(func() { slackAgentNoticeRunner = old })
	return calls
}

// seedSlackAgentTask wires (slack event → action → task) for the agent
// waiting trigger tests. status='backlog' satisfies the tasks-table CHECK
// without requiring a fabricated session_id.
func seedSlackAgentTask(t *testing.T, db *sql.DB, slug, channel, ts, threadTS string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'medium', ?, ?, ?)`,
		slug, slug+" task", t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	id := flowdb.MonitorEventID("slack", channel+":"+ts)
	rawJSON := `{"conversation":{"id":"` + channel + `"},"message":{"ts":"` + ts + `","user":"U_ORIGIN"`
	if threadTS != "" {
		rawJSON += `,"thread_ts":"` + threadTS + `"`
	}
	rawJSON += `}}`
	if _, err := db.Exec(
		`INSERT INTO monitor_events (id, source, kind, source_id, title, body, url, severity, status, first_seen_at, last_seen_at, last_seq, raw_json)
		 VALUES (?, 'slack', 'dm', ?, 'seed', NULL, NULL, 'medium', 'new', ?, ?, 0, ?)`,
		id, channel+":"+ts, now, now, rawJSON,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := flowdb.RecordMonitorEventAction(db, id, "spawn", slug, ""); err != nil {
		t.Fatalf("seed action: %v", err)
	}
}

func TestMaybePostAgentWaitingNoticePostsOnSlackOriginTask(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackAgentNoticeRunner(t, nil)
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	seedSlackAgentTask(t, db, "agent-task", "D9", "1710000005.0001", "")

	s := New(Config{DB: db, Version: "test"})
	posted := s.maybePostAgentWaitingNotice("claude", "0aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa", "agent-task")
	if !posted {
		t.Fatal("posted = false, want true")
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.channel != "D9" {
		t.Errorf("channel = %q, want D9", got.channel)
	}
	if got.userID != "U_FLOW" {
		t.Errorf("userID = %q, want U_FLOW", got.userID)
	}
	if got.threadTS != "1710000005.0001" {
		t.Errorf("threadTS = %q, want promoted top-level ts", got.threadTS)
	}
	if !contains(got.text, "agent-task task") || !contains(got.text, "http://flow.example/tasks/agent-task") {
		t.Errorf("text missing expected substrings: %s", got.text)
	}
}

func TestMaybePostAgentWaitingNoticeDebouncesWithinWindow(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackAgentNoticeRunner(t, nil)
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	seedSlackAgentTask(t, db, "flapping", "D1", "1.001", "")

	// Pin the clock so two rapid calls land "within" the debounce window
	// regardless of real test timing.
	fixedNow := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	oldNow := agentSlackNow
	agentSlackNow = func() time.Time { return fixedNow }
	t.Cleanup(func() { agentSlackNow = oldNow })

	s := New(Config{DB: db, Version: "test"})
	sessionID := "11111111-1111-4111-8111-111111111111"

	if posted := s.maybePostAgentWaitingNotice("claude", sessionID, "flapping"); !posted {
		t.Fatal("first call not posted")
	}
	// Second call within the same instant — must be debounced.
	if posted := s.maybePostAgentWaitingNotice("claude", sessionID, "flapping"); posted {
		t.Fatal("second call posted within debounce window")
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %d, want 1 (debounce broken)", len(*calls))
	}

	// Advance past the debounce window — next call should post again.
	agentSlackNow = func() time.Time { return fixedNow.Add(2 * time.Minute) }
	if posted := s.maybePostAgentWaitingNotice("claude", sessionID, "flapping"); !posted {
		t.Error("call after debounce window did not post")
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %d, want 2 after window expired", len(*calls))
	}
}

func TestMaybePostAgentWaitingNoticeIsNoOpForNonSlackOriginTask(t *testing.T) {
	_, db := testRootDB(t)
	calls := stubSlackAgentNoticeRunner(t, nil)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('adhoc', 'adhoc', 'backlog', 'medium', ?, ?, ?)`,
		t.TempDir(), now, now,
	); err != nil {
		t.Fatal(err)
	}
	s := New(Config{DB: db, Version: "test"})
	if posted := s.maybePostAgentWaitingNotice("claude", "sess-id", "adhoc"); posted {
		t.Errorf("posted = true for non-slack-origin task")
	}
	if len(*calls) != 0 {
		t.Errorf("calls = %d, want 0", len(*calls))
	}
}

func TestMaybePostAgentWaitingNoticeRollsBackDebounceOnRunnerFailure(t *testing.T) {
	_, db := testRootDB(t)
	stubSlackAgentNoticeRunner(t, errors.New("simulated outage"))
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	seedSlackAgentTask(t, db, "rollback", "D1", "2.001", "")

	s := New(Config{DB: db, Version: "test"})
	sessionID := "22222222-2222-4222-8222-222222222222"
	// First call: runner fails. Debounce should roll back.
	if posted := s.maybePostAgentWaitingNotice("claude", sessionID, "rollback"); posted {
		t.Error("posted = true despite runner error")
	}
	// Second call should be allowed to try again (not suppressed by a
	// rolled-back debounce stamp).
	s.agentSlackDebounce.mu.Lock()
	_, stamped := s.agentSlackDebounce.lastAt[`claude:`+sessionID]
	s.agentSlackDebounce.mu.Unlock()
	if stamped {
		t.Errorf("debounce stamp still set after runner failure — would suppress retries")
	}
}

func TestMaybePostAgentWaitingNoticeNoLinkWhenBaseURLAbsent(t *testing.T) {
	// FLOW_BASE_URL unset and no ~/.flow/server.url → FlowBaseURL() = "".
	// The notice must still post, but with a slug-name fallback so the
	// user has SOMETHING to act on. Pin FLOW_ROOT so the server.url
	// lookup in monitor.FlowBaseURL targets an empty temp dir.
	_, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("FLOW_BASE_URL", "")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U_FLOW")
	calls := stubSlackAgentNoticeRunner(t, nil)
	seedSlackAgentTask(t, db, "linkless", "D1", "3.001", "")

	s := New(Config{DB: db, Version: "test"})
	if posted := s.maybePostAgentWaitingNotice("claude", "abc", "linkless"); !posted {
		t.Fatal("posted = false, want true even without baseURL")
	}
	if len(*calls) != 1 {
		t.Fatal("calls = 0")
	}
	got := (*calls)[0].text
	if contains(got, "http://") {
		t.Errorf("URL present despite no baseURL: %s", got)
	}
	if !contains(got, "linkless") {
		t.Errorf("slug not in fallback text: %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestMonitorSyncStateAPISynthesizesKnownSourcesEvenWithoutData(t *testing.T) {
	// Fresh DB → no monitor_sync_state rows. The endpoint must still
	// return one entry per known source (github, slack) so the UI can
	// render every source badge from a single fetch.
	_, db := testRootDB(t)
	s := New(Config{DB: db, Version: "test"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/monitor/sync-state", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp MonitorSyncStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.States) != 2 {
		t.Fatalf("states len = %d, want 2 (github+slack); got %+v", len(resp.States), resp.States)
	}
	if resp.States[0].Source != "github" || resp.States[1].Source != "slack" {
		t.Errorf("ordering wrong: %+v", resp.States)
	}
	for _, st := range resp.States {
		if st.LastStatus != "unknown" {
			t.Errorf("%s last_status = %q, want 'unknown' for fresh DB", st.Source, st.LastStatus)
		}
		if st.IsSyncing {
			t.Errorf("%s is_syncing = true on fresh DB", st.Source)
		}
		if st.LastSyncAt != "" {
			t.Errorf("%s last_sync_at = %q, want empty", st.Source, st.LastSyncAt)
		}
	}
}

func TestMonitorSyncStateAPIReflectsRecordedSyncs(t *testing.T) {
	_, db := testRootDB(t)
	// Simulate one successful Slack sync and one failed GitHub sync.
	if _, err := flowdb.RecordMonitorSyncStart(db, "slack"); err != nil {
		t.Fatal(err)
	}
	if _, err := flowdb.RecordMonitorSyncEnd(db, "slack", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := flowdb.RecordMonitorSyncEnd(db, "github", "error", "rate limited"); err != nil {
		t.Fatal(err)
	}
	s := New(Config{DB: db, Version: "test"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/monitor/sync-state", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp MonitorSyncStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	bySource := map[string]MonitorSyncStateView{}
	for _, st := range resp.States {
		bySource[st.Source] = st
	}
	gh := bySource["github"]
	if gh.LastStatus != "error" || gh.LastError != "rate limited" {
		t.Errorf("github = %+v, want status=error and last_error='rate limited'", gh)
	}
	if gh.LastSyncAt == "" {
		t.Errorf("github last_sync_at empty even after a failed end")
	}
	sl := bySource["slack"]
	if sl.LastStatus != "ok" {
		t.Errorf("slack = %+v, want status=ok", sl)
	}
	if sl.LastError != "" {
		t.Errorf("slack last_error = %q, want empty on ok", sl.LastError)
	}
}
