package steering

import "flow/internal/monitor"

// connectorOf classifies an event's source connector. The cascade keys all
// connector-specific behavior off this — adding a connector means teaching this
// function its discriminator plus a Stage 0 policy and a deep-triage context
// hint (see Stage0 dispatch / contextHintFor). Slack is the default.
func connectorOf(ev monitor.InboundEvent) string {
	if ev.ChannelType == "github" {
		return "github"
	}
	return "slack"
}
