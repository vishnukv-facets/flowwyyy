package monitor

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
)

// SlackChannelInfo is the compact channel shape used by the channel picker.
type SlackChannelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
	IsMember  bool   `json:"is_member"`
}

// slackConversationsFn is the mockable seam that hits conversations.list.
var slackConversationsFn = func(ctx context.Context, token string) ([]SlackChannelInfo, error) {
	api := slack.New(token)
	var out []SlackChannelInfo
	cursor := ""
	for {
		channels, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range channels {
			out = append(out, SlackChannelInfo{
				ID:        c.ID,
				Name:      c.Name,
				IsPrivate: c.IsPrivate,
				IsMember:  c.IsMember,
			})
		}
		if strings.TrimSpace(next) == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// ListSlackChannels returns the channels visible to the configured bot token.
// When no token is configured it returns an empty list (not an error) so the
// UI can render a "configure Slack" empty state gracefully.
func ListSlackChannels(ctx context.Context) ([]SlackChannelInfo, error) {
	token := SlackBotToken()
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	return slackConversationsFn(ctx, token)
}
