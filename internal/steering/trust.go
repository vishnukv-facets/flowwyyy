package steering

import (
	"database/sql"
	"os"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// forwardAutoPermitStamp computes the (calibrated confidence, trusted-source)
// pair the forward path writes onto an inbox entry's meta. The confidence is
// run through the operator's empirical ConfidenceCalibrator for ActionForward
// (the calibrator falls back to the raw model value when it has no grounded
// history, matching the rest of the autonomy stack). Both signals are read by
// the unattended wake gate; neither is used here. A nil db or a calibrator load
// failure degrades to the raw confidence — trust, not confidence, is the gate's
// injection-safety lever, so a missing calibration never opens an unsafe door.
func forwardAutoPermitStamp(db *sql.DB, item flowdb.FeedItem) (confidence float64, trusted bool) {
	confidence = item.Confidence
	if db != nil {
		if cal, err := LoadConfidenceCalibrator(db); err == nil {
			confidence, _ = cal.Calibrate(ActionForward, item.Confidence)
		}
	}
	return confidence, ForwardSourceTrusted(item)
}

// ForwardSourceTrusted reports whether a forwarded feed item's source is
// operator-trusted for auto-permit into an UNATTENDED (bypass/auto) session.
// Trust — not routing confidence — is the lever that actually correlates with
// prompt-injection risk: a body we trust the origin of is safe to hand to a
// no-human-approval agent; routing confidence only tells us we picked the right
// task. A source is trusted when EITHER:
//
//   - it was authored by the operator themselves (Slack self user-id, or GitHub
//     self login) — their own words carry no third-party injection, or
//   - it arrived on a channel/repo the operator explicitly allow-listed via
//     FLOW_STEERING_TRUSTED_CHANNELS (comma/space separated ids).
//
// The allowlist defaults EMPTY, so out of the box only the operator's own
// authored content is trusted — the safe, fail-closed default. Everything else
// (unknown senders, unlisted channels, empty author) is untrusted.
func ForwardSourceTrusted(item flowdb.FeedItem) bool {
	if authorIsOperator(item) {
		return true
	}
	return channelAllowlisted(item.Channel)
}

// authorIsOperator reports whether the item's author is the operator's own
// identity for the item's source (Slack self user-ids or GitHub self logins).
func authorIsOperator(item flowdb.FeedItem) bool {
	author := strings.TrimSpace(item.Author)
	if author == "" {
		return false
	}
	var identities []string
	switch strings.ToLower(strings.TrimSpace(item.Source)) {
	case "github":
		identities = monitor.GitHubSelfLogins()
	default: // slack and any other connector keyed on a Slack-style user id
		identities = monitor.SelfUserIDs()
	}
	for _, id := range identities {
		if strings.EqualFold(strings.TrimSpace(id), author) {
			return true
		}
	}
	return false
}

// channelAllowlisted reports whether channel is on the operator's explicit
// trusted-channel allowlist (FLOW_STEERING_TRUSTED_CHANNELS). Empty channel or
// empty allowlist is never trusted.
func channelAllowlisted(channel string) bool {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return false
	}
	for _, c := range splitList(os.Getenv("FLOW_STEERING_TRUSTED_CHANNELS")) {
		if strings.EqualFold(c, channel) {
			return true
		}
	}
	return false
}
