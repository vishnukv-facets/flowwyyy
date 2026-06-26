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
