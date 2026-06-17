package monitor

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const slackPermalinkCacheTTL = time.Hour

// slackPermalinkFn is the mockable seam (tests swap it).
var slackPermalinkFn = func(ctx context.Context, token, channel, ts string) (string, error) {
	api := slack.New(token)
	return api.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: ts})
}

// SlackPermalinker resolves (channel, message-ts) → canonical https permalink
// via chat.getPermalink, cached in-memory with a TTL. Needs only channel+ts (no
// team_id), so it works for any historical item. Every method is nil-safe: a nil
// resolver (no token) returns "". Negative results are cached too.
type SlackPermalinker struct {
	tokenFn func() string
	mu      sync.Mutex
	cache   map[string]permaEntry
}
type permaEntry struct {
	link string
	at   time.Time
}

// NewSlackPermalinker returns a resolver backed by the bot token, or nil when no
// token is configured. The token is resolved per call so a rotated token takes
// effect without reconstructing the resolver.
func NewSlackPermalinker() *SlackPermalinker {
	if strings.TrimSpace(SlackBotToken()) == "" {
		return nil
	}
	return &SlackPermalinker{tokenFn: SlackBotToken, cache: map[string]permaEntry{}}
}

// Permalink returns the message permalink, or "" when nil/blank/lookup fails.
func (p *SlackPermalinker) Permalink(ctx context.Context, channel, ts string) string {
	channel, ts = strings.TrimSpace(channel), strings.TrimSpace(ts)
	if p == nil || channel == "" || ts == "" {
		return ""
	}
	key := channel + ":" + ts
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && time.Since(e.at) < slackPermalinkCacheTTL {
		p.mu.Unlock()
		return e.link
	}
	p.mu.Unlock()
	token := callSlackTokenFn(p.tokenFn)
	if token == "" {
		// No token configured right now (e.g. between disconnect and re-auth).
		// Return "" without caching so the link resolves once a token returns.
		return ""
	}
	link, err := slackPermalinkFn(ctx, token, channel, ts)
	if err != nil {
		link = "" // negative-cache
	}
	p.mu.Lock()
	p.cache[key] = permaEntry{link: link, at: time.Now()}
	p.mu.Unlock()
	return link
}

// CachedPermalink returns a permalink ONLY from the in-memory cache — it never
// makes a network call. "" on a miss (or nil/blank). The feed list uses this so a
// cold row degrades to a deep-link instead of a per-row chat.getPermalink stall;
// Warm does the network resolution concurrently up front.
func (p *SlackPermalinker) CachedPermalink(channel, ts string) string {
	channel, ts = strings.TrimSpace(channel), strings.TrimSpace(ts)
	if p == nil || channel == "" || ts == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.cache[channel+":"+ts]; ok && time.Since(e.at) < slackPermalinkCacheTTL {
		return e.link
	}
	return ""
}

// Warm concurrently resolves permalinks for the given (channel, ts) pairs into
// the cache, so a following batch of CachedPermalink calls are cache hits instead
// of one serial chat.getPermalink round-trip each — the cold-feed stall where a
// few-hundred dismissed rows took ~40s resolving serially. Bounded concurrency;
// best-effort within ctx: a pair that doesn't resolve before the deadline is left
// UNcached (never negative-cached on a timeout, so a transient stall doesn't blank
// a real link for the TTL) and resolves on a later load. Already-fresh pairs and
// duplicates are skipped.
func (p *SlackPermalinker) Warm(ctx context.Context, channels, tss []string) {
	if p == nil || len(channels) != len(tss) {
		return
	}
	token := callSlackTokenFn(p.tokenFn)
	if token == "" {
		return
	}
	type pair struct{ ch, ts string }
	seen := make(map[string]struct{}, len(channels))
	var todo []pair
	p.mu.Lock()
	for i := range channels {
		ch, ts := strings.TrimSpace(channels[i]), strings.TrimSpace(tss[i])
		if ch == "" || ts == "" {
			continue
		}
		key := ch + ":" + ts
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if e, ok := p.cache[key]; ok && time.Since(e.at) < slackPermalinkCacheTTL {
			continue
		}
		todo = append(todo, pair{ch, ts})
	}
	p.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	sem := make(chan struct{}, 24)
	var wg sync.WaitGroup
	for _, pr := range todo {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ch, ts string) {
			defer wg.Done()
			defer func() { <-sem }()
			link, err := slackPermalinkFn(ctx, token, ch, ts)
			if err != nil {
				if ctx.Err() != nil {
					return // timed out/cancelled — leave uncached for a later load
				}
				link = "" // real Slack error → negative-cache
			}
			p.mu.Lock()
			p.cache[ch+":"+ts] = permaEntry{link: link, at: time.Now()}
			p.mu.Unlock()
		}(pr.ch, pr.ts)
	}
	wg.Wait()
}
