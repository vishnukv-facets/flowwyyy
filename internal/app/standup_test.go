package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func TestCmdStandupRendersBriefing(t *testing.T) {
	root, db := showListEditDB(t)
	seedStandupProject(t, db, root)
	seedStandupTask(t, db, root, "deploy-followup", "Follow up on deploy", "in-progress", "high", "flow-manager", "")
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "standup-feed", Source: "slack", ThreadKey: "C1:1.1",
		Summary: "Rollback note needed before Friday deploy", SuggestedAction: "forward",
		MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Urgency: "urgent", Confidence: 0.9, URL: "https://example.slack.com/archives/C1/p11",
		Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	writeStandupUpdate(t, root, "deploy-followup", "2026-06-07-rollout.md", "Rollout note captured.")

	oldNow := standupNow
	standupNow = func() time.Time { return time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { standupNow = oldNow })

	out := captureStdout(t, func() {
		if rc := cmdStandup([]string{"--for", "today"}); rc != 0 {
			t.Fatalf("cmdStandup rc = %d, want 0", rc)
		}
	})
	for _, want := range []string{
		"Flow briefing",
		"## Needs you",
		"[attention] Rollback note needed before Friday deploy",
		"links: attention:standup-feed",
		"## Since you last looked",
		"[update]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("standup output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestCmdStandupRejectsUnknownWindow(t *testing.T) {
	showListEditDB(t)
	out := captureStdout(t, func() {
		if rc := cmdStandup([]string{"--for", "tomorrow"}); rc != 2 {
			t.Fatalf("cmdStandup rc = %d, want 2", rc)
		}
	})
	if !strings.Contains(out, "want today|monday|24h") {
		t.Fatalf("usage error should name valid windows, got %q", out)
	}
}

func seedStandupProject(t *testing.T, db *sql.DB, root string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('flow-manager', 'Flow Manager', 'active', 'medium', ?, '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z')`,
		filepath.Join(root, "repo"),
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedStandupTask(t *testing.T, db *sql.DB, root, slug, name, status, priority, project, waiting string) {
	t.Helper()
	sessionID := any(nil)
	if status != "backlog" {
		sessionID = fakeSessionID(slug)
	}
	_, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, project_slug, status, priority, work_dir, waiting_on,
			session_provider, session_id, status_changed_at, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, 'codex', ?, '2026-06-07T08:00:00Z', '2026-06-01T00:00:00Z', '2026-06-07T08:00:00Z')`,
		slug, name, project, status, priority, filepath.Join(root, "repo"), nullString(waiting), sessionID,
	)
	if err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func writeStandupUpdate(t *testing.T, root, slug, filename, body string) {
	t.Helper()
	dir := filepath.Join(root, "tasks", slug, "updates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir updates: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
