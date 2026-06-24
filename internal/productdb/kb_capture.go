package productdb

import (
	"database/sql"
	"errors"
	"fmt"
)

// KBCaptureCursor records how far the session KB distiller has swept a given
// agent session's transcript. cursor is the transcript byte offset captured
// through; only the delta beyond it is ever distilled again, so idle/unchanged
// sessions are skipped and old turns are never re-mined.
type KBCaptureCursor struct {
	SessionID  string
	Slug       string
	Kind       string // "task" | "chat"
	Cursor     int64
	CapturedAt string
}

// GetKBCaptureCursor returns the distiller cursor for a session. ok=false (with
// nil error) when the session has never been swept — callers treat that as
// cursor 0 (sweep everything once it qualifies).
func GetKBCaptureCursor(db *sql.DB, sessionID string) (KBCaptureCursor, bool, error) {
	var c KBCaptureCursor
	err := db.QueryRow(
		`SELECT session_id, slug, kind, cursor, captured_at FROM kb_capture WHERE session_id = ?`,
		sessionID,
	).Scan(&c.SessionID, &c.Slug, &c.Kind, &c.Cursor, &c.CapturedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KBCaptureCursor{}, false, nil
	}
	if err != nil {
		return KBCaptureCursor{}, false, fmt.Errorf("productdb: get kb cursor %q: %w", sessionID, err)
	}
	return c, true, nil
}

// UpsertKBCaptureCursor records a successful sweep: it advances the cursor to the
// byte offset distilled through and stamps captured_at. Keyed by session_id so a
// task and a chat are tracked independently.
func UpsertKBCaptureCursor(db *sql.DB, c KBCaptureCursor) error {
	_, err := db.Exec(
		`INSERT INTO kb_capture (session_id, slug, kind, cursor, captured_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(session_id) DO UPDATE SET
		   slug = excluded.slug,
		   kind = excluded.kind,
		   cursor = excluded.cursor,
		   captured_at = excluded.captured_at`,
		c.SessionID, c.Slug, c.Kind, c.Cursor, c.CapturedAt,
	)
	if err != nil {
		return fmt.Errorf("productdb: upsert kb cursor %q: %w", c.SessionID, err)
	}
	return nil
}
