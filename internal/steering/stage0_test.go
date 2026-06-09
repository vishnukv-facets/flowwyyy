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
		name       string
		ev         monitor.InboundEvent
		wantPass   bool
		wantKey    string
		wantReason string // asserted only when set and the case is a drop
	}{
		{
			name:     "dm passes",
			ev:       monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "1.1", ThreadTS: "1.1", UserID: "U_OTHER", Text: "hey"},
			wantPass: true, wantKey: "D1:1.1",
		},
		{
			// A DM channel id (D-prefix) is a DM regardless of whether the event
			// carried channel_type — the durable backfill recovers DM replies and
			// the parser doesn't always stamp "im". inScope must still treat it as
			// in scope, matching the D-prefix convention used in context_fetch.
			name:     "dm channel id passes even when channel_type is unset",
			ev:       monitor.InboundEvent{Kind: "message", Channel: "D7", TS: "10.1", ThreadTS: "10.0", UserID: "U_OTHER", Text: "recovered dm reply"},
			wantPass: true, wantKey: "D7:10.0",
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
			name:       "unwatched channel no mention drops (out of scope)",
			ev:         monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_OTHER", TS: "4.1", ThreadTS: "4.0", UserID: "U_OTHER", Text: "random chatter"},
			wantPass:   false,
			wantReason: "out of scope / not watched",
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
			// C_MUTED is muted AND not in WatchedChannels; mute must win so the
			// trace stays legible (ordering preserved, not the scope drop).
			name:       "muted channel drops (mute beats scope)",
			ev:         monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_MUTED", TS: "7.1", ThreadTS: "7.1", UserID: "U_OTHER", Text: "hi"},
			wantPass:   false,
			wantReason: "muted channel",
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
			if !c.wantPass && c.wantReason != "" && got.DropReason != c.wantReason {
				t.Errorf("DropReason = %q, want %q", got.DropReason, c.wantReason)
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

func TestStage0GitHubDropsUninvolved(t *testing.T) {
	// A webhook PR the operator has nothing to do with — not author/assignee/
	// reviewer, not @-mentioned, no tracking task — must drop. An org-wide webhook
	// install otherwise floods the cascade with the whole org's PR churn.
	ev := ghEvent("o/r", "reviewer", "please take a look")
	ev.Participants = []string{"reviewer", "someone-else"} // webhook event; operator absent
	got := Stage0(ev, githubCfg())
	if got.Pass {
		t.Fatalf("Pass = true, want dropped out-of-scope for an uninvolved PR")
	}
	if got.DropReason != "out of scope (operator not involved)" {
		t.Errorf("DropReason = %q, want out-of-scope", got.DropReason)
	}
}

func TestStage0GitHubFailsOpenWithoutParticipantData(t *testing.T) {
	// A poller-sourced event carries no Participants and is pre-filtered to
	// involve the operator — must pass (we can't and shouldn't gate it).
	got := Stage0(ghEvent("o/r", "reviewer", "please take a look"), githubCfg())
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want pass for a no-participant (poller) event", got.DropReason)
	}
}

func TestStage0GitHubPassesWhenMentioned(t *testing.T) {
	ev := ghEvent("o/r", "reviewer", "hey @octocat-self can you take a look?")
	ev.Participants = []string{"reviewer"} // webhook event; operator surfaces via mention
	got := Stage0(ev, githubCfg())
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want true when the operator is @-mentioned", got.DropReason)
	}
	if got.ThreadKey != "o/r:gh-pr:o/r#5" {
		t.Errorf("ThreadKey = %q", got.ThreadKey)
	}
}

func TestStage0GitHubPassesWhenParticipant(t *testing.T) {
	// Operator is the PR author / assignee / requested reviewer (a participant),
	// surfaced via Participants on the event.
	ev := ghEvent("o/r", "reviewer", "please take a look")
	ev.Participants = []string{"someone-else", "octocat-self"}
	got := Stage0(ev, githubCfg())
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want true when the operator is a participant", got.DropReason)
	}
}

func TestStage0GitHubPassesWhenTaskLinked(t *testing.T) {
	cfg := githubCfg()
	cfg.TaskLinkedGitHubThreads = map[string]bool{"o/r:gh-pr:o/r#5": true}
	got := Stage0(ghEvent("o/r", "reviewer", "please take a look"), cfg)
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want true when the PR is task-linked", got.DropReason)
	}
}

func TestStage0GitHubFailsOpenWhenIdentityUnset(t *testing.T) {
	// Without the operator's GitHub login we can't judge involvement — don't
	// silently drop everything; preserve prior behavior (pass).
	cfg := githubCfg()
	cfg.GitHubIdentity = nil
	got := Stage0(ghEvent("o/r", "reviewer", "please take a look"), cfg)
	if !got.Pass {
		t.Fatalf("Pass = false (reason %q), want pass (fail open) when identity unset", got.DropReason)
	}
}

func TestStage0GitHubMentionRespectsWordBoundary(t *testing.T) {
	// "@octocat-self-bot" must NOT match operator "octocat-self".
	ev := ghEvent("o/r", "reviewer", "ping @octocat-self-bot to run CI")
	ev.Participants = []string{"reviewer"} // webhook event so the gate fires
	got := Stage0(ev, githubCfg())
	if got.Pass {
		t.Fatalf("Pass = true, want drop — @octocat-self-bot is not @octocat-self")
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

func TestStage0GitHubAllowsTaskLinkedSelfAuthoredHeadUpdate(t *testing.T) {
	cfg := githubCfg()
	cfg.TaskLinkedGitHubThreads = map[string]bool{"o/r:gh-pr:o/r#21": true}
	ev := ghEvent("o/r", "octocat-self", "head changed")
	ev.Kind = "pr_head_updated"
	ev.ThreadTS = "gh-pr:o/r#21"
	got := Stage0(ev, cfg)
	if !got.Pass {
		t.Fatalf("Stage0 = %+v, want task-linked self-authored head update to pass", got)
	}
}

func TestStage0GitHubAllowsTaskLinkedAuthorlessHeadUpdate(t *testing.T) {
	cfg := githubCfg()
	cfg.TaskLinkedGitHubThreads = map[string]bool{"o/r:gh-pr:o/r#21": true}
	ev := ghEvent("o/r", "", "head changed")
	ev.Kind = "pr_head_updated"
	ev.ThreadTS = "gh-pr:o/r#21"
	got := Stage0(ev, cfg)
	if !got.Pass {
		t.Fatalf("Stage0 = %+v, want task-linked authorless head update to pass", got)
	}
}

func TestStage0GitHubStillDropsUnlinkedSelfAuthoredInvolved(t *testing.T) {
	cfg := githubCfg()
	ev := ghEvent("o/r", "octocat-self", "self authored")
	ev.Kind = "pr_involved"
	ev.ThreadTS = "gh-pr:o/r#21"
	got := Stage0(ev, cfg)
	if got.Pass || got.DropReason != "self-authored" {
		t.Fatalf("Stage0 = %+v, want self-authored drop", got)
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
