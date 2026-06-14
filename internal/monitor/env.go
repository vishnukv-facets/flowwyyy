// Package monitor hosts flow's Slack integration: a Web API client
// (SlackWriter) for posting back to Slack, plus environment-derived
// configuration shared by the writer and (in future) the Socket Mode
// listener.
//
// Tokens come from environment variables with a deliberate precedence:
// explicit write tokens (FLOW_SLACK_WRITE_TOKEN, SLACK_WRITE_TOKEN) win
// when set so operators can grant a separate "post on my behalf" token
// without exposing the broader read-side token. Otherwise the bot or
// user token from Socket Mode setup is reused.
package monitor

import (
	"os"
	"strings"
)

// SlackBotToken resolves a bot/user token for read-side Slack API calls
// and as a fallback for writes. The Socket Mode listener uses this for
// any HTTP API call it needs to make alongside its WebSocket connection.
func SlackBotToken() string {
	return firstNonEmpty(
		os.Getenv("SLACK_BOT_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_USER_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	)
}

// slackBotOnlyToken returns a genuine bot token (xoxb-) from the write- or
// bot-token env slots, or "" if none is set. Write-token slots come first
// because flow POSTS with that token (see slackToken()), so the echoes it must
// recognize as self carry that bot's user id.
//
// Unlike SlackBotToken(), it deliberately does NOT fall back to the user token:
// a user token (xoxp-) authenticates as the OPERATOR, so resolving flow's "own
// bot user id" from it would return the operator — poisoning self-echo detection
// (flow would then mistake the operator for itself and never recognize its own
// bot's echoes). Self-recognition must key on the bot, never the operator. When
// this returns "", self-echo detection falls back to the operator-configured
// SelfBotUserIDs().
func slackBotOnlyToken() string {
	for _, t := range []string{
		os.Getenv("FLOW_SLACK_WRITE_TOKEN"),
		os.Getenv("SLACK_WRITE_TOKEN"),
		os.Getenv("SLACK_BOT_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	} {
		if t = strings.TrimSpace(t); strings.HasPrefix(t, "xoxb-") {
			return t
		}
	}
	return ""
}

// SlackUserToken resolves the xoxp- user token. Used when the listener
// needs to act on behalf of the user (chat.postMessage as them, not as
// a bot). Falls back to SlackBotToken's token family for single-token
// setups.
func SlackUserToken() string {
	return firstNonEmpty(
		os.Getenv("FLOW_SLACK_USER_TOKEN"),
		os.Getenv("SLACK_USER_TOKEN"),
		os.Getenv("FLOW_SLACK_TOKEN"),
		os.Getenv("SLACK_TOKEN"),
	)
}

// SlackSendIdentity reports which Slack identity outbound messages should be
// posted under: "bot" (the flow app's bot user) or "user" (the operator, via
// their user token). Controlled by FLOW_SLACK_SEND_AS; defaults to "bot".
// Anything unrecognized falls back to "bot" — the safe identity that never
// impersonates the operator.
func SlackSendIdentity() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_SLACK_SEND_AS"))) {
	case "user":
		return "user"
	default:
		return "bot"
	}
}

// slackToken returns the token SlackWriter should use for outbound calls.
// Explicit write tokens win; otherwise we reuse the read-side token.
func slackToken() string {
	return firstNonEmpty(
		os.Getenv("FLOW_SLACK_WRITE_TOKEN"),
		os.Getenv("SLACK_WRITE_TOKEN"),
		SlackBotToken(),
		SlackUserToken(),
	)
}

// parseSlackIDList splits a comma/space/tab/newline-separated list of Slack IDs
// into a trimmed, de-duplicated slice (first-seen order). Returns nil for empty
// input. Shared by the operator (SelfUserIDs) and bot (SelfBotUserIDs) identity
// lists so both parse identically.
func parseSlackIDList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
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

// firstNonEmpty returns the first trimmed-nonempty string in values,
// or "" if all are empty/whitespace.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// envBoolDefault reads name as a boolean with fallback. Recognized truthy
// values: 1, true, yes, y, on. Recognized falsy: 0, false, no, n, off.
// Anything else returns fallback.
func envBoolDefault(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
