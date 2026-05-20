package server

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
	flowmonitor "flow/internal/monitor"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type slackEventContext struct {
	UserID      string
	BotID       string
	ChannelID   string
	ChannelType string
	TS          string
	ThreadTS    string
	Text        string
}

var (
	slackMentionRE = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]+)?>`)

	slackNameCacheMu   sync.Mutex
	slackUserNames     = map[string]string{}
	slackChannelNames  = map[string]string{}
	slackAuthedUserIDs = map[string]string{}

	slackResolveUserName    = resolveSlackUserName
	slackResolveChannelName = resolveSlackChannelName
)

func (s *Server) slackMentionUserIDs(ctx context.Context) []string {
	if ids := flowmonitor.SlackMentionUserIDs(); len(ids) > 0 {
		return ids
	}
	if id := resolveSlackAuthedUserID(ctx, slackExplicitUserToken()); id != "" {
		return []string{id}
	}
	return nil
}

func (s *Server) enrichSlackEventInputs(ctx context.Context, event slackevents.EventsAPIEvent, inputs []flowdb.MonitorEventInput) []flowdb.MonitorEventInput {
	if len(inputs) == 0 {
		return inputs
	}
	meta, ok := slackEventContextFromEvent(event)
	if !ok {
		return inputs
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	userName := slackDisplayUser(resolveCtx, meta.UserID, meta.BotID)
	channelName := slackDisplayChannel(resolveCtx, meta.ChannelID, meta.ChannelType, meta.UserID)
	body := replaceSlackUserMentions(resolveCtx, meta.Text)

	out := make([]flowdb.MonitorEventInput, len(inputs))
	copy(out, inputs)
	for i := range out {
		if body != "" {
			out[i].Body = body
		}
		switch out[i].Kind {
		case "mention":
			out[i].Title = fmt.Sprintf("Slack app mention from %s in %s", userName, channelName)
		case "personal_mention":
			out[i].Title = fmt.Sprintf("Slack mention of you from %s in %s", userName, channelName)
		case "dm":
			if strings.EqualFold(meta.ChannelType, slackevents.ChannelTypeMPIM) {
				out[i].Title = fmt.Sprintf("Slack group DM from %s in %s", userName, channelName)
			} else {
				out[i].Title = fmt.Sprintf("Slack DM from %s", userName)
			}
		case "channel_message":
			out[i].Title = fmt.Sprintf("Slack channel message from %s in %s", userName, channelName)
		}
	}
	return out
}

func slackEventContextFromEvent(event slackevents.EventsAPIEvent) (slackEventContext, bool) {
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if ev == nil {
			return slackEventContext{}, false
		}
		ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
		return slackEventContext{UserID: ev.User, BotID: ev.BotID, ChannelID: ev.Channel, ChannelType: slackevents.ChannelTypeChannel, TS: ts, ThreadTS: ev.ThreadTimeStamp, Text: ev.Text}, true
	case slackevents.AppMentionEvent:
		ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
		return slackEventContext{UserID: ev.User, BotID: ev.BotID, ChannelID: ev.Channel, ChannelType: slackevents.ChannelTypeChannel, TS: ts, ThreadTS: ev.ThreadTimeStamp, Text: ev.Text}, true
	case *slackevents.MessageEvent:
		if ev == nil {
			return slackEventContext{}, false
		}
		text := strings.TrimSpace(ev.Text)
		if text == "" && ev.Message != nil {
			text = strings.TrimSpace(ev.Message.Text)
		}
		ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
		threadTS := ev.ThreadTimeStamp
		if threadTS == "" && ev.Message != nil {
			threadTS = ev.Message.ThreadTimestamp
		}
		return slackEventContext{UserID: ev.User, BotID: ev.BotID, ChannelID: ev.Channel, ChannelType: ev.ChannelType, TS: ts, ThreadTS: threadTS, Text: text}, true
	case slackevents.MessageEvent:
		text := strings.TrimSpace(ev.Text)
		if text == "" && ev.Message != nil {
			text = strings.TrimSpace(ev.Message.Text)
		}
		ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
		threadTS := ev.ThreadTimeStamp
		if threadTS == "" && ev.Message != nil {
			threadTS = ev.Message.ThreadTimestamp
		}
		return slackEventContext{UserID: ev.User, BotID: ev.BotID, ChannelID: ev.Channel, ChannelType: ev.ChannelType, TS: ts, ThreadTS: threadTS, Text: text}, true
	default:
		return slackEventContext{}, false
	}
}

func slackDisplayUser(ctx context.Context, userID, botID string) string {
	if userID != "" {
		if name := slackResolveUserName(ctx, userID); name != "" {
			return name
		}
		return userID
	}
	if botID != "" {
		return botID
	}
	return "unknown user"
}

func slackDisplayChannel(ctx context.Context, channelID, channelType, senderUserID string) string {
	if channelID == "" {
		return "unknown conversation"
	}
	if name := slackResolveChannelName(ctx, channelID); name != "" {
		return name
	}
	if strings.EqualFold(channelType, slackevents.ChannelTypeIM) {
		return "DM with " + slackDisplayUser(ctx, senderUserID, "")
	}
	return channelID
}

func replaceSlackUserMentions(ctx context.Context, text string) string {
	if text == "" {
		return text
	}
	return slackMentionRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := slackMentionRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		name := slackResolveUserName(ctx, parts[1])
		if name == "" {
			return match
		}
		return "@" + name
	})
}

func resolveSlackUserName(ctx context.Context, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	slackNameCacheMu.Lock()
	cached := slackUserNames[userID]
	slackNameCacheMu.Unlock()
	if cached != "" {
		return cached
	}
	for _, token := range slackLookupTokens() {
		user, err := slack.New(token).GetUserInfoContext(ctx, userID)
		if err != nil || user == nil {
			continue
		}
		name := firstNonEmpty(user.Profile.DisplayName, user.Profile.DisplayNameNormalized, user.RealName, user.Name, user.ID)
		if name == "" {
			continue
		}
		slackNameCacheMu.Lock()
		slackUserNames[userID] = name
		slackNameCacheMu.Unlock()
		return name
	}
	return ""
}

func resolveSlackChannelName(ctx context.Context, channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return ""
	}
	slackNameCacheMu.Lock()
	cached := slackChannelNames[channelID]
	slackNameCacheMu.Unlock()
	if cached != "" {
		return cached
	}
	for _, token := range slackLookupTokens() {
		channel, err := slack.New(token).GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
		if err != nil || channel == nil {
			continue
		}
		name := slackChannelLabel(ctx, channel)
		if name == "" {
			continue
		}
		slackNameCacheMu.Lock()
		slackChannelNames[channelID] = name
		slackNameCacheMu.Unlock()
		return name
	}
	return ""
}

func slackChannelLabel(ctx context.Context, channel *slack.Channel) string {
	if channel == nil {
		return ""
	}
	name := firstNonEmpty(channel.NameNormalized, channel.Name)
	if name != "" {
		return "#" + name
	}
	if channel.IsIM && channel.User != "" {
		return "DM with " + slackDisplayUser(ctx, channel.User, "")
	}
	return channel.ID
}

func slackLookupTokens() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, token := range []string{flowmonitor.SlackBotToken(), flowmonitor.SlackUserToken()} {
		token = strings.TrimSpace(token)
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func slackExplicitUserToken() string {
	for _, token := range []string{
		os.Getenv("FLOW_SLACK_USER_TOKEN"),
		os.Getenv("SLACK_USER_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	} {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "xoxb-") {
			continue
		}
		return token
	}
	return ""
}

func resolveSlackAuthedUserID(ctx context.Context, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	slackNameCacheMu.Lock()
	cached := slackAuthedUserIDs[token]
	slackNameCacheMu.Unlock()
	if cached != "" {
		return cached
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	auth, err := slack.New(token).AuthTestContext(resolveCtx)
	if err != nil || auth == nil || auth.UserID == "" || auth.BotID != "" {
		return ""
	}
	slackNameCacheMu.Lock()
	slackAuthedUserIDs[token] = auth.UserID
	slackNameCacheMu.Unlock()
	return auth.UserID
}
