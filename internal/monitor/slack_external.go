package monitor

import (
	"context"
	"os"
	"strings"
)

// LookupConversation fetches conversations.info for the external-channel send
// gate, trying the bot token first and then the operator's user token (which
// can see private / Slack Connect channels the bot was never invited to).
// ok=false when no token is configured or both lookups fail — callers treat
// that as "can't determine" (fail-open).
func LookupConversation(ctx context.Context, channel string) (SlackConversation, bool) {
	for _, mk := range []func() SlackTitleClient{
		func() SlackTitleClient { return newSlackTitleAPIClient() },
		NewSlackTitleUserClient,
	} {
		client := mk()
		if client == nil {
			continue
		}
		if conv, err := client.ConversationInfo(ctx, channel); err == nil {
			return conv, true
		}
	}
	return SlackConversation{}, false
}

// OperatorTeamIDs returns the operator's own Slack workspace (team) ids — the
// "inside the org" set. Used by IsExternalToOrg to flag a cross-workspace
// conversation. Sourced from FLOW_SLACK_TEAM_IDS (comma/space list) or the
// singular FLOW_SLACK_TEAM_ID; empty when the operator hasn't configured it
// (in which case only the Slack Connect flags decide external-ness).
func OperatorTeamIDs() []string {
	ids := parseSlackIDList(firstNonEmpty(
		os.Getenv("FLOW_SLACK_TEAM_IDS"),
		os.Getenv("FLOW_SLACK_TEAM_ID"),
	))
	if len(ids) > 0 {
		return ids
	}
	// Not configured → fall back to the operator's own workspace resolved from
	// auth.test, so cross-workspace channels are gated by default (anything in
	// another team_id is external). Empty when Slack isn't reachable, in which
	// case only the Slack Connect flags drive the gate.
	if t := ResolvedTeamID(); t != "" {
		return []string{t}
	}
	return nil
}

// IsExternalToOrg reports whether the conversation includes people from outside
// the operator's org — the condition that gates an outbound send behind the
// operator's explicit approval.
//
// Two independent signals:
//   - Slack Connect flags (is_ext_shared / is_org_shared / is_pending_ext_shared)
//     mean the conversation is shared with another organization. These hold even
//     when the operator's own team is unknown, so they always gate.
//   - A participating team_id (connected / shared / context) that isn't one of
//     the operator's own teams marks a cross-workspace conversation. This is only
//     decidable when operatorTeams is known; with no configured team we do NOT
//     gate on a team mismatch alone (that would gate every channel before the
//     operator seeds their team), and rely on the Connect flags.
func (c SlackConversation) IsExternalToOrg(operatorTeams []string) bool {
	if c.IsExtShared || c.IsOrgShared || c.IsPendingExtShared {
		return true
	}
	if len(operatorTeams) == 0 {
		return false
	}
	own := make(map[string]bool, len(operatorTeams))
	for _, t := range operatorTeams {
		if t = strings.TrimSpace(t); t != "" {
			own[t] = true
		}
	}
	for _, t := range c.participantTeamIDs() {
		if t != "" && !own[t] {
			return true
		}
	}
	return false
}

// participantTeamIDs is the set of team_ids known to participate in the
// conversation. InternalTeamIDs are excluded: for an org-shared workspace they
// list teams internal to that org (already covered by the IsOrgShared flag),
// and for an ordinary channel they would just echo the operator's own team.
func (c SlackConversation) participantTeamIDs() []string {
	ids := make([]string, 0, len(c.ConnectedTeamIDs)+len(c.SharedTeamIDs)+1)
	ids = append(ids, c.ConnectedTeamIDs...)
	ids = append(ids, c.SharedTeamIDs...)
	if c.ContextTeamID != "" {
		ids = append(ids, c.ContextTeamID)
	}
	return ids
}
