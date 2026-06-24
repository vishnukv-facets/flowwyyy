package productdb

// connectors.go holds productdb's read/write helpers for flowwyyy-OWNED
// (Bucket F) connector tables on the shared flow.db: github_event_log,
// github_webhook_deliveries, steering_watermark, and the Slack thread-cursor
// read over attention_thread_state. These are tables the core `flow` engine has
// no concept of, so flowwyyy reads AND writes them directly here (no exec, no
// flowdb import) — the ownership model in seam §11. Each helper mirrors its
// flowdb twin exactly; parity is enforced by connectors_test.go.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------- github_event_log ----------

// GitHubEventLogEntry is one processed GitHub event/comment recorded for
// idempotency (twin of flowdb.GitHubEventLogEntry).
type GitHubEventLogEntry struct {
	EventKey  string
	EventKind string
	TaskSlug  string
	RawJSON   string
}

// HasGitHubEvent reports whether eventKey has already been processed.
func HasGitHubEvent(db *sql.DB, eventKey string) (bool, error) {
	key := strings.TrimSpace(eventKey)
	if key == "" {
		return false, fmt.Errorf("github event key is empty")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM github_event_log WHERE event_key = ?`, key).Scan(&n); err != nil {
		return false, fmt.Errorf("check github event %s: %w", key, err)
	}
	return n > 0, nil
}

// RecordGitHubEvent records a processed GitHub event. The returned bool is true
// only for the first insert; duplicate keys return false with nil error.
func RecordGitHubEvent(db *sql.DB, entry GitHubEventLogEntry) (bool, error) {
	key := strings.TrimSpace(entry.EventKey)
	if key == "" {
		return false, fmt.Errorf("github event key is empty")
	}
	kind := strings.TrimSpace(entry.EventKind)
	if kind == "" {
		return false, fmt.Errorf("github event kind is empty")
	}
	var taskSlug any
	if slug := strings.TrimSpace(entry.TaskSlug); slug != "" {
		taskSlug = slug
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO github_event_log (event_key, event_kind, task_slug, raw_json, processed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, kind, taskSlug, strings.TrimSpace(entry.RawJSON), NowISO(),
	)
	if err != nil {
		return false, fmt.Errorf("record github event %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record github event %s rows affected: %w", key, err)
	}
	return n > 0, nil
}

// ---------- github_webhook_deliveries ----------

// GitHubDeliveryEntry is one raw webhook delivery as received, before
// normalization (twin of flowdb.GitHubDeliveryEntry).
type GitHubDeliveryEntry struct {
	DeliveryID string
	EventType  string
	Action     string
}

// RecordGitHubDelivery inserts a received webhook delivery keyed on its
// X-GitHub-Delivery id. The returned bool is true only on the first insert; a
// redelivery returns false so the receiver can skip reprocessing.
func RecordGitHubDelivery(db *sql.DB, e GitHubDeliveryEntry) (bool, error) {
	id := strings.TrimSpace(e.DeliveryID)
	if id == "" {
		return false, fmt.Errorf("github delivery id is empty")
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO github_webhook_deliveries
		   (delivery_id, event_type, action, status, received_at)
		 VALUES (?, ?, ?, 'received', ?)`,
		id, strings.TrimSpace(e.EventType), strings.TrimSpace(e.Action), NowISO(),
	)
	if err != nil {
		return false, fmt.Errorf("record github delivery %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record github delivery %s rows: %w", id, err)
	}
	return n > 0, nil
}

// FinishGitHubDelivery records the terminal outcome of processing a delivery:
// status is one of "processed", "ignored", or "error".
func FinishGitHubDelivery(db *sql.DB, deliveryID, status, errMsg string, eventCount int) error {
	id := strings.TrimSpace(deliveryID)
	if id == "" {
		return fmt.Errorf("github delivery id is empty")
	}
	var errVal any
	if e := strings.TrimSpace(errMsg); e != "" {
		errVal = e
	}
	_, err := db.Exec(
		`UPDATE github_webhook_deliveries
		   SET status = ?, error = ?, event_count = ?, processed_at = ?
		 WHERE delivery_id = ?`,
		strings.TrimSpace(status), errVal, eventCount, NowISO(), id,
	)
	if err != nil {
		return fmt.Errorf("finish github delivery %s: %w", id, err)
	}
	return nil
}

// ---------- steering_watermark ----------

// GetSteeringWatermark returns the last processed Slack ts for a channel, or ""
// when none recorded yet.
func GetSteeringWatermark(db *sql.DB, channel string) (string, error) {
	var lastTS string
	err := db.QueryRow(`SELECT last_ts FROM steering_watermark WHERE channel = ?`, channel).Scan(&lastTS)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("productdb: get steering watermark: %w", err)
	}
	return lastTS, nil
}

// SetSteeringWatermark upserts the resume cursor for a channel.
func SetSteeringWatermark(db *sql.DB, channel, lastTS, updatedAt string) error {
	_, err := db.Exec(`
		INSERT INTO steering_watermark (channel, last_ts, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(channel) DO UPDATE SET last_ts = excluded.last_ts, updated_at = excluded.updated_at`,
		channel, lastTS, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("productdb: set steering watermark: %w", err)
	}
	return nil
}

// ThreadCursor + ListRecentSlackThreadCursors live in attention_thread_state.go
// (ported from flowdb alongside the rest of the thread-state read layer).
