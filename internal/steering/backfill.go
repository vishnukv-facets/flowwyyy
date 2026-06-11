package steering

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

const backfillInaccessibleCooldown = 15 * time.Minute

// SteeringBackfill is the steerer's durable catch-up. The live Socket Mode
// listener only sees events delivered while connected; this runner reconciles
// watched channels + DMs against the Slack Web API on boot and on an interval,
// feeding any messages newer than a per-channel watermark through the cascade
// (ObserveBatch, origin="backfill"). Idempotent across restarts: the watermark
// + the cascade's verdict cache + attention_feed coalescing prevent
// re-surfacing.
//
// It sweeps TOP-LEVEL channel/DM messages AND follows active threads: any parent
// in the catch-up window whose latest reply is newer than the watermark has its
// replies fetched (conversations.replies) and replayed too, so a reply that
// landed in a watched-channel thread while the socket was down — the laptop-sleep
// case — is recovered, not just top-level posts.
//
// Documented bounds (NOT silent): a reply to a thread whose PARENT predates the
// watermark won't be discovered during a SHORT gap, because conversations.history
// (oldest=watermark) doesn't return the old parent — only a longer downtime
// pushes the parent into the window. Mentions in UNWATCHED channels during
// downtime need a full-workspace scan and are out of scope. Per-channel volume is
// capped at FLOW_STEERING_BACKFILL_LIMIT (loudly logged when hit).
type SteeringBackfill struct {
	db       *sql.DB
	observe  func(ctx context.Context, evs []monitor.InboundEvent) error
	channels monitor.SlackHistory  // bot token — watched channels; nil → channel sweep skipped
	dms      monitor.SlackHistory  // user token — DMs; nil → DM sweep skipped
	ims      monitor.SlackIMLister // user token — enumerates DM channels; nil → DM sweep skipped
	// replies / dmReplies fetch a thread's replies so the sweep can recover
	// messages that arrived as thread replies during downtime — which a top-level
	// history sweep never surfaces. `replies` uses the bot token (watched
	// channels); `dmReplies` uses the user token (the bot can't read the
	// operator's DMs). nil → thread-reply following is skipped for that surface.
	replies   monitor.SlackThreadReplies
	dmReplies monitor.SlackThreadReplies
	configFn  func() WatchConfig
	interval  time.Duration
	lookback  time.Duration
	limit     int
	now       func() time.Time
	logFn     func(string, ...any)
	skipTill  map[string]time.Time
}

// NewSteeringBackfill builds the runner. Zero interval/lookback/limit fall back
// to env (FLOW_STEERING_BACKFILL_INTERVAL / _LOOKBACK / _LIMIT) then defaults
// (60s / 1h / 50).
func NewSteeringBackfill(db *sql.DB, observe func(context.Context, []monitor.InboundEvent) error,
	channels, dms monitor.SlackHistory, ims monitor.SlackIMLister,
	configFn func() WatchConfig, interval, lookback time.Duration, limit int) *SteeringBackfill {
	if interval <= 0 {
		interval = backfillInterval()
	}
	if lookback <= 0 {
		lookback = backfillLookback()
	}
	if limit <= 0 {
		limit = backfillLimit()
	}
	return &SteeringBackfill{
		db: db, observe: observe, channels: channels, dms: dms, ims: ims,
		configFn: configFn, interval: interval, lookback: lookback, limit: limit,
		now: time.Now, logFn: func(string, ...any) {}, skipTill: map[string]time.Time{},
	}
}

// SetLogger installs a printf-style logger. Optional.
func (b *SteeringBackfill) SetLogger(fn func(string, ...any)) {
	if fn != nil {
		b.logFn = fn
	}
}

// SetRepliesClient installs the bot-token client used to fetch thread replies
// for active threads discovered during the WATCHED-CHANNEL sweep. Optional —
// when unset, the channel sweep recovers top-level messages only (its prior
// behavior). Kept a setter (not a constructor arg) so existing callers and tests
// don't have to thread it through.
func (b *SteeringBackfill) SetRepliesClient(c monitor.SlackThreadReplies) {
	b.replies = c
}

// SetDMRepliesClient installs the user-token client used to fetch thread replies
// inside DM threads (the bot can't read the operator's DMs). Optional — when
// unset, DM thread replies are not recovered by this sweep.
func (b *SteeringBackfill) SetDMRepliesClient(c monitor.SlackThreadReplies) {
	b.dmReplies = c
}

// Run does an immediate pass then repeats every interval until ctx is done.
func (b *SteeringBackfill) Run(ctx context.Context) {
	if b == nil || b.db == nil || b.observe == nil {
		return
	}
	if b.channels == nil && (b.dms == nil || b.ims == nil) {
		b.logFn("steering backfill: no Slack history client configured; nothing to back-fill")
		return
	}
	b.runOnce(ctx)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.runOnce(ctx)
		}
	}
}

func (b *SteeringBackfill) runOnce(ctx context.Context) {
	cfg := b.configFn()
	if b.channels != nil {
		for ch := range cfg.WatchedChannels {
			select {
			case <-ctx.Done():
				return
			default:
			}
			b.backfillChannel(ctx, ch, "channel", b.channels)
		}
	}
	if b.dms != nil {
		var ids []string
		if b.ims != nil {
			var err error
			ids, err = b.ims.ListIMs(ctx)
			if err != nil {
				b.logFn("steering backfill: list DMs: %v; falling back to known DM watermarks", err)
				ids = b.knownDMWatermarkChannels()
			}
		} else {
			ids = b.knownDMWatermarkChannels()
		}
		for _, id := range uniqueStrings(ids) {
			select {
			case <-ctx.Done():
				return
			default:
			}
			b.backfillChannel(ctx, id, "im", b.dms)
		}
	}
}

func (b *SteeringBackfill) backfillChannel(ctx context.Context, channel, channelType string, client monitor.SlackHistory) {
	if b.backfillSkipped(channel) {
		return
	}
	wm, err := flowdb.GetSteeringWatermark(b.db, channel)
	if err != nil {
		b.logFn("steering backfill %s: watermark: %v", channel, err)
		return
	}
	cold := wm == ""
	oldest := wm
	if cold {
		oldest = slackTSFromTime(b.now().Add(-b.lookback))
	}
	msgs, err := client.History(ctx, channel, oldest, b.limit)
	if err != nil {
		b.noteBackfillHistoryError(channel, err)
		b.logFn("steering backfill %s: history: %v", channel, err)
		return
	}
	b.clearBackfillHistoryError(channel)
	if len(msgs) == 0 {
		return
	}
	if len(msgs) >= b.limit {
		// Truncated: there were at least as many messages as the cap. Loud, not
		// silent — older messages in the gap aren't covered this sweep.
		if cold {
			b.logFn("steering backfill %s: hit cap %d in cold-start lookback %s; older messages not covered", channel, b.limit, b.lookback)
		} else {
			b.logFn("steering backfill %s: hit cap %d catching up since watermark; older gap messages not covered — raise FLOW_STEERING_BACKFILL_LIMIT", channel, b.limit)
		}
	}
	// Pick the replies client matching this surface: bot token for watched
	// channels, user token for DMs (the bot can't read the operator's DMs).
	repliesClient := b.replies
	if channelType == "im" {
		repliesClient = b.dmReplies
	}
	maxTS := wm
	var evs []monitor.InboundEvent
	seen := map[string]bool{} // ts already queued this sweep — dedup across top-level + replies
	var followRoots []string  // thread roots that gained replies newer than the cursor
	for _, m := range msgs {
		ts := strings.TrimSpace(m.TS)
		if ts == "" {
			continue
		}
		// A thread parent that gained replies newer than the cursor is followed
		// below — even if the parent's own ts predates the cursor — because those
		// replies are NOT top-level messages and a history sweep never returns
		// them. This is the path that recovers replies missed during downtime.
		if repliesClient != nil && m.ReplyCount > 0 && slackTSGreater(m.LatestReply, wm) {
			root := strings.TrimSpace(m.ThreadTS)
			if root == "" {
				root = ts
			}
			followRoots = append(followRoots, root)
		}
		if !cold && !slackTSGreater(ts, wm) {
			continue // not newer than our cursor
		}
		if slackTSGreater(ts, maxTS) {
			maxTS = ts // advance past EVERY message we saw, even filtered ones
		}
		if !acceptBackfillMessage(m) {
			continue
		}
		threadTS := strings.TrimSpace(m.ThreadTS)
		if threadTS == "" {
			threadTS = ts
		}
		seen[ts] = true
		evs = append(evs, monitor.InboundEvent{
			Kind: "message", Channel: channel, ChannelType: channelType,
			TS: ts, ThreadTS: threadTS,
			UserID: strings.TrimSpace(m.User), Text: strings.TrimSpace(m.DisplayText()),
		})
	}
	// Recover replies on active threads. The channel watermark deliberately tracks
	// only top-level ts (so a parent stays discoverable for follow-up sweeps), so
	// replies are deduped here by `seen` + the cursor and downstream by the
	// cascade's verdict cache — not by advancing the channel watermark past them.
	evs = append(evs, b.followThreadReplies(ctx, repliesClient, channel, channelType, wm, followRoots, seen)...)
	if len(evs) > 0 {
		if err := b.observe(ctx, evs); err != nil {
			b.logFn("steering backfill %s: observe %d event(s): %v", channel, len(evs), err)
		} else {
			b.logFn("steering backfill %s: replayed %d message(s) through the cascade", channel, len(evs))
		}
	}
	if maxTS != "" && maxTS != wm {
		if err := flowdb.SetSteeringWatermark(b.db, channel, maxTS, b.now().UTC().Format(time.RFC3339)); err != nil {
			b.logFn("steering backfill %s: set watermark: %v", channel, err)
		}
	}
}

// followThreadReplies fetches replies for each active thread root and returns
// the ones newer than the cursor (skipping the thread root itself, which the
// caller already saw as a top-level message, and any ts already queued). A
// missing replies client or empty root list is a no-op. Errors per-thread are
// logged and skipped so one inaccessible thread doesn't abort the channel sweep.
func (b *SteeringBackfill) followThreadReplies(ctx context.Context, replies monitor.SlackThreadReplies, channel, channelType, cursor string, roots []string, seen map[string]bool) []monitor.InboundEvent {
	if replies == nil || len(roots) == 0 {
		return nil
	}
	var out []monitor.InboundEvent
	for _, root := range uniqueStrings(roots) {
		msgs, err := replies.Replies(ctx, channel, root, cursor, b.limit)
		if err != nil {
			b.logFn("steering backfill %s: thread %s replies: %v", channel, root, err)
			continue
		}
		for _, m := range msgs {
			ts := strings.TrimSpace(m.TS)
			if ts == "" || ts == root || seen[ts] {
				continue // root already counted as top-level; dedup repeats
			}
			if !slackTSGreater(ts, cursor) {
				continue // a prior sweep already covered this reply
			}
			if !acceptBackfillMessage(m) {
				continue
			}
			seen[ts] = true
			out = append(out, monitor.InboundEvent{
				Kind: "message", Channel: channel, ChannelType: channelType,
				TS: ts, ThreadTS: root,
				UserID: strings.TrimSpace(m.User), Text: strings.TrimSpace(m.DisplayText()),
			})
		}
	}
	return out
}

// --- small local helpers (monitor's equivalents are unexported) ---

func slackTSFromTime(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }

// slackTSGreater reports whether Slack ts a is strictly newer than b. Slack ts
// are "seconds.microseconds"; compare numerically, fall back to lexical.
func slackTSGreater(a, b string) bool {
	if b == "" {
		return a != ""
	}
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea != nil || eb != nil {
		return a > b
	}
	return fa > fb
}

// acceptBackfillMessage keeps real human/bot messages and drops system + edit
// subtypes (joins, leaves, message_changed, …). Mirrors monitor's accept rule.
func acceptBackfillMessage(m monitor.SlackMessage) bool {
	switch strings.TrimSpace(m.SubType) {
	case "", "bot_message", "thread_broadcast", "file_share":
		return strings.TrimSpace(m.DisplayText()) != "" || strings.TrimSpace(m.User) != ""
	default:
		return false
	}
}

func (b *SteeringBackfill) knownDMWatermarkChannels() []string {
	if b == nil || b.db == nil {
		return nil
	}
	rows, err := b.db.Query(`SELECT channel FROM steering_watermark`)
	if err != nil {
		b.logFn("steering backfill: list known DM watermarks: %v", err)
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			continue
		}
		ch = strings.TrimSpace(ch)
		if looksLikeSlackDMChannel(ch) {
			ids = append(ids, ch)
		}
	}
	if err := rows.Err(); err != nil {
		b.logFn("steering backfill: scan known DM watermarks: %v", err)
	}
	return ids
}

func looksLikeSlackDMChannel(ch string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ch)), "D") && !strings.Contains(ch, ":")
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (b *SteeringBackfill) backfillSkipped(channel string) bool {
	if b == nil || len(b.skipTill) == 0 {
		return false
	}
	until, ok := b.skipTill[strings.TrimSpace(channel)]
	if !ok {
		return false
	}
	if b.now().Before(until) {
		return true
	}
	delete(b.skipTill, strings.TrimSpace(channel))
	return false
}

func (b *SteeringBackfill) noteBackfillHistoryError(channel string, err error) {
	if b == nil || err == nil || !backfillInaccessibleError(err) {
		return
	}
	if b.skipTill == nil {
		b.skipTill = map[string]time.Time{}
	}
	b.skipTill[strings.TrimSpace(channel)] = b.now().Add(backfillInaccessibleCooldown)
}

func (b *SteeringBackfill) clearBackfillHistoryError(channel string) {
	if b == nil || len(b.skipTill) == 0 {
		return
	}
	delete(b.skipTill, strings.TrimSpace(channel))
}

func backfillInaccessibleError(err error) bool {
	if err == nil {
		return false
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		text := strings.ToLower(e.Error())
		for _, marker := range []string{"not_in_channel", "channel_not_found", "missing_scope"} {
			if strings.Contains(text, marker) {
				return true
			}
		}
	}
	return false
}

func backfillInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_BACKFILL_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

func backfillLookback() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_BACKFILL_LOOKBACK")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// backfillLimit is the total messages a single channel sweep pulls (paged under
// the hood). Default 200 so a multi-hour sleep gap is recovered in one pass
// rather than truncated at one page; raise FLOW_STEERING_BACKFILL_LIMIT for very
// long downtimes in busy channels.
func backfillLimit() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_BACKFILL_LIMIT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 200
}
