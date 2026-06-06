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

func TestBuildSeparatesNeedsActionFromFYIAndLinksEvidence(t *testing.T) {
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

	attention := requireItem(t, got.NeedsAction, "attention", "feed-deploy")
	if attention.Source != "slack" || attention.Project != "flow-manager" || attention.Urgency != "urgent" {
		t.Fatalf("attention grouping fields = %+v", attention)
	}
	requireLink(t, attention, "attention", "feed-deploy")
	requireLink(t, attention, "task", "deploy-followup")
	requireLink(t, attention, "source", "https://example.slack.com/archives/C1/p1001")
	requireLink(t, attention, "trace", "trace-deploy")

	requireItem(t, got.NeedsAction, "waiting", "blocked-rollout")
	requireItem(t, got.NeedsAction, "stale", "cold-session")
	requireItem(t, got.NeedsAction, "ready", "ready-briefing")

	requireItem(t, got.FYI, "shipped", "shipped-widget")
	update := requireItem(t, got.FYI, "update", "deploy-followup")
	requireLink(t, update, "task", "deploy-followup")

	if _, ok := findItem(got.NeedsAction, "shipped", "shipped-widget"); ok {
		t.Fatalf("shipped task must be FYI, not needs-action: %+v", got.NeedsAction)
	}
}

func TestRenderMarkdownGroupsNeedsActionAndFYI(t *testing.T) {
	b := Briefing{
		GeneratedAt: "2026-06-07T09:00:00Z",
		WindowStart: "2026-06-06T09:00:00Z",
		WindowEnd:   "2026-06-07T09:00:00Z",
		NeedsAction: []Item{{
			Kind: "attention", Ref: "feed-deploy", Source: "slack", Project: "flow-manager",
			Urgency: "urgent", Title: "Rollback note needed", Action: "forward",
			Links: []Link{{Kind: "attention", Target: "feed-deploy"}, {Kind: "task", Target: "deploy-followup"}},
		}},
		FYI: []Item{{
			Kind: "shipped", Ref: "shipped-widget", Project: "flow-manager",
			Title: "Shipped widget", Links: []Link{{Kind: "task", Target: "shipped-widget"}},
		}},
	}

	out := RenderMarkdown(b)
	for _, want := range []string{
		"## Needs action",
		"### flow-manager · slack · urgent",
		"- [attention] Rollback note needed",
		"action: forward",
		"links: attention:feed-deploy · task:deploy-followup",
		"## FYI",
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
