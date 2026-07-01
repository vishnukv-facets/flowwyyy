package flowdb

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
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

// AttentionWorkstream is the durable identity layer above source thread roots:
// multiple Slack roots can map to one canonical feed card/workstream.
type AttentionWorkstream struct {
	ID                  string
	Source              string
	Channel             string
	ChannelType         string
	CanonicalThreadKey  string
	CanonicalFeedItemID string
	OwnerTaskSlug       string
	Summary             string
	Status              string
	CreatedAt           string
	UpdatedAt           string
}

// EnsureAttentionWorkstreamForFeed records item.ThreadKey as the canonical
// workstream key and aliases rawThreadKey to it. rawThreadKey may differ when
// the steerer merged a new source root into an existing card.
func EnsureAttentionWorkstreamForFeed(db *sql.DB, item FeedItem, rawThreadKey, at string) (AttentionWorkstream, error) {
	canonical := strings.TrimSpace(item.ThreadKey)
	if canonical == "" {
		return AttentionWorkstream{}, fmt.Errorf("flowdb: attention workstream requires canonical thread key")
	}
	raw := strings.TrimSpace(rawThreadKey)
	if raw == "" {
		raw = canonical
	}
	if at == "" {
		at = NowISO()
	}
	ws, ok, err := AttentionWorkstreamByThreadKey(db, canonical)
	if err != nil {
		return AttentionWorkstream{}, err
	}
	if !ok {
		ws, ok, err = AttentionWorkstreamByThreadKey(db, raw)
		if err != nil {
			return AttentionWorkstream{}, err
		}
	}
	if !ok {
		ws = AttentionWorkstream{
			ID:                 attentionWorkstreamID(canonical),
			CanonicalThreadKey: canonical,
			CreatedAt:          at,
		}
	}
	ws.Source = item.Source
	ws.Channel = item.Channel
	ws.ChannelType = item.ChannelType
	ws.CanonicalThreadKey = canonical
	ws.CanonicalFeedItemID = item.ID
	if owner := firstNonEmptyString(item.MatchedTask, item.LinkedTask); owner != "" {
		ws.OwnerTaskSlug = owner
	}
	ws.Summary = item.Summary
	ws.Status = "open"
	ws.UpdatedAt = at

	if _, err := db.Exec(
		`INSERT INTO attention_workstreams (
		   id, source, channel, channel_type, canonical_thread_key, canonical_feed_item_id,
		   owner_task_slug, summary, status, created_at, updated_at
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   source=excluded.source,
		   channel=excluded.channel,
		   channel_type=excluded.channel_type,
		   canonical_thread_key=excluded.canonical_thread_key,
		   canonical_feed_item_id=excluded.canonical_feed_item_id,
		   owner_task_slug=COALESCE(excluded.owner_task_slug, attention_workstreams.owner_task_slug),
		   summary=excluded.summary,
		   status=excluded.status,
		   updated_at=excluded.updated_at`,
		ws.ID, ws.Source, NullIfEmpty(ws.Channel), NullIfEmpty(ws.ChannelType), ws.CanonicalThreadKey, NullIfEmpty(ws.CanonicalFeedItemID),
		NullIfEmpty(ws.OwnerTaskSlug), ws.Summary, ws.Status, ws.CreatedAt, ws.UpdatedAt,
	); err != nil {
		return AttentionWorkstream{}, fmt.Errorf("flowdb: upsert attention workstream: %w", err)
	}
	for _, key := range []string{canonical, raw} {
		if err := upsertAttentionWorkstreamAlias(db, ws, item, key, at); err != nil {
			return AttentionWorkstream{}, err
		}
	}
	return ws, nil
}

func upsertAttentionWorkstreamAlias(db *sql.DB, ws AttentionWorkstream, item FeedItem, threadKey, at string) error {
	threadKey = strings.TrimSpace(threadKey)
	if threadKey == "" {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO attention_workstream_aliases (
		   thread_key, workstream_id, feed_item_id, source, channel, created_at, updated_at
		 ) VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(thread_key) DO UPDATE SET
		   workstream_id=excluded.workstream_id,
		   feed_item_id=excluded.feed_item_id,
		   source=excluded.source,
		   channel=excluded.channel,
		   updated_at=excluded.updated_at`,
		threadKey, ws.ID, NullIfEmpty(item.ID), item.Source, NullIfEmpty(item.Channel), at, at,
	)
	if err != nil {
		return fmt.Errorf("flowdb: upsert attention workstream alias: %w", err)
	}
	return nil
}

// AttentionWorkstreamByThreadKey returns the workstream aliased to threadKey.
func AttentionWorkstreamByThreadKey(db *sql.DB, threadKey string) (AttentionWorkstream, bool, error) {
	threadKey = strings.TrimSpace(threadKey)
	if threadKey == "" {
		return AttentionWorkstream{}, false, nil
	}
	var ws AttentionWorkstream
	var channel, channelType, feedID, owner sql.NullString
	err := db.QueryRow(
		`SELECT w.id, w.source, w.channel, w.channel_type, w.canonical_thread_key,
		        w.canonical_feed_item_id, w.owner_task_slug, w.summary, w.status, w.created_at, w.updated_at
		   FROM attention_workstream_aliases a
		   JOIN attention_workstreams w ON w.id = a.workstream_id
		  WHERE a.thread_key = ?`,
		threadKey,
	).Scan(&ws.ID, &ws.Source, &channel, &channelType, &ws.CanonicalThreadKey, &feedID, &owner, &ws.Summary, &ws.Status, &ws.CreatedAt, &ws.UpdatedAt)
	if err == sql.ErrNoRows {
		return AttentionWorkstream{}, false, nil
	}
	if err != nil {
		return AttentionWorkstream{}, false, fmt.Errorf("flowdb: attention workstream by thread key: %w", err)
	}
	ws.Channel = channel.String
	ws.ChannelType = channelType.String
	ws.CanonicalFeedItemID = feedID.String
	ws.OwnerTaskSlug = owner.String
	return ws, true, nil
}

func attentionWorkstreamID(threadKey string) string {
	sum := sha256.Sum256([]byte(threadKey))
	return fmt.Sprintf("aws-%x", sum[:8])
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
	id, _, err := UpsertFeedItemSurfaced(db, item)
	return id, err
}

// UpsertFeedItemSurfaced is UpsertFeedItem that also reports whether the call
// produced a live ('new') card. surfaced == false means the upsert coalesced
// onto a dismissed row that it deliberately left dismissed — see the dismissal
// guard below. Callers (the cascade) use this to avoid re-surfacing or auto-
// acting on a thread the operator already cleared.
func UpsertFeedItemSurfaced(db *sql.DB, item FeedItem) (id string, surfaced bool, err error) {
	if item.ID == "" || item.ThreadKey == "" || item.SuggestedAction == "" {
		return "", false, fmt.Errorf("flowdb: feed item requires id, thread_key, suggested_action")
	}
	if item.Status == "" {
		item.Status = "new"
	}

	var existingID string
	lookup := `SELECT id, status, COALESCE(ts, '') FROM attention_feed WHERE thread_key = ? AND status = 'new' ORDER BY created_at DESC, id DESC LIMIT 1`
	if item.Status == "new" {
		lookup = `SELECT id, status, COALESCE(ts, '')
		          FROM attention_feed
		          WHERE thread_key = ? AND status IN ('new', 'dismissed')
		          ORDER BY CASE status WHEN 'new' THEN 0 ELSE 1 END, created_at DESC, id DESC
		          LIMIT 1`
	}
	var existingStatus, existingTS string
	err = db.QueryRow(lookup, item.ThreadKey).Scan(&existingID, &existingStatus, &existingTS)
	switch {
	case err == sql.ErrNoRows:
		// fall through to insert
	case err != nil:
		return "", false, fmt.Errorf("flowdb: lookup feed coalesce: %w", err)
	default:
		// Respect dismissal: a dismissed card must not be resurrected by re-
		// surfacing the SAME message (or an older one). The verdict cache is only
		// a short window and backfills replay threads, so the same message is
		// re-observed long after the operator dismissed it. Only genuinely newer
		// thread activity (a strictly newer message ts) reopens the card.
		if item.Status == "new" && existingStatus == "dismissed" && feedTSSameOrOlder(item.TS, existingTS) {
			return existingID, false, nil
		}
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
			return "", false, fmt.Errorf("flowdb: coalesce feed item: %w", uerr)
		}
		return existingID, item.Status == "new", nil
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
		return "", false, fmt.Errorf("flowdb: insert feed item: %w", err)
	}
	return item.ID, true, nil
}

// feedTSSameOrOlder reports whether incoming is the same Slack message ts as
// existing, or older. Slack ts are "seconds.micros" and monotonic per channel,
// so a strictly larger value is genuinely newer thread activity. When either ts
// is missing or non-numeric (some non-Slack sources) it returns false, so the
// caller keeps the prior reopen behavior rather than over-suppressing.
func feedTSSameOrOlder(incoming, existing string) bool {
	fi, ierr := strconv.ParseFloat(strings.TrimSpace(incoming), 64)
	fe, eerr := strconv.ParseFloat(strings.TrimSpace(existing), 64)
	if ierr != nil || eerr != nil {
		return false
	}
	return fi <= fe
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
	return scanFeedItemRows(rows)
}

const feedItemColumns = `id, source, thread_key, summary, suggested_action, matched_task,
	suggested_project, suggested_priority, urgency, is_vip, confidence,
	draft, reason, context_json, channel, channel_type, author, ts, team_id, url,
	status, snooze_until, linked_task, retriaging_at, created_at, acted_at`

// scanFeedItemRows scans rows selected with feedItemColumns into FeedItems.
func scanFeedItemRows(rows *sql.Rows) ([]FeedItem, error) {
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

// ListOpenClubCandidates returns open ('new') feed cards in `channel` that an
// incoming standalone message might continue: same channel, created at/after
// `since` (RFC3339; empty = no lower bound), excluding `excludeThreadKey` (the
// incoming message's own thread_key), newest first, capped at `limit`. The
// surface validation uses this to find existing cards around a new top-level
// message. An empty channel returns nil: a card without a channel anchor has no
// conversation to compare against.
func ListOpenClubCandidates(db *sql.DB, channel, excludeThreadKey, since string, limit int) ([]FeedItem, error) {
	if strings.TrimSpace(channel) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT ` + feedItemColumns + `
	      FROM attention_feed
	      WHERE status = 'new' AND channel = ?`
	args := []any{channel}
	if since != "" {
		q += ` AND created_at >= ?`
		args = append(args, since)
	}
	if excludeThreadKey != "" {
		q += ` AND thread_key <> ?`
		args = append(args, excludeThreadKey)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list club candidates: %w", err)
	}
	defer rows.Close()
	return scanFeedItemRows(rows)
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

// LatestFeedItemByThread returns the most recently created feed card for a
// thread regardless of status (the card may already be resolved). The bool is
// false (zero FeedItem, nil error) when the thread never had a card — e.g. it was
// deep-triaged and dropped without surfacing. Used by the operator-reply learning
// path to recover the agent's prior suggestion for a calibration signal.
func LatestFeedItemByThread(db *sql.DB, threadKey string) (FeedItem, bool, error) {
	if strings.TrimSpace(threadKey) == "" {
		return FeedItem{}, false, nil
	}
	rows, err := db.Query(
		`SELECT `+feedItemColumns+`
		 FROM attention_feed WHERE thread_key = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, threadKey,
	)
	if err != nil {
		return FeedItem{}, false, fmt.Errorf("flowdb: latest feed item for thread %q: %w", threadKey, err)
	}
	defer rows.Close()
	items, err := scanFeedItemRows(rows)
	if err != nil {
		return FeedItem{}, false, err
	}
	if len(items) == 0 {
		return FeedItem{}, false, nil
	}
	return items[0], true, nil
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
