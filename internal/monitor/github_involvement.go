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
// requested reviewer) or @-mentioned in its body. Fails OPEN when no operator
// login is configured — without knowing who the operator is we can't judge
// involvement, so preserve prior behavior rather than drop everything. Task
// linkage is checked by the caller (an already-tracked PR is always in scope).
func gitHubEventInvolvesOperator(ev GitHubEvent) bool {
	logins := GitHubSelfLogins()
	if len(logins) == 0 {
		return true
	}
	for _, p := range ev.Participants {
		if loginInSet(logins, p) {
			return true
		}
	}
	if MentionsLogin(ev.Body, logins) {
		return true
	}
	// No participant data means this is NOT a webhook firehose event — the
	// poller's discovery events (review-requested/assigned/mentioned/involved)
	// are pre-filtered to involve the operator by the search query, and don't
	// carry Participants. Can't gate them here, so fail open. Webhook events
	// always carry at least the subject author, so the gate above applies.
	if len(ev.Participants) == 0 {
		return true
	}
	return false
}
