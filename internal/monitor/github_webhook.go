package monitor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// NormalizeGitHubWebhook converts one raw GitHub webhook delivery into zero or
// more GitHubEvents. eventType is the X-GitHub-Event header; deliveryID is the
// X-GitHub-Delivery header (kept for the caller's delivery log, not embedded in
// the event keys).
//
// The cardinal rule: events produced here must carry the SAME Kind + EventKey
// the poller (github_client.go) produces for the same underlying object, so the
// shared github_event_log dedupes across both transports. The key construction
// here deliberately mirrors the toGitHubEvent helpers — review keys are
// "review:<id>" (no state suffix), comment ids prefer node_id with a numeric
// fallback, and head-update keys carry the SHA.
//
// Unsupported event/action combinations return (nil, nil): the receiver records
// them as "ignored" and still answers GitHub with a 2xx. A body that is not
// valid JSON is a hard error so the delivery can be marked errored.
func NormalizeGitHubWebhook(eventType, deliveryID string, payload []byte) ([]GitHubEvent, error) {
	_ = deliveryID
	eventType = strings.TrimSpace(eventType)
	var p ghWebhookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("parse github webhook %q: %w", eventType, err)
	}
	owner, repo := ownerRepoFromFullName(p.Repository.FullName)
	if owner == "" || repo == "" {
		// No repository in the payload (org/ping/installation events) — nothing
		// routable. Treat as ignored, not an error.
		return nil, nil
	}
	raw := string(payload)

	var (
		ev GitHubEvent
		ok bool
	)
	switch eventType {
	case "pull_request":
		ev, ok = normalizePullRequestEvent(p, owner, repo, raw)
	case "issues":
		ev, ok = normalizeIssuesEvent(p, owner, repo, raw)
	case "issue_comment":
		ev, ok = normalizeIssueCommentEvent(p, owner, repo, raw)
	case "pull_request_review":
		ev, ok = normalizeReviewEvent(p, owner, repo, raw)
	case "pull_request_review_comment":
		ev, ok = normalizeReviewCommentEvent(p, owner, repo, raw)
	default:
		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	return []GitHubEvent{ev}, nil
}

// ghWebhookPayload is the union of the fields Flow reads across the supported
// webhook event types. Each top-level object is a pointer so its absence is
// distinguishable from a zero value.
type ghWebhookPayload struct {
	Action      string              `json:"action"`
	Repository  ghWebhookRepository `json:"repository"`
	PullRequest *ghWebhookPR        `json:"pull_request"`
	Issue       *ghWebhookIssue     `json:"issue"`
	Comment     *ghWebhookComment   `json:"comment"`
	Review      *ghWebhookReview    `json:"review"`
}

type ghWebhookRepository struct {
	FullName string `json:"full_name"`
}

type ghWebhookPR struct {
	Number    int              `json:"number"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	HTMLURL   string           `json:"html_url"`
	State     string           `json:"state"`
	Merged    bool             `json:"merged"`
	User      githubUser       `json:"user"`
	Base      githubRef        `json:"base"`
	Head      githubRef        `json:"head"`
	Labels    []githubLabel    `json:"labels"`
	Milestone *githubMilestone `json:"milestone"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

type ghWebhookIssue struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	User        githubUser       `json:"user"`
	Labels      []githubLabel    `json:"labels"`
	Milestone   *githubMilestone `json:"milestone"`
	PullRequest json.RawMessage  `json:"pull_request"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
}

type ghWebhookComment struct {
	ID        int64      `json:"id"`
	NodeID    string     `json:"node_id"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	User      githubUser `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

type ghWebhookReview struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	User        githubUser `json:"user"`
	SubmittedAt string     `json:"submitted_at"`
}

func normalizePullRequestEvent(p ghWebhookPayload, owner, repo, raw string) (GitHubEvent, bool) {
	pr := p.PullRequest
	if pr == nil || pr.Number <= 0 {
		return GitHubEvent{}, false
	}
	ev := GitHubEvent{
		Owner: owner, Repo: repo, Number: pr.Number,
		Title: pr.Title, Body: pr.Body, URL: pr.HTMLURL, Author: pr.User.Login,
		BaseRef: pr.Base.Name, HeadRef: pr.Head.Name, HeadSHA: pr.Head.SHA,
		Labels: webhookLabelNames(pr.Labels), Milestone: webhookMilestoneTitle(pr.Milestone),
		RawJSON: raw, CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt,
	}
	switch p.Action {
	case "assigned":
		ev.Kind = GitHubEventPRAssigned
		ev.EventKey = discoveryEventKey(ev.Kind, owner, repo, pr.Number)
	case "review_requested":
		ev.Kind = GitHubEventPRReviewRequested
		ev.EventKey = discoveryEventKey(ev.Kind, owner, repo, pr.Number)
	case "synchronize":
		ev.Kind = GitHubEventPRHeadUpdated
		ev.EventKey = gitHubPRHeadEventKey(owner, repo, pr.Number, pr.Head.SHA)
		if ev.EventKey == "" {
			return GitHubEvent{}, false
		}
	case "closed":
		if pr.Merged {
			ev.Kind = GitHubEventPRMerged
			ev.EventKey = fmt.Sprintf("pr-merged:%s/%s#%d", owner, repo, pr.Number)
		} else {
			ev.Kind = GitHubEventPRClosed
			ev.EventKey = fmt.Sprintf("pr-closed:%s/%s#%d", owner, repo, pr.Number)
		}
	default:
		return GitHubEvent{}, false
	}
	return ev, true
}

func normalizeIssuesEvent(p ghWebhookPayload, owner, repo, raw string) (GitHubEvent, bool) {
	iss := p.Issue
	if iss == nil || iss.Number <= 0 {
		return GitHubEvent{}, false
	}
	if p.Action != "assigned" {
		return GitHubEvent{}, false
	}
	kind := GitHubEventIssueAssigned
	return GitHubEvent{
		Kind: kind, Owner: owner, Repo: repo, Number: iss.Number,
		Title: iss.Title, Body: iss.Body, URL: iss.HTMLURL, Author: iss.User.Login,
		Labels: webhookLabelNames(iss.Labels), Milestone: webhookMilestoneTitle(iss.Milestone),
		EventKey: discoveryEventKey(kind, owner, repo, iss.Number),
		RawJSON:  raw, CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt,
	}, true
}

func normalizeIssueCommentEvent(p ghWebhookPayload, owner, repo, raw string) (GitHubEvent, bool) {
	if p.Action != "created" {
		return GitHubEvent{}, false
	}
	iss, cmt := p.Issue, p.Comment
	if iss == nil || cmt == nil || iss.Number <= 0 {
		return GitHubEvent{}, false
	}
	id := webhookCommentID(cmt.NodeID, cmt.ID)
	if id == "" {
		return GitHubEvent{}, false
	}
	// GitHub serves PR top-level comments through the issue_comment event; the
	// issue object carries a pull_request link when the item is a PR.
	kind := GitHubEventIssueComment
	if len(iss.PullRequest) > 0 && string(iss.PullRequest) != "null" {
		kind = GitHubEventPRComment
	}
	return GitHubEvent{
		Kind: kind, Owner: owner, Repo: repo, Number: iss.Number,
		Body: cmt.Body, URL: cmt.HTMLURL, Author: cmt.User.Login,
		CommentID: id, EventKey: "issue-comment:" + id,
		RawJSON: raw, CreatedAt: cmt.CreatedAt, UpdatedAt: cmt.UpdatedAt,
	}, true
}

func normalizeReviewEvent(p ghWebhookPayload, owner, repo, raw string) (GitHubEvent, bool) {
	if p.Action != "submitted" {
		return GitHubEvent{}, false
	}
	pr, rev := p.PullRequest, p.Review
	if pr == nil || rev == nil || pr.Number <= 0 {
		return GitHubEvent{}, false
	}
	id := webhookCommentID(rev.NodeID, rev.ID)
	if id == "" {
		return GitHubEvent{}, false
	}
	var kind GitHubEventKind
	switch strings.ToUpper(strings.TrimSpace(rev.State)) {
	case "CHANGES_REQUESTED":
		kind = GitHubEventPRReviewChangesRequested
	case "APPROVED":
		kind = GitHubEventPRReviewApproved
	case "COMMENTED":
		kind = GitHubEventPRReviewComment
	default:
		return GitHubEvent{}, false
	}
	return GitHubEvent{
		Kind: kind, Owner: owner, Repo: repo, Number: pr.Number,
		Body: rev.Body, URL: rev.HTMLURL, Author: rev.User.Login,
		CommentID: id, EventKey: "review:" + id,
		RawJSON: raw, CreatedAt: rev.SubmittedAt, UpdatedAt: rev.SubmittedAt,
	}, true
}

func normalizeReviewCommentEvent(p ghWebhookPayload, owner, repo, raw string) (GitHubEvent, bool) {
	if p.Action != "created" {
		return GitHubEvent{}, false
	}
	pr, cmt := p.PullRequest, p.Comment
	if pr == nil || cmt == nil || pr.Number <= 0 {
		return GitHubEvent{}, false
	}
	id := webhookCommentID(cmt.NodeID, cmt.ID)
	if id == "" {
		return GitHubEvent{}, false
	}
	return GitHubEvent{
		Kind: GitHubEventPRReviewComment, Owner: owner, Repo: repo, Number: pr.Number,
		Body: cmt.Body, URL: cmt.HTMLURL, Author: cmt.User.Login,
		CommentID: id, EventKey: "review-comment:" + id,
		RawJSON: raw, CreatedAt: cmt.CreatedAt, UpdatedAt: cmt.UpdatedAt,
	}, true
}

// discoveryEventKey mirrors the poller's item key: "<link-tag>:<kind>", e.g.
// "gh-pr:o/r#5:pr_review_requested" or "gh-issue:o/r#7:issue_assigned".
func discoveryEventKey(kind GitHubEventKind, owner, repo string, number int) string {
	return fmt.Sprintf("%s:%s", linkTagForRecord(kind, owner, repo, number), kind)
}

func ownerRepoFromFullName(full string) (string, string) {
	parts := strings.SplitN(strings.Trim(strings.TrimSpace(full), "/"), "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

// webhookCommentID matches the poller's id rule: prefer the GraphQL node_id,
// fall back to the numeric REST id.
func webhookCommentID(nodeID string, numericID int64) string {
	id := strings.TrimSpace(nodeID)
	if id == "" && numericID > 0 {
		id = strconv.FormatInt(numericID, 10)
	}
	return id
}

func webhookLabelNames(labels []githubLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if strings.TrimSpace(l.Name) != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

func webhookMilestoneTitle(m *githubMilestone) string {
	if m == nil {
		return ""
	}
	return m.Title
}
