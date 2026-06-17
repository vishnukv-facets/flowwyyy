package monitor

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// slackNameCacheTTL bounds how long a resolved user/channel name is trusted
// before a re-lookup. Names change rarely, so an hour keeps the inbox UI
// responsive without hammering the rate-limited users.info /
// conversations.info endpoints.
const slackNameCacheTTL = time.Hour

type nameCacheEntry struct {
	name string
	at   time.Time
}

// SlackNameResolver resolves Slack user and channel IDs to human-readable
// names, caching results in-memory with a TTL. It exists so the Inbox UI can
// render display names instead of raw IDs (U…/C…) without making a Slack API
// call per row on every load — the cache absorbs repeat lookups across
// requests.
//
// Every method is nil-safe: a nil *SlackNameResolver (no Slack token
// configured) resolves to "", so callers MUST supply their own non-ID
// fallback. Negative lookups (id not found / API error) are cached as "" too,
// so a missing user doesn't trigger a fresh API call on every render.
//
// Safe for concurrent use.
type SlackNameResolver struct {
	client SlackTitleClient
	// fallback resolves channels the primary (bot) client can't see — a private
	// channel the bot was never invited to returns channel_not_found, but the
	// operator's user token resolves it. Optional; nil → no fallback.
	fallback SlackTitleClient
	ttl      time.Duration

	mu    sync.Mutex
	users map[string]nameCacheEntry
	chans map[string]nameCacheEntry
}

// SetFallbackClient installs a secondary client (typically user-token) consulted
// when the primary fails to resolve a channel name. Nil-safe; nil client is a
// no-op. Set once at construction, before concurrent use.
func (r *SlackNameResolver) SetFallbackClient(c SlackTitleClient) {
	if r == nil || c == nil {
		return
	}
	r.fallback = c
}

// NewSlackNameResolver builds a resolver backed by the Slack API using the
// configured bot token. Returns nil when no token is available; callers treat
// a nil resolver as "cannot resolve".
func NewSlackNameResolver() *SlackNameResolver {
	client := newSlackTitleAPIClient()
	if client == nil {
		return nil
	}
	return NewSlackNameResolverWithClient(client)
}

// NewSlackNameResolverWithClient builds a resolver over any SlackTitleClient.
// Production wiring uses NewSlackNameResolver; this constructor lets callers
// (and tests) supply a custom or fake client so resolution and cache
// behaviour can be exercised without a real Slack token.
func NewSlackNameResolverWithClient(client SlackTitleClient) *SlackNameResolver {
	if client == nil {
		return nil
	}
	return &SlackNameResolver{
		client: client,
		ttl:    slackNameCacheTTL,
		users:  map[string]nameCacheEntry{},
		chans:  map[string]nameCacheEntry{},
	}
}

// UserName resolves a Slack user ID to a display name
// (DisplayName → RealName → Name). Returns "" when the resolver is nil, the id
// is blank, or the lookup fails — never the raw ID.
func (r *SlackNameResolver) UserName(ctx context.Context, userID string) string {
	userID = strings.TrimSpace(userID)
	if r == nil || userID == "" {
		return ""
	}
	if name, ok := r.lookup(r.users, userID); ok {
		return name
	}
	name := ""
	if user, err := r.client.UserInfo(ctx, userID); err == nil {
		name = firstNonEmpty(user.DisplayName, user.RealName, user.Name)
	}
	r.store(r.users, userID, name)
	return name
}

// ChannelName resolves a Slack channel ID to a channel name, "#"-prefixed for
// public/private channels. Returns "" when the resolver is nil, the id is
// blank, or the lookup fails — never the raw ID.
func (r *SlackNameResolver) ChannelName(ctx context.Context, channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if r == nil || channelID == "" {
		return ""
	}
	if name, ok := r.lookup(r.chans, channelID); ok {
		return name
	}
	name := resolveChannelNameVia(ctx, r.client, channelID)
	// Bot couldn't see it (private channel it isn't in → channel_not_found).
	// Retry with the user-token fallback, which resolves channels the operator
	// is a member of — otherwise the UI falls back to the raw C… id.
	if name == "" && r.fallback != nil {
		name = resolveChannelNameVia(ctx, r.fallback, channelID)
	}
	r.store(r.chans, channelID, name)
	return name
}

// ConversationPeerTitle names a Slack DM / group-DM by its PARTICIPANTS, resolved
// from the CHANNEL — so it is correct on the operator's own first outbound message
// (a DM is named by its peer, not by whoever spoke first). IM → the other party's
// display name; MPIM → up to three non-self members ("A, B +N"). Tries the primary
// (bot) client, then the user-token fallback for conversations the bot isn't a
// member of (the operator's own DMs). selfUserIDs drops the operator from group
// member lists. Returns "" when unresolved or not a DM — NEVER a raw ID, so the
// caller keeps its placeholder and a later message can upgrade it. Cached per
// channel id (distinct key, so it never collides with ChannelName's cache).
func (r *SlackNameResolver) ConversationPeerTitle(ctx context.Context, channelID string, selfUserIDs []string) string {
	channelID = strings.TrimSpace(channelID)
	if r == nil || channelID == "" {
		return ""
	}
	cacheKey := "peer:" + channelID
	if name, ok := r.lookup(r.chans, cacheKey); ok {
		return name
	}
	title := r.resolveConversationPeerTitle(ctx, channelID, selfUserIDs)
	r.store(r.chans, cacheKey, title)
	return title
}

func (r *SlackNameResolver) resolveConversationPeerTitle(ctx context.Context, channelID string, selfUserIDs []string) string {
	conv, ok := r.conversationInfoWithFallback(ctx, channelID)
	if !ok {
		return ""
	}
	switch {
	case conv.IsIM:
		// users.info resolves any workspace user regardless of channel membership,
		// so the bot-client UserName works even for the operator's own DM peer.
		return r.UserName(ctx, conv.User)
	case conv.IsMpIM:
		// ponytail: trust conversations.info's Members; skip a separate
		// UsersInConversation call. Empty members → "" (placeholder + retry later).
		names := make([]string, 0, 3)
		remaining := 0
		for _, member := range conv.Members {
			if containsUserID(selfUserIDs, member) {
				continue
			}
			name := r.UserName(ctx, member)
			if name == "" {
				continue
			}
			if len(names) < 3 {
				names = append(names, name)
			} else {
				remaining++
			}
		}
		if len(names) == 0 {
			return ""
		}
		out := strings.Join(names, ", ")
		if remaining > 0 {
			out += " +" + strconv.Itoa(remaining)
		}
		return out
	}
	return ""
}

// conversationInfoWithFallback resolves conversations.info via the primary (bot)
// client, then the user-token fallback for conversations the bot can't see (the
// operator's own DMs). ok is false when neither client can resolve it.
func (r *SlackNameResolver) conversationInfoWithFallback(ctx context.Context, channelID string) (SlackConversation, bool) {
	if r.client != nil {
		if conv, err := r.client.ConversationInfo(ctx, channelID); err == nil {
			return conv, true
		}
	}
	if r.fallback != nil {
		if conv, err := r.fallback.ConversationInfo(ctx, channelID); err == nil {
			return conv, true
		}
	}
	return SlackConversation{}, false
}

// resolveChannelNameVia resolves one channel id to a "#"-prefixed name via the
// given client, or "" on any error/empty.
func resolveChannelNameVia(ctx context.Context, client SlackTitleClient, channelID string) string {
	if client == nil {
		return ""
	}
	conv, err := client.ConversationInfo(ctx, channelID)
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(conv.Name)
	if name != "" && (conv.IsChannel || conv.IsGroup) && !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	return name
}

// CleanText rewrites Slack message markup so a body never surfaces raw IDs:
//   - <@U123|label>  → @label
//   - <@U123>        → @<resolved name>  (or @user when unresolved)
//   - <url|label>    → label
//   - <url>          → url
//
// Unlike cleanSlackTitleText, it preserves newlines and does not truncate —
// it is meant for full message bodies, not titles. Non-Slack text (e.g.
// GitHub comment bodies) passes through unchanged since the markup patterns
// simply don't match.
// MentionedUserIDs returns the distinct Slack user IDs referenced as <@U…>
// mentions in text (label form <@U…|name> included). Callers use it to prewarm
// the name cache concurrently before CleanText runs — otherwise CleanText
// resolves each mention with a SERIAL UserInfo network call, which is what made
// the decision-trace modal slow to load. Returns nil when there are no mentions.
func MentionedUserIDs(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range slackMentionRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		id := strings.TrimSpace(m[1])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (r *SlackNameResolver) CleanText(ctx context.Context, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = slackMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		// Prefer an inline label (<@U123|name>) when Slack provided one.
		if _, label, ok := strings.Cut(match, "|"); ok {
			if label = strings.TrimSpace(strings.TrimRight(label, ">")); label != "" {
				return "@" + label
			}
		}
		sub := slackMentionRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		if name := r.UserName(ctx, sub[1]); name != "" {
			return "@" + name
		}
		return "@user"
	})
	text = slackLinkRe.ReplaceAllString(text, "$2")
	text = slackBareLinkRe.ReplaceAllString(text, "$1")
	return strings.TrimSpace(text)
}

// lookup returns a cached name when present and not past its TTL. The bool is
// false on a miss so the caller knows to hit the API; a cached negative ("")
// still counts as a hit.
func (r *SlackNameResolver) lookup(cache map[string]nameCacheEntry, id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := cache[id]
	if !ok {
		return "", false
	}
	if r.ttl > 0 && time.Since(entry.at) > r.ttl {
		delete(cache, id)
		return "", false
	}
	return entry.name, true
}

func (r *SlackNameResolver) store(cache map[string]nameCacheEntry, id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cache[id] = nameCacheEntry{name: name, at: time.Now()}
}

// Warm concurrently resolves (and caches) the given user and channel IDs so a
// subsequent batch of UserName/ChannelName/CleanText calls hits the warm cache
// instead of making one serial API round-trip per row. This is what keeps the
// Attention feed/trace fast: a cold render of N rows would otherwise block on N
// sequential users.info/conversations.info calls. Deduplicates, skips IDs
// already cached, bounds concurrency, and respects ctx (callers should pass a
// timeout). Nil-safe and safe for concurrent use.
func (r *SlackNameResolver) Warm(ctx context.Context, userIDs, channelIDs []string) {
	if r == nil {
		return
	}
	type job struct {
		id   string
		user bool
	}
	seen := make(map[string]bool)
	var jobs []job
	add := func(id string, user bool) {
		id = strings.TrimSpace(id)
		key := "c:" + id
		if user {
			key = "u:" + id
		}
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		cache := r.chans
		if user {
			cache = r.users
		}
		if _, ok := r.lookup(cache, id); ok {
			return // already cached (fresh) — no API call needed
		}
		jobs = append(jobs, job{id: id, user: user})
	}
	for _, id := range userIDs {
		add(id, true)
	}
	for _, id := range channelIDs {
		add(id, false)
	}
	if len(jobs) == 0 {
		return
	}
	const maxConcurrency = 8
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			if j.user {
				r.UserName(ctx, j.id)
			} else {
				r.ChannelName(ctx, j.id)
			}
		}(j)
	}
	wg.Wait()
}
