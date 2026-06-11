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

// slackHistoryPageSize is how many messages we ask for per conversations.history
// call. Slack allows up to 1000 but recommends <=200; we page until we reach the
// caller's total `limit` or the channel has no more messages since `oldest`.
const slackHistoryPageSize = 200

// slackHistoryFn hits conversations.history; swapped in tests. It pages through
// the cursor until it has gathered `limit` messages (the total cap, NOT a
// per-page count) or Slack reports no more since `oldest`. This lets a catch-up
// after a long downtime (e.g. the laptop slept for days) recover the whole gap
// instead of only the newest page. Mirrors slackIMListFn's cursor loop.
var slackHistoryFn = func(ctx context.Context, token, channelID, oldest string, limit int) ([]SlackMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	api := slack.New(token)
	oldest = strings.TrimSpace(oldest)
	channelID = normalizeSlackChannelID(channelID)
	out := make([]SlackMessage, 0, limit)
	cursor := ""
	for len(out) < limit {
		pageLimit := min(limit-len(out), slackHistoryPageSize)
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Oldest:    oldest,
			Inclusive: false,
			Limit:     pageLimit,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range resp.Messages {
			files := slackFilesFromAPIWithContent(ctx, api, m.Files)
			out = append(out, SlackMessage{
				User:        firstNonEmpty(m.User, m.Username),
				Text:        strings.TrimSpace(m.Text),
				TS:          m.Timestamp,
				ThreadTS:    m.ThreadTimestamp,
				SubType:     m.SubType,
				Files:       files,
				ReplyCount:  m.ReplyCount,
				LatestReply: m.LatestReply,
			})
		}
		cursor = strings.TrimSpace(resp.ResponseMetaData.NextCursor)
		if cursor == "" || !resp.HasMore {
			break
		}
	}
	return out, nil
}

type slackHistoryClient struct{ tokenFn func() string }

func (c slackHistoryClient) History(ctx context.Context, channelID, oldest string, limit int) ([]SlackMessage, error) {
	token := callSlackTokenFn(c.tokenFn)
	if token == "" {
		return nil, ErrNoToken
	}
	return slackHistoryFn(ctx, token, channelID, oldest, limit)
}

// NewSlackHistoryClient returns a bot-token history client (watched channels),
// or nil when no bot token is configured. The token is resolved per call so a
// rotated token takes effect without reconstructing the client.
func NewSlackHistoryClient() SlackHistory {
	if strings.TrimSpace(SlackBotToken()) == "" {
		return nil
	}
	return slackHistoryClient{tokenFn: SlackBotToken}
}

// NewSlackUserHistoryClient returns a user-token history client (DMs — the
// bot can't read the operator's DMs), or nil when no user token is configured.
func NewSlackUserHistoryClient() SlackHistory {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackHistoryClient{tokenFn: SlackUserToken}
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

type slackIMLister struct{ tokenFn func() string }

func (c slackIMLister) ListIMs(ctx context.Context) ([]string, error) {
	token := callSlackTokenFn(c.tokenFn)
	if token == "" {
		return nil, ErrNoToken
	}
	return slackIMListFn(ctx, token)
}

// NewSlackUserIMLister returns a user-token IM lister, or nil when no user
// token is configured. The token is resolved per call so a rotated token takes
// effect without reconstructing the lister.
func NewSlackUserIMLister() SlackIMLister {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil
	}
	return slackIMLister{tokenFn: SlackUserToken}
}
