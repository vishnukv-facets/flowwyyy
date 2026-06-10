package briefing

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"flow/internal/flowdb"
)

// Options controls briefing assembly. Since bounds Overnight activity; Now and
// StaleAfter make tests deterministic and keep stale detection local to this
// package rather than tied to Mission Control's process/liveness cache.
type Options struct {
	Now        time.Time
	Since      time.Time
	StaleAfter time.Duration
	Limit      int
	// WaitingSessions are live agents paused for the operator's input. The
	// briefing builder has no view of process liveness, so the server resolves
	// these from the runtime agent snapshot and passes them in to rank as tier-1
	// NeedsYou rows. Empty for the CLI standup, which runs without a live server.
	WaitingSessions []WaitingSession
}

// WaitingSession describes a live agent that is blocked on the operator. It is
// deliberately minimal: the server already holds the rich runtime view, and the
// briefing only needs enough to render and link a NeedsYou row.
type WaitingSession struct {
	TaskSlug string
	Name     string
	Project  string
	Detail   string
}

// Briefing is the shared on-demand/status-startup digest shape, organised as
// three ranked tiers that answer, in priority order: what needs me now, what
// changed while I was away, and what I should pick up next. The operator scans
// top-to-bottom and stops once the things that are actually on them are handled.
type Briefing struct {
	GeneratedAt string `json:"generated_at"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	// NeedsYou (tier 1) is work where you are the bottleneck: attention cards
	// awaiting a decision or reply, and tasks you own that are waiting on
	// someone (you still have to chase them). Ranked most-urgent first.
	NeedsYou []Item `json:"needs_you"`
	// Overnight (tier 2) is what changed since you last looked: tasks shipped,
	// update notes written, and digest-only attention. Read-only awareness — it
	// should never steal the top of the queue from NeedsYou.
	Overnight []Item `json:"overnight"`
	// NextUp (tier 3) is what to start or resume next: startable high-priority
	// backlog and in-progress sessions that have gone cold.
	NextUp []Item `json:"next_up"`
}

// Item is one briefing row. Source, Project, and Urgency are first-class so
// consumers can group cards without parsing the title.
type Item struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Source  string `json:"source,omitempty"`
	Project string `json:"project,omitempty"`
	Urgency string `json:"urgency,omitempty"`
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Action  string `json:"action,omitempty"`
	Links   []Link `json:"links,omitempty"`
}

// Link is intentionally small: Target is the stable internal id/slug/url, URL is
// set only when the target is externally openable.
type Link struct {
	Kind   string `json:"kind"`
	Label  string `json:"label,omitempty"`
	Target string `json:"target"`
	URL    string `json:"url,omitempty"`
}

// Build assembles a deterministic briefing from DB rows and markdown update
// files under flowRoot.
func Build(db *sql.DB, flowRoot string, opts Options) (Briefing, error) {
	if db == nil {
		return Briefing{}, errors.New("briefing: db is required")
	}
	opts = normalizeOptions(opts)
	out := Briefing{
		GeneratedAt: opts.Now.Format(time.RFC3339),
		WindowStart: opts.Since.Format(time.RFC3339),
		WindowEnd:   opts.Now.Format(time.RFC3339),
		NeedsYou:    []Item{},
		Overnight:   []Item{},
		NextUp:      []Item{},
	}

	projects, err := projectNames(db)
	if err != nil {
		return Briefing{}, err
	}
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Kind: "", IncludeArchived: false})
	if err != nil {
		return Briefing{}, err
	}
	taskBySlug := make(map[string]*flowdb.Task, len(tasks))
	for _, task := range tasks {
		taskBySlug[task.Slug] = task
	}

	attention, err := attentionItems(db, taskBySlug)
	if err != nil {
		return Briefing{}, err
	}
	// Tier 1 (NeedsYou): attention you must act on, plus attention nudges that
	// affect a waiting task (the wait may now be resolvable). Tier 2 (Overnight):
	// digest-only attention is awareness, not action.
	out.NeedsYou = append(out.NeedsYou, attention.NeedsAction...)
	out.NeedsYou = append(out.NeedsYou, attention.Waiting...)
	out.Overnight = append(out.Overnight, attention.FYI...)
	for _, item := range taskActionItems(db, tasks, projects, opts) {
		switch item.Kind {
		case "ready", "stale":
			// Startable backlog and cold in-progress sessions are both "what to
			// pick up next" — resume-first ordering is handled by actionRank.
			out.NextUp = append(out.NextUp, item)
		default:
			// "waiting" (you own it, chase it) and anything else lands on you.
			out.NeedsYou = append(out.NeedsYou, item)
		}
	}
	out.Overnight = append(out.Overnight, shippedItems(tasks, projects, opts)...)
	out.Overnight = append(out.Overnight, updateItems(flowRoot, taskBySlug, projects, opts)...)

	// A live agent paused for your input is the most literal "needs you" there
	// is. Append after the task-derived items so dedup can suppress a session
	// whose task is already surfaced (an attention card or waiting_on row).
	out.NeedsYou = append(out.NeedsYou, waitingSessionItems(opts.WaitingSessions, out.NeedsYou)...)

	sortItems(out.NeedsYou, true)
	sortItems(out.Overnight, false)
	sortItems(out.NextUp, true)
	if opts.Limit > 0 {
		out.NeedsYou = limitItems(out.NeedsYou, opts.Limit)
		out.Overnight = limitItems(out.Overnight, opts.Limit)
		out.NextUp = limitItems(out.NextUp, opts.Limit)
	}
	return out, nil
}

func normalizeOptions(opts Options) Options {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Since.IsZero() {
		opts.Since = opts.Now.Add(-24 * time.Hour)
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 72 * time.Hour
	}
	return opts
}

func projectNames(db *sql.DB) (map[string]string, error) {
	projects, err := flowdb.ListProjects(db, flowdb.ProjectFilter{IncludeArchived: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(projects))
	for _, p := range projects {
		out[p.Slug] = p.Name
	}
	return out, nil
}

type attentionBuckets struct {
	NeedsAction []Item
	Waiting     []Item
	FYI         []Item
}

func attentionItems(db *sql.DB, tasks map[string]*flowdb.Task) (attentionBuckets, error) {
	rows, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		return attentionBuckets{}, err
	}
	out := attentionBuckets{
		NeedsAction: []Item{},
		Waiting:     []Item{},
		FYI:         []Item{},
	}
	for _, row := range rows {
		item := Item{
			Kind:    "attention",
			Ref:     row.ID,
			Source:  nonEmpty(row.Source, "attention"),
			Project: row.SuggestedProject,
			Urgency: row.Urgency,
			Title:   nonEmpty(row.Summary, "Attention item "+row.ID),
			Detail:  row.Reason,
			Action:  readableAttentionAction(row.SuggestedAction),
			Links:   []Link{{Kind: "attention", Target: row.ID}},
		}
		if item.Project == "" {
			if task := tasks[row.MatchedTask]; task != nil && task.ProjectSlug.Valid {
				item.Project = task.ProjectSlug.String
			}
		}
		if row.MatchedTask != "" {
			item.Links = append(item.Links, Link{Kind: "task", Target: row.MatchedTask})
		}
		if row.URL != "" {
			item.Links = append(item.Links, Link{Kind: "source", Target: row.URL, URL: row.URL})
		}
		if trace, err := flowdb.GetSteeringTraceByFeedItem(db, row.ID); err == nil && trace.ID != "" {
			item.Links = append(item.Links, Link{Kind: "trace", Target: trace.ID})
		}
		waitingTask := matchedWaitingTask(row, tasks)
		if attentionNoAction(row.SuggestedAction) {
			if waitingTask != nil {
				item.Action = "Review affected task"
				item.Detail = waitingAttentionDetail(waitingTask, row.Reason)
				out.Waiting = append(out.Waiting, item)
				continue
			}
			out.FYI = append(out.FYI, item)
			continue
		}
		if waitingTask != nil && !attentionReplyAction(row.SuggestedAction) {
			item.Action = "Review affected task"
			item.Detail = waitingAttentionDetail(waitingTask, row.Reason)
			out.Waiting = append(out.Waiting, item)
			continue
		}
		out.NeedsAction = append(out.NeedsAction, item)
	}
	return out, nil
}

func matchedWaitingTask(row flowdb.FeedItem, tasks map[string]*flowdb.Task) *flowdb.Task {
	task := tasks[row.MatchedTask]
	if task == nil || task.Status == "done" || !task.WaitingOn.Valid {
		return nil
	}
	if strings.TrimSpace(task.WaitingOn.String) == "" {
		return nil
	}
	return task
}

func attentionNoAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "digest_only", "drop", "no_action", "none":
		return true
	default:
		return false
	}
}

func attentionReplyAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "reply", "send_reply", "afk_reply":
		return true
	default:
		return false
	}
}

func readableAttentionAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "digest_only", "drop", "no_action", "none":
		return "No action"
	case "forward":
		return "Forward to task"
	case "make_task":
		return "Make task"
	case "reply", "send_reply", "afk_reply":
		return "Review reply"
	default:
		return strings.ReplaceAll(strings.TrimSpace(action), "_", " ")
	}
}

func waitingAttentionDetail(task *flowdb.Task, reason string) string {
	detail := "Affects waiting task: " + task.Name
	if reason = strings.TrimSpace(reason); reason != "" {
		detail += " - " + reason
	}
	return detail
}

func taskActionItems(db *sql.DB, tasks []*flowdb.Task, projects map[string]string, opts Options) []Item {
	var out []Item
	for _, task := range tasks {
		if task.Kind != "" && task.Kind != "regular" {
			continue
		}
		project := taskProject(task, projects)
		if task.Status != "done" && task.WaitingOn.Valid && strings.TrimSpace(task.WaitingOn.String) != "" {
			out = append(out, Item{
				Kind: "waiting", Ref: task.Slug, Source: "task", Project: project,
				Urgency: "blocked", Title: task.Name, Detail: "waiting on " + strings.TrimSpace(task.WaitingOn.String),
				Links: []Link{{Kind: "task", Target: task.Slug}, sessionLink(task)},
			})
			continue
		}
		if task.Status == "in-progress" && staleTask(task, opts) {
			out = append(out, Item{
				Kind: "stale", Ref: task.Slug, Source: "task", Project: project,
				Urgency: "stale", Title: task.Name, Detail: "no task update in " + ageLabel(task.UpdatedAt, opts.Now),
				Links: []Link{{Kind: "task", Target: task.Slug}, sessionLink(task)},
			})
			continue
		}
		if task.Status == "backlog" && task.Priority == "high" {
			blocker, err := flowdb.TaskStartBlockerFor(db, task)
			if err == nil && blocker == nil {
				out = append(out, Item{
					Kind: "ready", Ref: task.Slug, Source: "task", Project: project,
					Urgency: "high", Title: task.Name, Detail: "high-priority backlog is startable",
					Links: []Link{{Kind: "task", Target: task.Slug}},
				})
			}
		}
	}
	return out
}

// waitingSessionItems maps live waiting agents into NeedsYou rows, skipping any
// whose task is already surfaced by an attention card or waiting_on row (the
// existing row already links the same task/session, so a second is just noise).
func waitingSessionItems(sessions []WaitingSession, existing []Item) []Item {
	if len(sessions) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, item := range existing {
		if item.Ref != "" {
			seen[item.Ref] = true
		}
		for _, link := range item.Links {
			if link.Kind == "task" || link.Kind == "session" {
				seen[link.Target] = true
			}
		}
	}
	var out []Item
	for _, ws := range sessions {
		slug := strings.TrimSpace(ws.TaskSlug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		detail := strings.TrimSpace(ws.Detail)
		if detail == "" {
			detail = "agent is paused for your input"
		}
		out = append(out, Item{
			Kind:    "session",
			Ref:     slug,
			Source:  "session",
			Project: strings.TrimSpace(ws.Project),
			Urgency: "urgent",
			Title:   nonEmpty(ws.Name, slug),
			Detail:  detail,
			Action:  "Send input",
			Links:   []Link{{Kind: "session", Target: slug}, {Kind: "task", Target: slug}},
		})
	}
	return out
}

func shippedItems(tasks []*flowdb.Task, projects map[string]string, opts Options) []Item {
	var out []Item
	for _, task := range tasks {
		if task.Kind != "" && task.Kind != "regular" {
			continue
		}
		if task.Status != "done" || !taskInWindow(task, opts) {
			continue
		}
		out = append(out, Item{
			Kind: "shipped", Ref: task.Slug, Source: "task", Project: taskProject(task, projects),
			Title: task.Name, Detail: "completed " + firstNonEmpty(task.StatusChangedAt.String, task.UpdatedAt),
			Links: []Link{{Kind: "task", Target: task.Slug}},
		})
	}
	return out
}

func updateItems(flowRoot string, tasks map[string]*flowdb.Task, projects map[string]string, opts Options) []Item {
	if strings.TrimSpace(flowRoot) == "" {
		return nil
	}
	var out []Item
	out = append(out, taskUpdateItems(flowRoot, tasks, projects, opts)...)
	out = append(out, projectUpdateItems(flowRoot, projects, opts)...)
	return out
}

func taskUpdateItems(flowRoot string, tasks map[string]*flowdb.Task, projects map[string]string, opts Options) []Item {
	base := filepath.Join(flowRoot, "tasks")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []Item
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		task := tasks[slug]
		if task == nil {
			continue
		}
		project := taskProject(task, projects)
		for _, file := range updateFilesInWindow(filepath.Join(base, slug, "updates"), opts) {
			links := []Link{{Kind: "update", Target: file.Path}}
			links = append([]Link{{Kind: "task", Target: slug}}, links...)
			out = append(out, Item{
				Kind: "update", Ref: slug, Source: "task", Project: project,
				Title: updateTitle(file.Path), Detail: file.Name,
				Links: links,
			})
		}
	}
	return out
}

func projectUpdateItems(flowRoot string, projects map[string]string, opts Options) []Item {
	base := filepath.Join(flowRoot, "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []Item
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		for _, file := range updateFilesInWindow(filepath.Join(base, slug, "updates"), opts) {
			out = append(out, Item{
				Kind: "update", Ref: slug, Source: "project", Project: nonEmpty(projects[slug], slug),
				Title: updateTitle(file.Path), Detail: file.Name,
				Links: []Link{{Kind: "project", Target: slug}, {Kind: "update", Target: file.Path}},
			})
		}
	}
	return out
}

type updateFile struct {
	Name string
	Path string
}

func updateFilesInWindow(dir string, opts Options) []updateFile {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []updateFile
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		if !updateFilenameInWindow(f.Name(), opts) {
			continue
		}
		out = append(out, updateFile{Name: f.Name(), Path: filepath.Join(dir, f.Name())})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func updateFilenameInWindow(name string, opts Options) bool {
	if len(name) < len("2006-01-02") {
		return false
	}
	day, err := time.ParseInLocation("2006-01-02", name[:10], opts.Now.Location())
	if err != nil {
		return false
	}
	sinceDay := time.Date(opts.Since.Year(), opts.Since.Month(), opts.Since.Day(), 0, 0, 0, 0, opts.Now.Location())
	nowDay := time.Date(opts.Now.Year(), opts.Now.Month(), opts.Now.Day(), 0, 0, 0, 0, opts.Now.Location())
	return !day.Before(sinceDay) && !day.After(nowDay)
}

func updateTitle(path string) string {
	data, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if line != "" {
				return line
			}
		}
	}
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	if len(base) > 11 && base[4] == '-' && base[7] == '-' {
		base = base[11:]
	}
	return strings.ReplaceAll(base, "-", " ")
}

func taskInWindow(task *flowdb.Task, opts Options) bool {
	ts := firstNonEmpty(task.StatusChangedAt.String, task.UpdatedAt)
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return !t.Before(opts.Since) && !t.After(opts.Now)
}

func staleTask(task *flowdb.Task, opts Options) bool {
	t, err := time.Parse(time.RFC3339, task.UpdatedAt)
	if err != nil {
		return false
	}
	return opts.Now.Sub(t) >= opts.StaleAfter
}

func taskProject(task *flowdb.Task, projects map[string]string) string {
	if task != nil && task.ProjectSlug.Valid {
		if name := strings.TrimSpace(projects[task.ProjectSlug.String]); name != "" {
			return task.ProjectSlug.String
		}
		return task.ProjectSlug.String
	}
	return "(floating)"
}

func sessionLink(task *flowdb.Task) Link {
	return Link{Kind: "session", Target: task.Slug}
}

func ageLabel(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "a while"
	}
	days := int(now.Sub(t).Hours() / 24)
	if days <= 0 {
		return "less than a day"
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

func sortItems(items []Item, action bool) {
	sort.SliceStable(items, func(i, j int) bool {
		if action {
			ri, rj := actionRank(items[i]), actionRank(items[j])
			if ri != rj {
				return ri < rj
			}
		}
		pi, pj := priorityRank(items[i].Urgency), priorityRank(items[j].Urgency)
		if pi != pj {
			return pi < pj
		}
		if items[i].Project != items[j].Project {
			return items[i].Project < items[j].Project
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Ref < items[j].Ref
	})
}

func actionRank(item Item) int {
	switch item.Kind {
	case "attention":
		return 0
	case "session":
		// A live agent paused for your input ranks just below attention cards
		// and above waiting-on-others tasks — one keystroke unblocks it.
		return 1
	case "waiting":
		return 2
	case "stale":
		return 3
	case "ready":
		return 4
	default:
		return 9
	}
}

func priorityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "urgent", "blocked":
		return 0
	case "high":
		return 1
	case "stale":
		return 2
	case "medium":
		return 3
	case "low":
		return 4
	default:
		return 5
	}
}

func limitItems(items []Item, limit int) []Item {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	return items[:limit]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}
