package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestWriteBriefCurrentState(t *testing.T) {
	const (
		origBrief = "# Fix bug\n\n## What\nFix the thing.\n\n## Done when\nTests pass.\n"
		date      = "2026-06-05"
	)

	t.Run("appends block when markers absent, preserving the original", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "brief.md")
		if err := os.WriteFile(path, []byte(origBrief), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeBriefCurrentState(path, "Blocked on deploy-role perms.", date); err != nil {
			t.Fatal(err)
		}
		got := readFile(t, path)
		if !strings.HasPrefix(got, origBrief) {
			t.Errorf("original brief not preserved at top:\n%s", got)
		}
		if strings.Count(got, briefStateStartMarker) != 1 || strings.Count(got, briefStateEndMarker) != 1 {
			t.Errorf("expected exactly one marker pair, got:\n%s", got)
		}
		if !strings.Contains(got, "**Current state** · updated "+date) {
			t.Errorf("missing stamped header:\n%s", got)
		}
		if !strings.Contains(got, "Blocked on deploy-role perms.") {
			t.Errorf("missing body:\n%s", got)
		}
	})

	t.Run("replaces the block in place without duplicating or touching the original", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "brief.md")
		if err := os.WriteFile(path, []byte(origBrief), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeBriefCurrentState(path, "first state", "2026-06-01"); err != nil {
			t.Fatal(err)
		}
		if err := writeBriefCurrentState(path, "second state", date); err != nil {
			t.Fatal(err)
		}
		got := readFile(t, path)
		if strings.Count(got, briefStateStartMarker) != 1 || strings.Count(got, briefStateEndMarker) != 1 {
			t.Errorf("block was duplicated, expected one pair:\n%s", got)
		}
		if strings.Contains(got, "first state") || strings.Contains(got, "2026-06-01") {
			t.Errorf("stale state not replaced:\n%s", got)
		}
		if !strings.Contains(got, "second state") || !strings.Contains(got, "updated "+date) {
			t.Errorf("new state not written:\n%s", got)
		}
		if !strings.HasPrefix(got, origBrief) {
			t.Errorf("original brief disturbed:\n%s", got)
		}
	})

	t.Run("creates the file with just the block when missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "brief.md")
		if err := writeBriefCurrentState(path, "fresh", date); err != nil {
			t.Fatal(err)
		}
		got := readFile(t, path)
		if !strings.Contains(got, briefStateStartMarker) || !strings.Contains(got, "fresh") {
			t.Errorf("block not written to new file:\n%s", got)
		}
	})

	t.Run("preserves content that follows the block on replace", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "brief.md")
		seed := origBrief + "\n" + renderBriefStateBlock("old", "2026-06-01") + "\n\n## Footer\nkeep me\n"
		if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeBriefCurrentState(path, "new", date); err != nil {
			t.Fatal(err)
		}
		got := readFile(t, path)
		if !strings.Contains(got, "## Footer\nkeep me") {
			t.Errorf("trailing content after block was lost:\n%s", got)
		}
		if strings.Contains(got, "old") {
			t.Errorf("old state survived replace:\n%s", got)
		}
	})
}

// TestCmdUpdateTaskBriefStatus exercises the CLI path end-to-end: a real task,
// the --brief-status flag, the on-disk brief, and the updated_at bump.
func TestCmdUpdateTaskBriefStatus(t *testing.T) {
	root := setupFlowRoot(t)
	if rc := cmdAdd([]string{"task", "Fix bug", "--agent", "claude"}); rc != 0 {
		t.Fatalf("add task rc=%d", rc)
	}
	briefPath := filepath.Join(root, "tasks", "fix-bug", "brief.md")

	db := openFlowDB(t)
	// Pin updated_at to an old value so the bump is unambiguous.
	if _, err := db.Exec(`UPDATE tasks SET updated_at=? WHERE slug=?`, "2020-01-01T00:00:00Z", "fix-bug"); err != nil {
		t.Fatal(err)
	}

	if rc := cmdUpdate([]string{"task", "fix-bug", "--brief-status", "Waiting on Omendra to retry the release."}); rc != 0 {
		t.Fatalf("update --brief-status rc=%d", rc)
	}
	got := readFile(t, briefPath)
	if !strings.Contains(got, "Waiting on Omendra to retry the release.") {
		t.Errorf("brief not updated with status:\n%s", got)
	}
	if !strings.Contains(got, briefStateStartMarker) {
		t.Errorf("marker missing:\n%s", got)
	}

	task, err := flowdb.GetTask(db, "fix-bug")
	if err != nil {
		t.Fatal(err)
	}
	if task.UpdatedAt == "2020-01-01T00:00:00Z" {
		t.Error("updated_at was not bumped")
	}

	// Empty body is a no-op: it must not wipe the prior state.
	if rc := cmdUpdate([]string{"task", "fix-bug", "--brief-status", "   "}); rc != 0 {
		t.Fatalf("empty --brief-status rc=%d", rc)
	}
	if !strings.Contains(readFile(t, briefPath), "Waiting on Omendra to retry the release.") {
		t.Error("empty body wiped the prior current state")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
