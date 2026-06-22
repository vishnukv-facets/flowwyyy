package monitor

import (
	"os"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// fallbackAppHandle is used only when the real app name can't be determined
// (no FLOW_SLACK_APP_NAME and auth.test unavailable) — a last resort, never a
// confident guess.
const fallbackAppHandle = "flow"

// SlackAppHandle resolves the @handle of the installed flow Slack app for the
// attribution footer — the ACTUAL app/bot name, never a hardcoded assumption.
// Priority: FLOW_SLACK_APP_NAME (persisted by the Slack connector at app
// creation) → the bot's own handle via auth.test (cached) → fallbackAppHandle.
func SlackAppHandle() string {
	if n := slackifyHandle(os.Getenv("FLOW_SLACK_APP_NAME")); n != "" {
		return n
	}
	if h := slackifyHandle(resolveSlackBotHandleFn()); h != "" {
		return h
	}
	return fallbackAppHandle
}

var (
	botHandleMu       sync.Mutex
	botHandleCache    string
	botHandleResolved bool
)

// resolveSlackBotHandleFn returns the installed bot's own username via auth.test
// on the bot token, memoized (the handle never changes within a process). A var
// so tests can stub it. Returns "" when no bot token is set or auth.test fails —
// the caller then falls back without caching, so a later token still resolves.
var resolveSlackBotHandleFn = func() string {
	botHandleMu.Lock()
	defer botHandleMu.Unlock()
	if botHandleResolved {
		return botHandleCache
	}
	token := slackBotOnlyToken()
	if strings.TrimSpace(token) == "" {
		return ""
	}
	resp, err := slack.New(token).AuthTest()
	if err != nil || resp == nil {
		return ""
	}
	botHandleCache = resp.User
	botHandleResolved = true
	return botHandleCache
}

// slackifyHandle trims whitespace and a leading '@' so the footer renders one
// clean "@name".
func slackifyHandle(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "@")
}

// ResolvedBotHandle returns the installed bot's own @handle via auth.test (the
// real app name), or "" when unresolvable. Cached. Used to pre-fill the App
// name setting with the actual value instead of a blank.
func ResolvedBotHandle() string { return slackifyHandle(resolveSlackBotHandleFn()) }

var (
	teamIDMu       sync.Mutex
	teamIDCache    string
	teamIDResolved bool
)

// resolveSlackTeamIDFn returns the operator's primary Slack workspace (team) id
// via auth.test — preferring the user token (the operator's own workspace), else
// the bot token. Memoized (a token's team id never changes). "" when no token /
// auth.test fails. A var so tests can stub it.
var resolveSlackTeamIDFn = func() string {
	teamIDMu.Lock()
	defer teamIDMu.Unlock()
	if teamIDResolved {
		return teamIDCache
	}
	token := firstNonEmpty(SlackUserToken(), slackBotOnlyToken(), SlackBotToken())
	if strings.TrimSpace(token) == "" {
		return ""
	}
	resp, err := slack.New(token).AuthTest()
	if err != nil || resp == nil {
		return ""
	}
	teamIDCache = strings.TrimSpace(resp.TeamID)
	teamIDResolved = true
	return teamIDCache
}

// ResolvedTeamID returns the operator's primary Slack workspace id, or "".
func ResolvedTeamID() string { return resolveSlackTeamIDFn() }

// SlackSendFooter resolves the outbound attribution footer. An explicitly-set
// FLOW_SLACK_SEND_FOOTER wins (including empty, which disables it); unset builds
// "_Sent using @<app>_" from the resolved app handle.
func SlackSendFooter() string {
	if v, ok := os.LookupEnv("FLOW_SLACK_SEND_FOOTER"); ok {
		return strings.TrimSpace(v)
	}
	return "_Sent using @" + SlackAppHandle() + "_"
}

// appendFooter joins a message body and a footer with a blank line. A blank
// footer leaves the body untouched; a blank body yields just the footer.
func appendFooter(text, footer string) string {
	footer = strings.TrimSpace(footer)
	if footer == "" {
		return text
	}
	body := strings.TrimRight(text, "\n")
	if body == "" {
		return footer
	}
	return body + "\n\n" + footer
}

// withSlackFooterForChannel appends the configured attribution footer to an
// outbound message, EXCEPT on the operator↔bot command DM — those are flow's own
// system messages (acks, hints, rejections), not replies sent to others, so
// they carry no "Sent using @app" attribution. Applied at the send chokepoint
// (SendAsThread / ScheduleAsThread) so every outward send gets exactly one
// footer and the composing agent never writes it.
func withSlackFooterForChannel(channel, text string) string {
	if botIsMemberOfIM(channel) {
		return text
	}
	return appendFooter(text, SlackSendFooter())
}
