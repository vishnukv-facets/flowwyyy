package steering

import (
	"database/sql"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

func seedTask(t *testing.T, db *sql.DB, slug string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'high', ?, 'default', 'claude', ?, ?, ?)`,
		slug, "seeded", t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func TestBackfillFeedTaskThreadTags(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// An acted feed item that spawned a task but predates source-thread tagging.
	seedTask(t, db, "att-eng")
	item := flowdb.FeedItem{
		ID: "bf1", Source: "slack", ThreadKey: "C_eng:1700000000.000100",
		SuggestedAction: "make_task", Status: "acted", LinkedTask: "att-eng",
		CreatedAt: "2026-06-05T10:00:00Z", ActedAt: "2026-06-05T10:01:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	// A github-sourced acted item, composite thread key → bare link tag.
	seedTask(t, db, "att-gh")
	gh := flowdb.FeedItem{
		ID: "bf2", Source: "github", ThreadKey: "owner/repo:gh-pr:owner/repo#9",
		SuggestedAction: "make_task", Status: "acted", LinkedTask: "att-gh",
		CreatedAt: "2026-06-05T10:00:00Z", ActedAt: "2026-06-05T10:01:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, gh); err != nil {
		t.Fatalf("seed gh feed: %v", err)
	}

	// A still-new (unacted) feed item with no linked task → must be skipped.
	skip := flowdb.FeedItem{
		ID: "bf3", Source: "slack", ThreadKey: "C_x:1.1",
		SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, skip); err != nil {
		t.Fatalf("seed new feed: %v", err)
	}

	n, err := BackfillFeedTaskThreadTags(db, nil)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("tagged = %d, want 2 (slack + github acted items)", n)
	}

	engTags, _ := flowdb.GetTaskTags(db, "att-eng")
	if !containsTag(engTags, "slack-thread:C_eng:1700000000.000100") {
		t.Errorf("att-eng tags = %v, want slack-thread linkage", engTags)
	}
	ghTags, _ := flowdb.GetTaskTags(db, "att-gh")
	if !containsTag(ghTags, "gh-pr:owner/repo#9") {
		t.Errorf("att-gh tags = %v, want gh-pr linkage", ghTags)
	}

	// Idempotent: a second pass tags nothing new.
	n2, err := BackfillFeedTaskThreadTags(db, nil)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second pass tagged = %d, want 0 (idempotent)", n2)
	}
}

func TestDismissSurfacedDropCards(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	mk := func(id, action, status string) {
		t.Helper()
		if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
			ID: id, Source: "slack", ThreadKey: "C:" + id, SuggestedAction: action,
			Status: status, CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	mk("d1", "drop", "new")   // surfaced drop → must be dismissed
	mk("d2", "drop", "new")   // another → dismissed
	mk("r1", "reply", "new")  // genuine card → untouched
	mk("a1", "drop", "acted") // already resolved → untouched

	n, err := DismissSurfacedDropCards(db, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("dismissed = %d, want 2", n)
	}
	if got, _ := flowdb.GetFeedItem(db, "d1"); got.Status != "dismissed" {
		t.Errorf("d1 status = %q, want dismissed", got.Status)
	}
	if got, _ := flowdb.GetFeedItem(db, "r1"); got.Status != "new" {
		t.Errorf("r1 (reply) status = %q, want new (untouched)", got.Status)
	}
	if newItems, _ := flowdb.ListFeedItems(db, "new"); len(newItems) != 1 {
		t.Errorf("remaining new cards = %d, want 1 (the reply)", len(newItems))
	}
}

func TestBackfillSkipsDeletedTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Feed item references a task that was never created (deleted). Backfill must
	// skip it rather than leave an orphan task_tags row.
	item := flowdb.FeedItem{
		ID: "bf4", Source: "slack", ThreadKey: "C_gone:5.5",
		SuggestedAction: "make_task", Status: "acted", LinkedTask: "att-gone",
		CreatedAt: "2026-06-05T10:00:00Z", ActedAt: "2026-06-05T10:01:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := BackfillFeedTaskThreadTags(db, nil)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("tagged = %d, want 0 (task does not exist)", n)
	}
	if tags, _ := flowdb.GetTaskTags(db, "att-gone"); len(tags) != 0 {
		t.Errorf("orphan tags created for deleted task: %v", tags)
	}
}
