package monitor

import (
	"encoding/json"
	"os"
	"strings"

	"flow/internal/flowdb"

	"github.com/slack-go/slack/slackevents"
)

func SlackAppToken() string {
	return firstNonEmpty(
		os.Getenv("FLOW_SLACK_APP_TOKEN"),
		os.Getenv("SLACK_APP_TOKEN"),
	)
}

func SlackBotToken() string {
	return firstNonEmpty(
		os.Getenv("SLACK_BOT_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_USER_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	)
}

func SlackUserToken() string {
	return firstNonEmpty(
		os.Getenv("FLOW_SLACK_USER_TOKEN"),
		os.Getenv("SLACK_USER_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	)
}

func SlackMentionUserIDs() []string {
	raw := firstNonEmpty(
		os.Getenv("FLOW_SLACK_MENTION_USER_IDS"),
		os.Getenv("FLOW_SLACK_MENTION_USER_ID"),
		os.Getenv("FLOW_SLACK_USER_ID"),
		os.Getenv("SLACK_USER_ID"),
	)
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func SlackSocketModeEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("FLOW_SLACK_SOCKET_MODE"))
	if raw != "" {
		return envBoolDefault("FLOW_SLACK_SOCKET_MODE", false)
	}
	return SlackAppToken() != ""
}

func SlackEventsAPIEventInputs(event slackevents.EventsAPIEvent) []flowdb.MonitorEventInput {
	return SlackEventsAPIEventInputsForUsers(event, SlackMentionUserIDs())
}

func SlackEventsAPIEventInputsForUsers(event slackevents.EventsAPIEvent, mentionUserIDs []string) []flowdb.MonitorEventInput {
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if input, ok := slackAppMentionEventInput(event, ev); ok {
			return []flowdb.MonitorEventInput{input}
		}
	case slackevents.AppMentionEvent:
		if input, ok := slackAppMentionEventInput(event, &ev); ok {
			return []flowdb.MonitorEventInput{input}
		}
	case *slackevents.MessageEvent:
		if input, ok := slackMessageEventInput(event, ev, mentionUserIDs); ok {
			return []flowdb.MonitorEventInput{input}
		}
	case slackevents.MessageEvent:
		if input, ok := slackMessageEventInput(event, &ev, mentionUserIDs); ok {
			return []flowdb.MonitorEventInput{input}
		}
	}
	return nil
}

func slackAppMentionEventInput(event slackevents.EventsAPIEvent, ev *slackevents.AppMentionEvent) (flowdb.MonitorEventInput, bool) {
	if ev == nil {
		return flowdb.MonitorEventInput{}, false
	}
	channel := strings.TrimSpace(ev.Channel)
	ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
	text := strings.TrimSpace(ev.Text)
	if channel == "" || ts == "" || text == "" {
		return flowdb.MonitorEventInput{}, false
	}
	return flowdb.MonitorEventInput{
		Source:   "slack",
		Kind:     "mention",
		SourceID: channel + ":" + ts,
		Title:    slackEventTitle("mention", ev.User, channel),
		Body:     text,
		Severity: "medium",
		RawJSON:  slackSocketRawJSON(event, ev),
	}, true
}

func slackMessageEventInput(event slackevents.EventsAPIEvent, ev *slackevents.MessageEvent, mentionUserIDs []string) (flowdb.MonitorEventInput, bool) {
	if ev == nil {
		return flowdb.MonitorEventInput{}, false
	}
	if ev.SubType != "" && ev.SubType != "bot_message" {
		return flowdb.MonitorEventInput{}, false
	}
	channel := strings.TrimSpace(ev.Channel)
	ts := firstNonEmpty(ev.TimeStamp, ev.EventTimeStamp)
	text := strings.TrimSpace(ev.Text)
	if text == "" && ev.Message != nil {
		text = strings.TrimSpace(ev.Message.Text)
	}
	if channel == "" || ts == "" || text == "" {
		return flowdb.MonitorEventInput{}, false
	}
	kind := "channel_message"
	if ev.IsIM() || strings.EqualFold(ev.ChannelType, slackevents.ChannelTypeIM) {
		kind = "dm"
	} else if ev.IsMpIM() || strings.EqualFold(ev.ChannelType, slackevents.ChannelTypeMPIM) {
		kind = "dm"
	} else if slackTextMentionsAnyUser(text, mentionUserIDs) {
		kind = "personal_mention"
	} else if !slackSocketChannelEnabled(channel) {
		return flowdb.MonitorEventInput{}, false
	}
	return flowdb.MonitorEventInput{
		Source:   "slack",
		Kind:     kind,
		SourceID: channel + ":" + ts,
		Title:    slackEventTitle(kind, ev.User, channel),
		Body:     text,
		Severity: "medium",
		RawJSON:  slackSocketRawJSON(event, ev),
	}, true
}

func slackEventTitle(kind, userID, channelID string) string {
	switch kind {
	case "mention":
		return "Slack app mention from " + firstNonEmpty(userID, "unknown user") + " in " + channelID
	case "personal_mention":
		return "Slack mention of you from " + firstNonEmpty(userID, "unknown user") + " in " + channelID
	case "dm":
		return "Slack DM from " + firstNonEmpty(userID, channelID)
	default:
		return "Slack " + strings.ReplaceAll(kind, "_", " ") + " from " + firstNonEmpty(userID, "unknown user") + " in " + channelID
	}
}

func slackTextMentionsAnyUser(text string, userIDs []string) bool {
	if strings.TrimSpace(text) == "" || len(userIDs) == 0 {
		return false
	}
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		if strings.Contains(text, "<@"+userID+">") || strings.Contains(text, "<@"+userID+"|") {
			return true
		}
	}
	return false
}

func slackSocketChannelEnabled(channelID string) bool {
	if envBool("FLOW_SLACK_INCLUDE_CHANNEL_MESSAGES") {
		return true
	}
	allowlist := slackChannelAllowlist()
	return allowlist[channelID]
}

func slackSocketRawJSON(event slackevents.EventsAPIEvent, inner any) string {
	raw, _ := json.Marshal(map[string]any{
		"team_id":    event.TeamID,
		"api_app_id": event.APIAppID,
		"event":      inner,
	})
	return string(raw)
}
