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

var (
	botUserIDMu       sync.Mutex
	botUserIDCache    string
	botUserIDResolved bool
)

// resolveSlackBotUserIDFn returns the installed bot's own USER id (e.g. "U0123…")
// via auth.test on the bot token, memoized. The user id — not the handle — is
// what a Slack mention (<@U…>) needs so the footer's "@flow" LINKS to the app
// instead of rendering as dead "@flow" text. "" when no bot token is set or
// auth.test fails (the caller then falls back to a plain "@handle" without
// caching, so a later token still resolves). A var so tests can stub it.
var resolveSlackBotUserIDFn = func() string {
	botUserIDMu.Lock()
	defer botUserIDMu.Unlock()
	if botUserIDResolved {
		return botUserIDCache
	}
	token := slackBotOnlyToken()
	if strings.TrimSpace(token) == "" {
		return ""
	}
	resp, err := slack.New(token).AuthTest()
	if err != nil || resp == nil {
		return ""
	}
	botUserIDCache = strings.TrimSpace(resp.UserID)
	botUserIDResolved = true
	return botUserIDCache
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
	// Prefer a real Slack mention (<@bot-user-id>) so the footer's "@flow" LINKS
	// to the flow app — a bare "@flow" is never a mention in Slack mrkdwn and
	// renders as dead text. The mention itself shows the bot's current display
	// name, so no separate handle is needed. Fall back to plain "@<handle>" only
	// when the bot user id can't be resolved (no bot token / auth.test failed).
	if id := strings.TrimSpace(resolveSlackBotUserIDFn()); id != "" {
		return "_Sent using_ <@" + id + ">"
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

// footerForChannel returns the attribution footer to render below an outbound
// message, or "" when it should carry none: the operator↔bot command DM (flow's
// own system messages — acks, hints, rejections — aren't replies sent to others)
// or a disabled/empty FLOW_SLACK_SEND_FOOTER. Resolved at the send chokepoint
// (sendAsBotFn / scheduleAsBotFn) and rendered as a non-editable context block
// (see slackPostOptions) — never appended to the body — so the composing agent
// never writes it and the recipient can't edit it out.
func footerForChannel(channel string) string {
	if botIsMemberOfIM(channel) {
		return ""
	}
	return SlackSendFooter()
}
