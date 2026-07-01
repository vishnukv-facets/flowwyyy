package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func stubUsersList(t *testing.T, users []slack.User, err error) {
	t.Helper()
	orig := slackUsersListFn
	t.Cleanup(func() { slackUsersListFn = orig })
	slackUsersListFn = func(context.Context, string) ([]slack.User, error) {
		return users, err
	}
}

func sampleReadUsers() []slack.User {
	u1 := slack.User{ID: "U1", Name: "vishnu.kv", RealName: "Vishnu KV", IsBot: false}
	u1.Profile.DisplayName = "vishnu"
	u1.Profile.Email = "vishnu.kv@facets.cloud"
	u1.Profile.Title = "Engineering"
	u2 := slack.User{ID: "U2", Name: "alice", RealName: "Alice Smith", IsBot: true}
	u2.Profile.DisplayName = "alice"
	return []slack.User{u1, u2}
}

func TestSearchUsersFiltersByQuery(t *testing.T) {
	stubUsersList(t, sampleReadUsers(), nil)

	got, err := SearchUsers(context.Background(), "xoxp-x", "vishnu")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "U1" {
		t.Fatalf("got %+v, want one match U1", got)
	}
	if got[0].DisplayName != "vishnu" || got[0].RealName != "Vishnu KV" || got[0].Email != "vishnu.kv@facets.cloud" {
		t.Fatalf("mapping wrong: %+v", got[0])
	}
}

func TestSearchUsersEmptyQueryReturnsAll(t *testing.T) {
	stubUsersList(t, sampleReadUsers(), nil)

	got, err := SearchUsers(context.Background(), "xoxp-x", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2", len(got))
	}
}

func TestSearchUsersPropagatesError(t *testing.T) {
	want := errors.New("slack down")
	stubUsersList(t, nil, want)

	if _, err := SearchUsers(context.Background(), "xoxp-x", "alice"); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestUserInfoAndLookupByEmailMapUser(t *testing.T) {
	oldInfo, oldEmail := slackUserDetailsFn, slackUserByEmailFn
	t.Cleanup(func() {
		slackUserDetailsFn = oldInfo
		slackUserByEmailFn = oldEmail
	})
	slackUserDetailsFn = func(context.Context, string, string) (*slack.User, error) {
		u := sampleReadUsers()[0]
		return &u, nil
	}
	slackUserByEmailFn = func(context.Context, string, string) (*slack.User, error) {
		u := sampleReadUsers()[1]
		return &u, nil
	}

	got, err := UserInfo(context.Background(), "xoxp-x", "U1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "U1" || got.Title != "Engineering" {
		t.Fatalf("UserInfo = %+v, want mapped U1", got)
	}
	got, err = LookupUserByEmail(context.Background(), "xoxp-x", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "U2" || !got.IsBot {
		t.Fatalf("LookupUserByEmail = %+v, want mapped bot U2", got)
	}
}

func TestResolveUserNamesUsesDirectoryAndInfoFallback(t *testing.T) {
	oldDir, oldInfo := slackUserDirectoryFn, slackUserInfoFn
	t.Cleanup(func() {
		slackUserDirectoryFn = oldDir
		slackUserInfoFn = oldInfo
	})
	slackUserDirectoryFn = func(context.Context, string) (map[string]string, error) {
		return map[string]string{"U1": "Vishnu"}, nil
	}
	slackUserInfoFn = func(context.Context, string, string) (string, error) {
		return "Alice", nil
	}

	got := ResolveUserNames(context.Background(), "xoxp-x", []string{"U1", "U2"})
	if got["U1"] != "Vishnu" || got["U2"] != "Alice" {
		t.Fatalf("names = %+v, want directory + fallback", got)
	}
}

func TestListSlackChannelsWithTokenIncludesDMsWhenRequested(t *testing.T) {
	oldCh, oldDM := slackConversationsFn, slackDMConversationsFn
	t.Cleanup(func() {
		slackConversationsFn = oldCh
		slackDMConversationsFn = oldDM
	})
	slackConversationsFn = func(context.Context, string) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{{ID: "C1", Name: "general"}}, nil
	}
	slackDMConversationsFn = func(context.Context) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{{ID: "D1", Name: "alice", Kind: "im"}}, nil
	}

	got, err := ListSlackChannelsWithToken(context.Background(), "xoxp-x", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "channel" || got[1].Kind != "im" {
		t.Fatalf("channels = %+v, want channel + im", got)
	}
	got, err = ListSlackChannelsWithToken(context.Background(), "xoxb-x", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("bot-only channels = %+v, want no DMs", got)
	}
}

func TestResolveSlackChannelResolvesUserIDToDM(t *testing.T) {
	old := slackOpenIMFn
	t.Cleanup(func() { slackOpenIMFn = old })
	var gotUser string
	slackOpenIMFn = func(_ context.Context, _ string, userID string) (string, error) {
		gotUser = userID
		return "D777", nil
	}

	got, err := ResolveSlackChannel(context.Background(), "xoxp-x", "U03LK2CCE68", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "D777" || got.Kind != "im" {
		t.Fatalf("resolved = %+v, want D777 im channel", got)
	}
	if gotUser != "U03LK2CCE68" {
		t.Fatalf("opened DM for %q, want U03LK2CCE68", gotUser)
	}
}

func TestResolveSlackChannelByNameAndID(t *testing.T) {
	old := slackConversationsFn
	t.Cleanup(func() { slackConversationsFn = old })
	slackConversationsFn = func(context.Context, string) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{{ID: "C1", Name: "general"}}, nil
	}

	got, err := ResolveSlackChannel(context.Background(), "xoxb-x", "#general", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "C1" {
		t.Fatalf("resolved = %+v, want C1", got)
	}
	got, err = ResolveSlackChannel(context.Background(), "xoxb-x", "C123", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "C123" {
		t.Fatalf("id passthrough = %+v, want C123", got)
	}
}

func TestConversationHistoryNormalizesOptions(t *testing.T) {
	old := slackConversationHistoryFn
	t.Cleanup(func() { slackConversationHistoryFn = old })
	var gotToken string
	var gotOpts SlackHistoryOptions
	slackConversationHistoryFn = func(_ context.Context, token string, opts SlackHistoryOptions) ([]SlackMessage, error) {
		gotToken, gotOpts = token, opts
		return []SlackMessage{{TS: "1.0", User: "U1", Text: "hi"}}, nil
	}

	got, err := ConversationHistory(context.Background(), "xoxp-x", SlackHistoryOptions{ChannelID: "slack:C1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "xoxp-x" || gotOpts.ChannelID != "C1" || gotOpts.Limit != 50 {
		t.Fatalf("token/options = %q/%+v", gotToken, gotOpts)
	}
	if len(got) != 1 || got[0].TS != "1.0" {
		t.Fatalf("messages = %+v", got)
	}
}

func TestConversationHistoryRateLimitNamesRetryAt(t *testing.T) {
	old := slackConversationHistoryFn
	t.Cleanup(func() { slackConversationHistoryFn = old })
	slackConversationHistoryFn = func(_ context.Context, _ string, _ SlackHistoryOptions) ([]SlackMessage, error) {
		return nil, &slack.RateLimitedError{RetryAfter: 10 * time.Second}
	}

	_, err := ConversationHistory(context.Background(), "xoxp-x", SlackHistoryOptions{ChannelID: "C1"})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if !strings.Contains(err.Error(), "retry at ") || !strings.Contains(err.Error(), "10s") {
		t.Fatalf("err = %q, want retry duration and absolute retry time", err.Error())
	}
}

func TestConversationRepliesRequiresThreadTS(t *testing.T) {
	if _, err := ConversationReplies(context.Background(), "xoxp-x", "C1", "", 10); err == nil {
		t.Fatal("blank thread ts should error")
	}
}

func TestConversationRepliesNormalizesInput(t *testing.T) {
	old := slackConversationRepliesFn
	t.Cleanup(func() { slackConversationRepliesFn = old })
	var gotChannel string
	var gotLimit int
	slackConversationRepliesFn = func(_ context.Context, _, channelID, threadTS string, limit int) ([]SlackMessage, error) {
		gotChannel, gotLimit = channelID, limit
		if threadTS != "123.000100" {
			t.Fatalf("thread ts = %q", threadTS)
		}
		return []SlackMessage{{TS: "123.000100", Text: "root"}}, nil
	}

	got, err := ConversationReplies(context.Background(), "xoxp-x", "slack:C1", "123.000100", 0)
	if err != nil {
		t.Fatal(err)
	}
	if gotChannel != "C1" || gotLimit != 100 || len(got) != 1 {
		t.Fatalf("channel/limit/messages = %q/%d/%+v", gotChannel, gotLimit, got)
	}
}

func TestConversationMembersResolvesNames(t *testing.T) {
	oldMembers, oldDir, oldInfo := slackConversationMembersFn, slackUserDirectoryFn, slackUserInfoFn
	t.Cleanup(func() {
		slackConversationMembersFn = oldMembers
		slackUserDirectoryFn = oldDir
		slackUserInfoFn = oldInfo
	})
	slackConversationMembersFn = func(_ context.Context, token, channelID string, limit int) ([]string, error) {
		if token != "xoxp-x" || channelID != "C1" || limit != 200 {
			t.Fatalf("members args = %q/%q/%d", token, channelID, limit)
		}
		return []string{"U1", "U2"}, nil
	}
	slackUserDirectoryFn = func(context.Context, string) (map[string]string, error) {
		return map[string]string{"U1": "Vishnu"}, nil
	}
	slackUserInfoFn = func(context.Context, string, string) (string, error) {
		return "Alice", nil
	}

	got, err := ConversationMembers(context.Background(), "xoxp-x", "slack:C1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DisplayName != "Vishnu" || got[1].DisplayName != "Alice" {
		t.Fatalf("members = %+v", got)
	}
}

func TestMessageReactionsMapsReactions(t *testing.T) {
	old := slackReactionsFn
	t.Cleanup(func() { slackReactionsFn = old })
	slackReactionsFn = func(_ context.Context, token, channelID, ts string) ([]slack.ItemReaction, error) {
		if token != "xoxp-x" || channelID != "C1" || ts != "123.000100" {
			t.Fatalf("reaction args = %q/%q/%q", token, channelID, ts)
		}
		return []slack.ItemReaction{{Name: "eyes", Count: 2, Users: []string{"U1", "U2"}}}, nil
	}

	got, err := MessageReactions(context.Background(), "xoxp-x", "slack:C1", "123.000100")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "eyes" || got[0].Count != 2 {
		t.Fatalf("reactions = %+v", got)
	}
}

func TestSearchMessagesMapsMatches(t *testing.T) {
	old := slackSearchMessagesFn
	t.Cleanup(func() { slackSearchMessagesFn = old })
	var gotLimit int
	var gotSort string
	slackSearchMessagesFn = func(_ context.Context, token, query, sort string, limit int) ([]slack.SearchMessage, error) {
		if token != "xoxp-x" || query != "deploy" {
			t.Fatalf("search args = %q/%q", token, query)
		}
		gotLimit, gotSort = limit, sort
		return []slack.SearchMessage{{
			Channel:   slack.CtxChannel{ID: "C1", Name: "general"},
			User:      "U1",
			Username:  "vishnu",
			Timestamp: "123.000100",
			Text:      "deploy done",
			Permalink: "https://example.slack.com/archives/C1/p123",
		}}, nil
	}

	got, err := SearchMessages(context.Background(), "xoxp-x", "deploy", "timestamp", 3)
	if err != nil {
		t.Fatal(err)
	}
	if gotLimit != 3 || gotSort != "timestamp" || len(got) != 1 || got[0].ChannelID != "C1" || got[0].Text != "deploy done" {
		t.Fatalf("search = limit %d sort %q matches %+v", gotLimit, gotSort, got)
	}
}

func TestSearchMessagesRequiresQuery(t *testing.T) {
	if _, err := SearchMessages(context.Background(), "xoxp-x", "", "score", 20); err == nil {
		t.Fatal("blank query should error")
	}
}
