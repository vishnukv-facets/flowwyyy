package flowdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// GitHubDeliveryEntry is one raw webhook delivery as received, before
// normalization. DeliveryID is the X-GitHub-Delivery header.
type GitHubDeliveryEntry struct {
	DeliveryID string
	EventType  string
	Action     string
}

// RecordGitHubDelivery inserts a received webhook delivery keyed on its
// X-GitHub-Delivery id. The returned bool is true only on the first insert; a
// redelivery (GitHub reuses the delivery id) returns false so the receiver can
// skip reprocessing it.
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
// status is one of "processed", "ignored", or "error". errMsg is stored only
// for the error case; eventCount is how many normalized events the delivery
// produced.
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

// GitHubWebhookHealthInfo summarizes the delivery log for the connector status
// surface: how many deliveries have ever arrived and the state of the latest.
type GitHubWebhookHealthInfo struct {
	Total          int
	LastReceivedAt string
	LastStatus     string
	LastError      string
}

// GitHubWebhookHealth reads delivery-log state for Mission Control's GitHub
// connector card. A zero Total with empty LastReceivedAt means "configured but
// not yet receiving".
func GitHubWebhookHealth(db *sql.DB) (GitHubWebhookHealthInfo, error) {
	var h GitHubWebhookHealthInfo
	if err := db.QueryRow(`SELECT COUNT(*) FROM github_webhook_deliveries`).Scan(&h.Total); err != nil {
		return h, fmt.Errorf("github webhook health count: %w", err)
	}
	if h.Total == 0 {
		return h, nil
	}
	err := db.QueryRow(
		`SELECT received_at, status, COALESCE(error, '')
		   FROM github_webhook_deliveries
		  ORDER BY received_at DESC, rowid DESC
		  LIMIT 1`,
	).Scan(&h.LastReceivedAt, &h.LastStatus, &h.LastError)
	if err != nil {
		return h, fmt.Errorf("github webhook health latest: %w", err)
	}
	return h, nil
}
