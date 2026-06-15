package flowdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// AttentionFeedback is the operator-outcome snapshot for one attention card
// action. It intentionally denormalizes the source fields from attention_feed
// so reports preserve what the operator saw at action time.
type AttentionFeedback struct {
	ID              string
	FeedItemID      string
	Source          string
	Channel         string
	Author          string
	ThreadType      string
	ThreadKey       string
	SuggestedAction string
	FinalAction     string
	Outcome         string // approved|dismissed|muted|retriaged|opened|sent
	Confidence      float64
	ConfidenceBand  string
	DraftBefore     string
	DraftAfter      string
	DraftEditDelta  string
	CreatedAt       string
}

// AttentionFeedbackFilter narrows ListAttentionFeedback.
type AttentionFeedbackFilter struct {
	FeedItemID string
	Source     string
	Since      string
	Limit      int
}

// AttentionFeedbackAggregate is one grouped row from the feedback report.
type AttentionFeedbackAggregate struct {
	Group        string
	Total        int
	Approved     int
	Dismissed    int
	Muted        int
	ApprovalRate float64
	DismissRate  float64
}

// LearnedAttentionPolicyOptions controls the simple learned-policy derivation.
type LearnedAttentionPolicyOptions struct {
	MinFeedback         int
	SuppressDismissRate float64
	ThresholdStep       float64
}

// LearnedAttentionPolicy is the compact policy overlay consumed by the steering
// cascade. It is derived on read and does not mutate operator settings.
//
// Suppression is learned at the CONVERSATION-THREAD grain, never the channel:
// dismissing a few cards in one thread means "stop surfacing this conversation",
// not "blackhole the whole channel". Channel-level auto-mute was too blunt — a
// handful of dismissals silently muted an explicitly-watched channel and every
// later message dropped at Stage 0 as "muted channel". Author suppression stays
// (broadcast-channel noise from one person), DM threads stay excluded.
type LearnedAttentionPolicy struct {
	SuppressThreads      map[string]bool // thread_key = "<channel>:<thread_ts>"
	SuppressAuthors      map[string]bool
	ThresholdAdjustments map[string]float64
}

// OutcomeOperatorHandled marks a calibration-only feedback row emitted when the
// operator handled a watched thread by replying in it themselves (steerer
// operator-reply learning). These rows preserve the agent's prior suggestion vs
// the operator's hand action for the confidence-calibration task, but are
// excluded from the operator-facing report and the learned-policy derivation so
// they never dilute approval/dismiss denominators.
const OutcomeOperatorHandled = "operator_handled"

// notOperatorHandled is the SQL predicate that excludes calibration-only rows
// from the feedback aggregations.
const notOperatorHandled = `outcome != '` + OutcomeOperatorHandled + `'`

// AttentionFeedbackFromFeed builds a feedback row from a feed item snapshot.
func AttentionFeedbackFromFeed(item FeedItem, finalAction, outcome, draftAfter, createdAt string) AttentionFeedback {
	before := strings.TrimSpace(item.Draft)
	after := strings.TrimSpace(draftAfter)
	final := normalizeFeedbackAction(finalAction)
	captureDraft := final == "send_reply" || final == "sent" || after != ""
	if captureDraft && after == "" && before != "" {
		after = before
	}
	fb := AttentionFeedback{
		FeedItemID:      item.ID,
		Source:          item.Source,
		Channel:         item.Channel,
		Author:          item.Author,
		ThreadType:      item.ChannelType,
		ThreadKey:       item.ThreadKey,
		SuggestedAction: item.SuggestedAction,
		FinalAction:     final,
		Outcome:         strings.TrimSpace(outcome),
		Confidence:      item.Confidence,
		ConfidenceBand:  ConfidenceBand(item.Confidence),
		CreatedAt:       strings.TrimSpace(createdAt),
	}
	if captureDraft {
		fb.DraftBefore = before
		fb.DraftAfter = after
	}
	if fb.Outcome == "" {
		fb.Outcome = outcomeForAction(fb.FinalAction)
	}
	if fb.CreatedAt == "" {
		fb.CreatedAt = NowISO()
	}
	if captureDraft {
		fb.DraftEditDelta = draftEditDelta(before, after)
	}
	return fb
}

// ConfidenceBand buckets model confidence into stable 10-point bands for
// feedback aggregation and learned threshold adjustments.
func ConfidenceBand(conf float64) string {
	switch {
	case conf < 0:
		conf = 0
	case conf > 1:
		conf = 1
	}
	idx := int(conf * 10)
	if idx >= 10 {
		return "0.90-1.00"
	}
	lo := float64(idx) / 10
	hi := lo + 0.09
	return fmt.Sprintf("%.2f-%.2f", lo, hi)
}

// RecordAttentionFeedback inserts one feedback row. Missing ID and confidence
// band are filled deterministically from local helpers; the caller owns the
// final action/outcome choice.
func RecordAttentionFeedback(db *sql.DB, fb AttentionFeedback) error {
	if db == nil {
		return fmt.Errorf("flowdb: record attention feedback requires db")
	}
	fb.FeedItemID = strings.TrimSpace(fb.FeedItemID)
	fb.Source = strings.TrimSpace(fb.Source)
	fb.ThreadKey = strings.TrimSpace(fb.ThreadKey)
	fb.SuggestedAction = normalizeFeedbackAction(fb.SuggestedAction)
	fb.FinalAction = normalizeFeedbackAction(fb.FinalAction)
	fb.Outcome = strings.TrimSpace(fb.Outcome)
	if fb.FeedItemID == "" || fb.Source == "" || fb.ThreadKey == "" || fb.SuggestedAction == "" || fb.FinalAction == "" || fb.Outcome == "" {
		return fmt.Errorf("flowdb: attention feedback requires feed_item_id, source, thread_key, suggested_action, final_action, outcome")
	}
	if fb.ID == "" {
		fb.ID = randomFeedbackID()
	}
	if fb.ConfidenceBand == "" {
		fb.ConfidenceBand = ConfidenceBand(fb.Confidence)
	}
	if fb.CreatedAt == "" {
		fb.CreatedAt = NowISO()
	}
	_, err := db.Exec(
		`INSERT INTO attention_feedback (
			id, feed_item_id, source, channel, author, thread_type, thread_key,
			suggested_action, final_action, outcome, confidence, confidence_band,
			draft_before, draft_after, draft_edit_delta, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fb.ID, fb.FeedItemID, fb.Source, NullIfEmpty(fb.Channel), NullIfEmpty(fb.Author), NullIfEmpty(fb.ThreadType), fb.ThreadKey,
		fb.SuggestedAction, fb.FinalAction, fb.Outcome, fb.Confidence, fb.ConfidenceBand,
		NullIfEmpty(fb.DraftBefore), NullIfEmpty(fb.DraftAfter), NullIfEmpty(fb.DraftEditDelta), fb.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("flowdb: record attention feedback: %w", err)
	}
	return nil
}

// RecentAgentReplyDrafts returns the draft texts of replies the agent posted on
// a thread (send_reply / sent feedback rows) at/after `since`, newest first. The
// operator-reply learning path uses these to recognize the echo of a reply the
// agent itself sent: it rides the operator's user token, so Slack re-delivers it
// as a self-authored event indistinguishable from a hand-typed reply except by
// its text. `since` (RFC3339, empty = no lower bound) bounds the match to recently
// sent drafts.
func RecentAgentReplyDrafts(db *sql.DB, threadKey, since string) ([]string, error) {
	if strings.TrimSpace(threadKey) == "" {
		return nil, nil
	}
	q := `SELECT draft_after FROM attention_feedback
	      WHERE thread_key = ? AND final_action IN ('send_reply','sent')
	        AND draft_after IS NOT NULL AND draft_after != ''`
	args := []any{threadKey}
	if strings.TrimSpace(since) != "" {
		q += " AND created_at >= ?"
		args = append(args, since)
	}
	q += " ORDER BY created_at DESC, id DESC"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: recent agent reply drafts for %q: %w", threadKey, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d sql.NullString
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("flowdb: scan agent reply draft: %w", err)
		}
		if d.String != "" {
			out = append(out, d.String)
		}
	}
	return out, rows.Err()
}

// ListAttentionFeedback returns feedback rows newest-first.
func ListAttentionFeedback(db *sql.DB, f AttentionFeedbackFilter) ([]AttentionFeedback, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, feed_item_id, source, channel, author, thread_type, thread_key,
	             suggested_action, final_action, outcome, confidence, confidence_band,
	             draft_before, draft_after, draft_edit_delta, created_at
	      FROM attention_feedback`
	args := []any{}
	conds := []string{}
	if f.FeedItemID != "" {
		conds = append(conds, "feed_item_id = ?")
		args = append(args, f.FeedItemID)
	}
	if f.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if f.Since != "" {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.Since)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list attention feedback: %w", err)
	}
	defer rows.Close()

	var out []AttentionFeedback
	for rows.Next() {
		fb, err := scanAttentionFeedback(rows)
		if err != nil {
			return nil, fmt.Errorf("flowdb: scan attention feedback: %w", err)
		}
		out = append(out, fb)
	}
	return out, rows.Err()
}

// AttentionFeedbackReport aggregates feedback by one stable dimension.
func AttentionFeedbackReport(db *sql.DB, groupBy string) ([]AttentionFeedbackAggregate, error) {
	expr, err := feedbackGroupExpr(groupBy)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT ` + expr + ` AS group_value,
		       COUNT(*) AS total,
		       SUM(CASE WHEN outcome IN ('approved','sent') THEN 1 ELSE 0 END) AS approved,
		       SUM(CASE WHEN outcome = 'dismissed' THEN 1 ELSE 0 END) AS dismissed,
		       SUM(CASE WHEN outcome = 'muted' THEN 1 ELSE 0 END) AS muted
		FROM attention_feedback
		WHERE ` + notOperatorHandled + `
		GROUP BY group_value
		ORDER BY total DESC, group_value ASC`)
	if err != nil {
		return nil, fmt.Errorf("flowdb: attention feedback report: %w", err)
	}
	defer rows.Close()

	var out []AttentionFeedbackAggregate
	for rows.Next() {
		var row AttentionFeedbackAggregate
		if err := rows.Scan(&row.Group, &row.Total, &row.Approved, &row.Dismissed, &row.Muted); err != nil {
			return nil, fmt.Errorf("flowdb: scan attention feedback report: %w", err)
		}
		if row.Total > 0 {
			row.ApprovalRate = float64(row.Approved) / float64(row.Total)
			row.DismissRate = float64(row.Dismissed) / float64(row.Total)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// LearnedAttentionPolicyFromFeedback derives small, auditable policy hints from
// repeated operator outcomes. It suppresses channels/authors only after repeated
// dismiss/mute outcomes, and adjusts thresholds without enabling any action.
func LearnedAttentionPolicyFromFeedback(db *sql.DB, opts LearnedAttentionPolicyOptions) (LearnedAttentionPolicy, error) {
	if opts.MinFeedback <= 0 {
		opts.MinFeedback = 3
	}
	if opts.SuppressDismissRate <= 0 {
		opts.SuppressDismissRate = 0.8
	}
	if opts.ThresholdStep <= 0 {
		opts.ThresholdStep = 0.05
	}
	out := LearnedAttentionPolicy{
		SuppressThreads:      map[string]bool{},
		SuppressAuthors:      map[string]bool{},
		ThresholdAdjustments: map[string]float64{},
	}
	if db == nil {
		return out, nil
	}
	if err := learnSuppressions(db, "thread_key", opts, out.SuppressThreads); err != nil {
		return out, err
	}
	if err := learnSuppressions(db, "author", opts, out.SuppressAuthors); err != nil {
		return out, err
	}
	if err := learnThresholdAdjustments(db, opts, out.ThresholdAdjustments); err != nil {
		return out, err
	}
	return out, nil
}

func scanAttentionFeedback(rows interface {
	Scan(dest ...any) error
}) (AttentionFeedback, error) {
	var fb AttentionFeedback
	var channel, author, threadType, before, after, delta sql.NullString
	if err := rows.Scan(
		&fb.ID, &fb.FeedItemID, &fb.Source, &channel, &author, &threadType, &fb.ThreadKey,
		&fb.SuggestedAction, &fb.FinalAction, &fb.Outcome, &fb.Confidence, &fb.ConfidenceBand,
		&before, &after, &delta, &fb.CreatedAt,
	); err != nil {
		return AttentionFeedback{}, err
	}
	fb.Channel = channel.String
	fb.Author = author.String
	fb.ThreadType = threadType.String
	fb.DraftBefore = before.String
	fb.DraftAfter = after.String
	fb.DraftEditDelta = delta.String
	return fb, nil
}

func feedbackGroupExpr(groupBy string) (string, error) {
	switch strings.TrimSpace(groupBy) {
	case "source":
		return "source", nil
	case "channel":
		return "COALESCE(NULLIF(channel, ''), '(none)')", nil
	case "author":
		return "COALESCE(NULLIF(author, ''), '(none)')", nil
	case "thread_key":
		return "thread_key", nil
	case "thread_type":
		return "COALESCE(NULLIF(thread_type, ''), '(none)')", nil
	case "suggested_action":
		return "suggested_action", nil
	case "confidence_band":
		return "confidence_band", nil
	default:
		return "", fmt.Errorf("flowdb: unsupported attention feedback group %q", groupBy)
	}
}

func learnSuppressions(db *sql.DB, col string, opts LearnedAttentionPolicyOptions, target map[string]bool) error {
	expr, err := feedbackGroupExpr(col)
	if err != nil {
		return err
	}
	// Exclude direct conversations (Slack im/mpim) from learned suppression:
	// a DM is an intentional 1:1 with a human, so dismissing a few of its cards
	// means "I don't need flow to act", NOT "never surface this channel/person".
	// Auto-muting a teammate's DM from card dismissals was a real bug. Broadcast-
	// channel noise (and its authors) is still learnable. NULL/empty thread_type
	// (e.g. GitHub) is treated as non-DM and stays eligible.
	rows, err := db.Query(`
		SELECT ` + expr + ` AS value,
		       COUNT(*) AS total,
		       SUM(CASE WHEN outcome IN ('dismissed','muted') THEN 1 ELSE 0 END) AS negative
		FROM attention_feedback
		WHERE ` + col + ` IS NOT NULL AND ` + col + ` != ''
		  AND COALESCE(thread_type, '') NOT IN ('im', 'mpim')
		  AND ` + notOperatorHandled + `
		GROUP BY value`)
	if err != nil {
		return fmt.Errorf("flowdb: learn attention suppressions by %s: %w", col, err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		var total, negative int
		if err := rows.Scan(&value, &total, &negative); err != nil {
			return fmt.Errorf("flowdb: scan learned attention suppressions: %w", err)
		}
		if total >= opts.MinFeedback && float64(negative)/float64(total) >= opts.SuppressDismissRate {
			target[value] = true
		}
	}
	return rows.Err()
}

func learnThresholdAdjustments(db *sql.DB, opts LearnedAttentionPolicyOptions, target map[string]float64) error {
	rows, err := db.Query(`
		SELECT suggested_action,
		       COUNT(*) AS total,
		       SUM(CASE WHEN outcome IN ('approved','sent') THEN 1 ELSE 0 END) AS approved,
		       SUM(CASE WHEN outcome IN ('dismissed','muted') THEN 1 ELSE 0 END) AS negative
		FROM attention_feedback
		WHERE suggested_action != ''
		  AND ` + notOperatorHandled + `
		GROUP BY suggested_action`)
	if err != nil {
		return fmt.Errorf("flowdb: learn attention threshold adjustments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var total, approved, negative int
		if err := rows.Scan(&action, &total, &approved, &negative); err != nil {
			return fmt.Errorf("flowdb: scan learned attention threshold adjustments: %w", err)
		}
		if total < opts.MinFeedback {
			continue
		}
		rateApproved := float64(approved) / float64(total)
		rateNegative := float64(negative) / float64(total)
		switch {
		case rateApproved >= opts.SuppressDismissRate:
			target[action] = -opts.ThresholdStep
		case rateNegative >= opts.SuppressDismissRate:
			target[action] = opts.ThresholdStep
		}
	}
	return rows.Err()
}

func normalizeFeedbackAction(action string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(action)), "-", "_")
}

func outcomeForAction(action string) string {
	switch normalizeFeedbackAction(action) {
	case "dismiss":
		return "dismissed"
	case "mute_channel", "mute_sender", "mute_thread":
		return "muted"
	case "capture_kb":
		return "captured"
	case "retriage":
		return "retriaged"
	case "open_source", "open_session":
		return "opened"
	case "sent":
		return "sent"
	default:
		return "approved"
	}
}

func draftEditDelta(before, after string) string {
	switch {
	case before == "" && after == "":
		return ""
	case before == after:
		return "unchanged"
	case before == "":
		return fmt.Sprintf("created +%d chars", len([]rune(after)))
	case after == "":
		return fmt.Sprintf("cleared -%d chars", len([]rune(before)))
	default:
		return fmt.Sprintf("edited %+d chars", len([]rune(after))-len([]rune(before)))
	}
}

func randomFeedbackID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
