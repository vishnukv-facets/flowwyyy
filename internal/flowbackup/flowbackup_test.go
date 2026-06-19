package flowbackup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// write is a tiny helper that creates parent dirs and writes a file.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// trackedList returns the repo-relative paths tracked at HEAD, sorted.
func trackedList(t *testing.T, root string) []string {
	t.Helper()
	repo, err := openRepo(root)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	paths, err := trackedPaths(repo)
	if err != nil {
		t.Fatalf("trackedPaths: %v", err)
	}
	sort.Strings(paths)
	return paths
}

func TestEnsureRepoIdempotent(t *testing.T) {
	root := t.TempDir()

	if err := EnsureRepo(root); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if !isRepo(root) {
		t.Fatal("expected .git after EnsureRepo")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); err != nil {
		t.Fatalf("expected .gitignore: %v", err)
	}
	repo, err := openRepo(root)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	if _, err := repo.Head(); err != nil {
		t.Fatalf("expected a baseline commit (HEAD): %v", err)
	}
	// Second call must be a clean no-op.
	if err := EnsureRepo(root); err != nil {
		t.Fatalf("EnsureRepo (2nd): %v", err)
	}
}

// TestNoDotGitAtRoot guards the invariant that makes this design safe: the
// backup repo uses a SEPARATED gitdir (.backupgit), and no `.git` (file or dir)
// is ever left at the flow root — otherwise adhoc task workspaces under the root
// would be detected as living inside a git repo.
func TestNoDotGitAtRoot(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepo(root); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	write(t, filepath.Join(root, "kb", "org.md"), "fact")
	if _, err := Checkpoint(root, "cp"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		t.Fatal(".git must not exist at the flow root (separated gitdir invariant)")
	}
	if _, err := os.Stat(filepath.Join(root, gitDirName)); err != nil {
		t.Fatalf("expected separated gitdir %s: %v", gitDirName, err)
	}
}

// TestGitignoreWhitelist is the safety-critical test: after a checkpoint, ONLY
// curated markdown (+ .gitignore) is tracked — never flow.db, the session
// token, logs, jsonl, metadata json, or foreign repos in adhoc workspaces.
func TestGitignoreWhitelist(t *testing.T) {
	root := t.TempDir()

	// Things that MUST NOT be tracked.
	write(t, filepath.Join(root, "flow.db"), strings.Repeat("x", 4096))
	write(t, filepath.Join(root, ".ui-session-token"), "secret-token")
	write(t, filepath.Join(root, "config.json"), "{}")
	write(t, filepath.Join(root, "logs", "ui.log"), "log line")
	write(t, filepath.Join(root, "cache", "blob"), "cache")
	write(t, filepath.Join(root, ".claude", "projects", "x.jsonl"), "{}")
	write(t, filepath.Join(root, "tasks", "t", "inbox.jsonl"), "{}")
	write(t, filepath.Join(root, "tasks", "t", "metadata", "git-start.json"), "{}")
	write(t, filepath.Join(root, "tasks", "t", "workspace", "foreign.md"), "# foreign repo readme")
	write(t, filepath.Join(root, "tasks", "t", "workspace", ".git", "config"), "[core]")
	write(t, filepath.Join(root, "backups", "db", "flow-x.db.gz"), "gz")

	// Things that MUST be tracked.
	write(t, filepath.Join(root, "kb", "org.md"), "# org")
	write(t, filepath.Join(root, "projects", "p", "brief.md"), "# project")
	write(t, filepath.Join(root, "tasks", "t", "brief.md"), "# task")
	write(t, filepath.Join(root, "tasks", "t", "updates", "2026-01-01-note.md"), "note")
	write(t, filepath.Join(root, "owners", "o", "charter.md"), "# owner")

	committed, err := Checkpoint(root, "test checkpoint")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !committed {
		t.Fatal("expected first checkpoint to commit")
	}

	got := trackedList(t, root)
	want := []string{
		".gitignore",
		"kb/org.md",
		"owners/o/charter.md",
		"projects/p/brief.md",
		"tasks/t/brief.md",
		"tasks/t/updates/2026-01-01-note.md",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tracked files mismatch.\n got: %v\nwant: %v", got, want)
	}
}

func TestCheckpointNoOpWhenClean(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "kb", "user.md"), "# user")

	c1, err := Checkpoint(root, "first")
	if err != nil || !c1 {
		t.Fatalf("first checkpoint: committed=%v err=%v", c1, err)
	}
	c2, err := Checkpoint(root, "second-noop")
	if err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if c2 {
		t.Fatal("expected no-op (no commit) when tree is clean")
	}
}

func TestCheckpointStagesDeletion(t *testing.T) {
	root := t.TempDir()
	kb := filepath.Join(root, "kb", "org.md")
	write(t, kb, "fact")
	if _, err := Checkpoint(root, "add"); err != nil {
		t.Fatalf("checkpoint add: %v", err)
	}
	if err := os.Remove(kb); err != nil {
		t.Fatalf("remove: %v", err)
	}
	committed, err := Checkpoint(root, "after-delete")
	if err != nil {
		t.Fatalf("checkpoint after delete: %v", err)
	}
	if !committed {
		t.Fatal("expected a commit recording the deletion")
	}
	for _, p := range trackedList(t, root) {
		if p == "kb/org.md" {
			t.Fatal("deleted file should no longer be tracked")
		}
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	kb := filepath.Join(root, "kb", "org.md")

	write(t, kb, "version one")
	if _, err := Checkpoint(root, "v1"); err != nil {
		t.Fatalf("checkpoint v1: %v", err)
	}
	commits, err := Log(root, "kb/org.md", 0)
	if err != nil || len(commits) == 0 {
		t.Fatalf("log after v1: commits=%d err=%v", len(commits), err)
	}
	v1Rev := commits[0].Rev

	write(t, kb, "version two — DESTRUCTIVE")
	if _, err := Checkpoint(root, "v2"); err != nil {
		t.Fatalf("checkpoint v2: %v", err)
	}

	// Simulate the incident: blow the file away entirely, then restore v1.
	if err := os.WriteFile(kb, []byte(""), 0o644); err != nil {
		t.Fatalf("blank file: %v", err)
	}
	if err := Restore(root, "kb/org.md", v1Rev); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(kb)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(got) != "version one" {
		t.Fatalf("restored content = %q, want %q", string(got), "version one")
	}
	// The restore itself is a checkpoint, so history grew beyond v1+v2.
	all, _ := Log(root, "", 0)
	if len(all) < 4 { // baseline + v1 + v2 + restore
		t.Fatalf("expected restore to add commits, got %d", len(all))
	}
}

func TestShowReturnsHistoricalContent(t *testing.T) {
	root := t.TempDir()
	kb := filepath.Join(root, "kb", "business.md")
	write(t, kb, "original facts")
	if _, err := Checkpoint(root, "orig"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	commits, _ := Log(root, "kb/business.md", 0)
	rev := commits[0].Rev
	write(t, kb, "changed")
	if _, err := Checkpoint(root, "changed"); err != nil {
		t.Fatalf("checkpoint2: %v", err)
	}
	body, err := Show(root, rev, "kb/business.md")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if string(body) != "original facts" {
		t.Fatalf("Show = %q, want %q", body, "original facts")
	}
}

func TestDisabledIsNoOp(t *testing.T) {
	t.Setenv("FLOW_BACKUP_ENABLED", "0")
	root := t.TempDir()
	write(t, filepath.Join(root, "kb", "user.md"), "x")
	committed, err := Checkpoint(root, "should-not-run")
	if err != nil {
		t.Fatalf("disabled Checkpoint err: %v", err)
	}
	if committed {
		t.Fatal("expected no commit when disabled")
	}
	if isRepo(root) {
		t.Fatal("expected no repo created when disabled")
	}
}
