package briefing

import (
	"crypto/sha1"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func TestBuildSeparatesNeedsYouFromOvernightAndLinksEvidence(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)

	seedBriefingProject(t, db, root, "flow-manager")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "deploy-followup", Name: "Follow up on deploy", Status: "in-progress",
		Priority: "high", Project: "flow-manager", UpdatedAt: "2026-06-07T07:00:00Z",
	})
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "blocked-rollout", Name: "Blocked rollout", Status: "in-progress",
		Priority: "medium", Project: "flow-manager", WaitingOn: "Omendra approval",
		UpdatedAt: "2026-06-07T06:00:00Z",
	})
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "cold-session", Name: "Cold session", Status: "in-progress",
		Priority: "medium", Project: "flow-manager", UpdatedAt: "2026-06-01T08:00:00Z",
	})
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "ready-briefing", Name: "Ready briefing work", Status: "backlog",
		Priority: "high", Project: "flow-manager", UpdatedAt: "2026-06-07T05:00:00Z",
	})
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "shipped-widget", Name: "Shipped widget", Status: "done",
		Priority: "low", Project: "flow-manager", UpdatedAt: "2026-06-07T04:00:00Z",
	})
	writeBriefingUpdate(t, root, "deploy-followup", "2026-06-07-rollout-note.md", "Rollout note\n\nNext: send the Friday update.")

	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-deploy", Source: "slack", ThreadKey: "C1:100.1",
		Summary: "Rollback note needed before Friday deploy", SuggestedAction: "forward",
		MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Urgency: "urgent", Confidence: 0.93, URL: "https://example.slack.com/archives/C1/p1001",
		Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "trace-deploy", CreatedAt: "2026-06-07T08:00:01Z", Origin: "live",
		Source: "slack", ThreadKey: "C1:100.1", Disposition: "surfaced",
		StageReached: "stage3", FinalAction: "forward", FinalConfidence: 0.93,
		FeedItemID: "feed-deploy",
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: since, StaleAfter: 72 * time.Hour})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	attention := requireItem(t, got.NeedsYou, "attention", "feed-deploy")
	if attention.Source != "slack" || attention.Project != "flow-manager" || attention.Urgency != "urgent" {
		t.Fatalf("attention grouping fields = %+v", attention)
	}
	requireLink(t, attention, "attention", "feed-deploy")
	requireLink(t, attention, "task", "deploy-followup")
	requireLink(t, attention, "source", "https://example.slack.com/archives/C1/p1001")
	requireLink(t, attention, "trace", "trace-deploy")

	// Tier 1 also owns waiting tasks (you have to chase them).
	requireItem(t, got.NeedsYou, "waiting", "blocked-rollout")
	// Tier 3 is what to resume/start next: cold sessions and startable backlog.
	requireItem(t, got.NextUp, "stale", "cold-session")
	requireItem(t, got.NextUp, "ready", "ready-briefing")

	// Tier 2 is what changed while away: shipped tasks and fresh update notes.
	requireItem(t, got.Overnight, "shipped", "shipped-widget")
	update := requireItem(t, got.Overnight, "update", "deploy-followup")
	requireLink(t, update, "task", "deploy-followup")

	if _, ok := findItem(got.NeedsYou, "shipped", "shipped-widget"); ok {
		t.Fatalf("shipped task must be overnight, not needs-you: %+v", got.NeedsYou)
	}
}

func TestBuildRoutesDigestOnlyAttentionToOvernight(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)

	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-leave", Source: "slack", ThreadKey: "C1:200.1",
		Summary: "Rohit is on leave tomorrow", SuggestedAction: "digest_only",
		Urgency: "low", Confidence: 0.82, URL: "https://example.slack.com/archives/C1/p2001",
		Status: "new", CreatedAt: "2026-06-07T08:15:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "trace-leave", CreatedAt: "2026-06-07T08:15:01Z", Origin: "live",
		Source: "slack", ThreadKey: "C1:200.1", Disposition: "surfaced",
		StageReached: "stage3", FinalAction: "digest_only", FinalConfidence: 0.82,
		FeedItemID: "feed-leave",
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := findItem(got.NeedsYou, "attention", "feed-leave"); ok {
		t.Fatalf("digest-only attention item should not need you: %+v", got.NeedsYou)
	}
	fyi := requireItem(t, got.Overnight, "attention", "feed-leave")
	if fyi.Action != "No action" {
		t.Fatalf("Overnight Action = %q, want No action", fyi.Action)
	}
	requireLink(t, fyi, "attention", "feed-leave")
	requireLink(t, fyi, "source", "https://example.slack.com/archives/C1/p2001")
	requireLink(t, fyi, "trace", "trace-leave")
}

func TestBuildRoutesAttentionMatchedToWaitingTask(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)

	seedBriefingProject(t, db, root, "flow-manager")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "raptor-review", Name: "Raptor PR review", Status: "in-progress",
		Priority: "high", Project: "flow-manager", WaitingOn: "Rohit review on PR #159",
		UpdatedAt: "2026-06-07T07:00:00Z",
	})
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-rohit-leave", Source: "slack", ThreadKey: "C1:300.1",
		Summary: "Rohit is on leave tomorrow", SuggestedAction: "forward",
		MatchedTask: "raptor-review", SuggestedProject: "flow-manager",
		Urgency: "normal", Confidence: 0.89, URL: "https://example.slack.com/archives/C1/p3001",
		Status: "new", CreatedAt: "2026-06-07T08:30:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "trace-rohit-leave", CreatedAt: "2026-06-07T08:30:01Z", Origin: "live",
		Source: "slack", ThreadKey: "C1:300.1", Disposition: "surfaced",
		StageReached: "stage3", FinalAction: "forward", FinalConfidence: 0.89,
		FeedItemID: "feed-rohit-leave",
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// An attention nudge that affects a waiting task lands in tier 1 (the wait
	// may now be resolvable), but is tagged "Review affected task" so it reads
	// differently from a card that needs a fresh reply or decision.
	waiting := requireItem(t, got.NeedsYou, "attention", "feed-rohit-leave")
	if waiting.Action != "Review affected task" {
		t.Fatalf("NeedsYou Action = %q, want Review affected task", waiting.Action)
	}
	if !strings.Contains(waiting.Detail, "Affects waiting task: Raptor PR review") {
		t.Fatalf("Waiting Detail = %q, want affected task name", waiting.Detail)
	}
	requireLink(t, waiting, "attention", "feed-rohit-leave")
	requireLink(t, waiting, "task", "raptor-review")
	requireLink(t, waiting, "source", "https://example.slack.com/archives/C1/p3001")
	requireLink(t, waiting, "trace", "trace-rohit-leave")
}

func TestBuildKeepsReplyAttentionForWaitingTaskAsReply(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)

	seedBriefingProject(t, db, root, "flow-manager")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "blocked-review", Name: "Blocked review", Status: "in-progress",
		Priority: "high", Project: "flow-manager", WaitingOn: "Rohit review",
		UpdatedAt: "2026-06-07T07:00:00Z",
	})
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-reply", Source: "slack", ThreadKey: "C1:400.1",
		Summary: "Reply needed on review thread", SuggestedAction: "reply",
		MatchedTask: "blocked-review", SuggestedProject: "flow-manager",
		Urgency: "urgent", Confidence: 0.91, URL: "https://example.slack.com/archives/C1/p4001",
		Status: "new", CreatedAt: "2026-06-07T08:45:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A reply card for a waiting task is a fresh action, not a "review the
	// affected task" nudge — it keeps the reply action even though the task is
	// waiting on someone.
	reply := requireItem(t, got.NeedsYou, "attention", "feed-reply")
	if reply.Action != "Review reply" {
		t.Fatalf("NeedsYou Action = %q, want Review reply", reply.Action)
	}
	requireLink(t, reply, "attention", "feed-reply")
	requireLink(t, reply, "task", "blocked-review")
}

func TestBuildRoutesDigestOnlyMatchedNonWaitingTaskToOvernight(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)

	seedBriefingProject(t, db, root, "flow-manager")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "deploy-followup", Name: "Follow up on deploy", Status: "in-progress",
		Priority: "medium", Project: "flow-manager", UpdatedAt: "2026-06-07T07:00:00Z",
	})
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-digest-matched", Source: "slack", ThreadKey: "C1:500.1",
		Summary: "Deploy FYI only", SuggestedAction: "digest_only",
		MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Urgency: "low", Confidence: 0.78, URL: "https://example.slack.com/archives/C1/p5001",
		Status: "new", CreatedAt: "2026-06-07T08:50:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := findItem(got.NeedsYou, "attention", "feed-digest-matched"); ok {
		t.Fatalf("digest-only matched attention item should be overnight, not needs-you: %+v", got.NeedsYou)
	}
	fyi := requireItem(t, got.Overnight, "attention", "feed-digest-matched")
	if fyi.Action != "No action" {
		t.Fatalf("Overnight Action = %q, want No action", fyi.Action)
	}
	requireLink(t, fyi, "attention", "feed-digest-matched")
	requireLink(t, fyi, "task", "deploy-followup")
}

func TestBuildSurfacesWaitingSessionsInNeedsYou(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)

	seedBriefingProject(t, db, root, "flow-manager")
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "needs-input", Name: "Port flow do", Status: "in-progress",
		Priority: "medium", Project: "flow-manager", UpdatedAt: "2026-06-07T08:00:00Z",
	})
	// busy-task already draws attention via a feed card; its waiting session
	// must NOT produce a second NeedsYou row.
	seedBriefingTask(t, db, root, taskSeed{
		Slug: "busy-task", Name: "Busy task", Status: "in-progress",
		Priority: "high", Project: "flow-manager", UpdatedAt: "2026-06-07T08:00:00Z",
	})
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "feed-busy", Source: "slack", ThreadKey: "C1:900.1",
		Summary: "Needs a reply", SuggestedAction: "reply", MatchedTask: "busy-task",
		SuggestedProject: "flow-manager", Urgency: "urgent", Confidence: 0.9,
		Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}

	got, err := Build(db, root, Options{
		Now: now, Since: now.Add(-24 * time.Hour),
		WaitingSessions: []WaitingSession{
			{TaskSlug: "needs-input", Name: "Port flow do", Project: "flow-manager", Detail: "agent is paused for your input · idle 1m"},
			{TaskSlug: "busy-task", Name: "Busy task", Project: "flow-manager"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	session := requireItem(t, got.NeedsYou, "session", "needs-input")
	if session.Action != "Send input" {
		t.Fatalf("session Action = %q, want Send input", session.Action)
	}
	if session.Source != "session" || session.Urgency != "urgent" {
		t.Fatalf("session fields = %+v", session)
	}
	requireLink(t, session, "session", "needs-input")
	requireLink(t, session, "task", "needs-input")

	// busy-task keeps its attention card and is not double-listed as a session.
	requireItem(t, got.NeedsYou, "attention", "feed-busy")
	if _, ok := findItem(got.NeedsYou, "session", "busy-task"); ok {
		t.Fatalf("waiting session for an already-surfaced task should be deduped: %+v", got.NeedsYou)
	}

	// Tier-1 ordering: attention cards sort above waiting sessions.
	attnIdx, sessIdx := -1, -1
	for i, item := range got.NeedsYou {
		if item.Kind == "attention" && attnIdx < 0 {
			attnIdx = i
		}
		if item.Kind == "session" && sessIdx < 0 {
			sessIdx = i
		}
	}
	if attnIdx < 0 || sessIdx < 0 || attnIdx > sessIdx {
		t.Fatalf("expected attention (idx %d) ranked above session (idx %d): %+v", attnIdx, sessIdx, got.NeedsYou)
	}
}

func TestBriefingSkipsOrphanTaskUpdateDirectory(t *testing.T) {
	db, root := briefingTestDB(t)
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)
	seedBriefingProject(t, db, root, "flow-manager")
	writeBriefingUpdate(t, root, "ghost-task", "2026-06-07-note.md", "Ghost note\n")

	got, err := Build(db, root, Options{Now: now, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := findItem(got.Overnight, "update", "ghost-task"); ok {
		t.Fatalf("orphan task update should not be shown in briefing: %+v", got.Overnight)
	}
}

func TestRenderMarkdownGroupsTiers(t *testing.T) {
	b := Briefing{
		GeneratedAt: "2026-06-07T09:00:00Z",
		WindowStart: "2026-06-06T09:00:00Z",
		WindowEnd:   "2026-06-07T09:00:00Z",
		NeedsYou: []Item{
			{
				Kind: "attention", Ref: "feed-deploy", Source: "slack", Project: "flow-manager",
				Urgency: "urgent", Title: "Rollback note needed", Action: "forward",
				Links: []Link{{Kind: "attention", Target: "feed-deploy"}, {Kind: "task", Target: "deploy-followup"}},
			},
			{
				Kind: "waiting", Ref: "blocked-rollout", Project: "flow-manager",
				Title: "Blocked rollout", Detail: "waiting on approval",
			},
		},
		NextUp: []Item{{
			Kind: "ready", Ref: "ready-briefing", Project: "flow-manager",
			Title: "Ready briefing work", Detail: "high-priority backlog is startable",
		}},
		Overnight: []Item{{
			Kind: "shipped", Ref: "shipped-widget", Project: "flow-manager",
			Title: "Shipped widget", Links: []Link{{Kind: "task", Target: "shipped-widget"}},
		}},
	}

	out := RenderMarkdown(b)
	for _, want := range []string{
		"## Needs you",
		"### flow-manager · slack · urgent",
		"- [attention] Rollback note needed",
		"action: forward",
		"links: attention:feed-deploy · task:deploy-followup",
		"- [waiting] Blocked rollout",
		"## Pick up next",
		"- [ready] Ready briefing work",
		"## Since you last looked",
		"### flow-manager",
		"- [shipped] Shipped widget",
	} {
		if !contains(out, want) {
			t.Fatalf("RenderMarkdown missing %q\n--- output ---\n%s", want, out)
		}
	}
}

type taskSeed struct {
	Slug, Name, Status, Priority, Project, WaitingOn, UpdatedAt string
}

func briefingTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, root
}

func seedBriefingProject(t *testing.T, db *sql.DB, root, slug string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, 'active', 'medium', ?, ?, ?)`,
		slug, "Flow Manager", filepath.Join(root, "repo"), "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedBriefingTask(t *testing.T, db *sql.DB, root string, s taskSeed) {
	t.Helper()
	sessionID := any(nil)
	if s.Status != "backlog" {
		sessionID = fakeBriefingSessionID(s.Slug)
	}
	_, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, project_slug, status, priority, work_dir, waiting_on,
			session_provider, session_id, status_changed_at, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, 'codex', ?, ?, ?, ?)`,
		s.Slug, s.Name, sqlNull(s.Project), s.Status, s.Priority, filepath.Join(root, "repo"), sqlNull(s.WaitingOn),
		sessionID, s.UpdatedAt, "2026-06-01T00:00:00Z", s.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("seed task %s: %v", s.Slug, err)
	}
}

func writeBriefingUpdate(t *testing.T, root, slug, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "tasks", slug, "updates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir updates: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}
}

func sqlNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func fakeBriefingSessionID(slug string) string {
	sum := sha1.Sum([]byte(slug))
	return fmt.Sprintf("00000000-0000-4000-8000-%x", sum[:6])
}

func requireItem(t *testing.T, items []Item, kind, ref string) Item {
	t.Helper()
	item, ok := findItem(items, kind, ref)
	if !ok {
		t.Fatalf("missing %s item %q in %+v", kind, ref, items)
	}
	return item
}

func findItem(items []Item, kind, ref string) (Item, bool) {
	for _, item := range items {
		if item.Kind == kind && item.Ref == ref {
			return item, true
		}
	}
	return Item{}, false
}

func requireLink(t *testing.T, item Item, kind, target string) {
	t.Helper()
	for _, link := range item.Links {
		if link.Kind == kind && link.Target == target {
			return
		}
	}
	t.Fatalf("item %s/%s missing %s link %q: %+v", item.Kind, item.Ref, kind, target, item.Links)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
