package server

import (
	"database/sql"
	"encoding/json"
	"flow/internal/productdb"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveChatTitle(t *testing.T) {
	long := "This prompt is intentionally written to be much longer than sixty runes so it must truncate"
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{"short", "Help me triage today", "Help me triage today"},
		{"empty", "", "New chat"},
		{"whitespace only", "   \n\t  \n", "New chat"},
		{"multi-line takes first non-empty line", "\n\n  Plan the release  \nignored second line", "Plan the release"},
		{"collapses internal whitespace", "Plan\tthe    release", "Plan the release"},
		{"long truncates with ellipsis", long, "This prompt is intentionally written to be much longer than…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveChatTitle(tt.prompt)
			if got != tt.want {
				t.Fatalf("deriveChatTitle(%q) = %q, want %q", tt.prompt, got, tt.want)
			}
			// Title must never exceed the rune budget (+1 for the ellipsis).
			if n := len([]rune(got)); n > chatTitleMaxRunes+1 {
				t.Fatalf("title too long: %d runes (%q)", n, got)
			}
		})
	}
}

func TestListChats_ExcludesArchivedAndOrders(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	// Insert out of order so ordering is exercised, not insertion order.
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-old", Title: "Older", Provider: "claude", Origin: "ui",
		CreatedAt: "2026-06-13T09:00:00+05:30", LastActivityAt: "2026-06-13T09:00:00+05:30",
	})
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-new", Title: "Newer", Provider: "codex", Origin: "ui",
		CreatedAt: "2026-06-13T10:00:00+05:30", LastActivityAt: "2026-06-13T12:00:00+05:30",
	})
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-arch", Title: "Archived", Provider: "claude", Origin: "ui",
		CreatedAt:      "2026-06-13T11:00:00+05:30",
		LastActivityAt: "2026-06-13T11:00:00+05:30",
		ArchivedAt:     sql.NullString{String: "2026-06-13T11:30:00+05:30", Valid: true},
	})

	// includeArchived=false hides the archived chat, newest activity first.
	active, err := srv.listChats(false)
	if err != nil {
		t.Fatalf("listChats(false): %v", err)
	}
	if active == nil {
		t.Fatal("listChats(false) returned nil, want non-nil slice")
	}
	if len(active) != 2 {
		t.Fatalf("listChats(false) = %d chats, want 2: %+v", len(active), active)
	}
	if active[0].Slug != "overview-new" || active[1].Slug != "overview-old" {
		t.Fatalf("ordering wrong: %s then %s (want overview-new, overview-old)", active[0].Slug, active[1].Slug)
	}
	for _, c := range active {
		if c.Archived {
			t.Fatalf("active list leaked archived chat %q", c.Slug)
		}
		// No PTY ever attached in this test, so Live must be false.
		if c.Live {
			t.Fatalf("chat %q reported Live with no running session", c.Slug)
		}
	}

	// includeArchived=true surfaces the archived chat too, with Archived set.
	all, err := srv.listChats(true)
	if err != nil {
		t.Fatalf("listChats(true): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("listChats(true) = %d chats, want 3", len(all))
	}
	var sawArchived bool
	for _, c := range all {
		if c.Slug == "overview-arch" {
			sawArchived = true
			if !c.Archived {
				t.Fatalf("archived chat %q not flagged Archived", c.Slug)
			}
		}
	}
	if !sawArchived {
		t.Fatal("listChats(true) did not include the archived chat")
	}
}

// listChats must be nil-safe: a Server with no terminalHub (terminals == nil)
// must not panic when computing the Live flag, and an empty list must encode as
// [] (non-nil), not null.
func TestListChats_NilTerminalsAndEmpty(t *testing.T) {
	root, db := testRootDB(t)
	srv := &Server{cfg: Config{DB: db, FlowRoot: root}} // no New() -> terminals is nil

	empty, err := srv.listChats(false)
	if err != nil {
		t.Fatalf("listChats on empty db: %v", err)
	}
	if empty == nil {
		t.Fatal("listChats returned nil, want non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Fatalf("expected no chats, got %d", len(empty))
	}

	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-x", Title: "X", Provider: "claude", Origin: "ui",
		CreatedAt: "2026-06-13T09:00:00+05:30", LastActivityAt: "2026-06-13T09:00:00+05:30",
	})
	got, err := srv.listChats(false)
	if err != nil {
		t.Fatalf("listChats with nil terminals: %v", err)
	}
	if len(got) != 1 || got[0].Live {
		t.Fatalf("with nil terminals: got %+v (want 1 chat, Live=false)", got)
	}
}

// The full Ask Flow launch path (overview-chat action) must record a durable
// chat row after the floating session registers, without breaking the launch.
func TestOverviewChatRecordsChatRow(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	resp, status := srv.runAction(actionRequest{
		Kind:     "overview-chat",
		Prompt:   "Help me plan the launch",
		Provider: "claude",
	})
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("overview-chat action failed: status=%d resp=%+v", status, resp)
	}
	if resp.FloatingTerminal == nil {
		t.Fatal("overview-chat returned no floating terminal")
	}
	slug := resp.FloatingTerminal.ID

	chat, err := productdb.GetChat(db, slug)
	if err != nil {
		t.Fatalf("expected chat row for %q: %v", slug, err)
	}
	if chat.Title != "Help me plan the launch" {
		t.Fatalf("chat title = %q, want %q", chat.Title, "Help me plan the launch")
	}
	if chat.Provider != "claude" || chat.Origin != "ui" {
		t.Fatalf("chat provider/origin = %q/%q", chat.Provider, chat.Origin)
	}
	if !chat.SessionID.Valid || chat.SessionID.String == "" {
		t.Fatalf("chat session id not recorded: %+v", chat.SessionID)
	}
	if chat.CreatedAt == "" || chat.LastActivityAt == "" {
		t.Fatalf("chat timestamps not set: created=%q last=%q", chat.CreatedAt, chat.LastActivityAt)
	}
	if chat.ArchivedAt.Valid || chat.DeletedAt.Valid {
		t.Fatalf("new chat should not be archived/deleted: %+v", chat)
	}

	// And it surfaces in listChats.
	chats, err := srv.listChats(false)
	if err != nil {
		t.Fatalf("listChats: %v", err)
	}
	if len(chats) != 1 || chats[0].Slug != slug {
		t.Fatalf("listChats = %+v, want one chat with slug %q", chats, slug)
	}
}

// TestHandleChats exercises GET /api/chats end-to-end through the HTTP handler:
// it must return a JSON array (never null), newest-activity first, hide archived
// chats by default, and surface them when include_archived is set.
func TestHandleChats(t *testing.T) {
	root, db := testRootDB(t)
	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"}))

	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-old", Title: "Older", Provider: "claude", Origin: "ui",
		CreatedAt: "2026-06-13T09:00:00+05:30", LastActivityAt: "2026-06-13T09:00:00+05:30",
	})
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-new", Title: "Newer", Provider: "codex", Origin: "ui",
		CreatedAt: "2026-06-13T10:00:00+05:30", LastActivityAt: "2026-06-13T12:00:00+05:30",
	})
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-arch", Title: "Archived", Provider: "claude", Origin: "ui",
		CreatedAt:      "2026-06-13T11:00:00+05:30",
		LastActivityAt: "2026-06-13T11:00:00+05:30",
		ArchivedAt:     sql.NullString{String: "2026-06-13T11:30:00+05:30", Valid: true},
	})

	get := func(t *testing.T, target string) []chatView {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", target, rec.Code, rec.Body.String())
		}
		// Body must be a JSON array, not null — assert on the raw bytes first.
		raw := rec.Body.Bytes()
		var probe any
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("GET %s: invalid JSON: %v (%s)", target, err, raw)
		}
		if _, ok := probe.([]any); !ok {
			t.Fatalf("GET %s: body is not a JSON array: %s", target, raw)
		}
		var out []chatView
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("GET %s: decode []chatView: %v", target, err)
		}
		return out
	}

	active := get(t, "/api/chats")
	if len(active) != 2 {
		t.Fatalf("GET /api/chats = %d chats, want 2: %+v", len(active), active)
	}
	if active[0].Slug != "overview-new" || active[1].Slug != "overview-old" {
		t.Fatalf("ordering wrong: %s then %s (want overview-new, overview-old)", active[0].Slug, active[1].Slug)
	}
	for _, c := range active {
		if c.Archived {
			t.Fatalf("default list leaked archived chat %q", c.Slug)
		}
	}

	all := get(t, "/api/chats?include_archived=1")
	if len(all) != 3 {
		t.Fatalf("GET /api/chats?include_archived=1 = %d chats, want 3", len(all))
	}

	// Empty DB still encodes [] (non-nil), and a non-GET method is rejected.
	_, emptyDB := testRootDB(t)
	emptySrv := authedTestHandler(New(Config{DB: emptyDB, FlowRoot: root, CommandPath: "/bin/false"}))
	rec := httptest.NewRecorder()
	emptySrv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/chats", nil))
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("empty /api/chats body = %q, want %q", got, "[]\n")
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chats", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/chats status = %d, want 405", rec.Code)
	}
}

// TestChatActionArchiveAndDelete drives chat-archive / chat-unarchive /
// chat-delete through runAction and asserts visibility in listChats.
func TestChatActionArchiveAndDelete(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-a", Title: "A", Provider: "claude", Origin: "ui",
		CreatedAt: "2026-06-13T09:00:00+05:30", LastActivityAt: "2026-06-13T09:00:00+05:30",
	})
	mustInsertChat(t, db, productdb.Chat{
		Slug: "overview-b", Title: "B", Provider: "claude", Origin: "ui",
		CreatedAt: "2026-06-13T10:00:00+05:30", LastActivityAt: "2026-06-13T10:00:00+05:30",
	})

	// Archive hides overview-a from the default list but keeps it in the archived view.
	resp, status := srv.runAction(actionRequest{Kind: "chat-archive", Target: "overview-a"})
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("chat-archive: status=%d resp=%+v", status, resp)
	}
	active, err := srv.listChats(false)
	if err != nil {
		t.Fatalf("listChats(false): %v", err)
	}
	if len(active) != 1 || active[0].Slug != "overview-b" {
		t.Fatalf("after archive listChats(false) = %+v, want only overview-b", active)
	}
	all, err := srv.listChats(true)
	if err != nil {
		t.Fatalf("listChats(true): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("after archive listChats(true) = %d, want 2", len(all))
	}

	// Unarchive restores it to the default list.
	resp, status = srv.runAction(actionRequest{Kind: "chat-unarchive", Slug: "overview-a"})
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("chat-unarchive: status=%d resp=%+v", status, resp)
	}
	active, _ = srv.listChats(false)
	if len(active) != 2 {
		t.Fatalf("after unarchive listChats(false) = %d, want 2", len(active))
	}

	// Delete removes overview-a from every list (default and archived).
	resp, status = srv.runAction(actionRequest{Kind: "chat-delete", Target: "overview-a"})
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("chat-delete: status=%d resp=%+v", status, resp)
	}
	all, _ = srv.listChats(true)
	if len(all) != 1 || all[0].Slug != "overview-b" {
		t.Fatalf("after delete listChats(true) = %+v, want only overview-b", all)
	}
	if _, err := productdb.GetChat(db, "overview-a"); err == nil {
		// GetChat still returns the soft-deleted row (no deleted-at filter), so
		// confirm the deleted_at marker was set rather than asserting ErrNoRows.
		c, _ := productdb.GetChat(db, "overview-a")
		if c != nil && !c.DeletedAt.Valid {
			t.Fatalf("chat-delete did not set deleted_at on overview-a: %+v", c)
		}
	}
}

// TestChatReopenNotFound covers the reopen path for a missing chat: it must
// return a not-found response, not panic or resume a phantom session. (A live
// reopen needs a real terminal hub + PTY and is verified manually end-to-end.)
func TestChatReopenNotFound(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	resp, status := srv.runAction(actionRequest{Kind: "chat-reopen", Target: "overview-missing"})
	if status != http.StatusNotFound || resp.OK {
		t.Fatalf("chat-reopen missing: status=%d resp=%+v, want 404 + OK=false", status, resp)
	}
	if resp.Message != "chat not found" {
		t.Fatalf("chat-reopen missing message = %q, want %q", resp.Message, "chat not found")
	}
}

func mustInsertChat(t *testing.T, db *sql.DB, c productdb.Chat) {
	t.Helper()
	if err := productdb.InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat(%q): %v", c.Slug, err)
	}
}
