package flowdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// Steering mute scopes. A row in steering_mutes with one of these scopes makes
// Stage 0 permanently drop matching events.
const (
	MuteScopeChannel = "channel" // Slack channel id, or owner/repo for GitHub
	MuteScopeAuthor  = "author"  // Slack user id, or GitHub login
	MuteScopeThread  = "thread"  // a thread key (channel:ts, or the gh link tag composite)
)

// SteeringMutes is the resolved set of operator suppressions, ready for Stage 0
// membership checks. Each map is keyed by the muted value.
type SteeringMutes struct {
	Channels map[string]bool
	Authors  map[string]bool
	Threads  map[string]bool
}

// AddSteeringMute records a permanent suppression. Idempotent (INSERT OR IGNORE
// on the (scope,value) primary key). Empty scope/value is a no-op error.
func AddSteeringMute(db *sql.DB, scope, value string) error {
	scope = strings.TrimSpace(scope)
	value = strings.TrimSpace(value)
	if scope == "" || value == "" {
		return fmt.Errorf("flowdb: steering mute requires scope and value")
	}
	_, err := db.Exec(
		`INSERT OR IGNORE INTO steering_mutes (scope, value, created_at) VALUES (?, ?, ?)`,
		scope, value, NowISO(),
	)
	if err != nil {
		return fmt.Errorf("flowdb: add steering mute %s=%q: %w", scope, value, err)
	}
	return nil
}

// RemoveSteeringMute deletes a suppression (for an unmute affordance). No error
// when the row doesn't exist.
func RemoveSteeringMute(db *sql.DB, scope, value string) error {
	_, err := db.Exec(`DELETE FROM steering_mutes WHERE scope = ? AND value = ?`, strings.TrimSpace(scope), strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("flowdb: remove steering mute %s=%q: %w", scope, value, err)
	}
	return nil
}

// ListSteeringMutes loads every suppression into membership sets. Returns empty
// (non-nil) maps when there are no rows, so callers can index without nil checks.
func ListSteeringMutes(db *sql.DB) (SteeringMutes, error) {
	out := SteeringMutes{
		Channels: map[string]bool{},
		Authors:  map[string]bool{},
		Threads:  map[string]bool{},
	}
	if db == nil {
		return out, nil
	}
	rows, err := db.Query(`SELECT scope, value FROM steering_mutes`)
	if err != nil {
		return out, fmt.Errorf("flowdb: list steering mutes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, value string
		if err := rows.Scan(&scope, &value); err != nil {
			return out, fmt.Errorf("flowdb: scan steering mute: %w", err)
		}
		switch scope {
		case MuteScopeChannel:
			out.Channels[value] = true
		case MuteScopeAuthor:
			out.Authors[value] = true
		case MuteScopeThread:
			out.Threads[value] = true
		}
	}
	return out, rows.Err()
}
