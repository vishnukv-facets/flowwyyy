package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// resolveSendIdentity picks the token + as_user flag for an outbound message to
// `channel`. The operator↔bot command IM ALWAYS uses the bot identity: posting
// flow's own replies there as the operator would loop (the reply re-enters as a
// command) and defeat self-echo detection (which keys on the bot's user id).
//
// `override` lets a specific caller force an identity regardless of the global
// FLOW_SLACK_SEND_AS setting — "bot" or "user". This is what `flow slack send
// --as bot` uses so automation (e.g. scheduled playbooks) posts as the bot,
// which carries chat:write, while interactive reply flows still default to the
// operator's identity. An empty override honors the global setting: "user"
// posts as the operator (user token, as_user=true) when a user token exists,
// otherwise it falls back to the bot.
func resolveSendIdentity(channel, override string) (token string, asUser bool) {
	// Command IM is bot-only, no matter what the caller or config asks for.
	if botIsMemberOfIM(channel) {
		return SlackBotToken(), false
	}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "bot":
		return SlackBotToken(), false
	case "user":
		if ut := strings.TrimSpace(SlackUserToken()); ut != "" {
			return ut, true
		}
		return SlackBotToken(), false
	}
	// No override: honor the global FLOW_SLACK_SEND_AS setting.
	if SlackSendIdentity() == "user" {
		if ut := strings.TrimSpace(SlackUserToken()); ut != "" {
			return ut, true
		}
	}
	return SlackBotToken(), false
}

// sendAsBotFn performs the actual post; a package var so tests don't hit Slack.
var sendAsBotFn = func(channel, threadTS, text, identity string) error {
	token, asUser := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	api := slack.New(token)
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionAsUser(asUser),
	}
	if threadTS = strings.TrimSpace(threadTS); threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := api.PostMessage(channel, opts...)
	return err
}

var scheduleAsBotFn = func(channel, threadTS, text, identity string, postAt int64) (string, error) {
	token, asUser := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	api := slack.New(token)
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionAsUser(asUser),
	}
	if threadTS = strings.TrimSpace(threadTS); threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, scheduledMessageID, err := api.ScheduleMessage(channel, strconv.FormatInt(postAt, 10), opts...)
	return scheduledMessageID, err
}

// SendAs posts text to a Slack channel/DM, forcing the send identity when
// `identity` is "bot" or "user" and otherwise honoring the global
// FLOW_SLACK_SEND_AS setting. Gated by FLOW_SLACK_WRITES_ENABLED.
func SendAs(channel, text, identity string) error {
	return SendAsThread(channel, "", text, identity)
}

// SendAsThread posts text to a Slack channel/DM or thread. An empty threadTS
// preserves the root-channel behavior of SendAs.
func SendAsThread(channel, threadTS, text, identity string) error {
	if !slackWritesEnabled() {
		return fmt.Errorf("slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)")
	}
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("channel is required")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("text is required")
	}
	return sendAsBotFn(channel, threadTS, withSlackFooterForChannel(channel, text), identity)
}

func ScheduleAsThread(channel, threadTS, text, identity string, postAt int64) (string, error) {
	if !slackWritesEnabled() {
		return "", fmt.Errorf("slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)")
	}
	if strings.TrimSpace(channel) == "" {
		return "", fmt.Errorf("channel is required")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("text is required")
	}
	if postAt <= 0 {
		return "", fmt.Errorf("post_at is required")
	}
	return scheduleAsBotFn(channel, threadTS, withSlackFooterForChannel(channel, text), identity, postAt)
}

// SendAsBot posts under flow's configured send identity (FLOW_SLACK_SEND_AS —
// the flow bot by default, or the operator's user identity for replies into
// channels/colleague threads). The operator↔bot command DM is always posted as
// the bot regardless of the setting. Gated by FLOW_SLACK_WRITES_ENABLED.
func SendAsBot(channel, text string) error {
	return SendAs(channel, text, "")
}

// uploadFileFn performs the file upload; a package var so tests don't hit Slack.
// It uses Slack's external-upload flow (getUploadURLExternal → upload bytes →
// completeUploadExternal), since the legacy files.upload is being sunset.
// Uploads require the files:write scope, which the manifest grants to the BOT
// token — pass identity "bot" (e.g. `flow slack send --as bot --file ...`).
var uploadFileFn = func(channel, threadTS, comment, filePath, identity string) error {
	token, _ := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", filePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a file", filePath)
	}
	api := slack.New(token)
	ctx := context.Background()
	name := filepath.Base(filePath)
	up, err := api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{
		FileName: name,
		FileSize: int(info.Size()),
	})
	if err != nil {
		return fmt.Errorf("get upload url: %w", err)
	}
	if err := api.UploadToURL(ctx, slack.UploadToURLParameters{
		UploadURL: up.UploadURL,
		File:      filePath,
		Filename:  name,
	}); err != nil {
		return fmt.Errorf("upload file bytes: %w", err)
	}
	if _, err := api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{
		Files:           []slack.FileSummary{{ID: up.FileID, Title: name}},
		Channel:         channel,
		InitialComment:  comment,
		ThreadTimestamp: strings.TrimSpace(threadTS),
	}); err != nil {
		return fmt.Errorf("complete upload: %w", err)
	}
	return nil
}

// SendFileAs uploads a local file as an attachment to a Slack channel/DM, with
// an optional initial comment. Identity selection matches SendAs ("bot"|"user"|
// ""), but note only the bot token carries files:write — automation should pass
// "bot". Gated by FLOW_SLACK_WRITES_ENABLED.
func SendFileAs(channel, comment, filePath, identity string) error {
	return SendFileAsThread(channel, "", comment, filePath, identity)
}

// SendFileAsThread uploads a local file as an attachment to a Slack channel/DM
// or thread. An empty threadTS preserves the root-channel behavior of
// SendFileAs.
func SendFileAsThread(channel, threadTS, comment, filePath, identity string) error {
	if !slackWritesEnabled() {
		return fmt.Errorf("slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)")
	}
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("channel is required")
	}
	if strings.TrimSpace(filePath) == "" {
		return fmt.Errorf("file is required")
	}
	return uploadFileFn(channel, threadTS, comment, filePath, identity)
}
