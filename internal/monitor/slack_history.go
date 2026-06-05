package monitor

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
)

// SlackHistory fetches a conversation's recent top-level messages for the
// steerer backfill. oldest is a Slack ts lower bound (exclusive).
type SlackHistory interface {
	History(ctx context.Context, channelID, oldest string, limit int) ([]SlackMessage, error)
}

// SlackIMLister enumerates the operator's open DM (im) channels. User token
// only — the bot isn't a party to the operator's DMs.
type SlackIMLister interface {
	ListIMs(ctx context.Context) ([]string, error)
}

// slackHistoryFn hits conversations.history; swapped in tests.
var slackHistoryFn = func(ctx context.Context, token, channelID, oldest string, limit int) ([]SlackMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	api := slack.New(token)
	resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: normalizeSlackChannelID(channelID),
		Oldest:    strings.TrimSpace(oldest),
		Inclusive: false,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SlackMessage, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		out = append(out, SlackMessage{
			User:     firstNonEmpty(m.User, m.Username),
			Text:     m.Text,
			TS:       m.Timestamp,
			ThreadTS: m.ThreadTimestamp,
			SubType:  m.SubType,
		})
	}
	return out, nil
}

type slackHistoryClient struct{ token string }

func (c slackHistoryClient) History(ctx context.Context, channelID, oldest string, limit int) ([]SlackMessage, error) {
	return slackHistoryFn(ctx, c.token, channelID, oldest, limit)
}

// NewSlackHistoryClient returns a bot-token history client (watched channels),
// or nil when no bot token is configured.
func NewSlackHistoryClient() SlackHistory {
	if strings.TrimSpace(SlackBotToken()) == "" {
		return nil
	}
	return slackHistoryClient{token: SlackBotToken()}
}

// NewSlackUserHistoryClient returns a user-token history client (DMs — the
// bot can't read the operator's DMs), or nil when no user token is configured.
func NewSlackUserHistoryClient() SlackHistory {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackHistoryClient{token: SlackUserToken()}
}

// slackIMListFn enumerates im channels; swapped in tests.
var slackIMListFn = func(ctx context.Context, token string) ([]string, error) {
	api := slack.New(token)
	var ids []string
	cursor := ""
	for {
		chans, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:  []string{"im"},
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range chans {
			ids = append(ids, c.ID)
		}
		if strings.TrimSpace(next) == "" {
			break
		}
		cursor = next
	}
	return ids, nil
}

type slackIMLister struct{ token string }

func (c slackIMLister) ListIMs(ctx context.Context) ([]string, error) {
	return slackIMListFn(ctx, c.token)
}

// NewSlackUserIMLister returns a user-token IM lister, or nil when no user
// token is configured.
func NewSlackUserIMLister() SlackIMLister {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackIMLister{token: SlackUserToken()}
}
