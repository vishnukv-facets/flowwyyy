package app

import (
	"os"
	"strings"
	"testing"

	"flow/internal/monitor"
)

func stubSlackHydrate(t *testing.T) {
	t.Helper()
	orig := slackHydrateFn
	t.Cleanup(func() { slackHydrateFn = orig })
	slackHydrateFn = func() {}
}

func withSlackEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	keys := []string{
		"SLACK_BOT_TOKEN",
		"FLOW_SLACK_TOKEN",
		"SLACK_USER_TOKEN",
		"FLOW_SLACK_USER_TOKEN",
		"SLACK_TOKEN",
	}
	for _, k := range keys {
		k := k
		orig, had := os.LookupEnv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, orig)
			} else {
				os.Unsetenv(k)
			}
		})
		os.Unsetenv(k)
	}
	for k, v := range kv {
		os.Setenv(k, v)
	}
}

func TestSlackReadTokenPrefersUserThenBot(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{
		"FLOW_SLACK_USER_TOKEN": "xoxp-user",
		"SLACK_BOT_TOKEN":       "xoxb-bot",
	})

	tok, err := slackReadToken("", false)
	if err != nil || tok != "xoxp-user" {
		t.Fatalf("default = (%q,%v), want (xoxp-user,nil)", tok, err)
	}
	tok, err = slackReadToken("bot", false)
	if err != nil || tok != "xoxb-bot" {
		t.Fatalf("as=bot = (%q,%v), want (xoxb-bot,nil)", tok, err)
	}
}

func TestSlackReadTokenRequireUserRejectsBot(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})

	if _, err := slackReadToken("bot", true); err == nil {
		t.Fatal("require-user with --as bot should error")
	}
	if _, err := slackReadToken("", true); err == nil {
		t.Fatal("require-user with no user token should error")
	}
}

func TestSlackReadTokenNoToken(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{})

	if _, err := slackReadToken("", false); err == nil {
		t.Fatal("no token configured should error")
	}
}

func TestValidSlackFormat(t *testing.T) {
	for _, f := range []string{"", "table", "json", "tsv"} {
		if !validSlackFormat(f) {
			t.Errorf("validSlackFormat(%q) = false, want true", f)
		}
	}
	if validSlackFormat("yaml") {
		t.Error("validSlackFormat(yaml) = true, want false")
	}
}

func TestEmitSlackJSON(t *testing.T) {
	if rc := emitSlack("yaml", nil, nil, nil); rc != 2 {
		t.Errorf("emitSlack bad format rc = %d, want 2", rc)
	}
	if rc := emitSlack("json", []string{"A"}, [][]string{{"1"}}, []string{"x"}); rc != 0 {
		t.Errorf("emitSlack json rc = %d, want 0", rc)
	}
}

func TestSlackSearchUsersJSON(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	old := slackSearchUsersFn
	t.Cleanup(func() { slackSearchUsersFn = old })
	var gotToken, gotQuery string
	slackSearchUsersFn = func(token string, query string) ([]monitor.SlackUser, error) {
		gotToken, gotQuery = token, query
		return []monitor.SlackUser{{ID: "U1", Name: "vishnu.kv", RealName: "Vishnu KV"}}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"search-users", "--format", "json", "vishnu"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if gotToken != "xoxp-user" || gotQuery != "vishnu" {
		t.Fatalf("called with token/query = %q/%q", gotToken, gotQuery)
	}
	if !strings.Contains(out, `"id": "U1"`) {
		t.Fatalf("json output missing user: %s", out)
	}
}

func TestSlackUserByEmailTable(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})
	old := slackLookupUserByEmailFn
	t.Cleanup(func() { slackLookupUserByEmailFn = old })
	var gotEmail string
	slackLookupUserByEmailFn = func(_ string, email string) (monitor.SlackUser, error) {
		gotEmail = email
		return monitor.SlackUser{ID: "U2", Name: "alice", RealName: "Alice Smith", IsBot: true}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"user", "--email", "alice@example.com"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if gotEmail != "alice@example.com" {
		t.Fatalf("email = %q", gotEmail)
	}
	if !strings.Contains(out, "Alice Smith") || !strings.Contains(out, "true") {
		t.Fatalf("table output missing fields: %s", out)
	}
}

func TestSlackUserRequiresExactlyOneIdentifier(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	if rc := cmdSlack([]string{"user"}); rc != 2 {
		t.Fatalf("no identifier rc = %d, want 2", rc)
	}
	if rc := cmdSlack([]string{"user", "--id", "U1", "--email", "u@example.com"}); rc != 2 {
		t.Fatalf("two identifiers rc = %d, want 2", rc)
	}
}

func TestSlackListChannelsFiltersKindAndMatch(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	old := slackListChannelsFn
	t.Cleanup(func() { slackListChannelsFn = old })
	var gotToken string
	var gotDMs bool
	slackListChannelsFn = func(token string, includeDMs bool) ([]monitor.SlackChannelInfo, error) {
		gotToken, gotDMs = token, includeDMs
		return []monitor.SlackChannelInfo{
			{ID: "C1", Name: "general", Kind: "channel"},
			{ID: "G1", Name: "eng-private", Kind: "channel", IsPrivate: true},
			{ID: "D1", Name: "alice", Kind: "im"},
		}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"list-channels", "--kind", "private", "--match", "eng", "--format", "json"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if gotToken != "xoxp-user" || !gotDMs {
		t.Fatalf("token/includeDMs = %q/%t", gotToken, gotDMs)
	}
	if !strings.Contains(out, `"id": "G1"`) || strings.Contains(out, `"id": "C1"`) {
		t.Fatalf("filtered output wrong: %s", out)
	}
}

func TestSlackSearchChannelsUsesQueryAsMatch(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})
	old := slackListChannelsFn
	t.Cleanup(func() { slackListChannelsFn = old })
	slackListChannelsFn = func(string, bool) ([]monitor.SlackChannelInfo, error) {
		return []monitor.SlackChannelInfo{
			{ID: "C1", Name: "general", Kind: "channel"},
			{ID: "C2", Name: "eng", Kind: "channel"},
		}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"search-channels", "eng"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "C2") || strings.Contains(out, "C1") {
		t.Fatalf("search output wrong: %s", out)
	}
}

func TestSlackHistoryResolvesChannelAndReadsJSON(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	oldResolve, oldHistory := slackResolveChannelFn, slackHistoryReadFn
	t.Cleanup(func() {
		slackResolveChannelFn = oldResolve
		slackHistoryReadFn = oldHistory
	})
	slackResolveChannelFn = func(token, ref string, includeDMs bool) (monitor.SlackChannelInfo, error) {
		if token != "xoxp-user" || ref != "#general" || !includeDMs {
			t.Fatalf("resolve args = %q/%q/%t", token, ref, includeDMs)
		}
		return monitor.SlackChannelInfo{ID: "C1", Name: "general"}, nil
	}
	slackHistoryReadFn = func(token string, opts monitor.SlackHistoryOptions) ([]monitor.SlackMessage, error) {
		if token != "xoxp-user" || opts.ChannelID != "C1" || opts.Limit != 2 || opts.Oldest != "1.0" || opts.Latest != "2.0" {
			t.Fatalf("history args = %q/%+v", token, opts)
		}
		return []monitor.SlackMessage{{TS: "1.5", User: "U1", Text: "hello"}}, nil
	}

	out := captureStdout(t, func() {
		rc := cmdSlack([]string{"history", "--channel", "#general", "--limit", "2", "--oldest", "1.0", "--latest", "2.0", "--format", "json"})
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "1.5") {
		t.Fatalf("history output wrong: %s", out)
	}
}

func TestSlackThreadRequiresTS(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	if rc := cmdSlack([]string{"thread", "--channel", "C1"}); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
}

func TestSlackThreadReadsTable(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})
	oldResolve, oldThread := slackResolveChannelFn, slackThreadReadFn
	t.Cleanup(func() {
		slackResolveChannelFn = oldResolve
		slackThreadReadFn = oldThread
	})
	slackResolveChannelFn = func(string, string, bool) (monitor.SlackChannelInfo, error) {
		return monitor.SlackChannelInfo{ID: "C1", Name: "general"}, nil
	}
	slackThreadReadFn = func(token, channelID, threadTS string, limit int) ([]monitor.SlackMessage, error) {
		if token != "xoxb-bot" || channelID != "C1" || threadTS != "123.000100" || limit != 100 {
			t.Fatalf("thread args = %q/%q/%q/%d", token, channelID, threadTS, limit)
		}
		return []monitor.SlackMessage{{TS: "123.000100", User: "U1", Text: "root"}}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"thread", "--channel", "C1", "--ts", "123.000100"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "root") {
		t.Fatalf("thread output wrong: %s", out)
	}
}

func TestSlackMembersResolvesChannelAndOutputsJSON(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	oldResolve, oldMembers := slackResolveChannelFn, slackMembersReadFn
	t.Cleanup(func() {
		slackResolveChannelFn = oldResolve
		slackMembersReadFn = oldMembers
	})
	slackResolveChannelFn = func(string, string, bool) (monitor.SlackChannelInfo, error) {
		return monitor.SlackChannelInfo{ID: "C1", Name: "general"}, nil
	}
	slackMembersReadFn = func(token, channelID string, limit int) ([]monitor.SlackUser, error) {
		if token != "xoxp-user" || channelID != "C1" || limit != 200 {
			t.Fatalf("members args = %q/%q/%d", token, channelID, limit)
		}
		return []monitor.SlackUser{{ID: "U1", Name: "vishnu", RealName: "Vishnu KV"}}, nil
	}

	out := captureStdout(t, func() {
		rc := cmdSlack([]string{"members", "--channel", "#general", "--format", "json"})
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, `"id": "U1"`) {
		t.Fatalf("members output wrong: %s", out)
	}
}

func TestSlackReactionsReadsTable(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})
	oldResolve, oldReactions := slackResolveChannelFn, slackReactionsReadFn
	t.Cleanup(func() {
		slackResolveChannelFn = oldResolve
		slackReactionsReadFn = oldReactions
	})
	slackResolveChannelFn = func(string, string, bool) (monitor.SlackChannelInfo, error) {
		return monitor.SlackChannelInfo{ID: "C1", Name: "general"}, nil
	}
	slackReactionsReadFn = func(token, channelID, ts string) ([]monitor.SlackReaction, error) {
		if token != "xoxb-bot" || channelID != "C1" || ts != "123.000100" {
			t.Fatalf("reaction args = %q/%q/%q", token, channelID, ts)
		}
		return []monitor.SlackReaction{{Name: "eyes", Count: 2, Users: []string{"U1", "U2"}}}, nil
	}

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"reactions", "--channel", "C1", "--ts", "123.000100"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "eyes") || !strings.Contains(out, "U1,U2") {
		t.Fatalf("reactions output wrong: %s", out)
	}
}

func TestSlackSearchRequiresUserToken(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"})

	out := captureStdout(t, func() {
		if rc := cmdSlack([]string{"search", "deploy"}); rc != 1 {
			t.Fatalf("rc = %d, want 1", rc)
		}
	})
	if !strings.Contains(out, "search:read") {
		t.Fatalf("missing search:read error: %s", out)
	}
}

func TestSlackSearchMessagesJSON(t *testing.T) {
	stubSlackHydrate(t)
	withSlackEnv(t, map[string]string{"FLOW_SLACK_USER_TOKEN": "xoxp-user"})
	old := slackSearchMessagesReadFn
	t.Cleanup(func() { slackSearchMessagesReadFn = old })
	slackSearchMessagesReadFn = func(token, query, sort string, limit int) ([]monitor.SlackSearchMatch, error) {
		if token != "xoxp-user" || query != "deploy" || sort != "timestamp" || limit != 3 {
			t.Fatalf("search args = %q/%q/%q/%d", token, query, sort, limit)
		}
		return []monitor.SlackSearchMatch{{ChannelID: "C1", ChannelName: "general", User: "U1", TS: "1.0", Text: "deploy done"}}, nil
	}

	out := captureStdout(t, func() {
		rc := cmdSlack([]string{"search", "--sort", "timestamp", "--limit", "3", "--format", "json", "deploy"})
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "deploy done") || !strings.Contains(out, `"channel_id": "C1"`) {
		t.Fatalf("search output wrong: %s", out)
	}
}
