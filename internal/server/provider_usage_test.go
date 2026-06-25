package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flow/internal/flowdb"

	_ "modernc.org/sqlite"
)

type providerUsageTestResponse struct {
	Provider          string `json:"provider"`
	Available         bool   `json:"available"`
	Limited           bool   `json:"limited"`
	LimitResetAt      string `json:"limit_reset_at,omitempty"`
	Reason            string `json:"reason,omitempty"`
	Source            string `json:"source,omitempty"`
	QueuedActions     int    `json:"queued_actions"`
	NextQueueRunAfter string `json:"next_queue_run_after,omitempty"`
	Windows           []struct {
		ID               string `json:"id"`
		Label            string `json:"label"`
		UsedPercent      int    `json:"used_percent"`
		RemainingPercent int    `json:"remaining_percent"`
		ResetAt          string `json:"reset_at"`
		WindowMinutes    int    `json:"window_minutes"`
	} `json:"windows"`
}

func TestProviderUsageClaudeStatuslineCache(t *testing.T) {
	root, db := testRootDB(t)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 37, "resets_at": 1782397800},
			"seven_day": {"used_percentage": 67, "resets_at": 1782752400}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=claude")
	if !got.Available || got.Provider != "claude" {
		t.Fatalf("usage = %+v, want available claude", got)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", got.Windows)
	}
	if got.Windows[0].ID != "five_hour" || got.Windows[0].Label != "5h" || got.Windows[0].UsedPercent != 37 || got.Windows[0].RemainingPercent != 63 {
		t.Fatalf("five-hour window = %+v", got.Windows[0])
	}
	if got.Windows[1].ID != "seven_day" || got.Windows[1].Label != "7d" || got.Windows[1].UsedPercent != 67 || got.Windows[1].RemainingPercent != 33 {
		t.Fatalf("seven-day window = %+v", got.Windows[1])
	}
	wantReset := time.Unix(1782397800, 0).UTC().Format(time.RFC3339)
	if got.Windows[0].ResetAt != wantReset {
		t.Fatalf("reset_at = %q, want %q", got.Windows[0].ResetAt, wantReset)
	}
}

func TestProviderUsageReadsFreshValues(t *testing.T) {
	root, db := testRootDB(t)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	writeClaudeUsageCache(t, cache, 10, 1782397800)
	s := New(Config{DB: db, FlowRoot: root})

	first := requestProviderUsage(t, s, "/api/provider-usage?provider=claude")
	if first.Windows[0].UsedPercent != 10 {
		t.Fatalf("first used = %d, want 10", first.Windows[0].UsedPercent)
	}

	writeClaudeUsageCache(t, cache, 88, 1782397800)
	second := requestProviderUsage(t, s, "/api/provider-usage?provider=claude")
	if second.Windows[0].UsedPercent != 88 {
		t.Fatalf("second used = %d, want fresh value 88", second.Windows[0].UsedPercent)
	}
}

func TestProviderUsageMarksLimitReached(t *testing.T) {
	root, db := testRootDB(t)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(cache, []byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 100, "resets_at": "`+reset+`"}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=claude")
	if !got.Limited || got.LimitResetAt != reset {
		t.Fatalf("limit fields = limited:%v reset:%q; want true %q", got.Limited, got.LimitResetAt, reset)
	}
}

func TestProviderUsageIncludesQueuedAutomation(t *testing.T) {
	root, db := testRootDB(t)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	writeClaudeUsageCache(t, cache, 25, 1782397800)
	next := "2026-06-25T12:00:00Z"
	if _, err := flowdb.EnqueueRateLimitQueue(db, flowdb.RateLimitQueueSlackEvent, "claude", []byte(`{"kind":"message"}`), next); err != nil {
		t.Fatalf("enqueue queue: %v", err)
	}

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=claude")
	if got.QueuedActions != 1 || got.NextQueueRunAfter != next {
		t.Fatalf("queue fields = count:%d next:%q; want 1 %q", got.QueuedActions, got.NextQueueRunAfter, next)
	}
}

func TestProviderUsageCodexLogsDB(t *testing.T) {
	root, db := testRootDB(t)
	logDB := filepath.Join(t.TempDir(), "logs_2.sqlite")
	writeCodexRateLimitLog(t, logDB, `session: websocket event: {"type":"codex.rate_limits","plan_type":"prolite","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":7,"window_minutes":300,"reset_at":1782408354},"secondary":{"used_percent":24,"window_minutes":10080,"reset_at":1782911617}}}`)
	t.Setenv("FLOW_CODEX_LOG_DB", logDB)

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=codex")
	if !got.Available || got.Provider != "codex" {
		t.Fatalf("usage = %+v, want available codex", got)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", got.Windows)
	}
	if got.Windows[0].ID != "five_hour" || got.Windows[0].UsedPercent != 7 || got.Windows[0].RemainingPercent != 93 {
		t.Fatalf("five-hour window = %+v", got.Windows[0])
	}
	if got.Windows[1].ID != "seven_day" || got.Windows[1].UsedPercent != 24 || got.Windows[1].WindowMinutes != 10080 {
		t.Fatalf("seven-day window = %+v", got.Windows[1])
	}
}

func TestProviderUsageCodexSkipsSelfEchoRows(t *testing.T) {
	root, db := testRootDB(t)
	logDB := filepath.Join(t.TempDir(), "logs_2.sqlite")
	initCodexLogDB(t, logDB)
	now := time.Now().Unix()
	insertCodexLog(t, logDB, now-1, `session: websocket event: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"window_minutes":300,"reset_at":1782408354}}}`)
	insertCodexLog(t, logDB, now, `session: websocket event: {"type":"response.output_item.done","item":{"arguments":"{\"cmd\":\"echo websocket event: {\\\"type\\\":\\\"codex.rate_limits\\\",\\\"rate_limits\\\":{}}\"}"}}`)
	t.Setenv("FLOW_CODEX_LOG_DB", logDB)

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=codex")
	if !got.Available || len(got.Windows) != 1 || got.Windows[0].UsedPercent != 42 {
		t.Fatalf("usage = %+v, want older real codex rate-limit event", got)
	}
}

func TestCodexUsageWindowsAllowsTrailingLogText(t *testing.T) {
	windows, limited, err := codexUsageWindows("session: websocket event: " +
		`{"type":"codex.rate_limits","rate_limits":{"allowed":false,"primary":{"used_percent":91,"window_minutes":300,"reset_at":1782408354}}}` +
		" trailing trace text `after`")
	if err != nil {
		t.Fatalf("codexUsageWindows error: %v", err)
	}
	if !limited || len(windows) != 1 || windows[0].UsedPercent != 91 {
		t.Fatalf("windows = %+v limited = %v, want parsed limited window", windows, limited)
	}
}

func TestProviderUsageUnavailableShape(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("HOME", t.TempDir())

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=claude")
	if got.Available {
		t.Fatalf("usage = %+v, want unavailable", got)
	}
	if got.Provider != "claude" || got.Reason == "" {
		t.Fatalf("unavailable response = %+v, want provider and reason", got)
	}
}

func requestProviderUsage(t *testing.T, s *Server, path string) providerUsageTestResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	authedTestHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got providerUsageTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func writeCodexRateLimitLog(t *testing.T, path, body string) {
	t.Helper()
	initCodexLogDB(t, path)
	insertCodexLog(t, path, time.Now().Unix(), body)
}

func initCodexLogDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		ts_nanos INTEGER NOT NULL,
		level TEXT NOT NULL,
		target TEXT NOT NULL,
		feedback_log_body TEXT,
		module_path TEXT,
		file TEXT,
		line INTEGER,
		thread_id TEXT,
		process_uuid TEXT,
		estimated_bytes INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
}

func insertCodexLog(t *testing.T, path string, ts int64, body string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO logs (ts, ts_nanos, level, target, feedback_log_body) VALUES (?, 0, 'INFO', 'codex_api::endpoint::responses_websocket', ?)`, ts, body); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeUsageCache(t *testing.T, path string, used int, reset int64) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":%d,"resets_at":%d}}}`, used, reset))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
