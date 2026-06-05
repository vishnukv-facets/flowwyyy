package monitor

import (
	"context"
	"errors"
	"testing"
)

// newTestPermalinker builds a resolver without depending on an env token, so
// the cache behaviour can be exercised hermetically.
func newTestPermalinker() *SlackPermalinker {
	return &SlackPermalinker{token: "x", cache: map[string]permaEntry{}}
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
