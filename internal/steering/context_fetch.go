package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"flow/internal/monitor"
)

const defaultContextFetchLimit = 50

// githubSlugSegment bounds an owner/repo segment to the chars GitHub itself
// allows. It exists so a tag-derived owner/repo can't smuggle a `..` or a `/`
// into the `gh api -X GET repos/<owner>/<repo>/...` endpoint and redirect the
// read-only context fetch at an unintended resource. Anchored, so `..` (and any
// path separator) is rejected outright.
var githubSlugSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// NewDefaultContextFetcher builds the production deterministic context fetcher
// used by the Attention Router. Slack uses Web API thread reads; GitHub uses
// gh api reads. Individual fetch failures are returned to the cascade, which
// records an explicit fallback context pack.
func NewDefaultContextFetcher(cleanText func(context.Context, string) string, permalinker *monitor.SlackPermalinker) func(context.Context, monitor.InboundEvent) (ThreadContext, error) {
	var permalink func(context.Context, string, string) string
	if permalinker != nil {
		permalink = permalinker.Permalink
	}
	slackFetcher := SlackContextFetcher{
		Replies:   monitor.NewSlackRepliesClient(),
		DMReplies: monitor.NewSlackUserRepliesClient(),
		CleanText: cleanText,
		Permalink: permalink,
	}
	ghFetcher := GitHubContextFetcher{}
	return func(ctx context.Context, ev monitor.InboundEvent) (ThreadContext, error) {
		if connectorOf(ev) == "github" {
			return ghFetcher.FetchContext(ctx, ev)
		}
		return slackFetcher.FetchContext(ctx, ev)
	}
}

// SlackContextFetcher deterministically reads a Slack thread through
// conversations.replies. Bot-token reads cover channels; user-token reads cover
// DM/MPIM threads where the bot is not a participant.
type SlackContextFetcher struct {
	Replies   monitor.SlackThreadReplies
	DMReplies monitor.SlackThreadReplies
	CleanText func(context.Context, string) string
	Permalink func(context.Context, string, string) string
	Limit     int
}

func (f SlackContextFetcher) FetchContext(ctx context.Context, ev monitor.InboundEvent) (ThreadContext, error) {
	channel := strings.TrimSpace(ev.Channel)
	threadTS := strings.TrimSpace(ev.ThreadTS)
	if channel == "" || threadTS == "" {
		return ThreadContext{}, fmt.Errorf("slack context fetch unavailable: missing channel or thread_ts")
	}
	client := f.Replies
	if (ev.ChannelType == "im" || ev.ChannelType == "mpim" || strings.HasPrefix(strings.ToUpper(channel), "D")) && f.DMReplies != nil {
		client = f.DMReplies
	}
	if client == nil {
		return ThreadContext{}, fmt.Errorf("slack context fetch unavailable: no Slack read token/client")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultContextFetchLimit
	}
	msgs, err := client.Replies(ctx, channel, threadTS, "", limit)
	if err != nil {
		return ThreadContext{}, fmt.Errorf("slack context fetch: %w", err)
	}
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   monitor.ThreadKey(channel, threadTS),
		FetchStatus: "ok",
	}
	parentIdx := -1
	for i, msg := range msgs {
		if strings.TrimSpace(msg.TS) == threadTS {
			parentIdx = i
			break
		}
	}
	if parentIdx < 0 && len(msgs) > 0 {
		parentIdx = 0
	}
	if parentIdx >= 0 {
		m := slackContextMessage(ctx, f, msgs[parentIdx], "parent")
		pack.Parent = &m
		pack.AttachmentPaths = append(pack.AttachmentPaths, slackMessageAttachmentPaths(msgs[parentIdx])...)
	}
	if pack.Permalink == "" {
		if f.Permalink != nil {
			pack.Permalink = f.Permalink(ctx, channel, threadTS)
		}
		if pack.Permalink == "" {
			pack.Permalink = strings.TrimSpace(ev.URL)
		}
	}
	for i, msg := range msgs {
		if i == parentIdx {
			continue
		}
		if strings.TrimSpace(msg.TS) == "" || strings.TrimSpace(msg.DisplayText()) == "" {
			continue
		}
		pack.Messages = append(pack.Messages, slackContextMessage(ctx, f, msg, "reply"))
		pack.AttachmentPaths = append(pack.AttachmentPaths, slackMessageAttachmentPaths(msg)...)
	}
	if pack.Parent == nil {
		pack.Parent = &ContextMessage{
			Kind:      "parent",
			Author:    strings.TrimSpace(ev.UserID),
			Text:      cleanContextText(ctx, f.CleanText, ev.Text),
			TS:        threadTS,
			Permalink: pack.Permalink,
		}
	}
	return normalizeThreadContext(pack, ev), nil
}

func slackContextMessage(ctx context.Context, f SlackContextFetcher, msg monitor.SlackMessage, kind string) ContextMessage {
	ts := strings.TrimSpace(msg.TS)
	return ContextMessage{
		Kind:   kind,
		Author: strings.TrimSpace(msg.User),
		Text:   cleanContextText(ctx, f.CleanText, msg.DisplayText()),
		TS:     ts,
	}
}

func slackMessageAttachmentPaths(msg monitor.SlackMessage) []string {
	var paths []string
	for _, file := range msg.Files {
		if path := strings.TrimSpace(file.LocalPath); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// GitHubContextFetcher deterministically reads a PR/issue body plus comments
// through gh api. It returns errors for missing gh/auth so the cascade can
// record a marked fallback pack.
type GitHubContextFetcher struct {
	Limit int
}

var githubContextRunner = func(ctx context.Context, endpoint string) ([]byte, error) {
	args := []string{"api", "-X", "GET", endpoint, "-f", "per_page=100"}
	out, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w (output: %s)", endpoint, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (f GitHubContextFetcher) FetchContext(ctx context.Context, ev monitor.InboundEvent) (ThreadContext, error) {
	ref, err := parseGitHubContextRef(ev)
	if err != nil {
		return ThreadContext{}, err
	}
	parentRaw, err := githubContextRunner(ctx, fmt.Sprintf("repos/%s/%s/issues/%d", ref.owner, ref.repo, ref.number))
	if err != nil {
		return ThreadContext{}, fmt.Errorf("github context fetch: %w", err)
	}
	var parent githubIssueContextRecord
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		return ThreadContext{}, fmt.Errorf("github context parent parse: %w", err)
	}
	pack := ThreadContext{
		Source:      "github",
		ThreadKey:   monitor.ThreadKey(ref.owner+"/"+ref.repo, ref.linkTag),
		Permalink:   firstContextString(parent.HTMLURL, strings.TrimSpace(ev.URL)),
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind:      "parent",
			Author:    strings.TrimSpace(parent.User.Login),
			Text:      joinTitleBody(parent.Title, parent.Body),
			TS:        firstContextString(parent.CreatedAt, parent.UpdatedAt),
			Permalink: firstContextString(parent.HTMLURL, strings.TrimSpace(ev.URL)),
		},
	}
	comments, err := fetchGitHubContextComments(ctx, fmt.Sprintf("repos/%s/%s/issues/%d/comments", ref.owner, ref.repo, ref.number), "comment")
	if err != nil {
		return ThreadContext{}, err
	}
	pack.Messages = append(pack.Messages, comments...)
	if ref.isPR {
		reviews, err := fetchGitHubContextReviews(ctx, fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", ref.owner, ref.repo, ref.number))
		if err != nil {
			return ThreadContext{}, err
		}
		pack.Messages = append(pack.Messages, reviews...)
		reviewComments, err := fetchGitHubContextComments(ctx, fmt.Sprintf("repos/%s/%s/pulls/%d/comments", ref.owner, ref.repo, ref.number), "review_comment")
		if err != nil {
			return ThreadContext{}, err
		}
		pack.Messages = append(pack.Messages, reviewComments...)
	}
	sort.SliceStable(pack.Messages, func(i, j int) bool {
		return pack.Messages[i].TS < pack.Messages[j].TS
	})
	if f.Limit > 0 && len(pack.Messages) > f.Limit {
		pack.Messages = pack.Messages[len(pack.Messages)-f.Limit:]
	}
	return normalizeThreadContext(pack, ev), nil
}

type githubContextRef struct {
	owner   string
	repo    string
	number  int
	isPR    bool
	linkTag string
}

func parseGitHubContextRef(ev monitor.InboundEvent) (githubContextRef, error) {
	tag := strings.TrimSpace(ev.ThreadTS)
	if tag == "" {
		_, tag = splitThreadKeyFirst(monitor.ThreadKey(ev.Channel, ev.ThreadTS))
	}
	var isPR bool
	switch {
	case strings.HasPrefix(tag, "gh-pr:"):
		isPR = true
		tag = strings.TrimPrefix(tag, "gh-pr:")
	case strings.HasPrefix(tag, "gh-issue:"):
		isPR = false
		tag = strings.TrimPrefix(tag, "gh-issue:")
	default:
		return githubContextRef{}, fmt.Errorf("github context fetch unavailable: unsupported thread tag %q", ev.ThreadTS)
	}
	repoKey, numText, ok := strings.Cut(tag, "#")
	if !ok {
		return githubContextRef{}, fmt.Errorf("github context fetch unavailable: malformed thread tag %q", ev.ThreadTS)
	}
	owner, repo, ok := strings.Cut(repoKey, "/")
	if !ok {
		return githubContextRef{}, fmt.Errorf("github context fetch unavailable: malformed repo %q", repoKey)
	}
	if !githubSlugSegment.MatchString(owner) || !githubSlugSegment.MatchString(repo) {
		return githubContextRef{}, fmt.Errorf("github context fetch unavailable: invalid owner/repo %q/%q", owner, repo)
	}
	n, err := strconv.Atoi(numText)
	if err != nil || n <= 0 {
		return githubContextRef{}, fmt.Errorf("github context fetch unavailable: malformed issue number %q", numText)
	}
	prefix := "gh-issue:"
	if isPR {
		prefix = "gh-pr:"
	}
	return githubContextRef{owner: owner, repo: repo, number: n, isPR: isPR, linkTag: prefix + repoKey + "#" + numText}, nil
}

type githubIssueContextRecord struct {
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	HTMLURL   string            `json:"html_url"`
	User      githubContextUser `json:"user"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

type githubContextUser struct {
	Login string `json:"login"`
}

type githubCommentContextRecord struct {
	Body        string            `json:"body"`
	HTMLURL     string            `json:"html_url"`
	User        githubContextUser `json:"user"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	SubmittedAt string            `json:"submitted_at"`
	State       string            `json:"state"`
}

func fetchGitHubContextComments(ctx context.Context, endpoint, kind string) ([]ContextMessage, error) {
	raw, err := githubContextRunner(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("github context fetch: %w", err)
	}
	var records []githubCommentContextRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("github context comments parse: %w", err)
	}
	out := make([]ContextMessage, 0, len(records))
	for _, r := range records {
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		out = append(out, ContextMessage{
			Kind:      kind,
			Author:    strings.TrimSpace(r.User.Login),
			Text:      strings.TrimSpace(r.Body),
			TS:        firstContextString(r.CreatedAt, r.UpdatedAt, r.SubmittedAt),
			Permalink: strings.TrimSpace(r.HTMLURL),
		})
	}
	return out, nil
}

func fetchGitHubContextReviews(ctx context.Context, endpoint string) ([]ContextMessage, error) {
	raw, err := githubContextRunner(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("github context fetch: %w", err)
	}
	var records []githubCommentContextRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("github context reviews parse: %w", err)
	}
	out := make([]ContextMessage, 0, len(records))
	for _, r := range records {
		body := strings.TrimSpace(r.Body)
		if body == "" {
			continue
		}
		kind := "review"
		if state := strings.ToLower(strings.TrimSpace(r.State)); state != "" {
			kind = "review_" + state
		}
		out = append(out, ContextMessage{
			Kind:      kind,
			Author:    strings.TrimSpace(r.User.Login),
			Text:      body,
			TS:        firstContextString(r.SubmittedAt, r.CreatedAt, r.UpdatedAt),
			Permalink: strings.TrimSpace(r.HTMLURL),
		})
	}
	return out, nil
}

func normalizeThreadContext(pack ThreadContext, ev monitor.InboundEvent) ThreadContext {
	if pack.Source == "" {
		pack.Source = connectorOf(ev)
	}
	if pack.ThreadKey == "" {
		pack.ThreadKey = monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	}
	if pack.Permalink == "" {
		pack.Permalink = strings.TrimSpace(ev.URL)
	}
	if pack.FetchStatus == "" {
		pack.FetchStatus = "ok"
	}
	pack.Participants, pack.Timestamps = deriveContextMeta(pack.Parent, pack.Messages)
	if pack.Summary == "" {
		pack.Summary = summarizeThreadContext(pack.Source, pack.Parent, pack.Messages)
	}
	return pack
}

func fallbackThreadContext(ev monitor.InboundEvent, status, errText, cleanedText string) ThreadContext {
	threadKey := monitor.ThreadKey(ev.Channel, ev.ThreadTS)
	parent := &ContextMessage{
		Kind:      "event",
		Author:    strings.TrimSpace(ev.UserID),
		Text:      strings.TrimSpace(cleanedText),
		TS:        firstContextString(ev.TS, ev.ThreadTS),
		Permalink: strings.TrimSpace(ev.URL),
	}
	pack := ThreadContext{
		Source:      connectorOf(ev),
		ThreadKey:   threadKey,
		Permalink:   strings.TrimSpace(ev.URL),
		Parent:      parent,
		FetchStatus: status,
		FetchError:  strings.TrimSpace(errText),
	}
	pack.Participants, pack.Timestamps = deriveContextMeta(pack.Parent, pack.Messages)
	pack.Summary = summarizeThreadContext(pack.Source, pack.Parent, pack.Messages)
	return pack
}

func deriveContextMeta(parent *ContextMessage, messages []ContextMessage) ([]string, []string) {
	var participants []string
	var timestamps []string
	seenParticipants := map[string]bool{}
	seenTimestamps := map[string]bool{}
	add := func(m ContextMessage) {
		if a := strings.TrimSpace(m.Author); a != "" && !seenParticipants[a] {
			seenParticipants[a] = true
			participants = append(participants, a)
		}
		if ts := strings.TrimSpace(m.TS); ts != "" && !seenTimestamps[ts] {
			seenTimestamps[ts] = true
			timestamps = append(timestamps, ts)
		}
	}
	if parent != nil {
		add(*parent)
	}
	for _, m := range messages {
		add(m)
	}
	return participants, timestamps
}

func summarizeThreadContext(source string, parent *ContextMessage, messages []ContextMessage) string {
	count := len(messages)
	if parent != nil && strings.TrimSpace(parent.Text) != "" {
		count++
	}
	label := "messages"
	if count == 1 {
		label = "message"
	}
	src := "source"
	switch source {
	case "slack":
		src = "Slack"
	case "github":
		src = "GitHub"
	}
	participants, _ := deriveContextMeta(parent, messages)
	if len(participants) == 0 {
		return fmt.Sprintf("%d %s %s", count, src, label)
	}
	if len(participants) > 4 {
		participants = append(participants[:4], fmt.Sprintf("+%d more", len(participants)-4))
	}
	return fmt.Sprintf("%d %s %s from %s", count, src, label, strings.Join(participants, ", "))
}

func cleanContextText(ctx context.Context, clean func(context.Context, string) string, text string) string {
	if clean != nil {
		return clean(ctx, text)
	}
	return strings.TrimSpace(text)
}

func firstContextString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func joinTitleBody(title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	switch {
	case title == "":
		return body
	case body == "":
		return title
	default:
		return title + "\n\n" + body
	}
}
