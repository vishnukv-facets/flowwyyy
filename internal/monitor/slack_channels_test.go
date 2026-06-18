package monitor

import (
	"context"
	"errors"
	"testing"
)

func TestListSlackChannels(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	old := slackConversationsFn
	slackConversationsFn = func(_ context.Context, token string) ([]SlackChannelInfo, error) {
		if token != "xoxb-test" {
			t.Fatalf("token = %q", token)
		}
		return []SlackChannelInfo{{ID: "C1", Name: "general", IsMember: true}}, nil
	}
	t.Cleanup(func() { slackConversationsFn = old })

	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("ListSlackChannels: %v", err)
	}
	if len(chans) != 1 || chans[0].ID != "C1" || chans[0].Name != "general" {
		t.Errorf("chans = %+v", chans)
	}
}

func TestListSlackChannelsMergesDMsAndDefaultsKind(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")

	oldCh, oldDM := slackConversationsFn, slackDMConversationsFn
	slackConversationsFn = func(_ context.Context, _ string) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{{ID: "C1", Name: "general", IsMember: true}}, nil // no Kind → defaults to channel
	}
	slackDMConversationsFn = func(_ context.Context) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{
			{ID: "D1", Name: "Anshul Sao", Kind: "im", IsPrivate: true},
			{ID: "G1", Name: "group DM", Kind: "mpim", IsPrivate: true},
		}, nil
	}
	t.Cleanup(func() { slackConversationsFn, slackDMConversationsFn = oldCh, oldDM })

	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("ListSlackChannels: %v", err)
	}
	byID := map[string]SlackChannelInfo{}
	for _, c := range chans {
		byID[c.ID] = c
	}
	if len(chans) != 3 {
		t.Fatalf("want 3 (channel + DM + group), got %d: %+v", len(chans), chans)
	}
	if byID["C1"].Kind != "channel" {
		t.Errorf("channel kind = %q, want defaulted to channel", byID["C1"].Kind)
	}
	if byID["D1"].Kind != "im" || byID["G1"].Kind != "mpim" {
		t.Errorf("DM/group kinds wrong: %+v %+v", byID["D1"], byID["G1"])
	}
}

func TestListSlackChannelsDMErrorDegradesToChannels(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")

	oldCh, oldDM := slackConversationsFn, slackDMConversationsFn
	slackConversationsFn = func(_ context.Context, _ string) ([]SlackChannelInfo, error) {
		return []SlackChannelInfo{{ID: "C1", Name: "general"}}, nil
	}
	slackDMConversationsFn = func(_ context.Context) ([]SlackChannelInfo, error) {
		return nil, errors.New("missing_scope")
	}
	t.Cleanup(func() { slackConversationsFn, slackDMConversationsFn = oldCh, oldDM })

	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("ListSlackChannels should degrade, not error: %v", err)
	}
	if len(chans) != 1 || chans[0].ID != "C1" {
		t.Errorf("want channels-only on DM error, got %+v", chans)
	}
}

func TestListSlackChannelsCachesSuccessfulList(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	old := slackConversationsFn
	calls := 0
	slackConversationsFn = func(_ context.Context, _ string) ([]SlackChannelInfo, error) {
		calls++
		if calls == 1 {
			return []SlackChannelInfo{
				{ID: "C1", Name: "general", IsMember: true},
				{ID: "C2", Name: "engineering", IsPrivate: true, IsMember: true},
			}, nil
		}
		return nil, errors.New("slack rate limit exceeded, retry after 30s")
	}
	t.Cleanup(func() { slackConversationsFn = old })

	first, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("first ListSlackChannels: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first channels = %+v", first)
	}

	second, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("rate-limited ListSlackChannels should use cache, got %v", err)
	}
	if len(second) != 2 || second[0].Name != "general" || second[1].Name != "engineering" {
		t.Fatalf("cached channels = %+v", second)
	}
}

func TestListSlackChannelsReturnsErrorWithoutCache(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	old := slackConversationsFn
	slackConversationsFn = func(_ context.Context, _ string) ([]SlackChannelInfo, error) {
		return nil, errors.New("slack rate limit exceeded, retry after 30s")
	}
	t.Cleanup(func() { slackConversationsFn = old })

	_, err := ListSlackChannels(context.Background())
	if err == nil {
		t.Fatal("ListSlackChannels error = nil, want Slack error when no cache exists")
	}
}

func TestListSlackChannelsNoToken(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("FLOW_SLACK_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("no-token should be a graceful empty, got err %v", err)
	}
	if len(chans) != 0 {
		t.Errorf("no token → empty, got %d", len(chans))
	}
}
