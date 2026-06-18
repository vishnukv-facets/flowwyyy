package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const OperatorQuestionWaitingPrefix = "operator question:"

// AskOperator posts a top-level message into the operator <-> flow-bot command
// DM and returns the Slack channel/thread anchor to track for the answer.
func AskOperator(ctx context.Context, text string) (channel, threadTS string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", errors.New("operator question is required")
	}
	w := NewSlackWriter()
	if w == nil || !w.Enabled {
		return "", "", errors.New("slack writes disabled; set FLOW_SLACK_WRITES_ENABLED=1")
	}
	token, peer, err := operatorAskTokenAndPeer()
	if err != nil {
		return "", "", err
	}
	w.Token = token

	var openResp struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := w.callJSON(ctx, "conversations.open", map[string]any{"users": peer}, &openResp); err != nil {
		return "", "", err
	}
	channel = strings.TrimSpace(openResp.Channel.ID)
	if channel == "" {
		return "", "", errors.New("slack conversations.open returned no channel")
	}

	var postResp struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
	}
	if err := w.callJSON(ctx, "chat.postMessage", map[string]any{"channel": channel, "text": text}, &postResp); err != nil {
		return "", "", err
	}
	threadTS = firstNonEmpty(postResp.TS, postResp.Message.TS)
	if threadTS == "" {
		return "", "", errors.New("slack chat.postMessage returned no ts")
	}
	if postResp.Channel != "" {
		channel = postResp.Channel
	}
	if ThreadKey(channel, threadTS) == "" {
		return "", "", fmt.Errorf("invalid Slack thread anchor channel=%q ts=%q", channel, threadTS)
	}
	return channel, threadTS, nil
}

func operatorAskTokenAndPeer() (token, peer string, err error) {
	token = strings.TrimSpace(slackToken())
	if token == "" {
		return "", "", errors.New("slack token required; set FLOW_SLACK_USER_TOKEN or FLOW_SLACK_WRITE_TOKEN")
	}
	if strings.HasPrefix(token, "xoxb-") {
		peer = operatorAskOperatorPeer()
		if peer == "" {
			return "", "", errors.New("operator Slack user ID unknown; set FLOW_SLACK_SELF_USER_IDS")
		}
		return token, peer, nil
	}
	peer = operatorAskBotPeer()
	if peer == "" {
		return "", "", errors.New("flow Slack bot user ID unknown; set FLOW_SLACK_SELF_BOT_USER_IDS")
	}
	return token, peer, nil
}

func operatorAskBotPeer() string {
	if peer := firstNonEmpty(SelfBotUserIDs()...); peer != "" {
		return peer
	}
	return selfBotUserID()
}

func operatorAskOperatorPeer() string {
	if peer := firstNonEmpty(SelfUserIDs()...); peer != "" {
		return peer
	}
	return operatorUserID()
}
