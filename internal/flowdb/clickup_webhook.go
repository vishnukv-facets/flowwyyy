package flowdb

import (
	"database/sql"
	"fmt"
	"strings"
)

type ClickUpDeliveryEntry struct {
	DeliveryID string
	EventType  string
	TaskID     string
	WebhookID  string
}

func RecordClickUpDelivery(db *sql.DB, e ClickUpDeliveryEntry) (bool, error) {
	id := strings.TrimSpace(e.DeliveryID)
	if id == "" {
		return false, fmt.Errorf("clickup delivery id is empty")
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO clickup_webhook_deliveries
		   (delivery_id, event_type, task_id, webhook_id, status, received_at)
		 VALUES (?, ?, ?, ?, 'received', ?)`,
		id, strings.TrimSpace(e.EventType), strings.TrimSpace(e.TaskID), strings.TrimSpace(e.WebhookID), NowISO(),
	)
	if err != nil {
		return false, fmt.Errorf("record clickup delivery %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record clickup delivery %s rows: %w", id, err)
	}
	return n > 0, nil
}

func FinishClickUpDelivery(db *sql.DB, deliveryID, status, errMsg string, eventCount int) error {
	id := strings.TrimSpace(deliveryID)
	if id == "" {
		return fmt.Errorf("clickup delivery id is empty")
	}
	var errVal any
	if e := strings.TrimSpace(errMsg); e != "" {
		errVal = e
	}
	_, err := db.Exec(
		`UPDATE clickup_webhook_deliveries
		   SET status = ?, error = ?, event_count = ?, processed_at = ?
		 WHERE delivery_id = ?`,
		strings.TrimSpace(status), errVal, eventCount, NowISO(), id,
	)
	if err != nil {
		return fmt.Errorf("finish clickup delivery %s: %w", id, err)
	}
	return nil
}

type ClickUpWebhookHealthInfo struct {
	Total          int
	LastReceivedAt string
	LastStatus     string
	LastError      string
	LastEventType  string
}

func ClickUpWebhookHealth(db *sql.DB) (ClickUpWebhookHealthInfo, error) {
	var h ClickUpWebhookHealthInfo
	if err := db.QueryRow(`SELECT COUNT(*) FROM clickup_webhook_deliveries`).Scan(&h.Total); err != nil {
		return h, fmt.Errorf("clickup webhook health count: %w", err)
	}
	if h.Total == 0 {
		return h, nil
	}
	err := db.QueryRow(
		`SELECT received_at, status, COALESCE(error, ''), event_type
		   FROM clickup_webhook_deliveries
		  ORDER BY received_at DESC, rowid DESC
		  LIMIT 1`,
	).Scan(&h.LastReceivedAt, &h.LastStatus, &h.LastError, &h.LastEventType)
	if err != nil {
		return h, fmt.Errorf("clickup webhook health latest: %w", err)
	}
	return h, nil
}
