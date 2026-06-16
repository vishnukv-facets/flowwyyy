package steering

import (
	"testing"

	"flow/internal/monitor"
)

func TestSessionKeyForEvent(t *testing.T) {
	// canonical maps PR owner/repo#1 → issue #3, proving a linked pair collapses to
	// one key. nil in a case ⇒ identity (each item keys on its own number).
	canonical := func(repo string, num int) (int, bool) {
		if repo == "owner/repo" && num == 1 {
			return 3, true
		}
		return 0, false
	}
	cases := []struct {
		name      string
		ev        monitor.InboundEvent
		canonical CanonicalGitHubNumFunc
		wantKey   string
		wantOK    bool
	}{
		{"slack channel", monitor.InboundEvent{Kind: "message", Channel: "C123", ChannelType: "channel", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, nil, "C123", true},
		{"slack dm", monitor.InboundEvent{Kind: "message", Channel: "D999", ChannelType: "im", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, nil, "D999", true},
		{"slack mpim", monitor.InboundEvent{Kind: "message", Channel: "G555", ChannelType: "mpim", TS: "1.0", ThreadTS: "1.0", UserID: "U1"}, nil, "G555", true},
		{"shared-ref keys origin channel", monitor.InboundEvent{Kind: "message", Channel: "Dme", ChannelType: "im", TS: "9.0", ThreadTS: "9.0", UserID: "U1", RefChannel: "Corigin", RefThreadTS: "5.0", RefTS: "5.1"}, nil, "Corigin", true},
		{"github pr identity (no resolver)", monitor.InboundEvent{Kind: "pr_comment", Channel: "owner/repo", ChannelType: "github", ItemTS: "1", TS: "1", ThreadTS: "gh-pr:owner/repo#1", UserID: "octocat"}, nil, "gh-owner-repo-1", true},
		{"github pr collapses to linked issue", monitor.InboundEvent{Kind: "pr_comment", Channel: "owner/repo", ChannelType: "github", ItemTS: "1", TS: "1", ThreadTS: "gh-pr:owner/repo#1", UserID: "octocat"}, canonical, "gh-owner-repo-3", true},
		{"github issue keys on own number", monitor.InboundEvent{Kind: "issue_comment", Channel: "owner/repo", ChannelType: "github", ItemTS: "3", TS: "3", ThreadTS: "gh-issue:owner/repo#3", UserID: "octocat"}, canonical, "gh-owner-repo-3", true},
		{"github no number", monitor.InboundEvent{Kind: "pr_comment", Channel: "owner/repo", ChannelType: "github", ItemTS: "", UserID: "octocat"}, nil, "", false},
		{"empty channel", monitor.InboundEvent{Kind: "message", Channel: "", UserID: "U1"}, nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotOK := sessionKeyForEvent(tc.ev, tc.canonical)
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
