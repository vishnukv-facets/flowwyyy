package server

import (
	"strings"
	"testing"
	"time"

	"flow/internal/steering"
)

func TestSteererChatSlug(t *testing.T) {
	got := steererChatSlug("C0A1B2C3")
	if got != "chat-steer-c0a1b2c3" {
		t.Fatalf("slug = %q", got)
	}
	if err := validateSlug(got); err != nil {
		t.Fatalf("slug %q invalid: %v", got, err)
	}
}

func TestSteererTitleFor(t *testing.T) {
	cases := []struct {
		name        string
		p           steering.SteererDelivery
		channelName string
		authorName  string
		want        string
	}{
		{"slack channel resolved", steering.SteererDelivery{Source: "slack", ChannelType: "channel", Channel: "C1"}, "#facets-coinswitch", "", "#facets-coinswitch"},
		{"slack channel unresolved → empty (caller falls back)", steering.SteererDelivery{Source: "slack", ChannelType: "channel", Channel: "C1"}, "", "", ""},
		{"slack dm", steering.SteererDelivery{Source: "slack", ChannelType: "im", Channel: "D1", Author: "U9"}, "", "Nayan Kalita", "DM · Nayan Kalita"},
		{"slack mpim", steering.SteererDelivery{Source: "slack", ChannelType: "mpim", Channel: "G1", Author: "U9"}, "", "Rohit", "Group · Rohit"},
		{"github pr", steering.SteererDelivery{Source: "github", ChannelType: "github", Channel: "vishnukv-facets/flowwyyy", ThreadTS: "gh-pr:vishnukv-facets/flowwyyy#17"}, "", "", "vishnukv-facets/flowwyyy#17"},
		{"github issue", steering.SteererDelivery{Source: "github", ChannelType: "github", Channel: "o/r", ThreadTS: "gh-issue:o/r#3"}, "", "", "o/r#3"},
		{"github no number → repo only", steering.SteererDelivery{Source: "github", ChannelType: "github", Channel: "o/r", ThreadTS: "garbage"}, "", "", "o/r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := steererTitleFor(tc.p, tc.channelName, tc.authorName); got != tc.want {
				t.Errorf("steererTitleFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSteererSessionProvider(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", "claude"},
		{"claude", "claude"},
		{"codex", "codex"},
		{"garbage", "claude"}, // invalid → safe default
	}
	for _, tc := range cases {
		t.Setenv("FLOW_STEERER_DEFAULT_PROVIDER", tc.env)
		if got := steererSessionProvider(); got != tc.want {
			t.Errorf("FLOW_STEERER_DEFAULT_PROVIDER=%q ⇒ %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestForkTriggerMatches(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Claude usage limit reached. Resets at 5pm.", true},
		{"Error: rate_limit_error", true},
		{"You have insufficient_quota for this request", true},
		{"Your credit balance is too low", true},
		{"", false},
		{"normal assistant reply about the PR", false},
		{"Error: overloaded_error, please retry", false}, // transient, not exhaustion
		{"request timeout (500)", false},
	}
	for _, tc := range cases {
		if got := forkTriggerMatches(tc.text); got != tc.want {
			t.Errorf("forkTriggerMatches(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestRecentSteererExhaustion(t *testing.T) {
	// marker only in an OLD entry (beyond the tail) is ignored; recent one matches.
	var old []TranscriptEntry
	for range 12 {
		old = append(old, TranscriptEntry{Type: "assistant", Text: "fine"})
	}
	old[0].Text = "usage limit reached" // beyond the 8-entry tail
	if recentSteererExhaustion(old) {
		t.Error("old marker beyond the tail must not trigger")
	}
	recent := []TranscriptEntry{
		{Type: "assistant", Text: "ok"},
		{Type: "tool_result", ToolResultText: "rate limit exceeded"},
	}
	if !recentSteererExhaustion(recent) {
		t.Error("recent exhaustion marker must trigger")
	}
}

func TestShouldRecoverToClaude(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	after := 2 * time.Hour
	cases := []struct {
		name     string
		forkedAt time.Time
		flagOn   bool
		want     bool
	}{
		{"flag off", now.Add(-3 * time.Hour), false, false},
		{"never forked", time.Time{}, true, false},
		{"within cooldown", now.Add(-1 * time.Hour), true, false},
		{"cooldown elapsed", now.Add(-3 * time.Hour), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRecoverToClaude(now, tc.forkedAt, after, tc.flagOn); got != tc.want {
				t.Errorf("shouldRecoverToClaude = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSteererDeliveryPlan(t *testing.T) {
	cases := []struct {
		name    string
		exists  bool
		running bool
		want    steererDeliveryAction
	}{
		{"no row → start", false, false, steererActStart},
		{"no row even if a stale running flag → start", false, true, steererActStart},
		{"row + live → wake", true, true, steererActWake},
		{"row + pty gone → resume", true, false, steererActResume},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := steererDeliveryPlan(tc.exists, tc.running); got != tc.want {
				t.Fatalf("plan(%v,%v) = %v, want %v", tc.exists, tc.running, got, tc.want)
			}
		})
	}
}

func TestSteererShouldSleep(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	ttl := 30 * time.Minute
	if !steererShouldSleep(now, now.Add(-31*time.Minute), ttl) {
		t.Error("quiet past TTL must sleep")
	}
	if steererShouldSleep(now, now.Add(-5*time.Minute), ttl) {
		t.Error("recent activity must not sleep")
	}
	if steererShouldSleep(now, time.Time{}, ttl) {
		t.Error("zero mtime must not sleep (unknown ⇒ keep alive)")
	}
}

func TestRenderSteererTurn(t *testing.T) {
	out := renderSteererTurn(steering.SteererDelivery{
		Source: "slack", Channel: "C1", ChannelType: "channel",
		TS: "100.1", ThreadTS: "100.1", Author: "U1", Text: "list the repo names",
	})
	for _, want := range []string{"slack", "C1", "100.1", "U1", "list the repo names"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered turn missing %q:\n%s", want, out)
		}
	}
	ctxOnly := renderSteererTurn(steering.SteererDelivery{Channel: "C1", TS: "1", Text: "x", ContextOnly: true})
	if !strings.Contains(ctxOnly, "context") {
		t.Errorf("context_only turn must be labeled:\n%s", ctxOnly)
	}
}
