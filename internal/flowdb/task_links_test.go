package flowdb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncTaskLinksIndexesTaskBriefsAndUpdates(t *testing.T) {
	db := openTempDB(t)
	root := t.TempDir()

	insertTask(t, db, "source-task", "Source task", "backlog", "medium", root, nil)
	insertTask(t, db, "target-task", "Target task", "backlog", "medium", root, nil)
	insertTask(t, db, "other-task", "Other task", "backlog", "medium", root, nil)

	sourceDir := filepath.Join(root, "tasks", "source-task")
	updateDir := filepath.Join(sourceDir, "updates")
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(sourceDir, "brief.md")
	updatePath := filepath.Join(updateDir, "2026-06-08-progress.md")
	if err := os.WriteFile(briefPath, []byte("Brief links [[target-task]] twice [[target-task]] and ignores [[missing-task]]."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(updatePath, []byte("Update links [[other-task]]."), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SyncTaskLinks(db, root); err != nil {
		t.Fatalf("SyncTaskLinks: %v", err)
	}

	targetLinks, err := TaskBacklinks(db, "target-task")
	if err != nil {
		t.Fatalf("TaskBacklinks target: %v", err)
	}
	if len(targetLinks) != 1 {
		t.Fatalf("target links = %+v, want one deduped brief backlink", targetLinks)
	}
	if targetLinks[0].FromSlug != "source-task" || targetLinks[0].FromKind != "brief" || targetLinks[0].SourceFile != briefPath {
		t.Fatalf("target backlink = %+v", targetLinks[0])
	}

	otherLinks, err := TaskBacklinks(db, "other-task")
	if err != nil {
		t.Fatalf("TaskBacklinks other: %v", err)
	}
	if len(otherLinks) != 1 || otherLinks[0].FromKind != "update" || otherLinks[0].SourceFile != updatePath {
		t.Fatalf("other links = %+v, want update backlink", otherLinks)
	}

	if err := os.WriteFile(briefPath, []byte("Brief now links [[other-task]] only."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(updatePath); err != nil {
		t.Fatal(err)
	}
	if err := SyncTaskLinks(db, root); err != nil {
		t.Fatalf("resync task links: %v", err)
	}

	targetLinks, err = TaskBacklinks(db, "target-task")
	if err != nil {
		t.Fatalf("TaskBacklinks target after resync: %v", err)
	}
	if len(targetLinks) != 0 {
		t.Fatalf("stale target links = %+v, want none", targetLinks)
	}
	otherLinks, err = TaskBacklinks(db, "other-task")
	if err != nil {
		t.Fatalf("TaskBacklinks other after resync: %v", err)
	}
	if len(otherLinks) != 1 || otherLinks[0].FromKind != "brief" || otherLinks[0].SourceFile != briefPath {
		t.Fatalf("other links after resync = %+v, want updated brief backlink only", otherLinks)
	}
}

func TestRenameTaskCascadesTaskLinks(t *testing.T) {
	db := openTempDB(t)
	root := t.TempDir()

	insertTask(t, db, "source-task", "Source task", "backlog", "medium", root, nil)
	insertTask(t, db, "target-task", "Target task", "backlog", "medium", root, nil)
	sourceDir := filepath.Join(root, "tasks", "source-task")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "brief.md"), []byte("[[target-task]]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncTaskLinks(db, root); err != nil {
		t.Fatal(err)
	}

	if err := RenameTask(db, "target-task", "renamed-target"); err != nil {
		t.Fatalf("RenameTask target: %v", err)
	}
	links, err := TaskBacklinks(db, "renamed-target")
	if err != nil {
		t.Fatalf("TaskBacklinks renamed target: %v", err)
	}
	if len(links) != 1 || links[0].ToSlug != "renamed-target" {
		t.Fatalf("target rename did not cascade task_links: %+v", links)
	}

	if err := RenameTask(db, "source-task", "renamed-source"); err != nil {
		t.Fatalf("RenameTask source: %v", err)
	}
	links, err = TaskBacklinks(db, "renamed-target")
	if err != nil {
		t.Fatalf("TaskBacklinks after source rename: %v", err)
	}
	if len(links) != 1 || links[0].FromSlug != "renamed-source" {
		t.Fatalf("source rename did not cascade task_links: %+v", links)
	}
}
