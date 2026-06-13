package flowdb

import "testing"

func TestKBCaptureCursorMissing(t *testing.T) {
	db := openTempDB(t)
	_, ok, err := GetKBCaptureCursor(db, "sess-none")
	if err != nil {
		t.Fatalf("GetKBCaptureCursor: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for an unseen session")
	}
}

func TestKBCaptureCursorUpsertRoundTrip(t *testing.T) {
	db := openTempDB(t)

	first := KBCaptureCursor{SessionID: "sess-1", Slug: "chat-slack-x", Kind: "chat", Cursor: 1024, CapturedAt: "2026-06-14T09:00:00Z"}
	if err := UpsertKBCaptureCursor(db, first); err != nil {
		t.Fatalf("UpsertKBCaptureCursor: %v", err)
	}
	got, ok, err := GetKBCaptureCursor(db, "sess-1")
	if err != nil || !ok {
		t.Fatalf("GetKBCaptureCursor: ok=%v err=%v", ok, err)
	}
	if got.Cursor != 1024 || got.Kind != "chat" || got.Slug != "chat-slack-x" || got.CapturedAt != "2026-06-14T09:00:00Z" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Upsert again advances the cursor (same session_id PK).
	if err := UpsertKBCaptureCursor(db, KBCaptureCursor{SessionID: "sess-1", Slug: "chat-slack-x", Kind: "chat", Cursor: 4096, CapturedAt: "2026-06-14T10:00:00Z"}); err != nil {
		t.Fatalf("UpsertKBCaptureCursor (advance): %v", err)
	}
	got, _, err = GetKBCaptureCursor(db, "sess-1")
	if err != nil {
		t.Fatalf("GetKBCaptureCursor (after advance): %v", err)
	}
	if got.Cursor != 4096 || got.CapturedAt != "2026-06-14T10:00:00Z" {
		t.Errorf("cursor should advance to 4096/10:00, got %+v", got)
	}
}
