package server

import (
	"bufio"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// readInboxEntries parses ~/.flow/tasks/<slug>/inbox.md into structured
// entries. The format is the one `flow tell <slug>` writes — `## TIMESTAMP
// — from: WHO` headers separating message bodies. This is the per-task
// agent-to-agent inbox, distinct from any external notification system.
func readInboxEntries(path string) ([]InboxEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []InboxEntry
	var current *InboxEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") && strings.Contains(line, " — from: ") {
			if current != nil {
				current.Body = strings.TrimSpace(current.Body)
				entries = append(entries, *current)
			}
			head := strings.TrimPrefix(line, "## ")
			parts := strings.SplitN(head, " — from: ", 2)
			current = &InboxEntry{Timestamp: parts[0]}
			if len(parts) == 2 {
				current.Sender = parts[1]
			}
			continue
		}
		if current == nil {
			continue // pre-header preamble (`# Inbox` etc.)
		}
		current.Body += line + "\n"
	}
	if err := scanner.Err(); err != nil {
		return entries, err
	}
	if current != nil {
		current.Body = strings.TrimSpace(current.Body)
		entries = append(entries, *current)
	}
	return entries, nil
}

// unreadInboxCount returns how many entries are newer than the task's
// inbox_seen_at watermark. Entries whose timestamps fail to parse are
// skipped (treated as "old enough") to avoid over-counting on malformed
// lines.
func unreadInboxCount(task *flowdb.Task, entries []InboxEntry) int {
	if len(entries) == 0 {
		return 0
	}
	if !task.InboxSeenAt.Valid || strings.TrimSpace(task.InboxSeenAt.String) == "" {
		return len(entries)
	}
	seenAt, err := time.Parse(time.RFC3339, task.InboxSeenAt.String)
	if err != nil {
		return len(entries)
	}
	count := 0
	for _, e := range entries {
		entryAt, err := time.Parse("2006-01-02 15:04:05Z", e.Timestamp)
		if err != nil {
			continue
		}
		if entryAt.After(seenAt) {
			count++
		}
	}
	return count
}

// inboxFeedBodySnippet trims an inbox body for list-view display. We keep
// the full body in the response so the UI can expand on demand, but the
// list itself shouldn't carry an unbounded blob per row.
func inboxFeedBodySnippet(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		body = body[:idx]
	}
	if len(body) > 220 {
		body = body[:220] + "…"
	}
	return body
}

// inboxFeedTimestampSortKey normalises a timestamp string for sort/compare.
// Handles inbox.md headers ("2006-01-02 15:04:05Z") AND inbox.jsonl
// enqueued_at (RFC3339). Falls back to the raw string on parse failure.
func inboxFeedTimestampSortKey(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	if t, err := time.Parse("2006-01-02 15:04:05Z", ts); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return ts
}

// inboxJSONLTitle returns a short human-readable title for an InboundEvent
// — used as the row "from" label and sort-stable title in the global feed.
// Kept terse: "pr_review_requested" → "PR review requested".
func inboxJSONLTitle(ev monitor.InboundEvent) string {
	kind := strings.TrimSpace(ev.Kind)
	if kind == "" {
		return "event"
	}
	switch kind {
	case "pr_review_requested":
		return "PR review requested"
	case "pr_review_comment":
		return "PR review comment"
	case "pr_review_changes_requested":
		return "PR changes requested"
	case "pr_head_updated":
		return "PR head updated"
	case "issue_opened":
		return "Issue opened"
	case "issue_comment":
		return "Issue comment"
	case "message":
		return "Slack message"
	case "app_mention":
		return "Slack mention"
	case "reaction_added":
		if ev.Reaction != "" {
			return "Reaction :" + ev.Reaction + ":"
		}
		return "Reaction"
	}
	// Fallback: humanise the snake_case kind.
	return strings.ReplaceAll(kind, "_", " ")
}

// inboxJSONLSender picks the most useful "from" label for a structured
// event. Falls back to the channel when no user id is attached
// (PR head_updated, CI-triggered events, etc.).
func inboxJSONLSender(ev monitor.InboundEvent) string {
	if u := strings.TrimSpace(ev.UserID); u != "" {
		return u
	}
	if a := strings.TrimSpace(ev.ItemAuthor); a != "" {
		return a
	}
	if c := strings.TrimSpace(ev.Channel); c != "" {
		return c
	}
	if c := strings.TrimSpace(ev.ChannelType); c != "" {
		return c
	}
	return "unknown"
}

// inboxJSONLEntryBody renders an event body for the feed snippet. Title +
// blank line + Text preserves PR descriptions and Slack messages
// in their original form; pure reaction events just get the title.
func inboxJSONLEntryBody(ev monitor.InboundEvent) string {
	title := inboxJSONLTitle(ev)
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return title
	}
	return title + "\n\n" + text
}

// handleInbox aggregates every non-archived task's inbox.jsonl into a
// single feed. inbox.jsonl is the structured event log appended by the
// monitor listeners — one line per Slack message or GitHub PR event
// (monitor.InboundEvent shape).
//
// The legacy per-task inbox.md (free-form "flow tell" notes) is
// deliberately not aggregated here: it was a dumping ground for the
// pre-d2900a0 attention-monitor lifecycle that has since been removed.
// The per-task /api/tasks/<slug>/inbox endpoint still surfaces it for
// the task detail drawer.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if r.URL.Path != "/api/inbox" {
		http.NotFound(w, r)
		return
	}
	tasks, err := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{IncludeArchived: false})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	entries := make([]InboxFeedEntry, 0, 64)
	unread := 0
	taskCount := 0
	for _, t := range tasks {
		jsonlEntries, jerr := monitor.ReadInboxEntries(t.Slug)
		if jerr != nil || len(jsonlEntries) == 0 {
			continue
		}
		var project *string
		if t.ProjectSlug.Valid && t.ProjectSlug.String != "" {
			ps := t.ProjectSlug.String
			project = &ps
		}
		var seenAt time.Time
		hasSeen := false
		if t.InboxSeenAt.Valid && strings.TrimSpace(t.InboxSeenAt.String) != "" {
			if v, perr := time.Parse(time.RFC3339, t.InboxSeenAt.String); perr == nil {
				seenAt = v
				hasSeen = true
			}
		}
		// A task with an active or completed agent has effectively been
		// "read" by that agent — the events were delivered into its session.
		// Only backlog tasks (no agent yet) should count entries as unread.
		agentHandled := t.Status == "in-progress" || t.Status == "done"
		taskCount++
		for _, je := range jsonlEntries {
			// enqueued_at is the wall-clock when the listener received the
			// event — preferred for "what's new on the inbox" sorting since
			// it's both monotonic-ish and human-readable.
			ts := strings.TrimSpace(je.EnqueuedAt)
			if ts == "" {
				ts = strings.TrimSpace(je.Event.TS)
			}
			isUnread := false
			if !agentHandled {
				isUnread = !hasSeen
				if hasSeen {
					if entryAt, perr := time.Parse(time.RFC3339, ts); perr == nil && entryAt.After(seenAt) {
						isUnread = true
					}
				}
			}
			if isUnread {
				unread++
			}
			body := inboxJSONLEntryBody(je.Event)
			entries = append(entries, InboxFeedEntry{
				TaskSlug:    t.Slug,
				TaskName:    t.Name,
				ProjectSlug: project,
				Status:      t.Status,
				Timestamp:   ts,
				Sender:      inboxJSONLSender(je.Event),
				Body:        body,
				BodySnippet: inboxFeedBodySnippet(body),
				Unread:      isUnread,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return inboxFeedTimestampSortKey(entries[i].Timestamp) > inboxFeedTimestampSortKey(entries[j].Timestamp)
	})
	writeJSON(w, InboxFeed{
		Entries:     entries,
		UnreadCount: unread,
		TaskCount:   taskCount,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	})
}
