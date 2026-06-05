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
	token string
	mu    sync.Mutex
	cache map[string]permaEntry
}
type permaEntry struct {
	link string
	at   time.Time
}

// NewSlackPermalinker returns a resolver backed by the bot token, or nil when no
// token is configured.
func NewSlackPermalinker() *SlackPermalinker {
	tok := SlackBotToken()
	if strings.TrimSpace(tok) == "" {
		return nil
	}
	return &SlackPermalinker{token: tok, cache: map[string]permaEntry{}}
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
	link, err := slackPermalinkFn(ctx, p.token, channel, ts)
	if err != nil {
		link = "" // negative-cache
	}
	p.mu.Lock()
	p.cache[key] = permaEntry{link: link, at: time.Now()}
	p.mu.Unlock()
	return link
}
