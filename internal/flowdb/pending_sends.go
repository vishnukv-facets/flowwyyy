package flowdb

import (
	"database/sql"
	"strings"
)

// PendingSend is one outbound Slack message held for the operator's approval
// because its target channel is outside the operator's org (Slack Connect /
// cross-workspace). Persisted in the pending_sends table so a queued send
// survives a restart.
type PendingSend struct {
	ID           string `json:"id"`
	Channel      string `json:"channel"`
	ChannelLabel string `json:"channel_label,omitempty"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	Text         string `json:"text"`
	Identity     string `json:"identity,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	PostAt       int64  `json:"post_at,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Origin       string `json:"origin,omitempty"`
	Status       string `json:"status"` // pending | sent | discarded
	CreatedAt    string `json:"created_at"`
	DecidedAt    string `json:"decided_at,omitempty"`
}

// CreatePendingSend inserts a new pending (un-approved) outbound send.
func CreatePendingSend(db *sql.DB, ps PendingSend) error {
	status := strings.TrimSpace(ps.Status)
	if status == "" {
		status = "pending"
	}
	_, err := db.Exec(
		`INSERT INTO pending_sends (
			id, channel, channel_label, thread_ts, text, identity, file_path,
			post_at, reason, origin, status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(ps.ID), strings.TrimSpace(ps.Channel), ps.ChannelLabel,
		strings.TrimSpace(ps.ThreadTS), ps.Text, strings.TrimSpace(ps.Identity),
		strings.TrimSpace(ps.FilePath), ps.PostAt, ps.Reason, ps.Origin, status, NowISO(),
	)
	return err
}

// ListPendingSends returns sends in the given status (oldest first). An empty
// status lists every row.
func ListPendingSends(db *sql.DB, status string) ([]PendingSend, error) {
	q := `SELECT id, channel, channel_label, thread_ts, text, identity, file_path,
		post_at, reason, origin, status, created_at, decided_at FROM pending_sends`
	var args []any
	if s := strings.TrimSpace(status); s != "" {
		q += ` WHERE status = ?`
		args = append(args, s)
	}
	q += ` ORDER BY created_at ASC, id ASC`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSend
	for rows.Next() {
		ps, err := scanPendingSend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

// GetPendingSend returns one send by id, ok=false when absent.
func GetPendingSend(db *sql.DB, id string) (PendingSend, bool, error) {
	row := db.QueryRow(
		`SELECT id, channel, channel_label, thread_ts, text, identity, file_path,
			post_at, reason, origin, status, created_at, decided_at
		 FROM pending_sends WHERE id = ?`,
		strings.TrimSpace(id),
	)
	ps, err := scanPendingSend(row)
	switch err {
	case nil:
		return ps, true, nil
	case sql.ErrNoRows:
		return PendingSend{}, false, nil
	default:
		return PendingSend{}, false, err
	}
}

// SetPendingSendStatus marks a send sent/discarded and stamps decided_at.
func SetPendingSendStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec(
		`UPDATE pending_sends SET status = ?, decided_at = ? WHERE id = ?`,
		strings.TrimSpace(status), NowISO(), strings.TrimSpace(id),
	)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPendingSend(row rowScanner) (PendingSend, error) {
	var ps PendingSend
	var label, threadTS, identity, filePath, reason, origin, decidedAt sql.NullString
	if err := row.Scan(
		&ps.ID, &ps.Channel, &label, &threadTS, &ps.Text, &identity, &filePath,
		&ps.PostAt, &reason, &origin, &ps.Status, &ps.CreatedAt, &decidedAt,
	); err != nil {
		return PendingSend{}, err
	}
	ps.ChannelLabel = label.String
	ps.ThreadTS = threadTS.String
	ps.Identity = identity.String
	ps.FilePath = filePath.String
	ps.Reason = reason.String
	ps.Origin = origin.String
	ps.DecidedAt = decidedAt.String
	return ps, nil
}
