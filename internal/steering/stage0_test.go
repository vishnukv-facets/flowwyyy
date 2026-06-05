package steering

import (
	"testing"

	"flow/internal/monitor"
)

func baseCfg() WatchConfig {
	return WatchConfig{
		WatchedChannels: map[string]bool{"C_WATCHED": true},
		MutedChannels:   map[string]bool{"C_MUTED": true},
		MutedKeywords:   []string{"lunch"},
		Identity:        OperatorIdentity{UserIDs: []string{"U_ME"}},
		MentionUserIDs:  []string{"U_ME"},
	}
}

func TestStage0(t *testing.T) {
	cases := []struct {
		name     string
		ev       monitor.InboundEvent
		wantPass bool
		wantKey  string
	}{
		{
			name:     "dm passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "1.1", ThreadTS: "1.1", UserID: "U_OTHER", Text: "hey"},
			wantPass: true, wantKey: "D1:1.1",
		},
		{
			name:     "watched channel passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_WATCHED", TS: "2.1", ThreadTS: "2.0", UserID: "U_OTHER", Text: "ship it"},
			wantPass: true, wantKey: "C_WATCHED:2.0",
		},
		{
			name:     "app_mention in unwatched channel passes (mention)",
			ev:       monitor.InboundEvent{Kind: "app_mention", ChannelType: "channel", Channel: "C_OTHER", TS: "3.1", ThreadTS: "3.1", UserID: "U_OTHER", Text: "<@U_ME> ping"},
			wantPass: true, wantKey: "C_OTHER:3.1",
		},
		{
			name:     "text mention in unwatched channel passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_OTHER", TS: "3.5", ThreadTS: "3.5", UserID: "U_OTHER", Text: "cc <@U_ME> please look"},
			wantPass: true, wantKey: "C_OTHER:3.5",
		},
		{
			name:     "unwatched channel no mention drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_OTHER", TS: "4.1", ThreadTS: "4.0", UserID: "U_OTHER", Text: "random chatter"},
			wantPass: false,
		},
		{
			name:     "self-authored drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "5.1", ThreadTS: "5.1", UserID: "U_ME", Text: "note to self"},
			wantPass: false,
		},
		{
			name:     "empty user (bot/system) drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_WATCHED", TS: "6.1", ThreadTS: "6.1", UserID: "", Text: "deploy ok"},
			wantPass: false,
		},
		{
			name:     "muted channel drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_MUTED", TS: "7.1", ThreadTS: "7.1", UserID: "U_OTHER", Text: "hi"},
			wantPass: false,
		},
		{
			name:     "muted keyword drops",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "8.1", ThreadTS: "8.1", UserID: "U_OTHER", Text: "Going for LUNCH?"},
			wantPass: false,
		},
		{
			name:     "reaction kind drops (belongs to reaction pipeline)",
			ev:       monitor.InboundEvent{Kind: "reaction_added", Channel: "C_WATCHED", TS: "9.1", ThreadTS: "9.0", UserID: "U_OTHER", Reaction: "eyes"},
			wantPass: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Stage0(c.ev, baseCfg())
			if got.Pass != c.wantPass {
				t.Fatalf("Pass = %v (reason %q), want %v", got.Pass, got.DropReason, c.wantPass)
			}
			if c.wantPass && got.ThreadKey != c.wantKey {
				t.Errorf("ThreadKey = %q, want %q", got.ThreadKey, c.wantKey)
			}
		})
	}
}

func githubCfg() WatchConfig {
	cfg := baseCfg()
	cfg.GitHubIdentity = []string{"octocat-self"}
	return cfg
}

func ghEvent(channel, author, body string) monitor.InboundEvent {
	return monitor.InboundEvent{
		Kind: "pr_comment", ChannelType: "github", Channel: channel,
		TS: "2026-06-05T10:00:00Z", ThreadTS: "gh-pr:" + channel + "#5",
		UserID: author, Text: body,
		URL: "https://github.com/" + channel + "/pull/5",
	}
}

func TestStage0GitHubPasses(t *testing.T) {
	got := Stage0(ghEvent("o/r", "reviewer", "please take a look"), githubCfg())
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want true", got.DropReason)
	}
	if got.ThreadKey != "o/r:gh-pr:o/r#5" {
		t.Errorf("ThreadKey = %q, want %q", got.ThreadKey, "o/r:gh-pr:o/r#5")
	}
}

func TestStage0GitHubSelfAuthored(t *testing.T) {
	got := Stage0(ghEvent("o/r", "octocat-self", "self note"), githubCfg())
	if got.Pass {
		t.Fatalf("Pass = true, want dropped self-authored")
	}
	if got.DropReason != "self-authored" {
		t.Errorf("DropReason = %q, want %q", got.DropReason, "self-authored")
	}
}

func TestStage0GitHubMutedRepo(t *testing.T) {
	cfg := githubCfg()
	cfg.MutedChannels = map[string]bool{"o/r": true}
	got := Stage0(ghEvent("o/r", "reviewer", "hi"), cfg)
	if got.Pass {
		t.Fatalf("Pass = true, want dropped muted repo")
	}
	if got.DropReason != "muted repo" {
		t.Errorf("DropReason = %q, want %q", got.DropReason, "muted repo")
	}
}

func TestStage0MutedSenderAndThread(t *testing.T) {
	// Muted sender → dropped even in a watched channel / DM.
	cfg := baseCfg()
	cfg.MutedAuthors = map[string]bool{"U_BOT": true}
	cfg.MutedThreads = map[string]bool{"D9:9.9": true}

	sender := monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D2", TS: "5.1", ThreadTS: "5.1", UserID: "U_BOT", Text: "noise"}
	if got := Stage0(sender, cfg); got.Pass || got.DropReason != "muted sender" {
		t.Errorf("muted sender = %+v, want dropped 'muted sender'", got)
	}

	thread := monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D9", TS: "9.10", ThreadTS: "9.9", UserID: "U_OTHER", Text: "anything"}
	if got := Stage0(thread, cfg); got.Pass || got.DropReason != "muted thread" {
		t.Errorf("muted thread = %+v, want dropped 'muted thread'", got)
	}

	// A non-muted sender in a non-muted thread still passes.
	ok := monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D3", TS: "7.1", ThreadTS: "7.1", UserID: "U_HUMAN", Text: "real question"}
	if got := Stage0(ok, cfg); !got.Pass {
		t.Errorf("unmuted event should pass, got %+v", got)
	}
}
