package steering

import (
	"testing"

	"flow/internal/monitor"
)

func TestSessionKeyForEvent(t *testing.T) {
	cases := []struct {
		name    string
		ev      monitor.InboundEvent
		wantKey string
		wantOK  bool
	}{
		{"slack channel", monitor.InboundEvent{Kind: "message", Channel: "C123", ChannelType: "channel", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, "C123", true},
		{"slack dm", monitor.InboundEvent{Kind: "message", Channel: "D999", ChannelType: "im", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, "D999", true},
		{"slack mpim", monitor.InboundEvent{Kind: "message", Channel: "G555", ChannelType: "mpim", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, "G555", true},
		{"shared-ref keys origin channel", monitor.InboundEvent{Kind: "message", Channel: "Dme", ChannelType: "im", TS: "9.0", ThreadTS: "9.0", UserID: "U1", RefChannel: "Corigin", RefThreadTS: "5.0", RefTS: "5.1"}, "Corigin", true},
		{"github deferred to P5", monitor.InboundEvent{Kind: "pr_comment", Channel: "owner/repo", ChannelType: "github", TS: "1", ThreadTS: "gh-pr:owner/repo#1", UserID: "octocat"}, "", false},
		{"empty channel", monitor.InboundEvent{Kind: "message", Channel: "", UserID: "U1"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotOK := sessionKeyForEvent(tc.ev)
			if gotKey != tc.wantKey || gotOK != tc.wantOK {
				t.Fatalf("sessionKeyForEvent = (%q,%v), want (%q,%v)", gotKey, gotOK, tc.wantKey, tc.wantOK)
			}
		})
	}
}

func TestSteererSessionsEnabled(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "")
	if SteererSessionsEnabled() {
		t.Fatal("default must be off")
	}
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	if !SteererSessionsEnabled() {
		t.Fatal("FLOW_STEERING_SESSIONS=1 must enable")
	}
	t.Setenv("FLOW_STEERING_SESSIONS", "off")
	if SteererSessionsEnabled() {
		t.Fatal("off must disable")
	}
}
