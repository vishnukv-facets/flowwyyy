package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
)

type ClickUpDispatcher struct {
	DB                 *sql.DB
	Opener             TaskOpener
	Steerer            MessageObserver
	SteererOwnsRouting func() bool
}

func NewClickUpDispatcher(db *sql.DB, opener TaskOpener) *ClickUpDispatcher {
	return &ClickUpDispatcher{DB: db, Opener: opener}
}

func (d *ClickUpDispatcher) Dispatch(ctx context.Context, ev ClickUpEvent) error {
	if d == nil || d.DB == nil || ev.LinkTag() == "" {
		return nil
	}
	if key := ev.EventKeyValue(); key != "" {
		seen, err := flowdb.HasClickUpEvent(d.DB, key)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
	}
	if d.steererOwnsRouting() {
		if err := d.Steerer.Observe(ctx, clickUpEventToInboxEvent(ev)); err != nil {
			return fmt.Errorf("clickup steerer observe: %w", err)
		}
		return d.recordEvent(ev, "")
	}
	if d.Steerer != nil {
		if err := d.Steerer.Observe(ctx, clickUpEventToInboxEvent(ev)); err != nil {
			fmt.Fprintf(os.Stderr, "clickup steerer observe: %v\n", err)
		}
	}
	return d.dispatchClickUpEvent(ctx, ev)
}

func (d *ClickUpDispatcher) steererOwnsRouting() bool {
	return d != nil && d.Steerer != nil && d.SteererOwnsRouting != nil && d.SteererOwnsRouting()
}

func (d *ClickUpDispatcher) dispatchClickUpEvent(ctx context.Context, ev ClickUpEvent) error {
	slug, found, err := d.findTaskByClickUpTag(ev.LinkTag())
	if err != nil {
		return fmt.Errorf("clickup monitor: lookup task by tag: %w", err)
	}
	if !found {
		slug, err = d.createClickUpTask(ctx, ev)
		if err != nil {
			return fmt.Errorf("clickup monitor: create task: %w", err)
		}
	}
	if err := AppendInboxEvent(slug, clickUpEventToInboxEvent(ev)); err != nil {
		return fmt.Errorf("clickup monitor: append inbox: %w", err)
	}
	if err := d.recordEvent(ev, slug); err != nil {
		return err
	}
	if !found && clickUpAutoOpenEnabled() {
		if d.Opener != nil {
			if err := d.Opener.OpenInUI(slug); err != nil {
				fmt.Fprintf(os.Stderr, "clickup monitor: open in UI: %v\n", err)
			}
		} else {
			_ = openSlackReplyTask(slug)
		}
	}
	return nil
}

func (d *ClickUpDispatcher) findTaskByClickUpTag(tag string) (string, bool, error) {
	tag = flowdb.NormalizeTag(tag)
	if tag == "" {
		return "", false, nil
	}
	tasks, err := flowdb.ListTasks(d.DB, flowdb.TaskFilter{Tag: tag, IncludeArchived: true})
	if err != nil {
		return "", false, err
	}
	if len(tasks) == 0 {
		return "", false, nil
	}
	best := ""
	bestScore := -1
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if s := githubTaskRoutingScore(task); s > bestScore {
			bestScore = s
			best = task.Slug
		}
	}
	if best == "" {
		best = tasks[0].Slug
	}
	return best, true, nil
}

func (d *ClickUpDispatcher) createClickUpTask(ctx context.Context, ev ClickUpEvent) (string, error) {
	slug := ClickUpSlugForEvent(ev)
	if slug == "" {
		return "", fmt.Errorf("cannot derive clickup task slug")
	}
	brief := clickUpTaskBrief(ev)
	provider, fellBack, ok := ResolveProvider("claude")
	if !ok {
		return "", fmt.Errorf("monitor: cannot start session — neither Claude Code nor Codex is installed")
	}
	if err := spawnFlowTask(ctx, clickUpTaskName(ev), slug, brief, provider, ""); err != nil {
		return "", err
	}
	if fellBack {
		providerNotice(slug, fmt.Sprintf(
			"%s isn't installed on this machine — started this session with %s instead.",
			ProviderDisplayName("claude"), ProviderDisplayName(provider),
		))
	}
	for _, tag := range []string{"clickup", ev.LinkTag()} {
		if err := tagFlowTask(ctx, slug, tag); err != nil {
			return slug, err
		}
	}
	return slug, nil
}

func (d *ClickUpDispatcher) recordEvent(ev ClickUpEvent, slug string) error {
	key := ev.EventKeyValue()
	if key == "" {
		return nil
	}
	taskSlug := strings.TrimSpace(slug)
	if taskSlug != "" {
		if _, err := flowdb.GetTask(d.DB, taskSlug); err != nil {
			taskSlug = ""
		}
	}
	_, err := flowdb.RecordClickUpEvent(d.DB, flowdb.ClickUpEventLogEntry{
		EventKey:  key,
		EventKind: string(ev.Kind),
		TaskSlug:  taskSlug,
		RawJSON:   ev.RawJSON,
	})
	return err
}

func clickUpTaskName(ev ClickUpEvent) string {
	title := strings.TrimSpace(ev.TaskName)
	if title == "" {
		title = strings.TrimSpace(ev.TaskURL)
	}
	if title == "" {
		title = strings.TrimSpace(ev.TaskID)
	}
	return "ClickUp " + title
}

func clickUpTaskBrief(ev ClickUpEvent) string {
	var b strings.Builder
	title := strings.TrimSpace(ev.TaskName)
	if title == "" {
		title = clickUpTaskName(ev)
	}
	b.WriteString("# " + title + "\n\n")
	b.WriteString("## What\n")
	fmt.Fprintf(&b, "ClickUp task: %s\n", strings.TrimSpace(ev.TaskID))
	if ev.TaskURL != "" {
		fmt.Fprintf(&b, "url: %s\n", strings.TrimSpace(ev.TaskURL))
	}
	if ev.Author != "" {
		fmt.Fprintf(&b, "latest author: %s\n", strings.TrimSpace(ev.Author))
	}
	if ev.Body != "" {
		b.WriteString("\n## Latest ClickUp event\n")
		fmt.Fprintf(&b, "%s: %s\n", ev.Kind, strings.TrimSpace(ev.Body))
	}
	b.WriteString("\n## Inbox\n")
	dir := TaskDir(ClickUpSlugForEvent(ev))
	if dir == "" {
		dir = "~/.flow/tasks/" + ClickUpSlugForEvent(ev)
	}
	fmt.Fprintf(&b, "ClickUp events for this task are appended to:\n  %s/inbox.jsonl\n", dir)
	b.WriteString("\n## Done when\n")
	b.WriteString("- The ClickUp task has been reviewed, answered, or otherwise handled.\n")
	b.WriteString("- Relevant comments or decisions have been captured in flow before closing.\n")
	b.WriteString("\n## Tags\n")
	fmt.Fprintf(&b, "clickup, %s\n", ev.LinkTag())
	b.WriteString("\n---\n*ClickUp-origin task. The ClickUp webhook inside flow ui serve writes incoming events to inbox.jsonl as they arrive.*\n")
	return b.String()
}

func clickUpAutoOpenEnabled() bool {
	return envBoolDefault("FLOW_CLICKUP_AUTOOPEN", true)
}
