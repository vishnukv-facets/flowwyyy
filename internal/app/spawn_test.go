package app

import (
	"flow/internal/flowdb"
	"flow/internal/iterm"
	"flow/internal/spawner"
	"testing"
)

func TestSpawnParentIsHierarchyNotBlocking(t *testing.T) {
	setupFlowRoot(t)

	// Stub iterm.Runner and pin spawner backend so no real terminal opens.
	origRunner := iterm.Runner
	iterm.Runner = func(args []string) error { return nil }
	t.Cleanup(func() { iterm.Runner = origRunner })

	oldOverride := spawner.Override
	spawner.Override = spawner.BackendITerm
	t.Cleanup(func() { spawner.Override = oldOverride })

	db := openFlowDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "in-progress", "medium", wd, nil)
	if _, err := db.Exec(`UPDATE tasks SET session_id='11111111-1111-4111-8111-111111111111', session_started=? WHERE slug='epic'`, flowdb.NowISO()); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	db.Close()

	rc := cmdSpawn([]string{"child work", "--parent", "epic", "--agent", "claude", "--no-open", "--work-dir", wd})
	if rc != 0 {
		t.Fatalf("spawn rc = %d, want 0", rc)
	}
	db = openFlowDB(t)
	defer db.Close()
	child, err := resolveJustCreatedTaskSlug(db, "child work", "")
	if err != nil {
		t.Fatalf("locate child: %v", err)
	}
	task, _ := flowdb.GetTask(db, child)
	if task.ParentSlug.String != "epic" {
		t.Fatalf("spawn --parent should set hierarchy; got %v", task.ParentSlug)
	}
	if blocker, _ := flowdb.TaskStartBlockerFor(db, task); blocker != nil {
		t.Fatalf("spawned child must not be blocked by an in-progress hierarchy parent; got %v", blocker)
	}
}
