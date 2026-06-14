package monitor

import "strings"

// GitHub operator-involvement helpers, shared by the legacy dispatcher's
// task-create gate and steering's Stage 0 scope gate. A webhook delivery is a
// firehose scoped to the App's installation (org-wide), not to the operator —
// so "does this involve me?" must be re-derived from the payload. The legacy
// poller got this for free by searching involves:@operator.

// MentionsLogin reports whether text @-mentions any of logins, matched
// case-insensitively at username boundaries: not preceded by an alphanumeric
// (so an email local-part "a@foo" is ignored) and not followed by another
// username byte (so "@foo-bot" does not match "foo").
func MentionsLogin(text string, logins []string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	hay := strings.ToLower(text)
	for _, l := range logins {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && mentionAt(hay, "@"+l) {
			return true
		}
	}
	return false
}

func mentionAt(hayLower, needleLower string) bool {
	for from := 0; ; {
		i := strings.Index(hayLower[from:], needleLower)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needleLower)
		beforeOK := i == 0 || !isAlnumByte(hayLower[i-1])
		afterOK := end >= len(hayLower) || !isGitHubLoginByte(hayLower[end])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
}

func isGitHubLoginByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-'
}

func isAlnumByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func loginInSet(logins []string, login string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return false
	}
	for _, l := range logins {
		if strings.ToLower(strings.TrimSpace(l)) == login {
			return true
		}
	}
	return false
}

// gitHubEventInvolvesOperator reports whether the operator (FLOW_GH_SELF_LOGINS)
// is a party to this event: a participant in its subject (author / assignee /
// requested reviewer) or @-mentioned in its body. Task linkage is checked by the
// caller (an already-tracked PR is always in scope).
//
// SECURITY: webhook deliveries are internet-reachable and org-wide — a fail-open
// gate here means any participant in any installed repo can spawn a task/session
// seeded with their PR/issue text (P0-2). So when no operator login is
// configured we fail CLOSED for webhook events: they always carry at least the
// subject author in Participants, so they're judged by the participant/mention
// checks above and drop here when the operator isn't among them. The
// no-participant carve-out below is the ONLY remaining fail-open, scoped to the
// retired poller path (see SchedulesPolling); webhook events never hit it.
func gitHubEventInvolvesOperator(ev GitHubEvent) bool {
	logins := GitHubSelfLogins()
	for _, p := range ev.Participants {
		if loginInSet(logins, p) {
			return true
		}
	}
	if MentionsLogin(ev.Body, logins) {
		return true
	}
	// No participant data means this is NOT a webhook firehose event — the
	// (now-retired) poller's discovery events were pre-filtered to involve the
	// operator by the search query and don't carry Participants, so we can't
	// gate them here and fail open. Webhook events always carry at least the
	// subject author, so they're judged above and fail closed when uninvolved.
	if len(ev.Participants) == 0 {
		return true
	}
	return false
}
