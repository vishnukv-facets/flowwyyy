package flowdb

import (
	"encoding/json"
	"testing"
)

func TestSessionReadItemAppendDedupeAndMark(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()
	insertTask(t, db, "setup", "Setup", "done", "high", t.TempDir(), nil)
	insertTask(t, db, "build", "Build", "backlog", "high", t.TempDir(), nil)
	if err := AddTaskDependency(db, "build", "setup"); err != nil {
		t.Fatalf("AddTaskDependency: %v", err)
	}
	ctx, err := CreateWorkContext(db, WorkContext{Title: "Build context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}
	if err := SetTaskWorkContext(db, "build", ctx.ID); err != nil {
		t.Fatalf("SetTaskWorkContext: %v", err)
	}

	first, inserted, err := AppendSessionReadItem(db, SessionReadItem{
		Kind:             "ask",
		Text:             "Should I use the ledger row?",
		Provider:         "codex",
		SessionID:        "codex-session-1",
		TaskSlug:         "build",
		WorkContextID:    ctx.ID,
		DedupeKey:        "build:question:1",
		DependenciesJSON: `[{"slug":"setup","status":"done"}]`,
		CreatedAt:        now,
	})
	if err != nil {
		t.Fatalf("AppendSessionReadItem first: %v", err)
	}
	if !inserted || first.ID == "" || first.Status != "pending" {
		t.Fatalf("first = %+v inserted=%v, want pending inserted row", first, inserted)
	}

	dup, inserted, err := AppendSessionReadItem(db, SessionReadItem{
		Kind:      "ask",
		Text:      "mutated text should not replace original",
		DedupeKey: "build:question:1",
	})
	if err != nil {
		t.Fatalf("AppendSessionReadItem duplicate: %v", err)
	}
	if inserted {
		t.Fatal("duplicate inserted=true, want false")
	}
	if dup.ID != first.ID || dup.Text != first.Text {
		t.Fatalf("duplicate mutated row: got %+v want original %+v", dup, first)
	}

	rows, err := ListSessionReadItems(db, SessionReadItemFilter{Status: "pending"})
	if err != nil {
		t.Fatalf("ListSessionReadItems: %v", err)
	}
	if len(rows) != 1 || rows[0].TaskSlug != "build" || rows[0].WorkContextID != ctx.ID {
		t.Fatalf("rows = %+v, want build context row", rows)
	}
	var deps []DependencyRef
	if err := json.Unmarshal([]byte(rows[0].DependenciesJSON), &deps); err != nil {
		t.Fatalf("dependencies json: %v", err)
	}
	if len(deps) != 1 || deps[0].Slug != "setup" {
		t.Fatalf("dependencies = %+v, want setup", deps)
	}

	if err := MarkSessionReadItem(db, first.ID, "read"); err != nil {
		t.Fatalf("MarkSessionReadItem read: %v", err)
	}
	got, err := GetSessionReadItem(db, first.ID)
	if err != nil {
		t.Fatalf("GetSessionReadItem: %v", err)
	}
	if got.Status != "read" || got.ReadAt == "" {
		t.Fatalf("after mark read = %+v, want read_at", got)
	}
}
