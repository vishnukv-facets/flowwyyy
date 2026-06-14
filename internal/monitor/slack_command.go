package monitor

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// CommandChannelEnabled gates the Slack AFK remote-control feature. Default
// OFF — this surface can run commands on the operator's machine, so it must be
// explicitly opted into.
func CommandChannelEnabled() bool {
	return envBoolDefault("FLOW_SLACK_COMMAND_ENABLED", false)
}

// AuthorizedOperator reports whether userID is the operator. It accepts EITHER
// source of operator identity: an id listed in FLOW_SLACK_SELF_USER_IDS, or the
// id that owns the Slack USER token (resolved via auth.test — the user token is
// the operator's by construction). Accepting the token-owner as a fallback fixes
// the failure where the operator's events carry an Enterprise-Grid alternate id
// (or the env lists a stale/different id) and they get rejected from their own
// bot DM. Empty/unknown authors are never authorized — the command channel is a
// remote shell and must be locked to the operator alone.
func AuthorizedOperator(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	if containsUserID(SelfUserIDs(), userID) {
		return true
	}
	if op := operatorUserID(); op != "" && op == userID {
		return true
	}
	return false
}

// OperatorIdentityKnown reports whether flow can positively identify the
// operator — either FLOW_SLACK_SELF_USER_IDS is set, or the user token resolves
// to an owner id. When this is false the dispatcher must NOT reject an unknown
// DM sender as a non-operator: we can't tell who the operator is, so rejecting
// risks declining the operator themselves.
func OperatorIdentityKnown() bool {
	return len(SelfUserIDs()) > 0 || operatorUserID() != ""
}

// operatorUserIDFn resolves the operator's Slack user id from the USER token
// (auth.test). The user token is the operator's, so its owner is definitively
// the operator — a robust fallback when FLOW_SLACK_SELF_USER_IDS is unset or
// lists a different identity than the events carry. Package var for tests;
// fail-safe "" on empty token or any error.
var operatorUserIDFn = func() string {
	token := SlackUserToken()
	if strings.TrimSpace(token) == "" {
		return ""
	}
	resp, err := slack.New(token).AuthTest() // AuthTest() (*AuthTestResponse, error); resp.UserID
	if err != nil || resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.UserID)
}

var (
	operatorIDMu       sync.Mutex
	operatorIDCache    string
	operatorIDResolved bool
)

// operatorUserID returns the token-owner operator id, resolving it once via
// operatorUserIDFn and memoizing the result (including a fail-safe ""), so the
// hot dispatch path doesn't re-hit Slack on every DM. Cleared on Slack settings
// change via resetCommandChannelCache().
func operatorUserID() string {
	operatorIDMu.Lock()
	defer operatorIDMu.Unlock()
	if operatorIDResolved {
		return operatorIDCache
	}
	operatorIDCache = operatorUserIDFn()
	operatorIDResolved = true
	return operatorIDCache
}

// selfBotUserIDFn resolves flow's OWN bot user id via auth.test on a genuine BOT
// token (xoxb-). A bot token authenticates as the flow app's bot user, so its
// auth.test owner is the bot itself (e.g. U0BA6B7DQKV). Used to recognize and
// drop flow's own outbound Slack messages when Slack echoes them back through
// the listener — otherwise the bot's acks / agent replies are re-ingested as
// inbound traffic (declined by the command channel as a "non-operator",
// surfaced as bogus self-acknowledgment attention).
//
// It resolves from slackBotOnlyToken(), NOT SlackBotToken(): the latter falls
// back to the user token, and auth.test on a user token returns the OPERATOR,
// not the bot — which would invert self-echo detection (drop the operator's own
// DMs, process the bot's echoes). When no bot token is configured this returns
// "" and self-echo detection relies on the operator-pinned SelfBotUserIDs().
// Package var for tests; fail-safe "" on empty token or any error.
var selfBotUserIDFn = func() string {
	token := slackBotOnlyToken()
	if token == "" {
		return ""
	}
	resp, err := slack.New(token).AuthTest() // AuthTest() (*AuthTestResponse, error); resp.UserID is the bot user
	if err != nil || resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.UserID)
}

// SelfBotUserIDs returns flow's OWN Slack bot user id(s), configured by the
// operator via FLOW_SLACK_SELF_BOT_USER_IDS (comma/space-separated; singular
// FLOW_SLACK_SELF_BOT_USER_ID accepted). This is the no-API source of truth for
// self-echo detection: it lets flow recognize and drop its own bot's messages
// even when the bot user id cannot be resolved from a token — e.g. a
// user-token-only deployment, where auth.test on the bot-token slot resolves to
// the OPERATOR, not the bot. Kept DISTINCT from SelfUserIDs (the operator) on
// purpose: a bot id here must never authorize itself in the command channel, and
// the operator's id must never be treated as a self-echo (that would silently
// drop their own command DMs).
func SelfBotUserIDs() []string {
	return parseSlackIDList(firstNonEmpty(
		os.Getenv("FLOW_SLACK_SELF_BOT_USER_IDS"),
		os.Getenv("FLOW_SLACK_SELF_BOT_USER_ID"),
	))
}

var (
	selfBotIDMu       sync.Mutex
	selfBotIDCache    string
	selfBotIDResolved bool
)

// selfBotUserID returns flow's own bot user id, resolving it once via
// selfBotUserIDFn and memoizing the result (including a fail-safe ""), so the
// hot dispatch path doesn't re-hit Slack on every event. Cleared on Slack
// settings / token change via resetCommandChannelCache().
func selfBotUserID() string {
	selfBotIDMu.Lock()
	defer selfBotIDMu.Unlock()
	if selfBotIDResolved {
		return selfBotIDCache
	}
	selfBotIDCache = selfBotUserIDFn()
	selfBotIDResolved = true
	return selfBotIDCache
}

// IsSelfAuthoredSlack reports whether ev was authored by flow's OWN Slack bot —
// a message flow itself posted (a command-channel ack, an agent reply via
// SendAsBot, an off-state hint) that Slack then echoed back to the listener as
// an inbound message event. Such events are not operator or third-party
// traffic and must never be dispatched: routing them declines the bot as a
// "non-operator" in its own command DM (a spurious reject AT the operator) and
// surfaces meaningless self-acknowledgment attention cards. Detection matches
// the inbound author against the resolved bot user id. FAIL SAFE: when the bot
// id can't be resolved this falls back to the operator-pinned SelfBotUserIDs(),
// and only when BOTH are empty does it return false (so real traffic is
// processed rather than silently swallowed). This is the Slack analogue of the
// GitHub self-echo stand-down.
func IsSelfAuthoredSlack(ev InboundEvent) bool {
	author := strings.TrimSpace(ev.UserID)
	if author == "" {
		return false
	}
	if self := selfBotUserID(); self != "" && author == self {
		return true
	}
	// Configured fallback: the operator-pinned bot id(s). Holds even when
	// auth.test can't resolve the bot (user-token-only deployments), where the
	// resolved id above is "". See SelfBotUserIDs.
	return containsUserID(SelfBotUserIDs(), author)
}

// conversationIsBotIMFn reports whether `channel` is an IM the flow BOT is a
// member of (i.e. a DM addressed to the bot). Uses the bot token's
// conversations.info: it succeeds only for conversations the bot participates
// in, so an operator's DM with a third party (bot absent) returns false.
// Package var for tests. FAIL SAFE: any error → false (never reply into a
// conversation we can't confirm is the bot's own DM).
var conversationIsBotIMFn = func(channel string) bool {
	token := SlackBotToken()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(channel) == "" {
		return false
	}
	ch, err := slack.New(token).GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil || ch == nil {
		return false
	}
	return ch.IsIM
}

var (
	botIMMu    sync.Mutex
	botIMCache map[string]bool
)

// botIsMemberOfIM reports (and caches) whether the flow bot is a member of the
// IM `channel`. The first lookup per channel calls conversationIsBotIMFn; the
// result is memoized so the hot dispatch path doesn't re-hit Slack on every DM.
// Cache is cleared by resetCommandChannelCache() on settings/token change.
func botIsMemberOfIM(channel string) bool {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return false
	}
	botIMMu.Lock()
	defer botIMMu.Unlock()
	if botIMCache == nil {
		botIMCache = map[string]bool{}
	}
	if v, ok := botIMCache[channel]; ok {
		return v
	}
	v := conversationIsBotIMFn(channel)
	botIMCache[channel] = v
	return v
}

// resetCommandChannelCache clears the cached bot-IM membership results so the
// next botIsMemberOfIM call re-resolves them. Useful after a token rotation or
// in tests.
func resetCommandChannelCache() {
	botIMMu.Lock()
	botIMCache = nil
	botIMMu.Unlock()
	operatorIDMu.Lock()
	operatorIDCache = ""
	operatorIDResolved = false
	operatorIDMu.Unlock()
	selfBotIDMu.Lock()
	selfBotIDCache = ""
	selfBotIDResolved = false
	selfBotIDMu.Unlock()
}

// ResetCommandChannelCache clears the bot-IM membership cache so the next
// IsCommandChannel call re-resolves it via conversations.info. Call after Slack
// token / self-user settings change, since the resolution depends on
// SlackBotToken().
func ResetCommandChannelCache() {
	resetCommandChannelCache()
}

// IsCommandChannel reports whether ev is a DM addressed to the flow bot — the
// only surface the operator may use to command flow. Detection is by bot
// membership (conversations.info via the bot token), which needs only im:read:
// it returns true only for IMs the bot itself participates in, so the
// operator's DMs with third parties (bot absent) return false. Non-im events
// and events with an empty Channel always return false. FAIL SAFE — any
// resolution error makes this false.
func IsCommandChannel(ev InboundEvent) bool {
	if ev.ChannelType != "im" || strings.TrimSpace(ev.Channel) == "" {
		return false
	}
	return botIsMemberOfIM(ev.Channel)
}

// unauthorizedDMReply is the static message sent to anyone other than the
// operator who DMs the flow bot. It is a CONSTANT — the sender's message text is
// never read or fed to any model, so there is no prompt-injection surface.
const unauthorizedDMReply = "👋 I'm a personal flow assistant and only take commands from my operator, so I can't help here. Please reach out to them directly."

var rejectedDMChannels sync.Map // channel id → struct{}; reply once per channel

// rejectUnauthorizedDM sends the static decline to a non-operator's bot DM,
// at most once per channel (avoids spamming / reply loops). NO agent is
// involved — the sender's text is discarded. SendAsBot is itself write-gated.
func rejectUnauthorizedDM(channel string) {
	if _, seen := rejectedDMChannels.LoadOrStore(channel, struct{}{}); seen {
		return
	}
	if err := SendAsBot(channel, unauthorizedDMReply); err != nil {
		// writes disabled or transient error — log, don't block. We already
		// stored the channel, so a failed first reply just means silence for
		// this channel, which is the safe outcome.
		fmt.Fprintf(os.Stderr, "monitor: reject unauthorized DM %s: %v\n", channel, err)
	}
}

// commandChannelDisabledHint nudges the operator (once per channel) when they
// DM the bot while the command channel is off, so a disabled feature isn't met
// with confusing silence. Static text — no agent, no injection surface.
const commandChannelDisabledHint = "👋 The flow DM command channel is off right now. Turn on “DM command channel” in flow's Slack connector settings, and I'll be able to take commands here."

var hintedDMChannels sync.Map // channel id → struct{}; nudge once per channel

// hintCommandChannelDisabled sends the off-state nudge to the operator at most
// once per channel. NO agent; SendAsBot is itself write-gated, so if posting is
// disabled this is silently a no-op (logged).
func hintCommandChannelDisabled(channel string) {
	if _, seen := hintedDMChannels.LoadOrStore(channel, struct{}{}); seen {
		return
	}
	if err := SendAsBot(channel, commandChannelDisabledHint); err != nil {
		fmt.Fprintf(os.Stderr, "monitor: command-disabled hint %s: %v\n", channel, err)
	}
}
