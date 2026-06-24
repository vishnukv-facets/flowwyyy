package workevents

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"flow/internal/inbox"
	"flow/internal/productdb"
)

// Build derives normalized assistant work events from existing Flow storage.
// It is a read model: callers can render or answer from the result without
// mutating inbox.jsonl, attention_feed, or task state.
func Build(db *sql.DB, flowRoot string, filter Filter) (Result, error) {
	_ = flowRoot // inbox.ReadInboxEntries already resolves FLOW_ROOT/HOME.
	if db == nil {
		return Result{}, fmt.Errorf("workevents: db is required")
	}
	tasks, err := productdb.ListTasks(db, productdb.TaskFilter{IncludeArchived: false})
	if err != nil {
		return Result{}, err
	}
	bySlug := make(map[string]*productdb.Task, len(tasks))
	for _, task := range tasks {
		bySlug[task.Slug] = task
	}

	var items []Event
	attention, err := attentionEvents(db, bySlug)
	if err != nil {
		return Result{}, err
	}
	items = append(items, attention...)
	items = append(items, taskStateEvents(db, tasks)...)
	items = append(items, inboxEvents(tasks)...)

	items = filterAndSort(items, filter)
	return Result{Items: items, Counts: Count(items)}, nil
}

func attentionEvents(db *sql.DB, tasks map[string]*productdb.Task) ([]Event, error) {
	rows, err := productdb.ListFeedItems(db, "new")
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		task := tasks[row.MatchedTask]
		project := row.SuggestedProject
		if project == "" && task != nil && task.ProjectSlug.Valid {
			project = task.ProjectSlug.String
		}
		links := validLinks([]Link{
			{Kind: "attention", Target: row.ID},
			taskLink(task),
			{Kind: "source", Target: row.URL, URL: row.URL},
		})
		if trace, err := productdb.GetSteeringTraceByFeedItem(db, row.ID); err == nil && trace.ID != "" {
			links = append(links, Link{Kind: "trace", Target: trace.ID})
		}
		out = append(out, Event{
			ID:          "attention:" + row.ID,
			Source:      firstNonEmpty(row.Source, "attention"),
			Kind:        "attention",
			ThreadKey:   row.ThreadKey,
			URL:         row.URL,
			Title:       firstNonEmpty(row.Summary, "Attention item "+row.ID),
			Summary:     row.Summary,
			Actor:       row.Author,
			OccurredAt:  row.TS,
			ObservedAt:  row.CreatedAt,
			TaskSlug:    row.MatchedTask,
			ProjectSlug: project,
			EntityKind:  "attention",
			EntityRef:   row.ID,
			Bucket:      BucketNeedsAction,
			Urgency:     row.Urgency,
			Confidence:  row.Confidence,
			ReasonCode:  "attention_unresolved",
			ReasonText:  firstNonEmpty(row.Reason, "Unresolved attention item needs an operator decision."),
			Links:       links,
		})
	}
	return out, nil
}

func taskStateEvents(db *sql.DB, tasks []*productdb.Task) []Event {
	var out []Event
	for _, task := range tasks {
		if task == nil || (task.Kind != "" && task.Kind != "regular") {
			continue
		}
		project := taskProject(task)
		if task.Status != "done" && task.WaitingOn.Valid && strings.TrimSpace(task.WaitingOn.String) != "" {
			out = append(out, Event{
				ID:          "task:" + task.Slug + ":waiting",
				Source:      "flow",
				Kind:        "waiting",
				Title:       task.Name,
				Summary:     "waiting on " + strings.TrimSpace(task.WaitingOn.String),
				ObservedAt:  task.UpdatedAt,
				TaskSlug:    task.Slug,
				ProjectSlug: project,
				EntityKind:  "task",
				EntityRef:   task.Slug,
				Bucket:      BucketWaiting,
				Urgency:     "blocked",
				ReasonCode:  "task_waiting_on",
				ReasonText:  "Task is waiting on " + strings.TrimSpace(task.WaitingOn.String) + ".",
				Links:       validLinks([]Link{taskLink(task)}),
			})
			continue
		}
		if task.Status == "backlog" && task.Priority == "high" {
			blocker, err := productdb.TaskStartBlockerFor(db, task)
			if err == nil && blocker == nil {
				out = append(out, Event{
					ID:          "task:" + task.Slug + ":next-up",
					Source:      "flow",
					Kind:        "ready",
					Title:       task.Name,
					Summary:     "high-priority backlog is startable",
					ObservedAt:  task.UpdatedAt,
					TaskSlug:    task.Slug,
					ProjectSlug: project,
					EntityKind:  "task",
					EntityRef:   task.Slug,
					Bucket:      BucketNextUp,
					Urgency:     "high",
					ReasonCode:  "task_high_priority_startable",
					ReasonText:  "High-priority backlog task has no start blocker.",
					Links:       validLinks([]Link{taskLink(task)}),
				})
			}
		}
	}
	return out
}

func inboxEvents(tasks []*productdb.Task) []Event {
	var out []Event
	for _, task := range tasks {
		if task == nil {
			continue
		}
		entries, err := inbox.ReadInboxEntries(task.Slug)
		if err != nil {
			continue
		}
		terminalPR := terminalGitHubPRIndexes(entries)
		for i, entry := range entries {
			source := strings.TrimSpace(entry.Meta.Source)
			if source == "" || source == "unknown" {
				source = inbox.ClassifyInboxEvent(entry.Event).Source
			}
			if source == "unknown" {
				source = ""
			}
			if source == "github" && githubActionSuperseded(entry.Event, i, terminalPR) {
				continue
			}
			bucket, reasonCode, reasonText := classifyInboxEvent(task, entry.Event, source)
			ev := entry.Event
			out = append(out, Event{
				ID:             fmt.Sprintf("inbox:%s:%d", task.Slug, i),
				Source:         source,
				Kind:           firstNonEmpty(ev.Kind, "inbox"),
				EventKey:       inboxEventKey(task.Slug, entry),
				ThreadKey:      inbox.ThreadKey(ev.Channel, ev.ThreadTS),
				URL:            ev.URL,
				Title:          inboxTitle(ev),
				Summary:        strings.TrimSpace(ev.Text),
				Actor:          firstNonEmpty(ev.UserID, ev.ItemAuthor),
				OccurredAt:     firstNonEmpty(ev.TS, entry.EnqueuedAt),
				ObservedAt:     entry.EnqueuedAt,
				TaskSlug:       task.Slug,
				ProjectSlug:    taskProject(task),
				EntityKind:     inboxEntityKind(ev, source),
				EntityRef:      firstNonEmpty(ev.ThreadTS, task.Slug),
				Bucket:         bucket,
				ReasonCode:     reasonCode,
				ReasonText:     reasonText,
				Links:          validLinks([]Link{taskLink(task), {Kind: "source", Target: ev.URL, URL: ev.URL}}),
				AuthoredBySelf: false,
			})
		}
	}
	return out
}

func terminalGitHubPRIndexes(entries []inbox.InboxEntry) map[string]int {
	out := map[string]int{}
	for i, entry := range entries {
		ev := entry.Event
		if !githubTerminalKind(ev.Kind) {
			continue
		}
		key := githubPRKey(ev)
		if key == "" {
			continue
		}
		if prev, ok := out[key]; !ok || i > prev {
			out[key] = i
		}
	}
	return out
}

func githubActionSuperseded(ev inbox.InboundEvent, index int, terminalPR map[string]int) bool {
	if !githubActionKind(ev.Kind) {
		return false
	}
	key := githubPRKey(ev)
	if key == "" {
		return false
	}
	terminalIndex, ok := terminalPR[key]
	return ok && terminalIndex > index
}

func githubTerminalKind(kind string) bool {
	return kind == "pr_merged" || kind == "pr_closed"
}

func githubActionKind(kind string) bool {
	switch kind {
	case "pr_head_updated", "pr_review_requested", "pr_review_comment", "pr_review_changes_requested", "pr_comment":
		return true
	default:
		return false
	}
}

func githubPRKey(ev inbox.InboundEvent) string {
	if strings.HasPrefix(ev.ThreadTS, "gh-pr:") {
		return ev.ThreadTS
	}
	if strings.Contains(ev.URL, "/pull/") {
		return ev.URL
	}
	return ""
}

func classifyInboxEvent(task *productdb.Task, ev inbox.InboundEvent, source string) (Bucket, string, string) {
	if source == "github" {
		switch ev.Kind {
		case "pr_head_updated":
			return BucketNeedsAction, "github_task_linked_pr_head_updated", "Task-linked PR changed after prior review."
		case "pr_review_requested":
			return BucketNeedsAction, "github_task_linked_review_requested", "Task-linked PR review was requested."
		case "pr_merged":
			if taskIsDone(task) {
				return BucketHandled, "github_task_linked_pr_merged_done", "Task-linked PR merged and the Flow task is already closed."
			}
			return BucketCloseout, "github_task_linked_pr_merged", "Task-linked PR merged; verify and close out the Flow task."
		case "pr_closed":
			if taskIsDone(task) {
				return BucketHandled, "github_task_linked_pr_closed_done", "Task-linked PR closed and the Flow task is already closed."
			}
			return BucketCloseout, "github_task_linked_pr_closed", "Task-linked PR closed; verify and close out the Flow task."
		case "pr_review_changes_requested", "pr_review_comment", "pr_comment", "issue_comment":
			return BucketNeedsAction, "github_task_linked_comment", "Task-linked GitHub activity needs review."
		case "pr_involved", "issue_involved":
			return BucketFYI, "github_involved_fyi", "GitHub involvement was recorded as FYI."
		default:
			if task != nil && task.Slug != "" {
				return BucketFYI, "github_activity_fyi", "Task-linked GitHub activity was recorded."
			}
		}
	}
	return BucketFYI, "inbox_activity_fyi", "Task inbox activity was recorded."
}

func taskIsDone(task *productdb.Task) bool {
	return task != nil && task.Status == "done"
}

func filterAndSort(items []Event, filter Filter) []Event {
	out := make([]Event, 0, len(items))
	for _, it := range items {
		if filter.Matches(it) {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ObservedAt != out[j].ObservedAt {
			return out[i].ObservedAt > out[j].ObservedAt
		}
		return out[i].ID > out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}

func validLinks(in []Link) []Link {
	out := make([]Link, 0, len(in))
	for _, l := range in {
		if l.Valid() {
			out = append(out, l)
		}
	}
	return out
}

func taskLink(task *productdb.Task) Link {
	if task == nil {
		return Link{}
	}
	return Link{Kind: "task", Target: task.Slug}
}

func taskProject(task *productdb.Task) string {
	if task != nil && task.ProjectSlug.Valid {
		return task.ProjectSlug.String
	}
	return ""
}

func inboxEntityKind(ev inbox.InboundEvent, source string) string {
	if source == "github" {
		if strings.HasPrefix(ev.ThreadTS, "gh-pr:") || strings.HasPrefix(ev.Kind, "pr_") {
			return "pr"
		}
		if strings.HasPrefix(ev.ThreadTS, "gh-issue:") || strings.HasPrefix(ev.Kind, "issue_") {
			return "issue"
		}
	}
	return "thread"
}

func inboxEventKey(taskSlug string, entry inbox.InboxEntry) string {
	ev := entry.Event
	if strings.TrimSpace(ev.EventKey) != "" {
		return strings.Join([]string{taskSlug, ev.EventKey}, ":")
	}
	return strings.Join([]string{taskSlug, ev.Kind, ev.Channel, ev.ThreadTS, ev.TS, entry.EnqueuedAt}, ":")
}

func inboxTitle(ev inbox.InboundEvent) string {
	switch ev.Kind {
	case "pr_review_requested":
		return "PR review requested"
	case "pr_review_comment":
		return "PR review comment"
	case "pr_review_changes_requested":
		return "PR changes requested"
	case "pr_head_updated":
		return "PR head updated"
	case "pr_merged":
		return "PR merged"
	case "issue_opened":
		return "Issue opened"
	case "issue_comment":
		return "Issue comment"
	case "pr_mentioned":
		return "PR mention"
	case "issue_mentioned":
		return "Issue mention"
	case "pr_involved":
		return "PR involvement"
	case "issue_involved":
		return "Issue involvement"
	case "message":
		return "Slack message"
	case "app_mention":
		return "Slack mention"
	}
	if ev.Kind != "" {
		return strings.ReplaceAll(ev.Kind, "_", " ")
	}
	return "Inbox event"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
