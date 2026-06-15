package flowdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchDocsIndexesBriefsUpdatesAndOptInTranscripts(t *testing.T) {
	isolateMemoryEnv(t)
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

func TestSearchDocsIndexesMemoriesByDefault(t *testing.T) {
	db := openTempDB(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	insertProject(t, db, "flow", "Flow", root, "medium")
	insertTask(t, db, "build-ui", "Build UI", "backlog", "high", root, "flow")
	if err := os.MkdirAll(filepath.Join(root, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kb", "user.md"), []byte("flow-kb-memory-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex", "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "memories", "raw_memories.md"), []byte("codex-memory-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(claudeTestMemoryPath(home, root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeTestMemoryPath(home, root), []byte("claude-memory-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SyncSearchDocs(db, root, false); err != nil {
		t.Fatalf("SyncSearchDocs: %v", err)
	}
	for _, marker := range []string{"flow-kb-memory-marker", "codex-memory-marker", "claude-memory-marker"} {
		got, err := SearchDocs(db, marker, DefaultSearchScopes(), 10)
		if err != nil {
			t.Fatalf("SearchDocs %s: %v", marker, err)
		}
		if len(got) != 1 || got[0].Scope != "memory" || got[0].Type != "memory" {
			t.Fatalf("memory results for %s = %+v", marker, got)
		}
	}
}

func TestParseSearchScopesAcceptsMemories(t *testing.T) {
	scopes, err := ParseSearchScopes("briefs,memories")
	if err != nil {
		t.Fatalf("ParseSearchScopes: %v", err)
	}
	if !SearchScopesInclude(scopes, SearchScope("memory")) {
		t.Fatalf("scopes = %+v, want memory", scopes)
	}

	scopes, err = ParseSearchScopes("all")
	if err != nil {
		t.Fatalf("ParseSearchScopes all: %v", err)
	}
	if !SearchScopesInclude(scopes, SearchScope("memory")) || !SearchScopesInclude(scopes, SearchScopeTranscript) {
		t.Fatalf("all scopes = %+v, want memory and transcript", scopes)
	}
}

func TestSearchDocsRefreshesChangedMarkdown(t *testing.T) {
	isolateMemoryEnv(t)
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

// SearchDocsMatch with an OR expression recalls docs that share only ONE term
// with the query — the recall the steerer needs from a whole message. The
// AND-of-prefixes SearchDocs builds would miss both, so this is the difference
// between working retrieval and retrieval that always returns nothing.
func TestSearchDocsMatchOR(t *testing.T) {
	isolateMemoryEnv(t)
	db := openTempDB(t)
	root := t.TempDir()
	for _, tc := range []struct{ slug, marker string }{
		{"alpha", "oauthmarker"},
		{"beta", "migrationmarker"},
	} {
		dir := filepath.Join(root, "tasks", tc.slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		insertTask(t, db, tc.slug, tc.slug, "backlog", "medium", root, nil)
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte("# "+tc.slug+"\n\n"+tc.marker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SyncSearchDocs(db, root, false); err != nil {
		t.Fatalf("SyncSearchDocs: %v", err)
	}

	// AND semantics: no single brief contains BOTH markers → zero recall.
	if got, err := SearchDocs(db, "oauthmarker migrationmarker", DefaultSearchScopes(), 10); err != nil || len(got) != 0 {
		t.Fatalf("AND query got=%+v err=%v, want 0 (proves the recall trap)", got, err)
	}
	// OR semantics: both briefs surface.
	got, err := SearchDocsMatch(db, "oauthmarker* OR migrationmarker*", DefaultSearchScopes(), 10)
	if err != nil {
		t.Fatalf("SearchDocsMatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("OR query got %d results, want 2: %+v", len(got), got)
	}
	// Empty expression degrades to no results, not an error.
	if got, err := SearchDocsMatch(db, "   ", DefaultSearchScopes(), 10); err != nil || got != nil {
		t.Fatalf("blank expr got=%+v err=%v, want nil/nil", got, err)
	}
}

func isolateMemoryEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
}

func claudeTestMemoryPath(home, workdir string) string {
	if abs, err := filepath.Abs(workdir); err == nil {
		workdir = abs
	}
	key := strings.ReplaceAll(filepath.ToSlash(filepath.Clean(workdir)), "/", "-")
	return filepath.Join(home, ".claude", "projects", key, "memory", "MEMORY.md")
}
