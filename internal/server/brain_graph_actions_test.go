package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBrainGraphSessionEventActionRequiresPrompt(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)

	for _, action := range []string{"seed", "send_event"} {
		t.Run(action, func(t *testing.T) {
			got, rec := postBrainGraphAction(t, s, BrainGraphActionRequest{
				Action: action,
				NodeID: "task:ship",
				Actor:  "operator",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.OK || !strings.Contains(got.Message, "prompt is required") {
				t.Fatalf("response = %#v, want prompt-required validation", got)
			}
		})
	}
}

func TestBrainGraphRejectsUnsupportedActions(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/echo"})
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)

	for _, action := range []string{"retry", "approve", "pause", "trigger_auto"} {
		t.Run(action, func(t *testing.T) {
			got, rec := postBrainGraphAction(t, s, BrainGraphActionRequest{
				Action: action,
				NodeID: "task:ship",
				Actor:  "operator",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.OK || !strings.Contains(got.Message, "unknown graph action") {
				t.Fatalf("response = %#v, want unsupported-action rejection", got)
			}
		})
	}
}

func TestBrainGraphActionsRejectStaleTargets(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/echo"})
	insertBrainGraphTask(t, db, "archived", "Archived", "backlog", nil)
	insertBrainGraphTask(t, db, "deleted", "Deleted", "backlog", nil)
	if _, err := db.Exec(`UPDATE tasks SET archived_at = '2026-06-12T10:00:00+05:30' WHERE slug = 'archived'`); err != nil {
		t.Fatalf("archive task: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET deleted_at = '2026-06-12T10:00:00+05:30' WHERE slug = 'deleted'`); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	for _, tc := range []BrainGraphActionRequest{
		{Action: "open_session", NodeID: "task:archived", Actor: "operator"},
		{Action: "open_session", NodeID: "task:deleted", Actor: "operator"},
		{Action: "open_session", NodeID: "task:missing", Actor: "operator"},
	} {
		t.Run(tc.NodeID, func(t *testing.T) {
			got, rec := postBrainGraphAction(t, s, tc)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.OK || !strings.Contains(got.Message, "graph node not found") {
				t.Fatalf("response = %#v, want stale node rejection", got)
			}
		})
	}
}

func TestBrainGraphOpenSessionActionOpensTaskBridge(t *testing.T) {
	fakeProviderOnPath(t, "claude")
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)

	got, rec := postBrainGraphAction(t, s, BrainGraphActionRequest{
		Action: "open_session",
		NodeID: "task:ship",
		Actor:  "operator",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !got.OK || got.ActionResponse == nil || !got.ActionResponse.OK {
		t.Fatalf("response = %#v, want successful open bridge", got)
	}
}

func TestBrainGraphSessionEventActionNudgesLiveSession(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)
	sessionFile := fakeRunningTerminalSession(t, s, "ship")

	got, rec := postBrainGraphAction(t, s, BrainGraphActionRequest{
		Action: "seed",
		NodeID: "task:ship",
		Prompt: "Focus on the failing retry path.",
		Actor:  "operator",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !got.OK {
		t.Fatalf("response = %#v, want successful seed", got)
	}
	written := readTerminalSessionFile(t, sessionFile)
	if !strings.Contains(written, "Flow Graph seed input for ship") || !strings.Contains(written, "Focus on the failing retry path.") {
		t.Fatalf("terminal prompt missing seed context:\n%s", written)
	}
}

func postBrainGraphAction(t *testing.T, s *Server, req BrainGraphActionRequest) (BrainGraphActionResponse, *httptest.ResponseRecorder) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/api/brain/graph/actions", bytes.NewReader(body))
	authedTestHandler(s).ServeHTTP(rec, httpReq)
	var got BrainGraphActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response status=%d body=%s: %v", rec.Code, rec.Body.String(), err)
	}
	return got, rec
}

func fakeProviderOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	exe := name
	content := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		exe += ".bat"
		content = "@echo off\r\nexit /b 0\r\n"
	}
	path := filepath.Join(dir, exe)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake provider: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func fakeRunningTerminalSession(t *testing.T, s *Server, slug string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "terminal-session-*")
	if err != nil {
		t.Fatalf("create terminal file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	s.terminals.mu.Lock()
	s.terminals.sessions[slug] = &terminalSession{
		hub:     s.terminals,
		slug:    slug,
		tty:     f,
		done:    make(chan struct{}),
		clients: map[*terminalClient]struct{}{},
	}
	s.terminals.mu.Unlock()
	return f
}

func readTerminalSessionFile(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek terminal file: %v", err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read terminal file: %v", err)
	}
	return string(raw)
}
