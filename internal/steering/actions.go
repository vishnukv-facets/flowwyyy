package steering

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// taskSpawner shells out to `flow spawn` to create a task from a feed item
// (mirrors the monitor.spawnFlowTask seam). Mockable in tests.
var taskSpawner = func(ctx context.Context, name, slug, brief, project string) error {
	args := []string{"spawn", name, "--slug", slug, "--priority", "high", "--prompt", brief, "--no-open", "--agent", "claude"}
	if p := strings.TrimSpace(project); p != "" {
		args = append(args, "--project", p)
	}
	cmd := exec.CommandContext(ctx, "flow", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("steering: flow spawn: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// taskTeller shells out to `flow tell` to forward a context block into an
// existing task's inbox. Mockable in tests.
var taskTeller = func(ctx context.Context, slug, message string) error {
	cmd := exec.CommandContext(ctx, "flow", "tell", slug, message)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("steering: flow tell %s: %w (output: %s)", slug, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// taskTagger shells out to `flow update task <slug> --tag <tag>` (mirrors
// monitor.tagFlowTask). Mockable in tests.
var taskTagger = func(ctx context.Context, slug, tag string) error {
	cmd := exec.CommandContext(ctx, "flow", "update", "task", slug, "--tag", tag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("steering: flow update task --tag %s: %w (output: %s)", tag, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// feedTrackingTag returns the linkage tag that ties a spawned task back to its
// source thread, so a later reply on that thread routes home (autoResolveWaitingOn
// + inbox-wake). It mirrors what the dispatchers look up:
//
//   - Slack:  "slack-thread:<channel>:<thread_ts>" — the feed item's ThreadKey
//     is already "<channel>:<thread_ts>", so we just prefix it.
//   - GitHub: the bare link tag "gh-pr:owner/repo#N" / "gh-issue:owner/repo#N".
//     The feed item's ThreadKey for GitHub is the composite
//     "owner/repo:gh-pr:owner/repo#N", so we slice from the link-tag marker.
//
// Returns "" when no deterministic linkage can be derived (the caller then
// skips tagging rather than inventing a tag).
func feedTrackingTag(item flowdb.FeedItem) string {
	key := strings.TrimSpace(item.ThreadKey)
	if key == "" {
		return ""
	}
	for _, marker := range []string{"gh-pr:", "gh-issue:"} {
		if i := strings.Index(key, marker); i >= 0 {
			return key[i:] // bare link tag, exactly what findTaskByGitHubTag matches
		}
	}
	if strings.EqualFold(strings.TrimSpace(item.Source), "github") {
		return "" // github-sourced but no recognizable link tag — don't guess
	}
	return monitor.SlackThreadTagPrefix + key
}

// MakeTaskFromFeed spawns a flow task from a feed item's pre-assembled context
// pack, tags it with its source thread so future replies route back, and marks
// the feed row 'acted'. Idempotent: if the deterministic task slug already exists
// (e.g. a retried action), it reuses that task instead of spawning a duplicate
// (which would hit the UNIQUE constraint on tasks.slug).
func MakeTaskFromFeed(ctx context.Context, db *sql.DB, item flowdb.FeedItem) error {
	slug := FeedTaskSlug(item)
	if !taskSlugExists(db, slug) {
		if err := taskSpawner(ctx, feedTaskName(item), slug, feedTaskBrief(item), item.SuggestedProject); err != nil {
			return err
		}
	}
	tagSourceThread(ctx, slug, item)
	return flowdb.SetFeedItemActed(db, item.ID, slug, nowRFC3339())
}

// taskSlugExists reports whether a task row already holds this slug — in ANY
// state (active, archived, or soft-deleted), since the UNIQUE constraint covers
// the column regardless. GetTask has no archived/deleted filter, so a nil error
// means the slug is taken. A nil db is treated as "doesn't exist".
func taskSlugExists(db *sql.DB, slug string) bool {
	if db == nil {
		return false
	}
	_, err := flowdb.GetTask(db, slug)
	return err == nil
}

// tagSourceThread best-effort tags a freshly spawned task with its source-thread
// linkage tag. Failure is non-fatal: the task still exists and is usable; it just
// won't auto-route in-thread replies until tagged. We log to stderr rather than
// abort the action.
func tagSourceThread(ctx context.Context, slug string, item flowdb.FeedItem) {
	tag := feedTrackingTag(item)
	if tag == "" {
		return
	}
	if err := taskTagger(ctx, slug, tag); err != nil {
		fmt.Fprintf(os.Stderr, "steering: tag source thread on %s: %v\n", slug, err)
	}
}

// ForwardFeed hands a summarized context block to the matched task's inbox via
// `flow tell` and marks the feed row 'acted'. Requires item.MatchedTask.
func ForwardFeed(ctx context.Context, db *sql.DB, item flowdb.FeedItem) error {
	target := strings.TrimSpace(item.MatchedTask)
	if target == "" {
		return fmt.Errorf("steering: forward requires a matched_task on feed item %q", item.ID)
	}
	if err := taskTeller(ctx, target, feedForwardMessage(item)); err != nil {
		return err
	}
	return flowdb.SetFeedItemActed(db, item.ID, target, nowRFC3339())
}

// DismissFeed marks a feed row 'dismissed' (no external effect).
func DismissFeed(db *sql.DB, id string) error {
	return flowdb.SetFeedItemStatus(db, id, "dismissed", nowRFC3339())
}

// InjectReplyToTask injects a "send this reply" instruction into an existing
// task's inbox/session (the agent posts it via its MCP tools) and marks the
// feed item acted + linked to that task. The agent sends — never the server.
func InjectReplyToTask(ctx context.Context, db *sql.DB, item flowdb.FeedItem, text, targetSlug, instructions string) error {
	if err := taskTeller(ctx, targetSlug, feedReplyInstruction(item, text, instructions)); err != nil {
		return err
	}
	// Record a durable note in the matched task's updates/ so its agent (and any
	// future session) knows what reply went out on this thread — the inbox
	// instruction above is transient and consumed, whereas updates/ is the
	// permanent progress log read at every SessionStart. Best-effort: a write
	// failure must not fail the reply (the inbox injection still carries it), so
	// we log and continue.
	if err := recordReplyUpdate(targetSlug, item, text, instructions); err != nil {
		fmt.Fprintf(os.Stderr, "steering: record reply update on %s: %v\n", targetSlug, err)
	}
	return flowdb.SetFeedItemActed(db, item.ID, targetSlug, nowRFC3339())
}

// recordReplyUpdate writes a date-stamped progress note into the task's updates/
// directory recording the reply dispatched from the attention feed. The filename
// carries the time so two replies on the same day don't clobber each other.
func recordReplyUpdate(slug string, item flowdb.FeedItem, text, instructions string) error {
	dir := strings.TrimSpace(monitor.TaskDir(slug))
	if dir == "" {
		return fmt.Errorf("cannot resolve task dir for %q", slug)
	}
	updatesDir := filepath.Join(dir, "updates")
	if err := os.MkdirAll(updatesDir, 0o755); err != nil {
		return err
	}
	now := time.Now()
	path := filepath.Join(updatesDir, now.Format("2006-01-02")+"-attention-reply-"+now.Format("150405")+".md")
	var b strings.Builder
	fmt.Fprintf(&b, "# Reply sent via attention router — %s\n\n", now.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Posted to %s thread `%s`.\n", item.Source, item.ThreadKey)
	if ch := strings.TrimSpace(item.Channel); ch != "" {
		fmt.Fprintf(&b, "Channel: %s\n", ch)
	}
	fmt.Fprintf(&b, "\n## Reply\n%s\n", strings.TrimSpace(text))
	if ins := strings.TrimSpace(instructions); ins != "" {
		fmt.Fprintf(&b, "\n## Operator revision instructions (applied before posting)\n%s\n", ins)
	}
	b.WriteString("\n---\n*Recorded automatically when the reply was dispatched from the attention feed.*\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// MakeReplyTaskFromFeed spawns a fresh task whose job is to post the reply, then
// marks the feed item acted + linked. Returns the new slug so the caller can
// open the session (the agent posts from there). The agent sends — never the
// server.
func MakeReplyTaskFromFeed(ctx context.Context, db *sql.DB, item flowdb.FeedItem, text string) (string, error) {
	slug := FeedTaskSlug(item)
	if taskSlugExists(db, slug) {
		// The task already exists (e.g. a retried Send reply, or a make_task ran
		// first) — inject the reply into it instead of spawning a duplicate, which
		// would fail on the UNIQUE tasks.slug constraint. That agent already has
		// the thread context and posts via its own MCP tools.
		if err := taskTeller(ctx, slug, feedReplyInstruction(item, text, "")); err != nil {
			return "", err
		}
		if err := flowdb.SetFeedItemActed(db, item.ID, slug, nowRFC3339()); err != nil {
			return slug, err
		}
		return slug, nil
	}
	if err := taskSpawner(ctx, feedTaskName(item), slug, feedReplyTaskBrief(item, text), item.SuggestedProject); err != nil {
		return "", err
	}
	tagSourceThread(ctx, slug, item)
	if err := flowdb.SetFeedItemActed(db, item.ID, slug, nowRFC3339()); err != nil {
		return slug, err
	}
	return slug, nil
}

// feedReplyInstruction is the inbox message handed to an existing session.
// instructions is optional extra operator guidance; when present the agent
// revises the draft per it before posting (rather than posting verbatim).
func feedReplyInstruction(item flowdb.FeedItem, text, instructions string) string {
	base := fmt.Sprintf(
		"The attention router drafted this reply for you to SEND now. Post it to the source — %s thread %s — using your MCP tools (Slack/GitHub), threaded appropriately. Do not ask for confirmation; the operator already approved sending.\n\nDraft reply:\n%s",
		item.Source, item.ThreadKey, strings.TrimSpace(text))
	if ins := strings.TrimSpace(instructions); ins != "" {
		base += "\n\nThe operator also gave these instructions — apply them to the reply before posting:\n" + ins
	} else {
		base += "\n\nKeep the intent; tighten wording only if needed."
	}
	return base
}

// feedReplyTaskBrief is the brief for a freshly-spawned reply task.
func feedReplyTaskBrief(item flowdb.FeedItem, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", feedTaskName(item))
	b.WriteString("## What\nPost the reply below to the source thread, then mark this task done.\n")
	b.WriteString("The operator has already approved sending this reply via the attention feed — send it (don't re-ask), tightening wording only if clearly needed.\n\n")
	fmt.Fprintf(&b, "## Reply to send\n%s\n\n", strings.TrimSpace(text))
	fmt.Fprintf(&b, "## Source\nthread: %s (%s)\n", item.ThreadKey, item.Source)
	if strings.TrimSpace(item.Channel) != "" {
		fmt.Fprintf(&b, "channel: %s\n", item.Channel)
	}
	b.WriteString("\n## How to send\nUse your MCP tools for this source (Slack MCP for slack, the `gh` CLI / GitHub MCP for github) to post the reply threaded to the source message. Confirm it posted, save a brief progress note, then `flow done` this task.\n")
	b.WriteString("\n---\n*Created from the attention feed (send-reply). You send it from this session — the server does not post on your behalf.*\n")
	return b.String()
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// feedTaskName is a short task title derived from the summary (or the thread
// key when there's no summary).
func feedTaskName(item flowdb.FeedItem) string {
	if s := strings.TrimSpace(item.Summary); s != "" {
		if len(s) > 60 {
			s = strings.TrimSpace(s[:60])
		}
		return s
	}
	return "Attention: " + item.ThreadKey
}

// FeedTaskSlug derives a stable, filesystem-safe slug from the thread key. It
// is deterministic ("att-<thread>") so callers can recover the slug a feed item
// would spawn without consulting the DB.
func FeedTaskSlug(item flowdb.FeedItem) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(item.ThreadKey) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "thread"
	}
	return "att-" + s
}

// feedTaskBrief assembles the context-pack brief for a new task (spec §8.2).
func feedTaskBrief(item flowdb.FeedItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", feedTaskName(item))
	summary := strings.TrimSpace(item.Summary)
	if summary == "" {
		summary = "Follow up on a message surfaced by the attention router."
	}
	fmt.Fprintf(&b, "## What\n%s\n\n", summary)
	fmt.Fprintf(&b, "## Why\nSurfaced by the attention router from %s.\n", item.Source)
	if r := strings.TrimSpace(item.Reason); r != "" {
		fmt.Fprintf(&b, "Reason flagged: %s\n", r)
	}
	fmt.Fprintf(&b, "\n## Source\nthread: %s (%s)\n", item.ThreadKey, item.Source)
	if d := strings.TrimSpace(item.Draft); d != "" {
		fmt.Fprintf(&b, "\n## Suggested reply (draft — review before sending)\n%s\n", d)
	}
	b.WriteString("\n---\n*Created from the attention feed. Read the linked thread before acting.*\n")
	return b.String()
}

// feedForwardMessage is the summarized context block forwarded to a matched
// task's inbox (spec §8.3).
func feedForwardMessage(item flowdb.FeedItem) string {
	var b strings.Builder
	b.WriteString("Forwarded by the attention router.\n")
	if s := strings.TrimSpace(item.Summary); s != "" {
		fmt.Fprintf(&b, "Summary: %s\n", s)
	}
	fmt.Fprintf(&b, "Source thread: %s (%s)\n", item.ThreadKey, item.Source)
	if r := strings.TrimSpace(item.Reason); r != "" {
		fmt.Fprintf(&b, "Why it may relate: %s\n", r)
	}
	return b.String()
}

// ErrAutonomyDenied is returned when an autonomous (non-manual) action is
// blocked by the autonomy policy.
var ErrAutonomyDenied = errors.New("steering: action denied by autonomy policy")

// ApplyAction performs action on a feed item. manual=true (operator-initiated)
// bypasses the autonomy gate — the operator IS the authorization. manual=false
// (autonomous) must pass autonomy.Allow(action, item.Confidence) or it returns
// ErrAutonomyDenied without side effects. Only make_task and forward are
// supported in P1.3; reply/afk_reply (outward sends) arrive in P2.
func ApplyAction(ctx context.Context, db *sql.DB, item flowdb.FeedItem, action Action, autonomy AutonomyPolicy, manual bool) error {
	if !manual && !autonomy.Allow(action, item.Confidence) {
		return ErrAutonomyDenied
	}
	switch action {
	case ActionMakeTask:
		return MakeTaskFromFeed(ctx, db, item)
	case ActionForward:
		return ForwardFeed(ctx, db, item)
	default:
		return fmt.Errorf("steering: action %q not supported in P1.3 (make_task/forward only)", action)
	}
}
