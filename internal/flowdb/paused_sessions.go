package flowdb

import (
	"database/sql"
	"strings"
)

type PausedSession struct {
	Slug      string
	Provider  string
	SessionID sql.NullString
	PausedAt  string
	UpdatedAt string
}

func PauseSession(db *sql.DB, slug, provider, sessionID string) error {
	now := NowISO()
	_, err := db.Exec(
		`INSERT INTO paused_sessions (slug, provider, session_id, paused_at, updated_at)
		 VALUES (?, ?, NULLIF(?, ''), ?, ?)
		 ON CONFLICT(slug) DO UPDATE SET
		   provider = excluded.provider,
		   session_id = excluded.session_id,
		   updated_at = excluded.updated_at`,
		strings.TrimSpace(slug), strings.TrimSpace(provider), strings.TrimSpace(sessionID), now, now,
	)
	return err
}

func ClearPausedSession(db *sql.DB, slug string) error {
	_, err := db.Exec(`DELETE FROM paused_sessions WHERE slug = ?`, strings.TrimSpace(slug))
	return err
}

func GetPausedSession(db *sql.DB, slug string) (PausedSession, bool, error) {
	row := db.QueryRow(
		`SELECT slug, provider, session_id, paused_at, updated_at
		   FROM paused_sessions WHERE slug = ?`,
		strings.TrimSpace(slug),
	)
	var ps PausedSession
	switch err := row.Scan(&ps.Slug, &ps.Provider, &ps.SessionID, &ps.PausedAt, &ps.UpdatedAt); err {
	case nil:
		return ps, true, nil
	case sql.ErrNoRows:
		return PausedSession{}, false, nil
	default:
		return PausedSession{}, false, err
	}
}

func EnqueuePausedSessionInput(db *sql.DB, slug, prompt string) (int64, error) {
	return EnqueuePausedSessionInputAfter(db, slug, prompt, "")
}

func EnqueuePausedSessionInputAfter(db *sql.DB, slug, prompt, notBefore string) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO paused_session_queue (slug, prompt, not_before, created_at)
		 VALUES (?, ?, ?, ?)`,
		strings.TrimSpace(slug), prompt, strings.TrimSpace(notBefore), NowISO(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func HasPausedSessionInput(db *sql.DB, slug string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM paused_session_queue WHERE slug = ?`,
		strings.TrimSpace(slug),
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func PeekPausedSessionInput(db *sql.DB, slug string) (PendingWake, bool, error) {
	row := db.QueryRow(
		`SELECT id, slug, prompt, COALESCE(not_before, ''), created_at
		   FROM paused_session_queue WHERE slug = ? ORDER BY id ASC LIMIT 1`,
		strings.TrimSpace(slug),
	)
	var pw PendingWake
	switch err := row.Scan(&pw.ID, &pw.Slug, &pw.Prompt, &pw.NotBefore, &pw.CreatedAt); err {
	case nil:
		return pw, true, nil
	case sql.ErrNoRows:
		return PendingWake{}, false, nil
	default:
		return PendingWake{}, false, err
	}
}

// MovePausedSessionInputsToPendingWakes transfers paused inputs into the
// existing wake FIFO atomically, preserving order and not_before holds.
func MovePausedSessionInputsToPendingWakes(db *sql.DB, slug string) (int, error) {
	slug = strings.TrimSpace(slug)
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT id, prompt, COALESCE(not_before, ''), created_at
		   FROM paused_session_queue WHERE slug = ? ORDER BY id ASC`,
		slug,
	)
	if err != nil {
		return 0, err
	}
	type item struct {
		id                int64
		prompt, notBefore string
	}
	var items []item
	for rows.Next() {
		var it item
		var createdAt string
		if err := rows.Scan(&it.id, &it.prompt, &it.notBefore, &createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, it := range items {
		if _, err := tx.Exec(
			`INSERT INTO pending_wakes (slug, prompt, not_before, created_at)
			 VALUES (?, ?, ?, ?)`,
			slug, it.prompt, strings.TrimSpace(it.notBefore), NowISO(),
		); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM paused_session_queue WHERE id = ?`, it.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}
