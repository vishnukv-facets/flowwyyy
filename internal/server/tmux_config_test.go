package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetTmuxConfigOnce flips the per-process write guard back to "not
// written yet" so consecutive tests within the same `go test` invocation
// each get a fresh write path. Without this, the second test sees
// tmuxConfigWritten=true from the first and short-circuits before
// creating a file. Production doesn't need this — a single flow server
// process only ever writes once.
func resetTmuxConfigOnce(t *testing.T) {
	t.Helper()
	tmuxConfigWriteOnce.Lock()
	tmuxConfigWritten = false
	tmuxConfigWriteOnce.Unlock()
}

func TestEnsureTmuxConfigWritesFileWhenAbsent(t *testing.T) {
	resetTmuxConfigOnce(t)
	dir := t.TempDir()
	path, err := ensureTmuxConfig(dir)
	if err != nil {
		t.Fatalf("ensureTmuxConfig: %v", err)
	}
	wantPath := filepath.Join(dir, "tmux.conf")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"set -g mouse on", "set -g history-limit 100000", "~/.tmux.conf"} {
		if !strings.Contains(string(contents), want) {
			t.Errorf("tmux.conf missing %q; contents=%s", want, contents)
		}
	}
}

func TestEnsureTmuxConfigPreservesUserEdits(t *testing.T) {
	resetTmuxConfigOnce(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux.conf")
	custom := "# user-edited\nset -g status-position top\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ensureTmuxConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	// Critical contract: a pre-existing file is NEVER overwritten so
	// users can hand-edit flow's defaults without losing them on restart.
	contents, _ := os.ReadFile(path)
	if string(contents) != custom {
		t.Errorf("user content overwritten\n got: %q\nwant: %q", contents, custom)
	}
}

func TestEnsureTmuxConfigIsIdempotentInProcess(t *testing.T) {
	resetTmuxConfigOnce(t)
	dir := t.TempDir()
	// First call writes; record mtime.
	if _, err := ensureTmuxConfig(dir); err != nil {
		t.Fatal(err)
	}
	stat1, err := os.Stat(filepath.Join(dir, "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	// Second call must short-circuit (sync.Mutex + tmuxConfigWritten
	// flag) — the file should not be touched again.
	if _, err := ensureTmuxConfig(dir); err != nil {
		t.Fatal(err)
	}
	stat2, err := os.Stat(filepath.Join(dir, "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !stat1.ModTime().Equal(stat2.ModTime()) {
		t.Errorf("file was re-written on second call; mtime changed %v → %v",
			stat1.ModTime(), stat2.ModTime())
	}
}

func TestEnsureTmuxConfigRejectsEmptyFlowRoot(t *testing.T) {
	resetTmuxConfigOnce(t)
	_, err := ensureTmuxConfig("")
	if err == nil {
		t.Errorf("expected error for empty flow root, got nil")
	}
}
