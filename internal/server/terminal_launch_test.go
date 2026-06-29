package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/steering"
)

// The web-UI launch path (buildBrowserTerminalBootstrapPrompt) is what runs when
// a session is opened from Mission Control. It must inject the operator's saved
// voice — the same guarantee as the CLI `flow do` launch and the steerer's
// per-channel sessions — or a UI-launched session drafts replies in a generic
// voice. This path is a separate copy of the bootstrap prompt, so it needs its
// own coverage.
func TestBuildBrowserTerminalBootstrapPromptInjectsVoice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	const marker = "ZZ-browser-voice-marker: sign off with a dash, lowercase"
	if err := os.WriteFile(steering.PersonaPath(), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, tk := range []*flowdb.Task{
		{Slug: "foo", Kind: "regular"},
		{Slug: "p--run", Kind: "playbook_run", PlaybookSlug: sql.NullString{String: "p", Valid: true}},
	} {
		got := buildBrowserTerminalBootstrapPrompt(db, tk)
		if !strings.Contains(got, marker) {
			t.Errorf("kind %q: browser bootstrap prompt missing the saved operator voice %q", tk.Kind, marker)
		}
		if !strings.Contains(got, "OPERATOR VOICE") {
			t.Errorf("kind %q: browser bootstrap prompt missing the OPERATOR VOICE directive header", tk.Kind)
		}
	}
}

func TestBuildBrowserTerminalBootstrapPromptUsesContextPackForDependencies(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := flowdb.NowISO()
	for _, row := range []struct {
		slug, name, status string
	}{
		{"upstream", "Build upstream ledger", "done"},
		{"child", "Build child pack", "in-progress"},
	} {
		if _, err := db.Exec(
			`INSERT INTO tasks (slug, name, status, kind, priority, work_dir, session_provider, created_at, updated_at)
			 VALUES (?, ?, ?, 'regular', 'high', ?, 'codex', ?, ?)`,
			row.slug, row.name, row.status, root, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := flowdb.AddTaskDependency(db, "child", "upstream"); err != nil {
		t.Fatal(err)
	}
	updatePath := filepath.Join(root, "tasks", "upstream", "updates", "2026-06-28-handoff.md")
	if err := os.MkdirAll(filepath.Dir(updatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(updatePath, []byte("# Handoff\n\nBrowser handoff shipped.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wc, err := flowdb.CreateWorkContext(db, flowdb.WorkContext{Title: "Browser context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := flowdb.SetTaskWorkContext(db, "child", wc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := flowdb.CreateWorkContextSourceAnchor(db, flowdb.WorkContextSourceAnchor{
		WorkContextID: wc.ID,
		Source:        "github",
		AnchorType:    "github_issue",
		ExternalID:    "acme/repo#12",
		URL:           "https://github.com/acme/repo/issues/12",
		Label:         "Issue #12",
	}); err != nil {
		t.Fatal(err)
	}

	got := buildBrowserTerminalBootstrapPrompt(db, &flowdb.Task{Slug: "child", Kind: "regular"})
	for _, want := range []string{"# ContextPack", "Dependencies And Upstream Outputs", "upstream", "Browser handoff shipped", "UNTRUSTED external evidence"} {
		if !strings.Contains(got, want) {
			t.Fatalf("browser prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Upstream dependencies — their changes may NOT") {
		t.Fatalf("browser prompt should use structured ContextPack, not legacy dependency note:\n%s", got)
	}
}
