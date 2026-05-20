package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"flow/internal/flowdb"
)

type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

type Poller struct {
	DB              *sql.DB
	Runner          CommandRunner
	SlackAPIBaseURL string
	// OnSyncChange is invoked after each sync-state transition (start
	// AND end of a per-source poll). Optional — when nil, transitions
	// are still persisted to monitor_sync_state but no live notification
	// fires. The server's monitorPoller wires this to the eventHub so
	// /ws/events?types=monitor_sync subscribers (the Inbox UI) see the
	// "syncing now…" → "synced X ago" flip in real time. The CLI poller
	// leaves it nil since there's no eventHub in a one-shot CLI process.
	OnSyncChange func(state *flowdb.MonitorSyncState)

	// OnNewEvent fires for each event that lands in monitor_events as a
	// genuinely-new row (isNew=true from the storage policy). Re-polls
	// of an existing (source, source_id) pair never invoke this. Used to
	// push inbox_item WS events for live UI updates and (via the client)
	// macOS desktop notifications. outcome and note are populated when
	// applyRule produced them; both empty when the event is logged
	// without routing (rule mode = "log").
	OnNewEvent func(event flowdb.MonitorEvent, outcome string, note string)
}

type PollSummary struct {
	Source      string   `json:"source"`
	Events      int      `json:"events"`
	New         int      `json:"new"`
	Errors      []string `json:"errors,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
	LastSync    string   `json:"last_sync"`
}

func (p Poller) Poll(ctx context.Context, source string) ([]PollSummary, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" || source == "all" {
		sources := []string{"github"}
		out := make([]PollSummary, 0, len(sources))
		for _, src := range sources {
			sum, err := p.pollOne(ctx, src)
			if err != nil {
				sum.Source = src
				sum.Errors = append(sum.Errors, err.Error())
			}
			out = append(out, sum)
		}
		return out, nil
	}
	sum, err := p.pollOne(ctx, source)
	if err != nil {
		sum.Source = source
		sum.Errors = append(sum.Errors, err.Error())
		return []PollSummary{sum}, nil
	}
	return []PollSummary{sum}, nil
}

func (p Poller) pollOne(ctx context.Context, source string) (PollSummary, error) {
	if p.DB == nil {
		return PollSummary{}, errors.New("monitor poller has no database")
	}
	if err := flowdb.EnsureDefaultAutomationRules(p.DB); err != nil {
		return PollSummary{}, err
	}
	// Record-start: lets the Inbox UI show "syncing now" while the poll
	// is in flight. Best-effort — a failed start record (e.g. DB locked)
	// shouldn't block the poll itself; we log and continue.
	if state, err := flowdb.RecordMonitorSyncStart(p.DB, source); err != nil {
		fmt.Fprintf(os.Stderr, "warning: monitor sync start (%s): %v\n", source, err)
	} else if p.OnSyncChange != nil {
		p.OnSyncChange(state)
	}
	var summary PollSummary
	var pollErr error
	switch source {
	case "github", "gh":
		summary, pollErr = p.pollGitHub(ctx)
	case "slack":
		summary = PollSummary{
			Source:      "slack",
			LastSync:    flowdb.NowISO(),
			Diagnostics: []string{"slack polling disabled; Slack ingest runs through Socket Mode"},
		}
	default:
		// Unknown source: clear the in-flight flag so the UI doesn't
		// permanently show "syncing" for a typo.
		_, _ = flowdb.RecordMonitorSyncEnd(p.DB, source, "error", fmt.Sprintf("unsupported monitor source %q", source))
		return PollSummary{Source: source}, fmt.Errorf("unsupported monitor source %q", source)
	}
	// Record-end: write the outcome regardless of error path. Status is
	// "error" when either the poll function returned an error OR the
	// summary's Errors slice is non-empty.
	status := "ok"
	errMsg := ""
	if pollErr != nil {
		status = "error"
		errMsg = pollErr.Error()
	} else if len(summary.Errors) > 0 {
		status = "error"
		errMsg = summary.Errors[0]
	}
	if state, err := flowdb.RecordMonitorSyncEnd(p.DB, source, status, errMsg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: monitor sync end (%s): %v\n", source, err)
	} else if p.OnSyncChange != nil {
		p.OnSyncChange(state)
	}
	return summary, pollErr
}

func (p Poller) pollGitHub(ctx context.Context) (PollSummary, error) {
	sum := PollSummary{Source: "github", LastSync: flowdb.NowISO()}
	commands := []struct {
		kind string
		args []string
	}{
		{kind: "review_requested", args: []string{"pr", "list", "--search", "review-requested:@me is:open", "--json", "number,title,url,author,updatedAt"}},
		{kind: "ci_failed", args: []string{"pr", "list", "--search", "author:@me is:open", "--json", "number,title,url,statusCheckRollup,updatedAt"}},
		{kind: "assigned_issue", args: []string{"issue", "list", "--assignee", "@me", "--state", "open", "--json", "number,title,url,updatedAt"}},
	}
	for _, cmd := range commands {
		out, err := p.run(ctx, "gh", cmd.args...)
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
			continue
		}
		events, err := githubEvents(cmd.kind, out)
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
			continue
		}
		kept, newCount, err := p.storeEvents(events)
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
			continue
		}
		sum.Events += kept
		sum.New += newCount
	}
	out, err := p.run(ctx, "gh", "api", "notifications")
	if err == nil {
		events, err := githubNotifications(out)
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
		} else {
			kept, newCount, err := p.storeEvents(events)
			if err != nil {
				sum.Errors = append(sum.Errors, err.Error())
			}
			sum.Events += kept
			sum.New += newCount
		}
	}
	closed, err := p.closeMergedLinkedPRs(ctx)
	if err != nil {
		sum.Errors = append(sum.Errors, err.Error())
	} else if closed > 0 {
		sum.Events += closed
	}
	return sum, nil
}

// storePolicy selects how a source's events should land in monitor_events.
// "upsert" (default) updates existing rows on (source, source_id) conflict —
// correct for sources whose event state evolves (GitHub PR / CI). "insert_new"
// freezes existing rows and only inserts unseen ones — correct for archival
// sources where re-polling overlapping windows must not rewrite history
// (Slack messages).
type storePolicy int

const (
	storePolicyUpsert storePolicy = iota
	storePolicyInsertNew
)

func (p Poller) storeEvents(events []flowdb.MonitorEventInput) (int, int, error) {
	return p.storeEventsWithPolicy(events, storePolicyUpsert)
}

func (p Poller) StoreSlackEvents(events []flowdb.MonitorEventInput) (int, int, error) {
	return p.storeEventsWithPolicy(events, storePolicyInsertNew)
}

func (p Poller) storeEventsWithPolicy(events []flowdb.MonitorEventInput, policy storePolicy) (int, int, error) {
	kept := 0
	newCount := 0
	for _, event := range events {
		var (
			ev    *flowdb.MonitorEvent
			isNew bool
			err   error
		)
		switch policy {
		case storePolicyInsertNew:
			ev, isNew, err = flowdb.InsertMonitorEventIfNew(p.DB, event)
		default:
			ev, isNew, err = flowdb.UpsertMonitorEvent(p.DB, event)
		}
		if err != nil {
			return kept, newCount, err
		}
		if ev == nil {
			continue
		}
		kept++
		if isNew {
			newCount++
		}
		// Rule application is gated on isNew under insert-new policy so a
		// re-poll of an already-stored Slack message doesn't re-fire the
		// notification/route logic. Under upsert policy we apply on every
		// pass (existing behavior) since the row's state may have changed.
		if policy == storePolicyInsertNew && !isNew {
			continue
		}
		if err := p.applyRule(*ev); err != nil {
			return kept, newCount, err
		}
		// Fire OnNewEvent for genuinely-new arrivals only. Re-polls and
		// upsert-only updates do NOT fire — the UI's inbox_item handler
		// should treat each callback as "new row, append + maybe notify".
		// Outcome / note are not threaded through applyRule yet; pass
		// empty strings for now and let the client-side classifier
		// fall back to the event severity / kind for needs-review.
		if isNew && p.OnNewEvent != nil {
			p.OnNewEvent(*ev, "", "")
		}
	}
	return kept, newCount, nil
}

func (p Poller) closeMergedLinkedPRs(ctx context.Context) (int, error) {
	links, err := flowdb.ListOpenTaskPRLinks(p.DB)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, link := range links {
		out, err := p.run(ctx, "gh", "pr", "view", link.PRURL, "--json", "state,mergedAt,url,number,title")
		if err != nil {
			return closed, err
		}
		var data map[string]any
		if err := json.Unmarshal(out, &data); err != nil {
			return closed, fmt.Errorf("parse gh pr view for %s: %w", link.PRURL, err)
		}
		state := strings.ToUpper(stringField(data, "state"))
		mergedAt := stringField(data, "mergedAt")
		if state != "MERGED" && mergedAt == "" {
			continue
		}
		if err := flowdb.MarkTaskPRMerged(p.DB, link.TaskSlug, link.Repo, link.PRNumber, mergedAt); err != nil {
			return closed, err
		}
		if _, err := flowdb.MarkTaskDoneIfSessionBound(p.DB, link.TaskSlug); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

func (p Poller) applyRule(event flowdb.MonitorEvent) error {
	mode, err := flowdb.AutomationModeFor(p.DB, event.Source, event.Kind)
	if err != nil {
		return err
	}
	switch mode {
	case "off", "log":
		return nil
	case "approval":
		return flowdb.CreateNotificationForEvent(p.DB, event, "approval")
	case "auto_agent", "auto_agent_draft_only":
		return flowdb.CreateNotificationForEvent(p.DB, event, "success")
	case "auto_task", "summarize", "notify":
		return flowdb.CreateNotificationForEvent(p.DB, event, "info")
	default:
		return flowdb.CreateNotificationForEvent(p.DB, event, "info")
	}
}

func (p Poller) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	runner := p.Runner
	if runner == nil {
		runner = defaultRunner
	}
	return runner(ctx, name, args...)
}

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func githubEvents(kind string, data []byte) ([]flowdb.MonitorEventInput, error) {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse gh %s json: %w", kind, err)
	}
	out := []flowdb.MonitorEventInput{}
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		number := jsonNumber(row["number"])
		title := stringField(row, "title")
		url := stringField(row, "url")
		repo := firstNonEmpty(repoName(row["repository"]), repoFromGitHubURL(url))
		sourceID := fmt.Sprintf("%s:%s:%d", kind, repo, number)
		if kind == "ci_failed" && !looksFailed(row["statusCheckRollup"]) {
			continue
		}
		displayKind := kind
		if kind == "review_requested" {
			displayKind = "review requested"
		} else if kind == "ci_failed" {
			displayKind = "CI failed"
		} else if kind == "assigned_issue" {
			displayKind = "assigned issue"
		}
		out = append(out, flowdb.MonitorEventInput{
			Source:   "github",
			Kind:     kind,
			SourceID: sourceID,
			Title:    fmt.Sprintf("%s: %s #%d", displayKind, repo, number),
			Body:     title,
			URL:      url,
			Severity: severityForKind(kind),
			RawJSON:  string(raw),
		})
	}
	return out, nil
}

func githubNotifications(data []byte) ([]flowdb.MonitorEventInput, error) {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse gh notifications json: %w", err)
	}
	out := []flowdb.MonitorEventInput{}
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		subject, _ := row["subject"].(map[string]any)
		repo := repoName(row["repository"])
		id := stringField(row, "id")
		title := stringField(subject, "title")
		url := stringField(subject, "url")
		if url == "" {
			url = stringField(row, "url")
		}
		if webURL := githubWebURL(url); webURL != "" {
			url = webURL
		}
		if id == "" || title == "" {
			continue
		}
		out = append(out, flowdb.MonitorEventInput{
			Source:   "github",
			Kind:     "notification",
			SourceID: "notification:" + id,
			Title:    "GitHub notification: " + title,
			Body:     repo,
			URL:      url,
			Severity: "medium",
			RawJSON:  string(raw),
		})
	}
	return out, nil
}

func repoName(v any) string {
	if m, ok := v.(map[string]any); ok {
		return firstNonEmpty(stringField(m, "nameWithOwner"), stringField(m, "full_name"), stringField(m, "name"))
	}
	return ""
}

func repoFromGitHubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func githubWebURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Host, "github.com") {
		return raw
	}
	if !strings.EqualFold(u.Host, "api.github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "repos" {
		return ""
	}
	switch parts[3] {
	case "pulls":
		return "https://github.com/" + parts[1] + "/" + parts[2] + "/pull/" + parts[4]
	case "issues":
		return "https://github.com/" + parts[1] + "/" + parts[2] + "/issues/" + parts[4]
	default:
		return ""
	}
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := stringField(m, key); s != "" {
			return s
		}
	}
	return ""
}

func jsonNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func looksFailed(v any) bool {
	raw, _ := json.Marshal(v)
	text := strings.ToLower(string(raw))
	return strings.Contains(text, "failure") || strings.Contains(text, "failed") || strings.Contains(text, "error") || strings.Contains(text, "cancelled")
}

func severityForKind(kind string) string {
	if kind == "ci_failed" {
		return "high"
	}
	return "medium"
}

func slackToken() string {
	return firstNonEmpty(
		os.Getenv("FLOW_SLACK_WRITE_TOKEN"),
		os.Getenv("SLACK_WRITE_TOKEN"),
		SlackBotToken(),
		SlackUserToken(),
	)
}

func slackChannelAllowlist() map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(os.Getenv("FLOW_SLACK_CHANNELS"), ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		item = strings.TrimPrefix(item, "#")
		out[item] = true
		out[strings.ToLower(item)] = true
	}
	return out
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func envBoolDefault(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
