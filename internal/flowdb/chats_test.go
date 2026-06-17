package flowdb

import (
	"database/sql"
	"errors"
	"testing"
)

func TestSetChatProviderAndTitle(t *testing.T) {
	db := openTempDB(t)
	c := Chat{
		Slug: "chat-steer-c123", Title: "Steering: C123", Provider: "claude", Origin: "steerer",
		SessionID: sql.NullString{String: "sess-1", Valid: true},
		CreatedAt: "2026-06-17T09:00:00Z", LastActivityAt: "2026-06-17T09:00:00Z",
	}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := SetChatProvider(db, c.Slug, "codex", "2026-06-17T10:00:00Z"); err != nil {
		t.Fatalf("SetChatProvider: %v", err)
	}
	if err := SetChatTitle(db, c.Slug, "#facets-coinswitch", "2026-06-17T10:01:00Z"); err != nil {
		t.Fatalf("SetChatTitle: %v", err)
	}
	got, err := GetChat(db, c.Slug)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if got.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", got.Provider)
	}
	if got.Title != "#facets-coinswitch" {
		t.Errorf("Title = %q, want #facets-coinswitch", got.Title)
	}
	if got.LastActivityAt != "2026-06-17T10:01:00Z" {
		t.Errorf("LastActivityAt = %q, want bumped", got.LastActivityAt)
	}

	// A deleted chat is never mutated by the setters.
	if err := DeleteChat(db, c.Slug, "2026-06-17T11:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if err := SetChatProvider(db, c.Slug, "claude", "2026-06-17T12:00:00Z"); err != nil {
		t.Fatalf("SetChatProvider (deleted): %v", err)
	}
	if _, err := GetChat(db, c.Slug); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted chat should stay absent, got err=%v", err)
	}
}

// GAP-14 reset-and-reopen: a deleted steerer chat is absent to GetChat, and the
// next session start (UpsertChat on the same deterministic slug) reclaims the
// tombstone into a FRESH chat — new session_id, deleted_at cleared.
func TestSteererChatResetAndReopen(t *testing.T) {
	db := openTempDB(t)
	slug := "chat-steer-c123"
	if err := InsertChat(db, Chat{
		Slug: slug, Title: "#coinswitch", Provider: "claude", Origin: "steerer",
		SessionID: sql.NullString{String: "old-sess", Valid: true},
		CreatedAt: "2026-06-17T09:00:00Z", LastActivityAt: "2026-06-17T09:00:00Z",
	}); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := DeleteChat(db, slug, "2026-06-17T10:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if _, err := GetChat(db, slug); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted chat must be absent, got %v", err)
	}
	// Next event reopens fresh (clean memory, new session) on the same slug.
	if err := UpsertChat(db, Chat{
		Slug: slug, Title: "#coinswitch", Provider: "claude", Origin: "steerer",
		SessionID: sql.NullString{String: "new-sess", Valid: true},
		CreatedAt: "2026-06-17T11:00:00Z", LastActivityAt: "2026-06-17T11:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertChat (reclaim tombstone): %v", err)
	}
	got, err := GetChat(db, slug)
	if err != nil {
		t.Fatalf("GetChat after reopen: %v", err)
	}
	if got.SessionID.String != "new-sess" {
		t.Errorf("reopened session = %q, want new-sess (fresh memory)", got.SessionID.String)
	}
	if got.DeletedAt.Valid {
		t.Error("reopened chat must have deleted_at cleared")
	}
}

func TestChatInsertAndGet(t *testing.T) {
	db := openTempDB(t)

	c := Chat{
		Slug:           "overview-abc123",
		Title:          "Morning Standup",
		Provider:       "claude",
		Origin:         "ui",
		SessionID:      sql.NullString{String: "sess-1", Valid: true},
		CreatedAt:      "2026-06-13T09:00:00Z",
		LastActivityAt: "2026-06-13T09:05:00Z",
	}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}

	got, err := GetChat(db, "overview-abc123")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if got.Slug != c.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, c.Slug)
	}
	if got.Title != c.Title {
		t.Errorf("Title = %q, want %q", got.Title, c.Title)
	}
	if got.Provider != c.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, c.Provider)
	}
	if got.Origin != c.Origin {
		t.Errorf("Origin = %q, want %q", got.Origin, c.Origin)
	}
	if !got.SessionID.Valid || got.SessionID.String != "sess-1" {
		t.Errorf("SessionID = %v, want sess-1", got.SessionID)
	}
	if got.CreatedAt != c.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", got.CreatedAt, c.CreatedAt)
	}
	if got.LastActivityAt != c.LastActivityAt {
		t.Errorf("LastActivityAt = %q, want %q", got.LastActivityAt, c.LastActivityAt)
	}
	if got.ArchivedAt.Valid {
		t.Errorf("ArchivedAt should be NULL, got %v", got.ArchivedAt)
	}
	if got.DeletedAt.Valid {
		t.Errorf("DeletedAt should be NULL, got %v", got.DeletedAt)
	}
}

func TestChatGetMissing(t *testing.T) {
	db := openTempDB(t)

	_, err := GetChat(db, "no-such-slug")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetChat missing = %v, want sql.ErrNoRows", err)
	}
}

// TestGetChatExcludesDeleted is the regression for "deleted chat is still
// reused": once a chat is soft-deleted, GetChat must report it as absent
// (sql.ErrNoRows) so the Slack command path opens a FRESH chat rather than
// resurrecting the deleted session.
func TestGetChatExcludesDeleted(t *testing.T) {
	db := openTempDB(t)

	c := Chat{
		Slug:           "chat-slack-d0ba9amrhez",
		Title:          "old conversation",
		Provider:       "claude",
		Origin:         "slack",
		SessionID:      sql.NullString{String: "sess-old", Valid: true},
		CreatedAt:      "2026-06-13T09:00:00Z",
		LastActivityAt: "2026-06-13T09:05:00Z",
	}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if _, err := GetChat(db, c.Slug); err != nil {
		t.Fatalf("GetChat before delete: %v", err)
	}
	if err := DeleteChat(db, c.Slug, "2026-06-13T10:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if _, err := GetChat(db, c.Slug); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetChat after delete = %v, want sql.ErrNoRows (deleted chats are hidden)", err)
	}
}

// TestUpsertChatResurrectsDeleted verifies that opening a new chat on a
// deterministic slug whose previous incarnation was soft-deleted REPLACES the
// tombstone with a fresh chat (new session id, cleared deleted_at/archived_at),
// rather than failing on the primary-key conflict.
func TestUpsertChatResurrectsDeleted(t *testing.T) {
	db := openTempDB(t)

	const slug = "chat-slack-d0ba9amrhez"
	old := Chat{
		Slug:           slug,
		Title:          "old conversation",
		Provider:       "claude",
		Origin:         "slack",
		SessionID:      sql.NullString{String: "sess-old", Valid: true},
		CreatedAt:      "2026-06-13T09:00:00Z",
		LastActivityAt: "2026-06-13T09:05:00Z",
	}
	if err := InsertChat(db, old); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := DeleteChat(db, slug, "2026-06-13T10:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	fresh := Chat{
		Slug:           slug,
		Title:          "brand new conversation",
		Provider:       "codex",
		Origin:         "slack",
		SessionID:      sql.NullString{String: "sess-new", Valid: true},
		CreatedAt:      "2026-06-13T11:00:00Z",
		LastActivityAt: "2026-06-13T11:00:00Z",
	}
	if err := UpsertChat(db, fresh); err != nil {
		t.Fatalf("UpsertChat (resurrect): %v", err)
	}

	got, err := GetChat(db, slug)
	if err != nil {
		t.Fatalf("GetChat after upsert: %v", err)
	}
	if got.DeletedAt.Valid {
		t.Errorf("resurrected chat must have deleted_at cleared, got %v", got.DeletedAt)
	}
	if got.Title != "brand new conversation" {
		t.Errorf("Title = %q, want the fresh title", got.Title)
	}
	if got.Provider != "codex" {
		t.Errorf("Provider = %q, want codex (fresh values applied)", got.Provider)
	}
	if !got.SessionID.Valid || got.SessionID.String != "sess-new" {
		t.Errorf("SessionID = %v, want sess-new (fresh session, NOT the deleted one)", got.SessionID)
	}
	if got.CreatedAt != "2026-06-13T11:00:00Z" {
		t.Errorf("CreatedAt = %q, want the fresh timestamp", got.CreatedAt)
	}
}

// TestUpsertChatInsertsWhenAbsent verifies UpsertChat behaves like InsertChat
// when there is no existing row for the slug.
func TestUpsertChatInsertsWhenAbsent(t *testing.T) {
	db := openTempDB(t)

	c := Chat{
		Slug:           "chat-slack-fresh",
		Title:          "first message",
		Provider:       "claude",
		Origin:         "slack",
		SessionID:      sql.NullString{String: "sess-1", Valid: true},
		CreatedAt:      "2026-06-13T11:00:00Z",
		LastActivityAt: "2026-06-13T11:00:00Z",
	}
	if err := UpsertChat(db, c); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	got, err := GetChat(db, c.Slug)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if got.Title != c.Title || got.Provider != c.Provider {
		t.Errorf("UpsertChat insert mismatch: got %+v", got)
	}
}

func TestChatInsertNullableSessionID(t *testing.T) {
	db := openTempDB(t)

	c := Chat{
		Slug:           "overview-noid",
		Title:          "No Session",
		Provider:       "codex",
		Origin:         "slack",
		CreatedAt:      "2026-06-13T10:00:00Z",
		LastActivityAt: "2026-06-13T10:00:00Z",
	}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	got, err := GetChat(db, "overview-noid")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if got.SessionID.Valid {
		t.Errorf("SessionID should be NULL when not set, got %v", got.SessionID)
	}
}

func TestListChatsOrderByLastActivity(t *testing.T) {
	db := openTempDB(t)

	chats := []Chat{
		{Slug: "chat-a", Title: "A", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"},
		{Slug: "chat-b", Title: "B", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T09:00:00Z", LastActivityAt: "2026-06-13T09:00:00Z"},
		{Slug: "chat-c", Title: "C", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T07:00:00Z", LastActivityAt: "2026-06-13T10:00:00Z"},
	}
	for _, c := range chats {
		if err := InsertChat(db, c); err != nil {
			t.Fatalf("InsertChat %s: %v", c.Slug, err)
		}
	}

	got, err := ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Expect newest last_activity_at first: chat-c (10:00), chat-b (09:00), chat-a (08:00)
	wantOrder := []string{"chat-c", "chat-b", "chat-a"}
	for i, want := range wantOrder {
		if got[i].Slug != want {
			t.Errorf("got[%d].Slug = %q, want %q", i, got[i].Slug, want)
		}
	}
}

func TestListChatsExcludesDeleted(t *testing.T) {
	db := openTempDB(t)

	alive := Chat{Slug: "chat-alive", Title: "Alive", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	dead := Chat{Slug: "chat-dead", Title: "Dead", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}

	if err := InsertChat(db, alive); err != nil {
		t.Fatalf("InsertChat alive: %v", err)
	}
	if err := InsertChat(db, dead); err != nil {
		t.Fatalf("InsertChat dead: %v", err)
	}
	if err := DeleteChat(db, "chat-dead", "2026-06-13T09:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	got, err := ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (deleted excluded)", len(got))
	}
	if got[0].Slug != "chat-alive" {
		t.Errorf("got[0].Slug = %q, want chat-alive", got[0].Slug)
	}
}

func TestListChatsDeletedAlwaysHidden(t *testing.T) {
	db := openTempDB(t)

	c := Chat{Slug: "chat-gone", Title: "Gone", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := DeleteChat(db, "chat-gone", "2026-06-13T09:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	// Even IncludeArchived=true must not return deleted chats.
	got, err := ListChats(db, ChatFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("deleted chat appeared in list: %+v", got)
	}
}

func TestListChatsExcludesArchivedByDefault(t *testing.T) {
	db := openTempDB(t)

	active := Chat{Slug: "chat-active", Title: "Active", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	archived := Chat{Slug: "chat-archived", Title: "Archived", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T07:00:00Z", LastActivityAt: "2026-06-13T07:00:00Z"}

	if err := InsertChat(db, active); err != nil {
		t.Fatalf("InsertChat active: %v", err)
	}
	if err := InsertChat(db, archived); err != nil {
		t.Fatalf("InsertChat archived: %v", err)
	}
	if err := ArchiveChat(db, "chat-archived", "2026-06-13T09:00:00Z"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}

	// Default filter: archived hidden.
	got, err := ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "chat-active" {
		t.Errorf("expected only chat-active, got %v", got)
	}

	// IncludeArchived=true: both visible.
	got, err = ListChats(db, ChatFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListChats IncludeArchived: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d with IncludeArchived, want 2", len(got))
	}
}

func TestTouchChatChangesOrdering(t *testing.T) {
	db := openTempDB(t)

	c1 := Chat{Slug: "chat-1", Title: "One", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	c2 := Chat{Slug: "chat-2", Title: "Two", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T09:00:00Z", LastActivityAt: "2026-06-13T09:00:00Z"}

	for _, c := range []Chat{c1, c2} {
		if err := InsertChat(db, c); err != nil {
			t.Fatalf("InsertChat %s: %v", c.Slug, err)
		}
	}

	// chat-2 is first (newer last_activity).
	got, err := ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if got[0].Slug != "chat-2" {
		t.Fatalf("before touch: want chat-2 first, got %s", got[0].Slug)
	}

	// Touch chat-1 to bump it ahead.
	if err := TouchChat(db, "chat-1", "2026-06-13T10:00:00Z"); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}

	got, err = ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats after touch: %v", err)
	}
	if got[0].Slug != "chat-1" {
		t.Errorf("after touch: want chat-1 first, got %s", got[0].Slug)
	}

	// Verify the stored value.
	c, err := GetChat(db, "chat-1")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if c.LastActivityAt != "2026-06-13T10:00:00Z" {
		t.Errorf("LastActivityAt = %q, want 2026-06-13T10:00:00Z", c.LastActivityAt)
	}
}

func TestArchiveChatAndUnarchive(t *testing.T) {
	db := openTempDB(t)

	c := Chat{Slug: "chat-x", Title: "X", Provider: "claude", Origin: "ui", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}

	if err := ArchiveChat(db, "chat-x", "2026-06-13T09:00:00Z"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}

	// Hidden in default list.
	got, err := ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("archived chat should be hidden, got %d rows", len(got))
	}

	// Visible with IncludeArchived.
	got, err = ListChats(db, ChatFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListChats IncludeArchived: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "chat-x" {
		t.Errorf("chat-x not found with IncludeArchived, got %v", got)
	}
	if !got[0].ArchivedAt.Valid {
		t.Errorf("ArchivedAt should be set")
	}

	// Unarchive brings it back.
	if err := UnarchiveChat(db, "chat-x"); err != nil {
		t.Fatalf("UnarchiveChat: %v", err)
	}
	got, err = ListChats(db, ChatFilter{})
	if err != nil {
		t.Fatalf("ListChats after unarchive: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "chat-x" {
		t.Errorf("chat-x not visible after unarchive, got %v", got)
	}
	if got[0].ArchivedAt.Valid {
		t.Errorf("ArchivedAt should be NULL after unarchive, got %v", got[0].ArchivedAt)
	}
}

func TestDeleteChatHidesFromAllLists(t *testing.T) {
	db := openTempDB(t)

	c := Chat{Slug: "chat-del", Title: "Del", Provider: "codex", Origin: "slack", CreatedAt: "2026-06-13T08:00:00Z", LastActivityAt: "2026-06-13T08:00:00Z"}
	if err := InsertChat(db, c); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := DeleteChat(db, "chat-del", "2026-06-13T09:00:00Z"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	for _, ia := range []bool{false, true} {
		got, err := ListChats(db, ChatFilter{IncludeArchived: ia})
		if err != nil {
			t.Fatalf("ListChats IncludeArchived=%v: %v", ia, err)
		}
		if len(got) != 0 {
			t.Errorf("deleted chat visible with IncludeArchived=%v, got %d rows", ia, len(got))
		}
	}

	// GetChat hides a soft-deleted chat (so the Slack path opens fresh, not
	// resurrects), but the row still physically exists (soft delete, not hard).
	if _, err := GetChat(db, "chat-del"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetChat deleted = %v, want sql.ErrNoRows (deleted chats are hidden)", err)
	}
	var deletedAt sql.NullString
	if err := db.QueryRow("SELECT deleted_at FROM chats WHERE slug = ?", "chat-del").Scan(&deletedAt); err != nil {
		t.Fatalf("tombstone row should still exist after soft delete: %v", err)
	}
	if !deletedAt.Valid {
		t.Errorf("deleted_at should be set on the tombstone row")
	}
}

func TestChatsTableExistsAfterOpenDB(t *testing.T) {
	db := openTempDB(t)
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='chats'").Scan(&name)
	if err != nil {
		t.Fatalf("chats table missing: %v", err)
	}
	if name != "chats" {
		t.Errorf("table name = %q, want chats", name)
	}
}
