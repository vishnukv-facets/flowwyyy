package monitor

import (
	"context"
	"errors"
	"testing"
)

// newTestPermalinker builds a resolver without depending on an env token, so
// the cache behaviour can be exercised hermetically.
func newTestPermalinker() *SlackPermalinker {
	return &SlackPermalinker{tokenFn: func() string { return "x" }, cache: map[string]permaEntry{}}
}

func TestSlackPermalinkerResolvesAndCaches(t *testing.T) {
	old := slackPermalinkFn
	t.Cleanup(func() { slackPermalinkFn = old })

	calls := 0
	slackPermalinkFn = func(_ context.Context, token, channel, ts string) (string, error) {
		calls++
		if token != "x" {
			t.Errorf("token = %q, want x", token)
		}
		return "https://slack.example/archives/" + channel + "/p" + ts, nil
	}

	p := newTestPermalinker()
	got := p.Permalink(context.Background(), "C1", "123.45")
	want := "https://slack.example/archives/C1/p123.45"
	if got != want {
		t.Fatalf("Permalink = %q, want %q", got, want)
	}
	// Second call for the same (channel, ts) is a cache hit — no extra API call.
	if got2 := p.Permalink(context.Background(), "C1", "123.45"); got2 != want {
		t.Fatalf("cached Permalink = %q, want %q", got2, want)
	}
	if calls != 1 {
		t.Errorf("fake called %d times, want 1 (second is cached)", calls)
	}
}

func TestSlackPermalinkerNegativeCaches(t *testing.T) {
	old := slackPermalinkFn
	t.Cleanup(func() { slackPermalinkFn = old })

	calls := 0
	slackPermalinkFn = func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return "", errors.New("message_not_found")
	}

	p := newTestPermalinker()
	if got := p.Permalink(context.Background(), "C1", "9.9"); got != "" {
		t.Fatalf("Permalink on error = %q, want empty", got)
	}
	// The negative result is cached too — no second API call.
	if got := p.Permalink(context.Background(), "C1", "9.9"); got != "" {
		t.Fatalf("cached negative Permalink = %q, want empty", got)
	}
	if calls != 1 {
		t.Errorf("fake called %d times, want 1 (negative is cached)", calls)
	}
}

func TestSlackPermalinkerBlankAndNilSafe(t *testing.T) {
	old := slackPermalinkFn
	t.Cleanup(func() { slackPermalinkFn = old })
	slackPermalinkFn = func(_ context.Context, _, _, _ string) (string, error) {
		t.Fatal("slackPermalinkFn must not be called for blank/nil inputs")
		return "", nil
	}

	p := newTestPermalinker()
	if got := p.Permalink(context.Background(), "", "123.45"); got != "" {
		t.Errorf("blank channel: Permalink = %q, want empty", got)
	}
	if got := p.Permalink(context.Background(), "C1", ""); got != "" {
		t.Errorf("blank ts: Permalink = %q, want empty", got)
	}
	// A nil resolver (no token configured) returns "" without panicking.
	var nilp *SlackPermalinker
	if got := nilp.Permalink(context.Background(), "C1", "123.45"); got != "" {
		t.Errorf("nil resolver: Permalink = %q, want empty", got)
	}
}

// Warm pre-resolves a batch into the cache so CachedPermalink (no network) hits;
// this is what keeps the feed list from making one serial getPermalink per row.
func TestSlackPermalinkerWarmThenCachedHit(t *testing.T) {
	old := slackPermalinkFn
	t.Cleanup(func() { slackPermalinkFn = old })
	slackPermalinkFn = func(_ context.Context, _, channel, ts string) (string, error) {
		return "https://slack.example/" + channel + "/p" + ts, nil
	}
	p := newTestPermalinker()

	// Cache-only lookup is empty before warming — it must NOT hit the network.
	if got := p.CachedPermalink("C1", "1.1"); got != "" {
		t.Fatalf("CachedPermalink before warm = %q, want empty", got)
	}

	p.Warm(context.Background(), []string{"C1", "C2", "C1"}, []string{"1.1", "2.2", "1.1"})

	if got, want := p.CachedPermalink("C1", "1.1"), "https://slack.example/C1/p1.1"; got != want {
		t.Errorf("CachedPermalink(C1) = %q, want %q", got, want)
	}
	if got, want := p.CachedPermalink("C2", "2.2"), "https://slack.example/C2/p2.2"; got != want {
		t.Errorf("CachedPermalink(C2) = %q, want %q", got, want)
	}
	// An unwarmed pair is still a cache miss (no network in CachedPermalink).
	if got := p.CachedPermalink("C9", "9.9"); got != "" {
		t.Errorf("CachedPermalink(unwarmed) = %q, want empty", got)
	}
}

// A timed-out Warm must NOT negative-cache — a transient stall can't blank a real
// link for the TTL. After the deadline passes, the pair is simply uncached.
func TestSlackPermalinkerWarmCanceledLeavesUncached(t *testing.T) {
	old := slackPermalinkFn
	t.Cleanup(func() { slackPermalinkFn = old })
	slackPermalinkFn = func(ctx context.Context, _, _, _ string) (string, error) {
		<-ctx.Done() // never resolves before the deadline
		return "", ctx.Err()
	}
	p := newTestPermalinker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	p.Warm(ctx, []string{"C1"}, []string{"1.1"})
	if _, cached := p.cache["C1:1.1"]; cached {
		t.Errorf("a cancelled Warm must not cache the pair")
	}
}
