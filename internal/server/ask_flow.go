package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"flow/internal/productdb"
	"flow/internal/workevents"
)

const askFlowMaxCitations = 12

type askFlowChangedRow struct {
	task TaskView
	file FileRef
}

func (s *Server) handleAskFlow(w http.ResponseWriter, r *http.Request) {
	var req AskFlowRequest
	switch r.Method {
	case http.MethodPost:
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	case http.MethodGet:
		req.Query = r.URL.Query().Get("q")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, errors.New("query is required"), http.StatusBadRequest)
		return
	}
	resp, err := s.answerAskFlow(r.Context(), req.Query)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) answerAskFlow(ctx context.Context, query string) (AskFlowResponse, error) {
	intent := classifyAskFlowIntent(query)
	var (
		answer    string
		citations []AskFlowCitation
		err       error
	)
	switch intent {
	case "needs_action":
		answer, citations, err = s.askFlowWorkEvents(
			workevents.Filter{Bucket: workevents.BucketNeedsAction, Limit: 8},
			"Work that needs you",
			"No WorkEvents currently need your attention.",
		)
	case "closeout":
		answer, citations, err = s.askFlowWorkEvents(
			workevents.Filter{Bucket: workevents.BucketCloseout, Limit: 8},
			"Work you can close out",
			"No closeout WorkEvents found.",
		)
	case "look_now":
		answer, citations, err = s.askFlowLookNow(ctx)
	case "blockers":
		answer, citations, err = s.askFlowBlockers()
	case "stale":
		answer, citations, err = s.askFlowStale()
	case "changed":
		answer, citations, err = s.askFlowChanged(ctx)
	case "draft_replies":
		answer, citations, err = s.askFlowDraftReplies(ctx)
	case "related":
		answer, citations, err = s.askFlowSearch(query, "related")
	default:
		answer, citations, err = s.askFlowSearch(query, "search")
	}
	if err != nil {
		return AskFlowResponse{}, err
	}
	return AskFlowResponse{
		Query:     query,
		Intent:    intent,
		Answer:    answer,
		Citations: limitAskFlowCitations(dedupeAskFlowCitations(citations), askFlowMaxCitations),
	}, nil
}

func classifyAskFlowIntent(query string) string {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "needs me") || strings.Contains(q, "needs my attention") || strings.Contains(q, "need my attention"):
		return "needs_action"
	case strings.Contains(q, "what can i close") || strings.Contains(q, "can i close") || strings.Contains(q, "closeout") || strings.Contains(q, "close out"):
		return "closeout"
	case strings.Contains(q, "draft") && strings.Contains(q, "repl"):
		return "draft_replies"
	case strings.Contains(q, "blocker") || strings.Contains(q, "blocked") || strings.Contains(q, "waiting on"):
		return "blockers"
	case strings.Contains(q, "stale"):
		return "stale"
	case strings.Contains(q, "changed") || strings.Contains(q, "while i was away") || strings.Contains(q, "away"):
		return "changed"
	case strings.Contains(q, "related") || strings.Contains(q, "slack thread") || strings.Contains(q, "github thread"):
		return "related"
	case strings.Contains(q, "look at now") || strings.Contains(q, "what should") || strings.Contains(q, "triage my day") || strings.Contains(q, "work on"):
		return "look_now"
	default:
		return "search"
	}
}

func (s *Server) askFlowLookNow(ctx context.Context) (string, []AskFlowCitation, error) {
	_ = ctx
	sections := []struct {
		bucket workevents.Bucket
		label  string
	}{
		{workevents.BucketNeedsAction, "Needs action"},
		{workevents.BucketCloseout, "Closeout"},
		{workevents.BucketWaiting, "Waiting"},
		{workevents.BucketNextUp, "Next up"},
	}
	lines := []string{"Start with needs-action and closeout, then waiting or next-up work."}
	var citations []AskFlowCitation
	for _, section := range sections {
		eventLines, eventCitations, err := s.askFlowWorkEventLines(workevents.Filter{Bucket: section.bucket, Limit: 4}, nil)
		if err != nil {
			return "", nil, err
		}
		if len(eventLines) == 0 {
			continue
		}
		lines = append(lines, "", section.label+":")
		lines = append(lines, eventLines...)
		citations = append(citations, eventCitations...)
	}
	if len(citations) == 0 {
		return "No actionable WorkEvents are visible right now.", nil, nil
	}
	return strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowBlockers() (string, []AskFlowCitation, error) {
	tasks, err := s.askFlowTaskViews()
	if err != nil {
		return "", nil, err
	}
	var citations []AskFlowCitation
	var lines []string
	for _, task := range tasks {
		if task.Status == "done" {
			continue
		}
		if task.WaitingOn != nil && strings.TrimSpace(*task.WaitingOn) != "" {
			lines = append(lines, fmt.Sprintf("- %s — waiting on %s", task.Name, *task.WaitingOn))
			citations = append(citations, taskCitation(task))
		}
		for _, parent := range task.Parents {
			if parent.Status == "done" || parent.Slug == task.ParentSlugValue() {
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s — blocked by %s (%s)", task.Name, parent.Name, parent.Status))
			citations = append(citations, taskCitation(task), taskSummaryCitation(parent))
		}
	}
	if len(lines) == 0 {
		return "No open blockers found in active tasks. I checked waiting notes and task dependencies.", nil, nil
	}
	return "Open blockers:\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowStale() (string, []AskFlowCitation, error) {
	tasks, err := s.askFlowTaskViews()
	if err != nil {
		return "", nil, err
	}
	var citations []AskFlowCitation
	var lines []string
	for _, task := range tasks {
		if task.StaleDays == nil {
			continue
		}
		waiting := ""
		if task.WaitingOn != nil && *task.WaitingOn != "" {
			waiting = " and is waiting on " + *task.WaitingOn
		}
		lines = append(lines, fmt.Sprintf("- %s — stale for %d day(s)%s", task.Name, *task.StaleDays, waiting))
		citations = append(citations, taskCitation(task))
	}
	if len(lines) == 0 {
		return "No stale in-progress tasks found.", nil, nil
	}
	return "Stale work:\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowChanged(ctx context.Context) (string, []AskFlowCitation, error) {
	tasks, err := s.askFlowTaskViews()
	if err != nil {
		return "", nil, err
	}
	var rows []askFlowChangedRow
	for _, task := range tasks {
		for _, file := range task.Updates {
			rows = append(rows, askFlowChangedRow{task: task, file: file})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].file.MTime > rows[j].file.MTime })
	var citations []AskFlowCitation
	var lines []string
	for _, row := range takeChanged(rows, 6) {
		lines = append(lines, fmt.Sprintf("- %s — %s", row.task.Name, row.file.Filename))
		citations = append(citations, updateCitation(row.task, row.file))
	}
	eventLines, eventCitations, err := s.askFlowWorkEventLines(workevents.Filter{Limit: 32}, func(ev workevents.Event) bool {
		return ev.Source != "flow"
	})
	if err != nil {
		return "", nil, err
	}
	lines = append(lines, eventLines...)
	citations = append(citations, eventCitations...)
	items, err := productdb.ListFeedItems(s.cfg.DB, "new")
	if err != nil {
		return "", nil, err
	}
	for _, it := range takeFeedItems(items, 3) {
		lines = append(lines, fmt.Sprintf("- Attention: %s — %s", nonempty(it.Summary, it.ThreadKey), actionLabel(it.SuggestedAction)))
		citations = append(citations, attentionCitation(ctx, s, it))
	}
	if len(lines) == 0 {
		return "No task updates or new Attention cards were found.", nil, nil
	}
	return "Recent changes I found:\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowWorkEvents(filter workevents.Filter, heading, empty string) (string, []AskFlowCitation, error) {
	lines, citations, err := s.askFlowWorkEventLines(filter, nil)
	if err != nil {
		return "", nil, err
	}
	if len(lines) == 0 {
		return empty, nil, nil
	}
	return heading + ":\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowWorkEventLines(filter workevents.Filter, keep func(workevents.Event) bool) ([]string, []AskFlowCitation, error) {
	if filter.Limit <= 0 {
		filter.Limit = 8
	}
	result, err := workevents.Build(s.cfg.DB, s.cfg.FlowRoot, filter)
	if err != nil {
		return nil, nil, err
	}
	var lines []string
	var citations []AskFlowCitation
	for _, ev := range result.Items {
		if keep != nil && !keep(ev) {
			continue
		}
		lines = append(lines, askFlowWorkEventLine(ev))
		citations = append(citations, workEventCitations(ev)...)
	}
	return lines, citations, nil
}

func askFlowWorkEventLine(ev workevents.Event) string {
	title := nonempty(ev.Title, ev.EntityRef, ev.ID)
	if ev.TaskSlug != "" {
		title += " [" + ev.TaskSlug + "]"
	}
	reason := nonempty(ev.ReasonText, ev.Summary, ev.Kind)
	if summary := strings.TrimSpace(ev.Summary); summary != "" && summary != reason {
		reason += "; " + summary
	}
	bucket := strings.ReplaceAll(string(ev.Bucket), "_", " ")
	return fmt.Sprintf("- %s: %s — %s", bucket, title, reason)
}

func (s *Server) askFlowDraftReplies(ctx context.Context) (string, []AskFlowCitation, error) {
	items, err := productdb.ListFeedItems(s.cfg.DB, "new")
	if err != nil {
		return "", nil, err
	}
	var citations []AskFlowCitation
	var lines []string
	for _, it := range items {
		if strings.TrimSpace(it.Draft) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s\n  Draft: %s", nonempty(it.Summary, it.ThreadKey), strings.TrimSpace(it.Draft)))
		citations = append(citations, attentionCitation(ctx, s, it))
	}
	if len(lines) == 0 {
		return "No new Attention cards with draft replies found.", nil, nil
	}
	return "Draft replies from new Attention cards:\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowSearch(query, intent string) (string, []AskFlowCitation, error) {
	terms := askFlowSearchTerms(query)
	if terms == "" {
		terms = query
	}
	scopes := productdb.DefaultSearchScopes()
	s.syncSearchThrottled(scopes)
	results, err := productdb.SearchDocs(s.cfg.DB, terms, scopes, 8)
	if err != nil {
		return "", nil, err
	}
	results = append(results, s.askFlowNameMatches(terms, 8-len(results))...)
	if len(results) == 0 {
		return fmt.Sprintf("I could not find Flow records matching %q.", terms), nil, nil
	}
	var citations []AskFlowCitation
	var lines []string
	prefix := "Related Flow records"
	if intent == "search" {
		prefix = "Grounded matches"
	}
	for _, result := range results {
		c := searchCitation(result)
		lines = append(lines, fmt.Sprintf("- %s — %s", c.Title, nonempty(result.Snippet, result.Scope)))
		citations = append(citations, c)
	}
	return prefix + ":\n" + strings.Join(lines, "\n"), citations, nil
}

func (s *Server) askFlowNameMatches(q string, limit int) []productdb.SearchResult {
	if limit <= 0 {
		return nil
	}
	like := "%" + q + "%"
	rows, err := s.cfg.DB.Query(
		`SELECT slug, name, status, updated_at FROM tasks
		 WHERE name LIKE ? AND archived_at IS NULL AND deleted_at IS NULL
		 ORDER BY updated_at DESC LIMIT ?`,
		like, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []productdb.SearchResult
	for rows.Next() {
		var slug, name, status, updated string
		if err := rows.Scan(&slug, &name, &status, &updated); err != nil {
			return out
		}
		out = append(out, productdb.SearchResult{
			Type:       "task_name",
			Scope:      "name",
			EntityType: "task",
			Slug:       slug,
			Name:       name,
			Snippet:    status,
			UpdatedAt:  updated,
		})
	}
	return out
}

func (s *Server) askFlowTaskViews() ([]TaskView, error) {
	tasks, err := productdb.ListTasks(s.cfg.DB, productdb.TaskFilter{IncludeArchived: false, IncludeDeleted: false, Kind: ""})
	if err != nil {
		return nil, err
	}
	return buildTaskViewsWithLive(s.cfg.DB, s.cfg.FlowRoot, tasks, map[string]bool{})
}

func askFlowSearchTerms(query string) string {
	stop := map[string]bool{
		"a": true, "about": true, "am": true, "are": true, "flow": true, "for": true, "from": true,
		"i": true, "is": true, "me": true, "my": true, "of": true, "please": true, "show": true,
		"task": true, "tasks": true, "the": true, "this": true, "thread": true, "to": true,
		"what": true, "which": true, "who": true, "why": true, "with": true,
		"related": true, "slack": true, "github": true,
	}
	var terms []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) {
		tok = strings.Trim(tok, "-_")
		if tok == "" || stop[tok] {
			continue
		}
		terms = append(terms, tok)
	}
	return strings.Join(terms, " ")
}

func filterTaskViews(tasks []TaskView, keep func(TaskView) bool) []TaskView {
	var out []TaskView
	for _, task := range tasks {
		if keep(task) {
			out = append(out, task)
		}
	}
	return out
}

func takeTaskViews(in []TaskView, n int) []TaskView {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func takeFeedItems(in []productdb.FeedItem, n int) []productdb.FeedItem {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func takeChanged(in []askFlowChangedRow, n int) []askFlowChangedRow {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func taskCitation(task TaskView) AskFlowCitation {
	return AskFlowCitation{
		Type:  "task",
		Slug:  task.Slug,
		Title: task.Name,
		URL:   "/task/" + task.Slug,
	}
}

func taskSummaryCitation(task TaskSummary) AskFlowCitation {
	return AskFlowCitation{
		Type:  "task",
		Slug:  task.Slug,
		Title: task.Name,
		URL:   "/task/" + task.Slug,
	}
}

func updateCitation(task TaskView, file FileRef) AskFlowCitation {
	return AskFlowCitation{
		Type:       "update",
		Slug:       task.Slug,
		Title:      task.Name + " update " + file.Filename,
		URL:        "/task/" + task.Slug,
		SourcePath: file.Path,
	}
}

func attentionCitation(ctx context.Context, s *Server, it productdb.FeedItem) AskFlowCitation {
	title := nonempty(it.Summary, it.ThreadKey)
	if s != nil && s.nameResolver != nil {
		title = s.nameResolver.CleanText(ctx, title)
	}
	return AskFlowCitation{
		Type:    "attention",
		ID:      it.ID,
		Title:   title,
		URL:     "/attention",
		Snippet: nonempty(it.Reason, actionLabel(it.SuggestedAction)),
	}
}

func workEventCitations(ev workevents.Event) []AskFlowCitation {
	var citations []AskFlowCitation
	for _, link := range ev.Links {
		switch link.Kind {
		case "attention":
			citations = append(citations, AskFlowCitation{
				Type:    "attention",
				ID:      link.Target,
				Title:   nonempty(ev.Title, link.Target),
				URL:     "/attention",
				Snippet: nonempty(ev.ReasonText, ev.Summary),
			})
		case "project":
			citations = append(citations, AskFlowCitation{
				Type:    "project",
				Slug:    link.Target,
				Title:   nonempty(link.Label, ev.ProjectSlug, link.Target),
				URL:     "/project/" + url.PathEscape(link.Target),
				Snippet: nonempty(ev.ReasonText, ev.Summary),
			})
		case "source":
			target := nonempty(link.URL, link.Target, ev.URL)
			if target == "" {
				continue
			}
			citations = append(citations, AskFlowCitation{
				Type:    "source",
				ID:      target,
				Title:   nonempty(link.Label, ev.Source, "source"),
				URL:     target,
				Snippet: nonempty(ev.Summary, ev.ReasonText),
			})
		case "task":
			citations = append(citations, AskFlowCitation{
				Type:    "task",
				Slug:    link.Target,
				Title:   nonempty(link.Label, ev.Title, link.Target),
				URL:     "/session/" + url.PathEscape(link.Target),
				Snippet: nonempty(ev.ReasonText, ev.Summary),
			})
		case "trace":
			citations = append(citations, AskFlowCitation{
				Type:    "trace",
				ID:      link.Target,
				Title:   nonempty(link.Label, ev.Title+" trace", link.Target),
				URL:     "/attention",
				Snippet: nonempty(ev.ReasonText, ev.Summary),
			})
		}
	}
	if len(citations) == 0 && ev.TaskSlug != "" {
		citations = append(citations, AskFlowCitation{
			Type:    "task",
			Slug:    ev.TaskSlug,
			Title:   nonempty(ev.Title, ev.TaskSlug),
			URL:     "/session/" + url.PathEscape(ev.TaskSlug),
			Snippet: nonempty(ev.ReasonText, ev.Summary),
		})
	}
	return citations
}

func searchCitation(result productdb.SearchResult) AskFlowCitation {
	typ := result.Scope
	if result.EntityType != "" && result.Scope == string(productdb.SearchScopeBrief) {
		typ = result.EntityType
	}
	if result.EntityType == "memory" {
		typ = "memory"
	}
	if typ == "" {
		typ = result.EntityType
	}
	return AskFlowCitation{
		Type:       typ,
		Slug:       result.Slug,
		Title:      result.Name,
		URL:        searchResultURL(result.EntityType, result.Slug),
		SourcePath: result.SourcePath,
		Snippet:    result.Snippet,
	}
}

func dedupeAskFlowCitations(in []AskFlowCitation) []AskFlowCitation {
	seen := map[string]bool{}
	var out []AskFlowCitation
	for _, c := range in {
		key := c.Type + "\x00" + c.ID + "\x00" + c.Slug + "\x00" + c.SourcePath
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func limitAskFlowCitations(in []AskFlowCitation, n int) []AskFlowCitation {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func actionLabel(action string) string {
	switch strings.TrimSpace(strings.ToLower(action)) {
	case "make_task", "make-task":
		return "make a task"
	case "make_task_start", "make-task-start":
		return "make and start a task"
	case "send_reply", "send-reply", "reply":
		return "draft/send a reply"
	case "forward":
		return "forward to an existing task"
	case "confirm_handoff", "confirm-handoff":
		return "confirm task handoff"
	case "dismiss":
		return "dismiss"
	default:
		return nonempty(action, "review")
	}
}

func nonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (t TaskView) ParentSlugValue() string {
	if t.ParentSlug == nil {
		return ""
	}
	return *t.ParentSlug
}
