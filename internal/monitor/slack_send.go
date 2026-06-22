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

// sectionTextLimit is Slack's hard cap on a section block's text (3000 chars).
// Bodies longer than this can't ride in a section block, so they fall back to a
// plain-text post with the footer appended (editable, but Slack rejects the
// alternative outright). Headroom below 3000 for safety.
const sectionTextLimit = 2900

// slackPostOptions builds the chat.postMessage / scheduleMessage options. When a
// footer is present (and the body fits a section block), the footer renders as a
// non-editable Block Kit CONTEXT block BELOW the message — NOT appended to the
// body — so neither the recipient nor the sender can edit or delete the
// attribution from the message text (mirroring Slack's native "Sent using @app").
// MsgOptionText still carries the raw body as the notification/fallback text.
// A footer-less send (command DM, or footer disabled) posts plain text exactly as
// before, and an over-limit body falls back to the legacy text-append.
func slackPostOptions(text, footer string, asUser bool, threadTS string) []slack.MsgOption {
	footer = strings.TrimSpace(footer)
	var opts []slack.MsgOption
	if footer != "" && len([]rune(text)) <= sectionTextLimit {
		opts = append(opts,
			slack.MsgOptionText(text, false), // notification + fallback body
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
				slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, footer, false, false)),
			),
		)
	} else {
		opts = append(opts, slack.MsgOptionText(appendFooter(text, footer), false))
	}
	opts = append(opts, slack.MsgOptionAsUser(asUser))
	if threadTS = strings.TrimSpace(threadTS); threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	return opts
}

// sendAsBotFn performs the actual post; a package var so tests don't hit Slack.
// text is the RAW body; the attribution footer is resolved here and rendered as a
// non-editable context block (see slackPostOptions), never appended to the body.
var sendAsBotFn = func(channel, threadTS, text, identity string) error {
	token, asUser := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	_, _, err := slack.New(token).PostMessage(channel, slackPostOptions(text, footerForChannel(channel), asUser, threadTS)...)
	return err
}

var scheduleAsBotFn = func(channel, threadTS, text, identity string, postAt int64) (string, error) {
	token, asUser := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	_, scheduledMessageID, err := slack.New(token).ScheduleMessage(channel, strconv.FormatInt(postAt, 10), slackPostOptions(text, footerForChannel(channel), asUser, threadTS)...)
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
	return sendAsBotFn(channel, threadTS, text, identity)
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
	return scheduleAsBotFn(channel, threadTS, text, identity, postAt)
}

// reactAsBotFn performs the actual reactions.add; a package var so tests don't
// hit Slack. Idempotent: Slack's "already_reacted" is treated as success.
var reactAsBotFn = func(channel, ts, emoji, identity string) error {
	token, _ := resolveSendIdentity(channel, identity)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no slack token configured (FLOW_SLACK_TOKEN / user token)")
	}
	err := slack.New(token).AddReaction(emoji, slack.NewRefToMessage(channel, ts))
	if err != nil && strings.Contains(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

// ReactAsThread adds an emoji reaction to a Slack message (channel + ts), using
// the same identity model as SendAsThread ("bot"|"user"|"" → global). Reacting
// as the operator's user token is what lets the ack land even when the bot is
// not a member of the channel (the common Slack-Connect / colleague-thread case).
// emoji is the Slack short name with or without surrounding colons (e.g. "+1" or
// ":+1:"). Idempotent. Carries no footer — a reaction has no body. Gated by
// FLOW_SLACK_WRITES_ENABLED.
func ReactAsThread(channel, ts, emoji, identity string) error {
	if !slackWritesEnabled() {
		return fmt.Errorf("slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)")
	}
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("channel is required")
	}
	if strings.TrimSpace(ts) == "" {
		return fmt.Errorf("message timestamp (ts) is required")
	}
	emoji = strings.Trim(strings.TrimSpace(emoji), ":")
	if emoji == "" {
		return fmt.Errorf("emoji is required")
	}
	return reactAsBotFn(channel, ts, emoji, identity)
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
