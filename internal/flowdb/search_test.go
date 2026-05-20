package flowdb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSearchDocsIndexesBriefsUpdatesAndOptInTranscripts(t *testing.T) {
	db := openTempDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks", "build-ui", "updates"), 0o755); err != nil {
		t.Fatal(err)
	}
	insertTask(t, db, "build-ui", "Build UI", "backlog", "high", root, nil)
	if err := os.WriteFile(filepath.Join(root, "tasks", "build-ui", "brief.md"), []byte("# Build UI\n\nbrief-only-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "build-ui", "updates", "2026-05-20-progress.md"), []byte("update-only-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"content":"transcript-only-marker"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET session_path = ? WHERE slug = 'build-ui'`, transcriptPath); err != nil {
		t.Fatal(err)
	}

	if err := SyncSearchDocs(db, root, false); err != nil {
		t.Fatalf("SyncSearchDocs: %v", err)
	}
	got, err := SearchDocs(db, "brief-only-marker", DefaultSearchScopes(), 10)
	if err != nil {
		t.Fatalf("SearchDocs brief: %v", err)
	}
	if len(got) != 1 || got[0].Scope != string(SearchScopeBrief) || got[0].Slug != "build-ui" {
		t.Fatalf("brief results = %+v", got)
	}
	got, err = SearchDocs(db, "update-only-marker", DefaultSearchScopes(), 10)
	if err != nil {
		t.Fatalf("SearchDocs update: %v", err)
	}
	if len(got) != 1 || got[0].Scope != string(SearchScopeUpdate) || got[0].Slug != "build-ui" {
		t.Fatalf("update results = %+v", got)
	}
	got, err = SearchDocs(db, "transcript-only-marker", []SearchScope{SearchScopeTranscript}, 10)
	if err != nil {
		t.Fatalf("SearchDocs transcript before opt-in: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("transcript indexed before opt-in: %+v", got)
	}

	if err := SyncSearchDocs(db, root, true); err != nil {
		t.Fatalf("SyncSearchDocs transcript: %v", err)
	}
	got, err = SearchDocs(db, "transcript-only-marker", []SearchScope{SearchScopeTranscript}, 10)
	if err != nil {
		t.Fatalf("SearchDocs transcript after opt-in: %v", err)
	}
	if len(got) != 1 || got[0].Scope != string(SearchScopeTranscript) || got[0].Slug != "build-ui" {
		t.Fatalf("transcript results = %+v", got)
	}
}

func TestSearchDocsRefreshesChangedMarkdown(t *testing.T) {
	db := openTempDB(t)
	root := t.TempDir()
	taskDir := filepath.Join(root, "tasks", "refresh", "updates")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	insertTask(t, db, "refresh", "Refresh docs", "backlog", "medium", root, nil)
	briefPath := filepath.Join(root, "tasks", "refresh", "brief.md")
	if err := os.WriteFile(briefPath, []byte("old-brief-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncSearchDocs(db, root, false); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if got, err := SearchDocs(db, "old-brief-marker", DefaultSearchScopes(), 10); err != nil || len(got) != 1 {
		t.Fatalf("old marker before update got=%+v err=%v", got, err)
	}

	if err := os.WriteFile(briefPath, []byte("new-brief-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncSearchDocs(db, root, false); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got, err := SearchDocs(db, "old-brief-marker", DefaultSearchScopes(), 10); err != nil || len(got) != 0 {
		t.Fatalf("old marker after update got=%+v err=%v", got, err)
	}
	if got, err := SearchDocs(db, "new-brief-marker", DefaultSearchScopes(), 10); err != nil || len(got) != 1 {
		t.Fatalf("new marker got=%+v err=%v", got, err)
	}
}
