package workevents

import (
	"database/sql"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

func testDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, root
}

func seedProject(t *testing.T, db *sql.DB) {
	t.Helper()
	now := "2026-06-07T08:00:00Z"
	if _, err := db.Exec(`INSERT INTO projects (slug,name,status,priority,work_dir,created_at,updated_at)
		VALUES ('flow-manager','Flow Manager','active','high',?,?,?)`, t.TempDir(), now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedTask(t *testing.T, db *sql.DB, slug, status, priority string) {
	t.Helper()
	now := "2026-06-07T08:00:00Z"
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,project_slug,status,priority,work_dir,session_provider,created_at,updated_at)
		VALUES (?,?, 'flow-manager', ?, ?, ?, 'codex', ?, ?)`, slug, slug, status, priority, t.TempDir(), now, now); err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func TestBuildIncludesAttentionAsNeedsAction(t *testing.T) {
	db, root := testDB(t)
	seedProject(t, db)
	seedTask(t, db, "deploy-followup", "in-progress", "high")
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed1", Source: "slack", ThreadKey: "C1:1", Summary: "Deploy needs reply",
		SuggestedAction: "reply", MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Urgency: "urgent", Confidence: 0.92, Reason: "operator was asked a direct question",
		URL: "https://example.slack.com/archives/C1/p1", Status: "new", CreatedAt: "2026-06-07T08:02:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	got, err := Build(db, root, Filter{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ev := requireEvent(t, got.Items, "attention:feed1")
	if ev.Bucket != BucketNeedsAction || ev.ReasonCode != "attention_unresolved" {
		t.Fatalf("attention event = %+v", ev)
	}
}

func TestBuildClassifiesGitHubTaskLinkedInboxEvents(t *testing.T) {
	db, root := testDB(t)
	seedProject(t, db)
	seedTask(t, db, "autonomy-trust-ladder", "in-progress", "high")
	if err := flowdb.AddTaskTag(db, "autonomy-trust-ladder", "gh-pr:vishnukv-facets/flow-manager#21"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := monitor.AppendInboxEvent("autonomy-trust-ladder", monitor.InboundEvent{
		Kind: "pr_head_updated", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: "gh-pr:vishnukv-facets/flow-manager#21", URL: "https://github.com/vishnukv-facets/flow-manager/pull/21",
		Text: "Pull request head changed. Review the PR again.", UserID: "",
	}); err != nil {
		t.Fatalf("append inbox: %v", err)
	}
	got, err := Build(db, root, Filter{Source: "github"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ev := requireKind(t, got.Items, "pr_head_updated")
	if ev.Bucket != BucketNeedsAction || ev.ReasonCode != "github_task_linked_pr_head_updated" {
		t.Fatalf("github head update event = %+v", ev)
	}
}

func TestInboxEventKeyPrefersConnectorEventKey(t *testing.T) {
	got := inboxEventKey("raptor-airgapped", monitor.InboxEntry{
		EnqueuedAt: "2026-06-07T10:00:00Z",
		Event: monitor.InboundEvent{
			Kind: "pr_review_comment", ChannelType: "github",
			Channel: "facets-cloud/raptor", ThreadTS: "gh-pr:facets-cloud/raptor#159",
			TS: "2026-06-07T09:58:19Z", EventKey: "review-comment:PRRC_1",
		},
	})
	if got != "raptor-airgapped:review-comment:PRRC_1" {
		t.Fatalf("inboxEventKey = %q, want connector event key", got)
	}
}

func TestBuildClassifiesGitHubReviewRequestAsNeedsAction(t *testing.T) {
	db, root := testDB(t)
	seedProject(t, db)
	seedTask(t, db, "review-this-pr", "in-progress", "high")
	if err := monitor.AppendInboxEvent("review-this-pr", monitor.InboundEvent{
		Kind: "pr_review_requested", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: "gh-pr:vishnukv-facets/flow-manager#22", URL: "https://github.com/vishnukv-facets/flow-manager/pull/22",
		Text: "Review requested.",
	}); err != nil {
		t.Fatalf("append inbox: %v", err)
	}
	got, err := Build(db, root, Filter{Source: "github"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ev := requireKind(t, got.Items, "pr_review_requested")
	if ev.Bucket != BucketNeedsAction || ev.ReasonCode != "github_task_linked_review_requested" {
		t.Fatalf("github review request event = %+v", ev)
	}
}

func TestBuildClassifiesMergedPRAsCloseout(t *testing.T) {
	db, root := testDB(t)
	seedProject(t, db)
	seedTask(t, db, "ask-flow-command-center", "in-progress", "high")
	if err := flowdb.AddTaskTag(db, "ask-flow-command-center", "gh-pr:vishnukv-facets/flow-manager#19"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := monitor.AppendInboxEvent("ask-flow-command-center", monitor.InboundEvent{
		Kind: "pr_merged", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: "gh-pr:vishnukv-facets/flow-manager#19", URL: "https://github.com/vishnukv-facets/flow-manager/pull/19",
		Text: "Pull request merged.",
	}); err != nil {
		t.Fatalf("append inbox: %v", err)
	}
	got, err := Build(db, root, Filter{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ev := requireKind(t, got.Items, "pr_merged")
	if ev.Bucket != BucketCloseout || ev.ReasonCode != "github_task_linked_pr_merged" {
		t.Fatalf("merged PR event = %+v", ev)
	}
}

func TestBuildSuppressesHeadUpdateAfterMergedPR(t *testing.T) {
	db, root := testDB(t)
	seedProject(t, db)
	seedTask(t, db, "attention-digest-briefing", "done", "high")
	if err := flowdb.AddTaskTag(db, "attention-digest-briefing", "gh-pr:vishnukv-facets/flow-manager#20"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	thread := "gh-pr:vishnukv-facets/flow-manager#20"
	if err := monitor.AppendInboxEvent("attention-digest-briefing", monitor.InboundEvent{
		Kind: "pr_head_updated", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: thread, URL: "https://github.com/vishnukv-facets/flow-manager/pull/20",
		Text: "Pull request head changed. Review the PR again.",
	}); err != nil {
		t.Fatalf("append head update: %v", err)
	}
	if err := monitor.AppendInboxEvent("attention-digest-briefing", monitor.InboundEvent{
		Kind: "pr_merged", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: thread, URL: "https://github.com/vishnukv-facets/flow-manager/pull/20",
		Text: "Pull request merged.",
	}); err != nil {
		t.Fatalf("append merge: %v", err)
	}

	got, err := Build(db, root, Filter{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := findKind(got.Items, "pr_head_updated"); ok {
		t.Fatalf("merged PR should suppress stale head-update needs-action event: %+v", got.Items)
	}
	ev := requireKind(t, got.Items, "pr_merged")
	if ev.Bucket != BucketHandled || ev.ReasonCode != "github_task_linked_pr_merged_done" {
		t.Fatalf("merged PR event = %+v", ev)
	}
}

func requireEvent(t *testing.T, items []Event, id string) Event {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("event %s missing from %+v", id, items)
	return Event{}
}

func requireKind(t *testing.T, items []Event, kind string) Event {
	t.Helper()
	for _, it := range items {
		if it.Kind == kind {
			return it
		}
	}
	t.Fatalf("kind %s missing from %+v", kind, items)
	return Event{}
}

func findKind(items []Event, kind string) (Event, bool) {
	for _, it := range items {
		if it.Kind == kind {
			return it, true
		}
	}
	return Event{}, false
}
