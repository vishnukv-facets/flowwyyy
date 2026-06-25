package flowdb

import (
	"database/sql"
	"strings"
)

const (
	RateLimitQueueSlackEvent  = "slack_event"
	RateLimitQueueGitHubEvent = "github_event"
	RateLimitQueueOpenTask    = "open_task"
)

// RateLimitQueueItem is one automatic action held until a provider reset time.
// The payload is connector-specific JSON owned by the caller; flowdb preserves
// it durably and orders replay FIFO by id once run_after is due.
type RateLimitQueueItem struct {
	ID          int64
	Kind        string
	Provider    string
	PayloadJSON string
	RunAfter    string
	Status      string
	Attempts    int
	LastError   sql.NullString
	CreatedAt   string
	UpdatedAt   string
}

// EnqueueRateLimitQueue inserts one provider-limited action for later replay.
func EnqueueRateLimitQueue(db *sql.DB, kind, provider string, payloadJSON []byte, runAfter string) (int64, error) {
	now := NowISO()
	res, err := db.Exec(
		`INSERT INTO rate_limit_queue
		 (kind, provider, payload_json, run_after, status, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'pending', 0, ?, ?)`,
		strings.TrimSpace(kind),
		strings.TrimSpace(provider),
		string(payloadJSON),
		strings.TrimSpace(runAfter),
		now,
		now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListReadyRateLimitQueue returns pending queue rows whose hold time has passed.
func ListReadyRateLimitQueue(db *sql.DB, now string, limit int) ([]RateLimitQueueItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(
		`SELECT id, kind, provider, payload_json, run_after, status, attempts,
		        last_error, created_at, updated_at
		   FROM rate_limit_queue
		  WHERE status = 'pending' AND run_after <= ?
		  ORDER BY id ASC
		  LIMIT ?`,
		strings.TrimSpace(now),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RateLimitQueueItem
	for rows.Next() {
		var it RateLimitQueueItem
		if err := rows.Scan(
			&it.ID, &it.Kind, &it.Provider, &it.PayloadJSON, &it.RunAfter, &it.Status,
			&it.Attempts, &it.LastError, &it.CreatedAt, &it.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// NextRateLimitQueueRunAfter returns the next pending run_after timestamp.
func NextRateLimitQueueRunAfter(db *sql.DB) (string, bool, error) {
	var runAfter string
	err := db.QueryRow(
		`SELECT run_after FROM rate_limit_queue
		  WHERE status = 'pending'
		  ORDER BY run_after ASC, id ASC
		  LIMIT 1`,
	).Scan(&runAfter)
	switch err {
	case nil:
		return runAfter, true, nil
	case sql.ErrNoRows:
		return "", false, nil
	default:
		return "", false, err
	}
}

// AckRateLimitQueue marks one queued action as replayed.
func AckRateLimitQueue(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE rate_limit_queue
		    SET status = 'done', updated_at = ?
		  WHERE id = ?`,
		NowISO(),
		id,
	)
	return err
}

// RescheduleRateLimitQueue keeps a failed/continued hold pending for a later
// replay attempt and records the last error for diagnostics.
func RescheduleRateLimitQueue(db *sql.DB, id int64, runAfter, lastError string) error {
	_, err := db.Exec(
		`UPDATE rate_limit_queue
		    SET status = 'pending',
		        attempts = attempts + 1,
		        run_after = ?,
		        last_error = ?,
		        updated_at = ?
		  WHERE id = ?`,
		strings.TrimSpace(runAfter),
		strings.TrimSpace(lastError),
		NowISO(),
		id,
	)
	return err
}

// CountPendingRateLimitQueue is used by tests and diagnostics.
func CountPendingRateLimitQueue(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(1) FROM rate_limit_queue WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// PendingRateLimitQueueStats reports how many pending actions are held by a
// provider limit and when the next one is eligible to replay.
func PendingRateLimitQueueStats(db *sql.DB, provider string) (int, string, error) {
	provider = strings.TrimSpace(provider)
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM rate_limit_queue WHERE status = 'pending' AND provider = ?`,
		provider,
	).Scan(&n); err != nil {
		return 0, "", err
	}
	var next sql.NullString
	if err := db.QueryRow(
		`SELECT MIN(run_after) FROM rate_limit_queue WHERE status = 'pending' AND provider = ?`,
		provider,
	).Scan(&next); err != nil {
		return 0, "", err
	}
	if next.Valid {
		return n, next.String, nil
	}
	return n, "", nil
}
