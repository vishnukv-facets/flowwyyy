package flowdb

import (
	"database/sql"
	"fmt"
)

// FeedItem mirrors a row in the attention_feed table (spec §7). It is the
// durable record of one triage candidate surfaced to the operator.
type FeedItem struct {
	ID                string
	Source            string
	ThreadKey         string
	Summary           string
	SuggestedAction   string
	MatchedTask       string
	SuggestedProject  string
	SuggestedPriority string
	Urgency           string
	IsVIP             bool
	Confidence        float64
	Draft             string
	Reason            string
	ContextJSON       string
	Channel           string // source channel/conversation id (Slack C…/D…, or owner/repo for github)
	ChannelType       string // "channel" | "im" | "mpim" | "group" | "github" | ""
	Author            string // source message author (Slack user id, or github login)
	TS                string // source message timestamp anchor (Slack ts, etc.)
	TeamID            string // Slack team/workspace id (for permalink construction)
	URL               string // connector permalink (github item URL, etc.)
	Status            string // new|acted|dismissed|snoozed|deferred
	SnoozeUntil       string
	LinkedTask        string // slug of the task this item spawned/forwarded to (set when acted)
	RetriagingAt      string // RFC3339, set while an operator-forced re-triage is in flight; cleared on completion
	CreatedAt         string // RFC3339
	ActedAt           string // RFC3339, set when status leaves 'new'
}

// SetFeedRetriaging marks a feed item as having an in-flight re-triage (at = now)
// or clears it (at = ""). Surfaced to the UI so the spinner + disabled state
// survive a page refresh and prevent double-firing the async re-triage.
func SetFeedRetriaging(db *sql.DB, id, at string) error {
	if _, err := db.Exec(`UPDATE attention_feed SET retriaging_at = ? WHERE id = ?`, NullIfEmpty(at), id); err != nil {
		return fmt.Errorf("flowdb: set feed retriaging %q: %w", id, err)
	}
	return nil
}

// UpsertFeedItem inserts a feed item, coalescing by thread_key: if a row for
// the same thread_key already exists with status 'new', that row is updated
// in place (and its existing id is returned) instead of creating a duplicate
// card. A fresh 'new' item also reopens a dismissed same-thread row, refreshing
// the card with the latest thread-level context instead of fragmenting one
// source thread across unrelated cards. Otherwise the item is inserted as given.
// Returns the id of the row written.
func UpsertFeedItem(db *sql.DB, item FeedItem) (string, error) {
	if item.ID == "" || item.ThreadKey == "" || item.SuggestedAction == "" {
		return "", fmt.Errorf("flowdb: feed item requires id, thread_key, suggested_action")
	}
	if item.Status == "" {
		item.Status = "new"
	}

	var existingID string
	lookup := `SELECT id, status FROM attention_feed WHERE thread_key = ? AND status = 'new' ORDER BY created_at DESC, id DESC LIMIT 1`
	if item.Status == "new" {
		lookup = `SELECT id, status
		          FROM attention_feed
		          WHERE thread_key = ? AND status IN ('new', 'dismissed')
		          ORDER BY CASE status WHEN 'new' THEN 0 ELSE 1 END, created_at DESC, id DESC
		          LIMIT 1`
	}
	var ignoredStatus string
	err := db.QueryRow(lookup, item.ThreadKey).Scan(&existingID, &ignoredStatus)
	switch {
	case err == sql.ErrNoRows:
		// fall through to insert
	case err != nil:
		return "", fmt.Errorf("flowdb: lookup feed coalesce: %w", err)
	default:
		args := []any{
			item.Source, item.Summary, item.SuggestedAction, NullIfEmpty(item.MatchedTask),
			NullIfEmpty(item.SuggestedProject), NullIfEmpty(item.SuggestedPriority), NullIfEmpty(item.Urgency), boolToInt(item.IsVIP),
			item.Confidence, NullIfEmpty(item.Draft), NullIfEmpty(item.Reason), NullIfEmpty(item.ContextJSON),
			NullIfEmpty(item.Channel), NullIfEmpty(item.ChannelType), NullIfEmpty(item.Author), NullIfEmpty(item.TS), NullIfEmpty(item.TeamID), NullIfEmpty(item.URL), NullIfEmpty(item.LinkedTask),
		}
		update := `UPDATE attention_feed SET
			   source=?, summary=?, suggested_action=?, matched_task=?,
			   suggested_project=?, suggested_priority=?, urgency=?, is_vip=?,
			   confidence=?, draft=?, reason=?, context_json=?,
			   channel=?, channel_type=?, author=?, ts=?, team_id=?, url=?, linked_task=?`
		if item.Status == "new" {
			update += `, status='new', snooze_until=NULL, acted_at=NULL, retriaging_at=NULL`
			if item.CreatedAt != "" {
				update += `, created_at=?`
				args = append(args, item.CreatedAt)
			}
		}
		update += ` WHERE id=?`
		args = append(args, existingID)
		_, uerr := db.Exec(update, args...)
		if uerr != nil {
			return "", fmt.Errorf("flowdb: coalesce feed item: %w", uerr)
		}
		return existingID, nil
	}

	_, err = db.Exec(
		`INSERT INTO attention_feed (
		   id, source, thread_key, summary, suggested_action, matched_task,
		   suggested_project, suggested_priority, urgency, is_vip, confidence,
		   draft, reason, context_json, channel, channel_type, author, ts, team_id, url,
		   status, snooze_until, linked_task, created_at, acted_at
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.Source, item.ThreadKey, item.Summary, item.SuggestedAction, NullIfEmpty(item.MatchedTask),
		NullIfEmpty(item.SuggestedProject), NullIfEmpty(item.SuggestedPriority), NullIfEmpty(item.Urgency), boolToInt(item.IsVIP), item.Confidence,
		NullIfEmpty(item.Draft), NullIfEmpty(item.Reason), NullIfEmpty(item.ContextJSON),
		NullIfEmpty(item.Channel), NullIfEmpty(item.ChannelType), NullIfEmpty(item.Author), NullIfEmpty(item.TS), NullIfEmpty(item.TeamID), NullIfEmpty(item.URL),
		item.Status, NullIfEmpty(item.SnoozeUntil), NullIfEmpty(item.LinkedTask), item.CreatedAt, NullIfEmpty(item.ActedAt),
	)
	if err != nil {
		return "", fmt.Errorf("flowdb: insert feed item: %w", err)
	}
	return item.ID, nil
}

// ListFeedItems returns feed rows, newest first. An empty status returns all
// rows; otherwise it filters to that status.
func ListFeedItems(db *sql.DB, status string) ([]FeedItem, error) {
	q := `SELECT id, source, thread_key, summary, suggested_action, matched_task,
	             suggested_project, suggested_priority, urgency, is_vip, confidence,
	             draft, reason, context_json, channel, channel_type, author, ts, team_id, url,
	             status, snooze_until, linked_task, retriaging_at, created_at, acted_at
	      FROM attention_feed`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list feed items: %w", err)
	}
	defer rows.Close()

	var out []FeedItem
	for rows.Next() {
		var it FeedItem
		var matched, project, priority, urgency, draft, reason, ctx, channel, channelType, author, ts, teamID, url, snooze, linked, retriaging, acted sql.NullString
		var isVIP int
		if err := rows.Scan(
			&it.ID, &it.Source, &it.ThreadKey, &it.Summary, &it.SuggestedAction, &matched,
			&project, &priority, &urgency, &isVIP, &it.Confidence,
			&draft, &reason, &ctx, &channel, &channelType, &author, &ts, &teamID, &url,
			&it.Status, &snooze, &linked, &retriaging, &it.CreatedAt, &acted,
		); err != nil {
			return nil, fmt.Errorf("flowdb: scan feed item: %w", err)
		}
		it.MatchedTask = matched.String
		it.SuggestedProject = project.String
		it.SuggestedPriority = priority.String
		it.Urgency = urgency.String
		it.IsVIP = isVIP != 0
		it.Draft = draft.String
		it.Reason = reason.String
		it.ContextJSON = ctx.String
		it.Channel = channel.String
		it.ChannelType = channelType.String
		it.Author = author.String
		it.TS = ts.String
		it.TeamID = teamID.String
		it.URL = url.String
		it.SnoozeUntil = snooze.String
		it.LinkedTask = linked.String
		it.RetriagingAt = retriaging.String
		it.ActedAt = acted.String
		out = append(out, it)
	}
	return out, rows.Err()
}

// ResolveOpenFeedItemsByThread marks every still-'new' feed item for a thread as
// 'acted' — used when the operator handles the thread directly (replies in
// Slack/GitHub themselves), so the surfaced "needs you" card stops nagging.
// Returns the number of items resolved (0 when none were open for that thread).
func ResolveOpenFeedItemsByThread(db *sql.DB, threadKey, actedAt string) (int, error) {
	if threadKey == "" {
		return 0, nil
	}
	res, err := db.Exec(
		`UPDATE attention_feed SET status = 'acted', acted_at = ? WHERE thread_key = ? AND status = 'new'`,
		NullIfEmpty(actedAt), threadKey,
	)
	if err != nil {
		return 0, fmt.Errorf("flowdb: resolve open feed items for thread %q: %w", threadKey, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetFeedItemAction rewrites a still-'new' feed item's suggested action and
// matched task in place. Used to reconcile open cards when a tracking task is
// (re)discovered after the card was written — e.g. flipping make_task → forward
// once we find the existing (possibly archived) task for the thread. No-op
// unless the row is still 'new', so it never disturbs an already-acted card.
func SetFeedItemAction(db *sql.DB, id, action, matchedTask string) error {
	res, err := db.Exec(
		`UPDATE attention_feed SET suggested_action = ?, matched_task = ? WHERE id = ? AND status = 'new'`,
		action, NullIfEmpty(matchedTask), id,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set feed action: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("flowdb: no open feed item with id %q", id)
	}
	return nil
}

// SetFeedItemStatus moves a feed item to a new lifecycle status and stamps
// acted_at. Used when the operator (or an autonomous action) resolves a card.
func SetFeedItemStatus(db *sql.DB, id, status, actedAt string) error {
	res, err := db.Exec(
		`UPDATE attention_feed SET status = ?, acted_at = ? WHERE id = ?`,
		status, NullIfEmpty(actedAt), id,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set feed status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("flowdb: no feed item with id %q", id)
	}
	return nil
}

// SetFeedItemActed resolves a feed item to status 'acted', stamping acted_at
// and recording the task slug it spawned/forwarded to (linked_task) in a single
// UPDATE. An empty linkedTask stores NULL. Used by the steering actions so the
// UI can offer a "Go to session" link from an acted card.
func SetFeedItemActed(db *sql.DB, id, linkedTask, actedAt string) error {
	res, err := db.Exec(
		`UPDATE attention_feed SET status = 'acted', acted_at = ?, linked_task = ? WHERE id = ?`,
		NullIfEmpty(actedAt), NullIfEmpty(linkedTask), id,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set feed acted: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("flowdb: no feed item with id %q", id)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetFeedItem fetches a single feed row by id. Returns a wrapped error
// (including sql.ErrNoRows) when no row matches.
func GetFeedItem(db *sql.DB, id string) (FeedItem, error) {
	var it FeedItem
	var matched, project, priority, urgency, draft, reason, ctx, channel, channelType, author, ts, teamID, url, snooze, linked, retriaging, acted sql.NullString
	var isVIP int
	err := db.QueryRow(
		`SELECT id, source, thread_key, summary, suggested_action, matched_task,
		        suggested_project, suggested_priority, urgency, is_vip, confidence,
		        draft, reason, context_json, channel, channel_type, author, ts, team_id, url,
		        status, snooze_until, linked_task, retriaging_at, created_at, acted_at
		 FROM attention_feed WHERE id = ?`, id,
	).Scan(
		&it.ID, &it.Source, &it.ThreadKey, &it.Summary, &it.SuggestedAction, &matched,
		&project, &priority, &urgency, &isVIP, &it.Confidence,
		&draft, &reason, &ctx, &channel, &channelType, &author, &ts, &teamID, &url,
		&it.Status, &snooze, &linked, &retriaging, &it.CreatedAt, &acted,
	)
	if err != nil {
		return FeedItem{}, fmt.Errorf("flowdb: get feed item %q: %w", id, err)
	}
	it.MatchedTask = matched.String
	it.SuggestedProject = project.String
	it.SuggestedPriority = priority.String
	it.Urgency = urgency.String
	it.IsVIP = isVIP != 0
	it.Draft = draft.String
	it.Reason = reason.String
	it.ContextJSON = ctx.String
	it.Channel = channel.String
	it.ChannelType = channelType.String
	it.Author = author.String
	it.TS = ts.String
	it.TeamID = teamID.String
	it.URL = url.String
	it.SnoozeUntil = snooze.String
	it.LinkedTask = linked.String
	it.RetriagingAt = retriaging.String
	it.ActedAt = acted.String
	return it, nil
}
