package monitor

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"flow/internal/productdb"

	"github.com/slack-go/slack"
)

// SlackThreadReplies fetches a thread's replies for reconciliation. Only the
// fields the backfill needs are surfaced (see SlackMessage). `oldest` is a
// Slack ts lower bound (exclusive) so we fetch just the tail of the thread.
type SlackThreadReplies interface {
	Replies(ctx context.Context, channelID, threadTS, oldest string, limit int) ([]SlackMessage, error)
}

type slackRepliesAPIClient struct{ lazy *lazySlackClient }

// NewSlackRepliesClient returns a production replies client, or nil when no
// Slack bot/read token is configured — in which case the caller skips
// backfill entirely. The returned client resolves its token per call (see
// lazySlackClient), so a token rotated while the server runs is picked up
// without reconstructing the client.
func NewSlackRepliesClient() SlackThreadReplies {
	if strings.TrimSpace(SlackBotToken()) == "" {
		return nil
	}
	return slackRepliesAPIClient{lazy: newLazySlackClient(SlackBotToken)}
}

func (c slackRepliesAPIClient) Replies(ctx context.Context, channelID, threadTS, oldest string, limit int) ([]SlackMessage, error) {
	api := c.lazy.client()
	if api == nil {
		return nil, ErrNoToken
	}
	if limit <= 0 {
		limit = 200
	}
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: normalizeSlackChannelID(channelID),
		Timestamp: strings.TrimSpace(threadTS),
		Oldest:    strings.TrimSpace(oldest),
		Inclusive: false,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SlackMessage, 0, len(msgs))
	for _, m := range msgs {
		files := slackFilesFromAPIWithContent(ctx, api, channelID, m.Files)
		out = append(out, SlackMessage{
			User:     firstNonEmpty(m.User, m.Username),
			Text:     strings.TrimSpace(m.Text),
			TS:       m.Timestamp,
			ThreadTS: m.ThreadTimestamp,
			SubType:  m.SubType,
			Files:    files,
		})
	}
	return out, nil
}

// NewSlackUserRepliesClient returns a replies client backed by the USER token,
// or nil when no user token is configured. DM-channel threads are only readable
// with the user token — the bot isn't a member of the operator's DMs — so the
// backfill uses this client for slack-thread tags whose channel is a DM.
func NewSlackUserRepliesClient() SlackThreadReplies {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackRepliesAPIClient{lazy: newLazySlackClient(SlackUserToken)}
}

// SlackBackfill is the durable safety net behind the live Socket Mode
// listener. The live listener only sees events delivered while its socket is
// connected; anything that arrives during a disconnect — every server restart,
// any network blip — is lost, because Socket Mode never replays missed events.
// SlackBackfill periodically pulls each monitored thread's recent replies from
// the Slack Web API and appends any that are missing from the task's
// inbox.jsonl, so the Inbox and the same-session monitor eventually see every
// message regardless of socket gaps. It runs independently of the socket, so
// it works even while Socket Mode is mid-reconnect.
type SlackBackfill struct {
	db       *sql.DB
	client   SlackThreadReplies // bot token — channel threads
	dmClient SlackThreadReplies // user token — DM-channel threads; nil → DM backfill skipped
	// Observer receives recovered replies when the steerer owns routing. In that
	// mode backfill must not append directly to task inboxes; it replays the
	// normalized event through the same cascade as live traffic.
	Observer           MessageObserver
	SteererOwnsRouting func() bool
	// SteererSessionsEnabled mirrors the dispatcher field: when active, self-
	// authored events are replayed into the per-channel session as context_only
	// instead of being skipped, so post-restart replay rebuilds session memory.
	SteererSessionsEnabled func() bool
	interval               time.Duration
	limit                  int
	logFn                  func(string, ...any)
}

// NewSlackBackfill builds a backfiller. A zero interval defaults to 45s — well
// inside Slack's conversations.replies rate budget even with a few dozen
// monitored threads.
func NewSlackBackfill(db *sql.DB, client SlackThreadReplies, interval time.Duration) *SlackBackfill {
	if interval <= 0 {
		interval = 45 * time.Second
	}
	return &SlackBackfill{db: db, client: client, interval: interval, limit: 200, logFn: func(string, ...any) {}}
}

// SetLogger installs a printf-style logger (e.g. the server's). Optional.
func (b *SlackBackfill) SetLogger(fn func(string, ...any)) {
	if fn != nil {
		b.logFn = fn
	}
}

// SetDMRepliesClient installs the user-token replies client used to reconcile
// DM-channel threads (slack-thread tags whose channel is a DM). Optional — when
// unset, DM threads are not backfilled. Kept a setter (not a constructor arg) so
// existing callers and tests don't have to thread it through.
func (b *SlackBackfill) SetDMRepliesClient(c SlackThreadReplies) {
	b.dmClient = c
}

// Run does an immediate reconciliation pass — catching anything missed while
// the server was down — then repeats every interval until ctx is cancelled.
func (b *SlackBackfill) Run(ctx context.Context) {
	if b == nil || b.db == nil || b.client == nil {
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

func (b *SlackBackfill) runOnce(ctx context.Context) {
	// Only non-done Slack-reply tasks: finished threads don't need waking, and
	// the tag is the authoritative source of (channel, thread_ts).
	tasks, err := productdb.ListTasks(b.db, productdb.TaskFilter{Tag: "slack-reply", ExcludeDone: true})
	if err != nil {
		b.logFn("slack backfill: list tasks: %v", err)
		return
	}
	for _, task := range tasks {
		if task == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		tags, err := productdb.GetTaskTags(b.db, task.Slug)
		if err != nil {
			continue
		}
		// Reconcile every monitored conversation on this task: its origin channel
		// thread plus any DM threads registered via the tool-use hook (all stored
		// as slack-thread:<channel>:<thread_ts>). DM-channel threads are read with
		// the user token — the bot can't see the operator's DMs.
		for _, ref := range threadRefsFromTags(tags) {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := b.reconcile(ctx, task.Slug, ref.Channel, ref.ThreadTS)
			if err != nil {
				b.logFn("slack backfill %s (%s): %v", task.Slug, ref.Channel, err)
				continue
			}
			if n > 0 {
				b.logFn("slack backfill %s (%s): recovered %d missed message(s)", task.Slug, ref.Channel, n)
			}
		}
	}
	// Beyond slack-reply tasks, recover gaps on steerer-watched threads that have
	// no task (the coverage gap that lost messages over a laptop sleep).
	b.reconcileSteererThreadsOnce(ctx)
}

// reconcile appends any thread replies newer than what's already in the task's
// inbox.jsonl. inbox.jsonl is treated as the durable cursor (its max message
// ts), so reconcile self-heals across restarts and never double-appends —
// every candidate is deduped by ts against what's already recorded.
func (b *SlackBackfill) reconcile(ctx context.Context, slug, channel, threadTS string) (int, error) {
	// The operator↔bot command IM is the command surface, never a steered
	// conversation: neither the operator's commands nor flow's own bot replies
	// there belong in the attention feed or a task inbox. The live path excludes
	// it via the command short-circuit; exclude it from backfill too so it can't
	// resurface self-echoes — including bot messages with NO `user` field, which
	// would otherwise slip past the user-id self-check straight to the LLM.
	if botIsMemberOfIM(channel) {
		return 0, nil
	}
	steererOwned := b.steererOwnsRouting()
	// Per-channel cursor: a task can mix its origin thread with DM channels, so
	// the resume point must be the newest ts in THIS channel, not a global max
	// (a newer DM message must not advance the thread cursor past unseen thread
	// replies, and vice-versa).
	maxTS, seen, err := inboxSlackTSIndexForChannel(slug, channel)
	if err != nil {
		return 0, err
	}
	// No message baseline yet → let the live listener establish the first
	// entry. Backfilling the whole thread here could flood the inbox with old
	// history the user never had and wake the session for ancient messages.
	if maxTS == "" {
		return 0, nil
	}
	cursor := maxTS
	watermarkKey := ""
	if steererOwned {
		watermarkKey = slackThreadSteeringWatermarkKey(slug, channel, threadTS)
		if wm, err := productdb.GetSteeringWatermark(b.db, watermarkKey); err != nil {
			return 0, err
		} else if strings.TrimSpace(wm) != "" {
			cursor = wm
		}
	}
	// DM channels are only readable via the user token (the bot isn't a member
	// of the operator's DMs); channel threads use the bot-token client.
	var client SlackThreadReplies = b.client
	if isDMChannel(channel) && b.dmClient != nil {
		client = b.dmClient
	}
	// Type recovered events by their channel id: a DM (D-prefix) must be "im" so
	// Stage 0's inScope treats it as a DM (always in scope). Labeling a DM
	// "channel" makes the cascade drop it as "out of scope / not watched" — the
	// channel id is authoritative, same as the DM-client selection above.
	channelType := "channel"
	if isDMChannel(channel) {
		channelType = "im"
	}
	msgs, err := client.Replies(ctx, channel, threadTS, cursor, b.limit)
	if err != nil {
		return 0, err
	}
	delivered := 0
	newMax := cursor
	for _, m := range msgs {
		ts := strings.TrimSpace(m.TS)
		if ts == "" || ts == threadTS {
			continue // skip the thread root Slack always returns first
		}
		if seen[ts] || !slackTSLess(cursor, ts) {
			continue // already recorded, or not newer than our cursor
		}
		if slackTSLess(newMax, ts) {
			newMax = ts
		}
		if !backfillAcceptMessage(m) {
			continue
		}
		ev := InboundEvent{
			Kind:        "message",
			Channel:     channel,
			ChannelType: channelType,
			TS:          ts,
			ThreadTS:    threadTS,
			UserID:      strings.TrimSpace(m.User),
			Text:        strings.TrimSpace(m.DisplayText()),
		}
		// Never re-ingest flow's own bot messages (acks, agent replies) when
		// reconciling history — same self-echo guard as the live Dispatch path.
		// Exception (GAP-10): when the per-channel session model is active, replay
		// self-authored events into the session as context_only so post-restart
		// memory includes the operator's own messages and delivered replies.
		if IsSelfAuthoredSlack(ev) {
			if b.steererOwnsRouting() && b.SteererSessionsEnabled != nil && b.SteererSessionsEnabled() {
				if obs, ok := b.Observer.(SelfAuthoredObserver); ok {
					if err := obs.ObserveSelfAuthored(ctx, ev); err != nil {
						return delivered, err
					}
					seen[ts] = true
					delivered++
					continue
				}
			}
			continue
		}
		if steererOwned {
			if err := b.Observer.Observe(ctx, ev); err != nil {
				return delivered, err
			}
		} else {
			if err := AppendInboxEvent(slug, ev); err != nil {
				return delivered, err
			}
		}
		seen[ts] = true
		delivered++
	}
	if steererOwned && newMax != cursor {
		if err := productdb.SetSteeringWatermark(b.db, watermarkKey, newMax, productdb.NowISO()); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

// BackfillObserver is an optional refinement of MessageObserver: it tags
// recovered events as "backfill" origin in the steering trace. *steering.Cascade
// implements it; when absent (tests/CLI) we fall back to plain Observe.
type BackfillObserver interface {
	ObserveBackfill(ctx context.Context, ev InboundEvent) error
}

func (b *SlackBackfill) observeRecovered(ctx context.Context, ev InboundEvent) error {
	if bo, ok := b.Observer.(BackfillObserver); ok {
		return bo.ObserveBackfill(ctx, ev)
	}
	return b.Observer.Observe(ctx, ev)
}

// steererBackfillThreadLimit bounds how many recently-active steerer threads each
// pass reconciles, so the backfill can't fan out one Slack API call per thread
// ever seen. The watermark makes a caught-up thread a cheap no-op.
const steererBackfillThreadLimit = 60

// reconcileSteererThreadsOnce recovers replies the steerer missed (e.g. over a
// laptop sleep) on watched threads that have NO slack-reply task — runOnce's
// task-scoped loop never reaches those, so without this their gap messages were
// lost forever. Each thread's recorded last_seen_ts is the recovery floor and a
// steering watermark is the resume cursor, so it self-heals and never
// re-delivers. Steerer-routing only; a no-op otherwise.
//
// Scope note: this recovers missed THREAD REPLIES (the common case — an ongoing
// thread the steerer has already seen). Brand-new top-level channel messages that
// arrived entirely during the gap are not covered here (they need
// conversations.history, not conversations.replies); the live listener catches
// those on reconnect.
func (b *SlackBackfill) reconcileSteererThreadsOnce(ctx context.Context) {
	if !b.steererOwnsRouting() {
		return
	}
	cursors, err := productdb.ListRecentSlackThreadCursors(b.db, steererBackfillThreadLimit)
	if err != nil {
		b.logFn("slack backfill (steerer threads): list: %v", err)
		return
	}
	for _, tc := range cursors {
		select {
		case <-ctx.Done():
			return
		default:
		}
		channel, threadTS := splitBackfillThreadKey(tc.ThreadKey)
		if channel == "" || threadTS == "" {
			continue
		}
		n, err := b.reconcileSteererThread(ctx, channel, threadTS, tc.LastSeenTS)
		if err != nil {
			b.logFn("slack backfill (steerer thread %s): %v", tc.ThreadKey, err)
			continue
		}
		if n > 0 {
			b.logFn("slack backfill (steerer thread %s): recovered %d missed message(s)", tc.ThreadKey, n)
		}
	}
}

func splitBackfillThreadKey(key string) (channel, threadTS string) {
	parts := strings.SplitN(strings.TrimSpace(key), ":", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

// reconcileSteererThread fetches a steerer thread's replies newer than the resume
// cursor (max of the steering watermark and the last_seen_ts floor) and feeds each
// through the cascade, advancing the watermark. Mirrors reconcile's client choice,
// accept filter and self-echo handling, but is keyed by the steering watermark
// rather than a task inbox (these threads have no task).
func (b *SlackBackfill) reconcileSteererThread(ctx context.Context, channel, threadTS, floorTS string) (int, error) {
	if botIsMemberOfIM(channel) {
		return 0, nil // the operator↔bot command IM is never a steered conversation
	}
	wmKey := slackThreadSteeringWatermarkKey("steerer", channel, threadTS)
	cursor := strings.TrimSpace(floorTS)
	if wm, err := productdb.GetSteeringWatermark(b.db, wmKey); err == nil && wm != "" && slackTSLess(cursor, wm) {
		cursor = wm
	}
	if cursor == "" {
		return 0, nil // no baseline; let the live listener establish the first entry
	}
	client := b.client
	channelType := "channel"
	if isDMChannel(channel) {
		if b.dmClient != nil {
			client = b.dmClient
		}
		channelType = "im"
	}
	msgs, err := client.Replies(ctx, channel, threadTS, cursor, b.limit)
	if err != nil {
		return 0, err
	}
	delivered := 0
	newMax := cursor
	for _, m := range msgs {
		ts := strings.TrimSpace(m.TS)
		if ts == "" || ts == threadTS || !slackTSLess(cursor, ts) {
			continue // root, or not newer than the cursor
		}
		if slackTSLess(newMax, ts) {
			newMax = ts
		}
		if !backfillAcceptMessage(m) {
			continue
		}
		ev := InboundEvent{
			Kind: "message", Channel: channel, ChannelType: channelType,
			TS: ts, ThreadTS: threadTS, UserID: strings.TrimSpace(m.User),
			Text: strings.TrimSpace(m.DisplayText()),
		}
		if IsSelfAuthoredSlack(ev) {
			// Our own bot echo — feed as context_only when the session model is on
			// (so the session knows its reply landed), otherwise skip. Same as the
			// task-reconcile self-echo handling.
			if b.SteererSessionsEnabled != nil && b.SteererSessionsEnabled() {
				if obs, ok := b.Observer.(SelfAuthoredObserver); ok {
					if err := obs.ObserveSelfAuthored(ctx, ev); err != nil {
						return delivered, err
					}
					delivered++
				}
			}
			continue
		}
		if err := b.observeRecovered(ctx, ev); err != nil {
			return delivered, err
		}
		delivered++
	}
	if newMax != cursor {
		if err := productdb.SetSteeringWatermark(b.db, wmKey, newMax, productdb.NowISO()); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

func (b *SlackBackfill) steererOwnsRouting() bool {
	return b != nil && b.db != nil && b.Observer != nil && b.SteererOwnsRouting != nil && b.SteererOwnsRouting()
}

func slackThreadSteeringWatermarkKey(slug, channel, threadTS string) string {
	return "slack-thread:" + strings.TrimSpace(slug) + ":" + normalizeSlackChannelID(channel) + ":" + strings.TrimSpace(threadTS)
}

// inboxSlackTSIndexForChannel reads a task's inbox.jsonl once and returns the
// newest Slack message ts in the given channel (the resume cursor) plus the set
// of that channel's message ts, for dedup. Scoping by channel keeps each
// monitored conversation's cursor independent — see reconcile's note.
func inboxSlackTSIndexForChannel(slug, channel string) (maxTS string, seen map[string]bool, err error) {
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		return "", nil, err
	}
	want := normalizeSlackChannelID(channel)
	seen = make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Event.Kind != "message" && e.Event.Kind != "app_mention" {
			continue
		}
		if normalizeSlackChannelID(e.Event.Channel) != want {
			continue
		}
		ts := strings.TrimSpace(e.Event.TS)
		if ts == "" {
			continue
		}
		seen[ts] = true
		if maxTS == "" || slackTSLess(maxTS, ts) {
			maxTS = ts
		}
	}
	return maxTS, seen, nil
}

// slackTSLess reports whether Slack ts a is older than b. Slack ts are
// "seconds.microseconds" strings; compare numerically, falling back to lexical
// order when either fails to parse.
func slackTSLess(a, b string) bool {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea != nil || eb != nil {
		return a < b
	}
	return fa < fb
}

// backfillAcceptMessage keeps real human/bot/broadcast replies and drops
// system + edit/delete subtypes (joins, leaves, message_changed, …). It also
// accepts thread_broadcast — which the live parser drops — so a broadcast
// reply still reaches the inbox via the durable path.
func backfillAcceptMessage(m SlackMessage) bool {
	switch strings.TrimSpace(m.SubType) {
	case "", "bot_message", "thread_broadcast", slackMessageSubTypeFileShare:
		return strings.TrimSpace(m.DisplayText()) != "" || strings.TrimSpace(m.User) != ""
	default:
		return false
	}
}
