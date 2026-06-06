package workdirreg

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestDetectGitRemoteFromWorktree is the regression guard for the bug that
// broke server-side PR auto-linking: a linked worktree's `.git` is a pointer
// file to a per-worktree gitdir that holds NO config of its own — the shared
// origin remote lives in the common dir, reached via the `commondir` pointer.
// resolveGitConfigPath used to append /config to the per-worktree gitdir (a
// file that never exists), so DetectGitRemote returned "" from any worktree
// checkout, and linkInProgressTaskPRs skipped every worktree task forever.
func TestDetectGitRemoteFromWorktree(t *testing.T) {
	const remote = "git@github.com:acme/app.git"
	root := t.TempDir()
	main := filepath.Join(root, "main")
	git(t, root, "init", "-q", "-b", "main", main)
	git(t, main, "remote", "add", "origin", remote)
	git(t, main, "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "init")

	if got := DetectGitRemote(main); got != remote {
		t.Fatalf("main checkout: DetectGitRemote = %q, want %q", got, remote)
	}

	wt := filepath.Join(root, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "feature", wt)

	if got := DetectGitRemote(wt); got != remote {
		t.Fatalf("worktree checkout: DetectGitRemote = %q, want %q (origin lives in the common .git/config, reached via commondir)", got, remote)
	}
}
