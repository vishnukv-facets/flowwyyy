package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	ObservedAt        string `json:"observed_at,omitempty"`
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

func TestProviderUsageTrustsStaleClaudeLimitUntilReset(t *testing.T) {
	root, db := testRootDB(t)
	cache := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(cache, []byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 42, "resets_at": "`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"},
			"seven_day": {"used_percentage": 100, "resets_at": "`+reset+`"}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-7 * time.Hour)
	if err := os.Chtimes(cache, old, old); err != nil {
		t.Fatal(err)
	}

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=claude")
	if !got.Available || !got.Limited || got.LimitResetAt != reset {
		t.Fatalf("usage = %+v; want available limited until %q", got, reset)
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

func TestProviderQueueAPIListsAndDismisses(t *testing.T) {
	root, db := testRootDB(t)
	next := "2026-06-25T12:00:00Z"
	id1, err := flowdb.EnqueueRateLimitQueue(db, flowdb.RateLimitQueueSlackEvent, "claude", []byte(`{"kind":"message","channel":"C1","text":"hello"}`), next)
	if err != nil {
		t.Fatalf("enqueue slack: %v", err)
	}
	if _, err := flowdb.EnqueueRateLimitQueue(db, flowdb.RateLimitQueueOpenTask, "codex", []byte(`{"slug":"demo-task"}`), "2026-06-25T13:00:00Z"); err != nil {
		t.Fatalf("enqueue open: %v", err)
	}
	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root}))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/provider-queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var listed providerQueueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count != 2 || len(listed.Items) != 2 {
		t.Fatalf("listed = %+v; want two items", listed)
	}
	if listed.Items[0].ID != id1 || listed.Items[0].Summary == "" || listed.Items[1].Target != "demo-task" {
		t.Fatalf("unexpected listed items: %+v", listed.Items)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider-queue/dismiss", strings.NewReader(`{"id":`+fmt.Sprint(id1)+`}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d body=%s", rec.Code, rec.Body.String())
	}
	if count, err := flowdb.CountPendingRateLimitQueue(db); err != nil || count != 1 {
		t.Fatalf("pending after dismiss one = %d err=%v; want 1,nil", count, err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider-queue/dismiss", strings.NewReader(`{"all":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss all status = %d body=%s", rec.Code, rec.Body.String())
	}
	if count, err := flowdb.CountPendingRateLimitQueue(db); err != nil || count != 0 {
		t.Fatalf("pending after dismiss all = %d err=%v; want 0,nil", count, err)
	}
}

func clearCodexUsageCache() {
	codexUsageMu.Lock()
	codexUsageCacheOK = false
	codexUsageCache = providerUsageResponse{}
	codexUsageCacheAt = time.Time{}
	codexUsageMu.Unlock()
}

// codexLogOnly forces readCodexProviderUsage down the local-log path: it points
// the live-endpoint auth at a nonexistent file (no token → no HTTP call) and
// clears the process-wide live cache, keeping log-path tests offline.
func codexLogOnly(t *testing.T) {
	t.Helper()
	t.Setenv("FLOW_CODEX_AUTH", filepath.Join(t.TempDir(), "noauth.json"))
	clearCodexUsageCache()
}

func TestProviderUsageCodexLiveEndpoint(t *testing.T) {
	root, db := testRootDB(t)
	clearCodexUsageCache()
	t.Cleanup(clearCodexUsageCache)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"plan_type":"prolite","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":2326,"reset_at":1782408355},"secondary_window":{"used_percent":39,"limit_window_seconds":604800,"reset_after_seconds":505588,"reset_at":1782911618}}}`)
	}))
	t.Cleanup(srv.Close)

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"test-token"}}`), 0o644); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	t.Setenv("FLOW_CODEX_AUTH", authPath)
	t.Setenv("FLOW_CODEX_USAGE_URL", srv.URL)
	// A log DB with different numbers — if the live endpoint is preferred, these
	// log values (7/1) must NOT appear in the response.
	logDB := filepath.Join(t.TempDir(), "logs_2.sqlite")
	writeCodexRateLimitLog(t, logDB, `Received message {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":7,"window_minutes":300,"reset_at":1782408354},"secondary":{"used_percent":1,"window_minutes":10080,"reset_at":1782911617}}}`)
	t.Setenv("FLOW_CODEX_LOG_DB", logDB)

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=codex")
	if !got.Available || got.Provider != "codex" {
		t.Fatalf("usage = %+v, want available codex", got)
	}
	if !got.Limited {
		t.Fatalf("want limited=true (limit_reached), got %+v", got)
	}
	if len(got.Windows) != 2 || got.Windows[0].UsedPercent != 100 || got.Windows[1].UsedPercent != 39 {
		t.Fatalf("windows = %+v, want live 100/39 (not log 7/1)", got.Windows)
	}
	if got.Windows[0].WindowMinutes != 300 || got.Windows[1].WindowMinutes != 10080 {
		t.Fatalf("window minutes = %+v, want 300/10080", got.Windows)
	}
	if obs, err := time.Parse(time.RFC3339, got.ObservedAt); err != nil || time.Since(obs) > time.Minute {
		t.Fatalf("observed_at = %q (err %v), want fresh live timestamp", got.ObservedAt, err)
	}
}

func TestProviderUsageCodexLogsDB(t *testing.T) {
	root, db := testRootDB(t)
	codexLogOnly(t)
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

func TestProviderUsageCodexReceivedMessageFormat(t *testing.T) {
	// Regression: codex switched its log prefix from "websocket event: {...}"
	// to "Received message {...}". flow must match on the JSON object, not the
	// prefix, or the quota silently sticks on the last old-format row.
	root, db := testRootDB(t)
	codexLogOnly(t)
	logDB := filepath.Join(t.TempDir(), "logs_2.sqlite")
	writeCodexRateLimitLog(t, logDB, `Received message {"type":"codex.rate_limits","plan_type":"prolite","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":95,"window_minutes":300,"reset_after_seconds":3939,"reset_at":1782408354},"secondary":{"used_percent":38,"window_minutes":10080,"reset_at":1782911617}}}`)
	t.Setenv("FLOW_CODEX_LOG_DB", logDB)

	got := requestProviderUsage(t, New(Config{DB: db, FlowRoot: root}), "/api/provider-usage?provider=codex")
	if !got.Available || got.Provider != "codex" {
		t.Fatalf("usage = %+v, want available codex", got)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", got.Windows)
	}
	if got.Windows[0].ID != "five_hour" || got.Windows[0].UsedPercent != 95 {
		t.Fatalf("five-hour window = %+v, want used 95", got.Windows[0])
	}
	if got.Windows[1].ID != "seven_day" || got.Windows[1].UsedPercent != 38 {
		t.Fatalf("seven-day window = %+v, want used 38", got.Windows[1])
	}
}

func TestProviderUsageCodexSkipsSelfEchoRows(t *testing.T) {
	root, db := testRootDB(t)
	codexLogOnly(t)
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
