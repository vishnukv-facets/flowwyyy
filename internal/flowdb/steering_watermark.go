package flowdb

import (
	"database/sql"
	"errors"
	"fmt"
)

// GetSteeringWatermark returns the last processed Slack ts for a channel, or
// "" when none recorded yet.
func GetSteeringWatermark(db *sql.DB, channel string) (string, error) {
	var lastTS string
	err := db.QueryRow(`SELECT last_ts FROM steering_watermark WHERE channel = ?`, channel).Scan(&lastTS)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("flowdb: get steering watermark: %w", err)
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
		return fmt.Errorf("flowdb: set steering watermark: %w", err)
	}
	return nil
}
