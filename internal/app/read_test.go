package app

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestCmdReadAskFromTaskSessionRecordsContextAndLedger(t *testing.T) {
	clearReadSessionEnv(t)
	root := setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"task", "Setup", "--slug", "setup", "--work-dir", wd, "--agent", "claude"}); rc != 0 {
		t.Fatalf("cmdAdd setup rc=%d", rc)
	}
	if rc := cmdAdd([]string{"task", "Build", "--slug", "build", "--work-dir", wd, "--agent", "codex", "--depends-on", "setup"}); rc != 0 {
		t.Fatalf("cmdAdd build rc=%d", rc)
	}
	db := openFlowDB(t)
	ctx, err := flowdb.CreateWorkContext(db, flowdb.WorkContext{Title: "Build context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}
	if err := flowdb.SetTaskWorkContext(db, "build", ctx.ID); err != nil {
		t.Fatalf("SetTaskWorkContext: %v", err)
	}
	const sid = "codex-read-session"
	if _, err := db.Exec(`UPDATE tasks SET status='in-progress', session_provider='codex', session_id=?, session_started=? WHERE slug='build'`, sid, flowdb.NowISO()); err != nil {
		t.Fatalf("bind task session: %v", err)
	}
	t.Setenv("CODEX_THREAD_ID", sid)

	if rc := cmdRead([]string{"ask", "Should I route this through flow tell?", "--key", "build-q1"}); rc != 0 {
		t.Fatalf("cmdRead ask rc=%d", rc)
	}
	if rc := cmdRead([]string{"ask", "duplicate should be ignored", "--key", "build-q1"}); rc != 0 {
		t.Fatalf("cmdRead duplicate ask rc=%d", rc)
	}

	rows, err := flowdb.ListSessionReadItems(db, flowdb.SessionReadItemFilter{Status: "pending"})
	if err != nil {
		t.Fatalf("ListSessionReadItems: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Kind != "ask" || got.TaskSlug != "build" || got.Provider != "codex" || got.SessionID != sid || got.WorkContextID != ctx.ID {
		t.Fatalf("ask context = %+v", got)
	}
	var deps []flowdb.DependencyRef
	if err := json.Unmarshal([]byte(got.DependenciesJSON), &deps); err != nil {
		t.Fatalf("dependencies json: %v", err)
	}
	if len(deps) != 1 || deps[0].Slug != "setup" {
		t.Fatalf("dependencies = %+v, want setup", deps)
	}
	ledger, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "flow_read_ask", TaskSlug: "build"})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(ledger) != 1 || ledger[0].ExternalID != got.ID {
		t.Fatalf("ledger = %+v, want one row linked to ask", ledger)
	}

	out := captureStdout(t, func() {
		if rc := cmdRead([]string{"list"}); rc != 0 {
			t.Fatalf("cmdRead list rc=%d", rc)
		}
	})
	if !strings.Contains(out, got.ID) || !strings.Contains(out, "flow tell build") {
		t.Fatalf("list output missing id/reply path:\n%s", out)
	}
	if !strings.Contains(out, "setup") || !strings.Contains(out, ctx.ID) {
		t.Fatalf("list output missing dependency/work context:\n%s", out)
	}

	if rc := cmdRead([]string{"mark", got.ID, "--read"}); rc != 0 {
		t.Fatalf("cmdRead mark rc=%d", rc)
	}
	marked, err := flowdb.GetSessionReadItem(db, got.ID)
	if err != nil {
		t.Fatalf("GetSessionReadItem: %v", err)
	}
	if marked.Status != "read" || marked.ReadAt == "" {
		t.Fatalf("marked = %+v, want read", marked)
	}
	out = captureStdout(t, func() {
		if rc := cmdRead([]string{"list"}); rc != 0 {
			t.Fatalf("cmdRead list after mark rc=%d", rc)
		}
	})
	if strings.Contains(out, got.ID) {
		t.Fatalf("default list still showed read item:\n%s", out)
	}

	if _, err := flowdb.OpenDB(filepath.Join(root, "flow.db")); err != nil {
		t.Fatalf("db remained openable: %v", err)
	}
}

func TestCmdReadSayFromChatSessionAndUnboundAsk(t *testing.T) {
	clearReadSessionEnv(t)
	setupFlowRoot(t)
	db := openFlowDB(t)
	now := flowdb.NowISO()
	ctx, err := flowdb.CreateWorkContext(db, flowdb.WorkContext{Title: "Chat context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}
	if err := flowdb.InsertChat(db, flowdb.Chat{
		Slug:           "chat-build",
		Title:          "Chat build",
		Provider:       "claude",
		Origin:         "ui",
		WorkContextID:  sqlNull(ctx.ID),
		SessionID:      sqlNull("chat-session-1"),
		CreatedAt:      now,
		LastActivityAt: now,
	}); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "chat-session-1")

	if rc := cmdRead([]string{"say", "Found the config knob", "--key", "chat-note"}); rc != 0 {
		t.Fatalf("cmdRead say rc=%d", rc)
	}
	rows, err := flowdb.ListSessionReadItems(db, flowdb.SessionReadItemFilter{Status: "unread"})
	if err != nil {
		t.Fatalf("ListSessionReadItems: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "say" || rows[0].ChatSlug != "chat-build" || rows[0].WorkContextID != ctx.ID {
		t.Fatalf("chat say rows = %+v", rows)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if rc := cmdRead([]string{"ask", "Unbound question", "--key", "unbound-q"}); rc != 0 {
		t.Fatalf("cmdRead unbound ask rc=%d", rc)
	}
	pending, err := flowdb.ListSessionReadItems(db, flowdb.SessionReadItemFilter{Status: "pending"})
	if err != nil {
		t.Fatalf("ListSessionReadItems pending: %v", err)
	}
	if len(pending) != 1 || pending[0].TaskSlug != "" || pending[0].ChatSlug != "" {
		t.Fatalf("unbound pending = %+v, want no task/chat", pending)
	}
}

func sqlNull(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func clearReadSessionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("FLOW_TASK", "")
}
