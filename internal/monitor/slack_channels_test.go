package monitor

import (
	"context"
	"errors"
	"testing"
)

func TestListSlackChannels(t *testing.T) {
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
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

func TestListSlackChannelsNoToken(t *testing.T) {
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
	_ = errors.New // keep import if unused elsewhere
}
