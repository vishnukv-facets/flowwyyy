package steering

import (
	"os"
	"strings"

	"flow/internal/monitor"
)

// SteererSessionsEnabled reports whether the per-channel live-session model is
// on (FLOW_STEERING_SESSIONS). Default OFF: the cascade keeps its stateless
// DeepTriageIncremental path until the operator opts in. Phase 2 of 6.
func SteererSessionsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_STEERING_SESSIONS")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// SteererDelivery is the lean payload the cascade hands a per-channel session for
// one event: the cleaned message plus the deterministic context pack that anchors
// the *specific* message (permalink, parent, participants). ContextOnly marks a
// memory-only feed (operator-self / bot-echo) the session must absorb but never
// surface or reply to; SelfEcho further marks a bot reply echoed back (a
// non-genuine delivery confirmation) so the session stops re-nagging the thread.
type SteererDelivery struct {
	Source      string // "slack" | "github"
	Channel     string
	ChannelType string // "channel" | "im" | "mpim" | "github"
	TS          string
	ThreadTS    string
	Author      string
	Text        string        // cleaned message text (mentions resolved to names)
	Context     ThreadContext // deterministic context pack for this event
	ContextOnly bool
	SelfEcho    bool
}

// SteererSessionSink is the steering→server boundary (GAP-1). The cascade resolves
// a session key and hands the survivor to the sink; the server owns the terminal
// hub and ensures the channel's chat-steer-<key> session is live (start / resume /
// wake). It is injected onto Cascade exactly as monitor.ChatCommandSink is injected
// onto Dispatcher — server imports steering, so *server.Server implements this.
type SteererSessionSink interface {
	DeliverToChannelSession(key string, payload SteererDelivery) error
}

// sessionKeyForEvent resolves the deterministic session key for an event (GAP-4):
//   - Slack channel / DM / MPDM → the channel id.
//   - SharedRef forward → the ORIGIN channel (so a reply forwarded into a DM reaches
//     the origin channel's session, mirroring routeViaSharedRef).
//   - GitHub → deferred to Phase 5 (canonical PR↔issue keying); returns ok=false so
//     the cascade keeps the cold path for GitHub.
//
// ok=false means "no session for this event" — the caller falls through to the
// stateless cold path. The server turns the returned key into the chat slug.
func sessionKeyForEvent(ev monitor.InboundEvent) (string, bool) {
	if connectorOf(ev) == "github" {
		return "", false
	}
	if ref, ok := ev.SharedRef(); ok {
		if ch := strings.TrimSpace(ref.Channel); ch != "" {
			return ch, true
		}
	}
	if ch := strings.TrimSpace(ev.Channel); ch != "" {
		return ch, true
	}
	return "", false
}
