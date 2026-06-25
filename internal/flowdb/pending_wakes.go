package flowdb

import (
	"database/sql"
	"strings"
)

// PendingWake is one buffered wake prompt for a task/session slug. Buffered when
// a wake arrived while the session was blocked on the operator's input, so it
// re-delivers (FIFO) once the session is free. Rows are persisted in the
// pending_wakes table so a server restart never loses a buffered wake.
type PendingWake struct {
	ID        int64
	Slug      string
	Prompt    string
	NotBefore string
	CreatedAt string
}

// EnqueuePendingWake appends a buffered wake for slug and returns its row id.
func EnqueuePendingWake(db *sql.DB, slug, prompt string) (int64, error) {
	return EnqueuePendingWakeAfter(db, slug, prompt, "")
}

// EnqueuePendingWakeAfter appends a buffered wake that must not be delivered
// before notBefore. An empty notBefore preserves the existing "ready now"
// behavior used when a session is only waiting on operator input.
func EnqueuePendingWakeAfter(db *sql.DB, slug, prompt, notBefore string) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO pending_wakes (slug, prompt, not_before, created_at) VALUES (?, ?, ?, ?)`,
		strings.TrimSpace(slug), prompt, strings.TrimSpace(notBefore), NowISO(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PeekPendingWake returns the oldest buffered wake for slug (FIFO by id) without
// removing it. ok=false (nil error) when the queue is empty. Non-destructive so
// the row survives if delivery fails or the process dies mid-flush — it is
// removed only by AckPendingWake after a confirmed inject.
func PeekPendingWake(db *sql.DB, slug string) (PendingWake, bool, error) {
	row := db.QueryRow(
		`SELECT id, slug, prompt, COALESCE(not_before, ''), created_at FROM pending_wakes
		 WHERE slug = ? ORDER BY id ASC LIMIT 1`,
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

// AckPendingWake removes a delivered wake row by id.
func AckPendingWake(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM pending_wakes WHERE id = ?`, id)
	return err
}

// HasPendingWakes reports whether slug has any buffered wake.
func HasPendingWakes(db *sql.DB, slug string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM pending_wakes WHERE slug = ?`, strings.TrimSpace(slug),
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// PendingWakeSlugs returns the distinct slugs that currently have buffered
// wakes, oldest-first. Used on startup to resume delivery after a restart.
func PendingWakeSlugs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT slug FROM pending_wakes GROUP BY slug ORDER BY MIN(id) ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		slugs = append(slugs, s)
	}
	return slugs, rows.Err()
}
