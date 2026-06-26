package steering

import (
	"strings"

	"flow/internal/monitor"
)

// WatchConfig is the operator's Stage 0 configuration (spec §10). Channel sets
// are maps for O(1) membership. Identity drives the self-drop; MentionUserIDs
// drives "is this addressed to me" detection in otherwise-unwatched channels.
// GitHubIdentity holds the operator's GitHub login(s) for the GitHub
// connector's self-drop (see stage0GitHub).
type WatchConfig struct {
	WatchedChannels map[string]bool
	MutedChannels   map[string]bool
	MutedKeywords   []string
	// MutedAuthors / MutedThreads are operator "perma drop" suppressions set from
	// a feed card (stored in steering_mutes). MutedAuthors keys are Slack user ids
	// / GitHub logins; MutedThreads keys are thread keys.
	MutedAuthors   map[string]bool
	MutedThreads   map[string]bool
	Identity       OperatorIdentity
	MentionUserIDs []string
	GitHubIdentity []string
	// TaskLinkedGitHubThreads contains monitor.ThreadKey(repo, gh-pr:/gh-issue:
	// tag) entries for active tasks that already own a GitHub item. Lifecycle
	// events on these threads must not be dropped just because the poll event is
	// self-authored or authorless.
	TaskLinkedGitHubThreads map[string]bool
}

// Stage0Result is the outcome of the free deterministic filter.
type Stage0Result struct {
	Pass       bool
	DropReason string // non-empty when Pass == false (for the explainability log)
	ThreadKey  string // channel:thread_ts coalescing key, set when Pass == true
}

// Stage0 applies the no-LLM drop rules (spec §6, Stage 0), dispatching to a
// per-connector policy keyed off connectorOf. Adding a connector means adding a
// case here plus its stage0<Connector> function — the rest of the cascade is
// connector-agnostic. Slack is the default.
func Stage0(ev monitor.InboundEvent, cfg WatchConfig) Stage0Result {
	switch connectorOf(ev) {
	case "github":
		return stage0GitHub(ev, cfg)
	case "clickup":
		return stage0ClickUp(ev, cfg)
	default:
		return stage0Slack(ev, cfg)
	}
}

func stage0ClickUp(ev monitor.InboundEvent, cfg WatchConfig) Stage0Result {
	if strings.TrimSpace(ev.UserID) == "" {
		return Stage0Result{DropReason: "no author"}
	}
	if cfg.MutedAuthors[ev.UserID] {
		return Stage0Result{DropReason: "muted sender"}
	}
	if hasMutedKeyword(ev.Text, cfg.MutedKeywords) {
		return Stage0Result{DropReason: "muted keyword"}
	}
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return Stage0Result{DropReason: "no thread key"}
	}
	if cfg.MutedThreads[key] {
		return Stage0Result{DropReason: "muted thread"}
	}
	return Stage0Result{Pass: true, ThreadKey: key}
}

// stage0GitHub is the Stage 0 policy for the GitHub connector. GitHub events
// are pre-scoped by the poller (already operator-relevant: assigned, review-
// requested, mentioned, involved), so the policy is light: drop self-authored,
// authorless, muted-repo, and muted-keyword events; everything else passes with
// the LinkTag-derived thread key. ev.Channel is owner/repo; ev.ThreadTS is the
// gh-pr/gh-issue link tag.
func stage0GitHub(ev monitor.InboundEvent, cfg WatchConfig) Stage0Result {
	taskLinked := githubTaskLinked(ev, cfg)
	// The self-authored drop suppresses ECHOES of your own activity (comments,
	// pushes, reviews). It must NOT swallow an inbound ASK — being assigned an
	// issue/PR or having your review requested. The webhook stamps the issue/PR
	// *creator* as the event author, so an issue you filed and assigned to
	// yourself looks self-authored even though the assignment is the canonical
	// "track this" signal (the legacy poller's assignee:@me trigger). The scope
	// gate below still requires the operator to be a participant, so this only
	// lets through asks that genuinely involve them.
	if containsFold(cfg.GitHubIdentity, ev.UserID) &&
		!(taskLinked && githubLifecycleNeedsTaskAttention(ev.Kind)) &&
		!githubInboundAsk(ev.Kind) {
		return Stage0Result{DropReason: "self-authored"}
	}
	if strings.TrimSpace(ev.UserID) == "" && !(taskLinked && githubLifecycleNeedsTaskAttention(ev.Kind)) {
		return Stage0Result{DropReason: "no author"}
	}
	if cfg.MutedChannels[ev.Channel] { // ev.Channel is owner/repo
		return Stage0Result{DropReason: "muted repo"}
	}
	if cfg.MutedAuthors[ev.UserID] {
		return Stage0Result{DropReason: "muted sender"}
	}
	if hasMutedKeyword(ev.Text, cfg.MutedKeywords) {
		return Stage0Result{DropReason: "muted keyword"}
	}
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return Stage0Result{DropReason: "no thread key"}
	}
	if cfg.MutedThreads[key] {
		return Stage0Result{DropReason: "muted thread"}
	}
	// Scope gate (runs last, after mutes): only GitHub events that involve the
	// operator reach the classifier — otherwise an org-wide webhook install
	// floods the cascade with the whole org's PR churn (mirrors the Slack scope
	// gate). The legacy poller got this for free by searching involves:@operator.
	if !githubInScope(ev, cfg) {
		return Stage0Result{DropReason: "out of scope (operator not involved)"}
	}
	return Stage0Result{Pass: true, ThreadKey: key}
}

func githubTaskLinked(ev monitor.InboundEvent, cfg WatchConfig) bool {
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	return key != "" && cfg.TaskLinkedGitHubThreads[key]
}

// githubInScope reports whether a GitHub event involves the operator enough to
// warrant attention: the PR/issue is already tracked by a task, the operator is
// @-mentioned, or the operator is a participant (subject author / assignee /
// requested reviewer).
//
// SECURITY: this mirrors monitor.gitHubEventInvolvesOperator. When the
// operator's GitHub identity is unconfigured we fail CLOSED for webhook events
// rather than flooding the cascade with the whole org's PR churn (P0-2):
// webhook events always carry the subject author in Participants, so they're
// judged by the mention/participant checks above and drop here when the
// operator isn't among them. The no-participant carve-out below is the only
// remaining fail-open, scoped to the retired poller path; webhook events never
// reach it.
func githubInScope(ev monitor.InboundEvent, cfg WatchConfig) bool {
	if githubTaskLinked(ev, cfg) {
		return true
	}
	if monitor.MentionsLogin(ev.Text, cfg.GitHubIdentity) {
		return true
	}
	for _, login := range ev.Participants {
		if containsFold(cfg.GitHubIdentity, login) {
			return true
		}
	}
	// No participant data → not a webhook firehose event. The (now-retired)
	// poller's events were pre-filtered to involve the operator and don't carry
	// Participants, so we fail open rather than drop them. Webhook events always
	// carry the subject author, so the gate above applies and fails closed.
	if len(ev.Participants) == 0 {
		return true
	}
	return false
}

// githubInboundAsk reports whether a GitHub event is an inbound work assignment
// (you were assigned an issue/PR, or your review was requested) rather than an
// echo of your own activity. These survive the self-authored drop because the
// webhook stamps the issue/PR creator as the event author — so a self-filed,
// self-assigned issue would otherwise be dropped as self-authored.
func githubInboundAsk(kind string) bool {
	switch kind {
	case "issue_assigned", "pr_assigned", "pr_review_requested":
		return true
	}
	return false
}

func githubLifecycleNeedsTaskAttention(kind string) bool {
	switch kind {
	case "pr_head_updated", "pr_merged", "pr_review_changes_requested", "pr_review_comment", "pr_comment", "issue_comment":
		return true
	default:
		return false
	}
}

// stage0Slack applies the no-LLM drop rules (spec §6, Stage 0) for the Slack
// connector. It only considers human chat events ("message"/"app_mention");
// reactions belong to the existing reaction-trigger pipeline and are dropped
// here. Order: kind → self → bot → mute → scope. Scope is a strict drop
// criterion: only in-scope events (DMs/MPIMs, operator @mentions, and watched
// channels — see inScope) reach the Stage 1 classifier; everything else drops
// deterministically as out-of-scope, so classifier budget is never spent on
// channels the operator isn't watching. The scope gate runs last so the more
// specific mute reasons (muted channel/sender/keyword/thread) keep precedence.
func stage0Slack(ev monitor.InboundEvent, cfg WatchConfig) Stage0Result {
	if ev.Kind != "message" && ev.Kind != "app_mention" {
		return Stage0Result{DropReason: "not a chat event"}
	}
	if containsFold(cfg.Identity.UserIDs, ev.UserID) {
		return Stage0Result{DropReason: "self-authored"}
	}
	if strings.TrimSpace(ev.UserID) == "" {
		return Stage0Result{DropReason: "system/bot (no user)"}
	}
	if cfg.MutedChannels[ev.Channel] {
		return Stage0Result{DropReason: "muted channel"}
	}
	if cfg.MutedAuthors[ev.UserID] {
		return Stage0Result{DropReason: "muted sender"}
	}
	if hasMutedKeyword(ev.Text, cfg.MutedKeywords) {
		return Stage0Result{DropReason: "muted keyword"}
	}
	key := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	if key == "" {
		return Stage0Result{DropReason: "no thread key"}
	}
	if cfg.MutedThreads[key] {
		return Stage0Result{DropReason: "muted thread"}
	}
	if !inScope(ev, cfg) {
		return Stage0Result{DropReason: "out of scope / not watched"}
	}
	return Stage0Result{Pass: true, ThreadKey: key}
}

// inScope passes DMs/MPIMs, anything that mentions the operator, and messages
// in watched channels. DM detection falls back to the channel-id prefix (Slack
// DM channels are "D…") because not every ingestion path stamps channel_type —
// the durable backfill and some payloads omit it. Same convention as the
// context fetcher's DM-client gate.
func inScope(ev monitor.InboundEvent, cfg WatchConfig) bool {
	if ev.ChannelType == "im" || ev.ChannelType == "mpim" || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ev.Channel)), "D") {
		return true
	}
	if ev.Kind == "app_mention" || mentionsOperator(ev.Text, cfg.MentionUserIDs) {
		return true
	}
	return cfg.WatchedChannels[ev.Channel]
}

// mentionsOperator reports whether text contains a Slack-style <@UID> mention
// for any of the operator's user IDs.
func mentionsOperator(text string, ids []string) bool {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && strings.Contains(text, "<@"+id+">") {
			return true
		}
	}
	return false
}

func hasMutedKeyword(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func containsFold(haystack []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), needle) {
			return true
		}
	}
	return false
}
