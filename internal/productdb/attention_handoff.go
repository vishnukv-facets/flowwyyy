package productdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// AttentionHandoff is one confirmed-routing attempt for an attention feed item.
// The feed card remains the durable source item; this row records the ask sent
// to a candidate owning task and that task's eventual verdict.
type AttentionHandoff struct {
	ID               string
	FeedItemID       string
	Sender           string
	Receiver         string
	Context          string
	RequestedVerdict string
	Status           string // pending|accepted|declined|timeout
	Reason           string
	RequestedAt      string
	ExpiresAt        string
	RespondedAt      string
}

func CreateAttentionHandoff(db *sql.DB, h AttentionHandoff) (AttentionHandoff, error) {
	if db == nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: create attention handoff requires db")
	}
	h.FeedItemID = strings.TrimSpace(h.FeedItemID)
	h.Sender = strings.TrimSpace(h.Sender)
	h.Receiver = strings.TrimSpace(h.Receiver)
	h.Context = strings.TrimSpace(h.Context)
	h.RequestedVerdict = strings.TrimSpace(h.RequestedVerdict)
	h.Status = normalizeHandoffStatus(h.Status)
	h.Reason = strings.TrimSpace(h.Reason)
	h.RequestedAt = strings.TrimSpace(h.RequestedAt)
	h.ExpiresAt = strings.TrimSpace(h.ExpiresAt)
	h.RespondedAt = strings.TrimSpace(h.RespondedAt)
	if h.ID == "" {
		h.ID = randomHandoffID()
	}
	if h.Status == "" {
		h.Status = "pending"
	}
	if h.FeedItemID == "" || h.Sender == "" || h.Receiver == "" || h.Context == "" || h.RequestedVerdict == "" || h.RequestedAt == "" || h.ExpiresAt == "" {
		return AttentionHandoff{}, fmt.Errorf("productdb: attention handoff requires feed_item_id, sender, receiver, context, requested_verdict, requested_at, expires_at")
	}
	if !validHandoffStatus(h.Status) {
		return AttentionHandoff{}, fmt.Errorf("productdb: unsupported attention handoff status %q", h.Status)
	}
	_, err := db.Exec(
		`INSERT INTO attention_handoffs (
			id, feed_item_id, sender, receiver, context, requested_verdict,
			status, reason, requested_at, expires_at, responded_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.FeedItemID, h.Sender, h.Receiver, h.Context, h.RequestedVerdict,
		h.Status, NullIfEmpty(h.Reason), h.RequestedAt, h.ExpiresAt, NullIfEmpty(h.RespondedAt),
	)
	if err != nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: create attention handoff: %w", err)
	}
	return h, nil
}

func GetAttentionHandoff(db *sql.DB, id string) (AttentionHandoff, error) {
	if db == nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: get attention handoff requires db")
	}
	id = strings.TrimSpace(id)
	var h AttentionHandoff
	var reason, respondedAt sql.NullString
	err := db.QueryRow(
		`SELECT id, feed_item_id, sender, receiver, context, requested_verdict,
		        status, reason, requested_at, expires_at, responded_at
		   FROM attention_handoffs
		  WHERE id = ?`,
		id,
	).Scan(&h.ID, &h.FeedItemID, &h.Sender, &h.Receiver, &h.Context, &h.RequestedVerdict,
		&h.Status, &reason, &h.RequestedAt, &h.ExpiresAt, &respondedAt)
	if err != nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: get attention handoff %q: %w", id, err)
	}
	h.Reason = reason.String
	h.RespondedAt = respondedAt.String
	return h, nil
}

func LatestAttentionHandoffForFeed(db *sql.DB, feedItemID string) (AttentionHandoff, bool, error) {
	if db == nil {
		return AttentionHandoff{}, false, fmt.Errorf("productdb: latest attention handoff requires db")
	}
	feedItemID = strings.TrimSpace(feedItemID)
	var h AttentionHandoff
	var reason, respondedAt sql.NullString
	err := db.QueryRow(
		`SELECT id, feed_item_id, sender, receiver, context, requested_verdict,
		        status, reason, requested_at, expires_at, responded_at
		   FROM attention_handoffs
		  WHERE feed_item_id = ?
		  ORDER BY requested_at DESC, id DESC
		  LIMIT 1`,
		feedItemID,
	).Scan(&h.ID, &h.FeedItemID, &h.Sender, &h.Receiver, &h.Context, &h.RequestedVerdict,
		&h.Status, &reason, &h.RequestedAt, &h.ExpiresAt, &respondedAt)
	if err == sql.ErrNoRows {
		return AttentionHandoff{}, false, nil
	}
	if err != nil {
		return AttentionHandoff{}, false, fmt.Errorf("productdb: latest attention handoff for feed %q: %w", feedItemID, err)
	}
	h.Reason = reason.String
	h.RespondedAt = respondedAt.String
	return h, true, nil
}

func DeleteAttentionHandoff(db *sql.DB, id string) error {
	if db == nil {
		return fmt.Errorf("productdb: delete attention handoff requires db")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("productdb: delete attention handoff requires id")
	}
	if _, err := db.Exec(`DELETE FROM attention_handoffs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("productdb: delete attention handoff %q: %w", id, err)
	}
	return nil
}

func RespondAttentionHandoff(db *sql.DB, id, verdict, reason, at string) (AttentionHandoff, error) {
	if db == nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: respond attention handoff requires db")
	}
	id = strings.TrimSpace(id)
	status, err := handoffVerdictStatus(verdict)
	if err != nil {
		return AttentionHandoff{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return AttentionHandoff{}, fmt.Errorf("productdb: attention handoff response requires a reason")
	}
	at = strings.TrimSpace(at)
	if at == "" {
		at = NowISO()
	}
	h, err := GetAttentionHandoff(db, id)
	if err != nil {
		return AttentionHandoff{}, err
	}
	if h.Status != "pending" {
		return AttentionHandoff{}, fmt.Errorf("productdb: attention handoff %s is %s, not pending", id, h.Status)
	}
	res, err := db.Exec(
		`UPDATE attention_handoffs
		    SET status = ?, reason = ?, responded_at = ?
		  WHERE id = ? AND status = 'pending'`,
		status, reason, at, id,
	)
	if err != nil {
		return AttentionHandoff{}, fmt.Errorf("productdb: respond attention handoff: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return AttentionHandoff{}, fmt.Errorf("productdb: attention handoff %s is no longer pending", id)
	}
	return GetAttentionHandoff(db, id)
}

func ExpireAttentionHandoffs(db *sql.DB, now string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("productdb: expire attention handoffs requires db")
	}
	now = strings.TrimSpace(now)
	if now == "" {
		now = NowISO()
	}
	res, err := db.Exec(
		`UPDATE attention_handoffs
		    SET status = 'timeout', responded_at = ?
		  WHERE status = 'pending'
		    AND expires_at <= ?`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("productdb: expire attention handoffs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func handoffVerdictStatus(verdict string) (string, error) {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(verdict)), "-", "_") {
	case "accept", "accepted":
		return "accepted", nil
	case "decline", "declined":
		return "declined", nil
	default:
		return "", fmt.Errorf("productdb: unsupported attention handoff verdict %q (want accept|decline)", verdict)
	}
}

func normalizeHandoffStatus(status string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(status)), "-", "_")
}

func validHandoffStatus(status string) bool {
	switch status {
	case "pending", "accepted", "declined", "timeout":
		return true
	default:
		return false
	}
}

func randomHandoffID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ah-" + hex.EncodeToString(b[:])
}
