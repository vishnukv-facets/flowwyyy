// internal/steering/taskindex_test.go
package steering

import (
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestBuildTaskIndex(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Seed a project and two tasks (one active, one done) via raw SQL — we
	// only assert what BuildTaskIndex renders, so direct inserts are fine.
	now := "2026-06-05T10:00:00Z"
	if _, err := db.Exec(`INSERT INTO projects (slug,name,status,priority,work_dir,created_at,updated_at) VALUES ('goniyo','Goniyo','active','high','/tmp',?,?)`, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,project_slug,status,kind,priority,work_dir,session_provider,session_id,created_at,updated_at) VALUES ('kong-split','Kong split','goniyo','in-progress','regular','high','/tmp','claude','sess-1',?,?)`, now, now); err != nil {
		t.Fatalf("seed task1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,status,kind,priority,work_dir,session_provider,created_at,updated_at) VALUES ('old','Old thing','done','regular','low','/tmp','claude',?,?)`, now, now); err != nil {
		t.Fatalf("seed task2: %v", err)
	}
	// An archived-but-in-progress task must STILL appear (archive ≠ stop tracking).
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,project_slug,status,kind,priority,work_dir,session_provider,session_id,archived_at,created_at,updated_at) VALUES ('arch','Archived live','goniyo','in-progress','regular','high','/tmp','claude','sess-2',?,?,?)`, now, now, now); err != nil {
		t.Fatalf("seed task3: %v", err)
	}

	t.Setenv("FLOW_ROOT", "/tmp/flowroot")
	idx, err := BuildTaskIndex(db)
	if err != nil {
		t.Fatalf("BuildTaskIndex: %v", err)
	}
	if !strings.Contains(idx, "kong-split") || !strings.Contains(idx, "goniyo") {
		t.Errorf("index missing active task/project:\n%s", idx)
	}
	if strings.Contains(idx, "old") {
		t.Errorf("done task should be excluded from index:\n%s", idx)
	}
	// Archived live task included, flagged as archived.
	if !strings.Contains(idx, "arch") || !strings.Contains(idx, "archived") {
		t.Errorf("archived in-progress task should be included + flagged:\n%s", idx)
	}
	// Brief + updates paths are surfaced so the deep-triage agent can read them.
	if !strings.Contains(idx, "/tmp/flowroot/tasks/kong-split/brief.md") || !strings.Contains(idx, "/tmp/flowroot/tasks/kong-split/updates/") {
		t.Errorf("index should surface brief + updates paths:\n%s", idx)
	}
}
