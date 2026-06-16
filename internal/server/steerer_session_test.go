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
