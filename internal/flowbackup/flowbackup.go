// Package flowbackup maintains a durable, self-managed git repository over the
// flow root (~/.flow or $FLOW_ROOT) so that curated markdown — the knowledge
// base (kb/*.md) and every project/task/playbook/owner brief.md + updates/*.md
// — is versioned and recoverable independent of any single feature's
// correctness.
//
// Why git: many KB writes happen out-of-process (the flow-done close-out sweep,
// the capture_kb action, and the dreaming "move to Pending removal" pass all
// edit kb/*.md from headless `claude -p` sessions via the skill's file tools).
// A per-write Go hook can't see those. A git repo is write-source-agnostic: a
// checkpoint commits whatever is on disk, no matter who wrote it (Go code, a
// headless agent, or the operator hand-editing markdown).
//
// Implementation: pure-Go go-git (github.com/go-git/go-git/v5) — no external
// `git` binary, consistent with flow's no-CGO/pure-Go ethos. The set of tracked
// files is chosen in Go (walkCurated), not by .gitignore pattern matching, so
// flow.db (476MB), the .ui-session-token secret, logs, caches, and agent session
// files are never staged. A .gitignore is still written so raw-git users and
// go-git's own Status skip those paths.
//
// Best-effort: every operation degrades to a no-op + warning rather than
// blocking the underlying KB write, `flow done`, or UI save. Checkpoints are
// serialized by an advisory file lock so a server-side dreamer pass and a
// `flow done` in another tab don't race on the git index.
//
// The database (flow.db) is NOT tracked here (binary, huge, high-churn); it gets
// its own rotated snapshot net — see dbsnapshot.go.
package flowbackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	dmp "github.com/sergi/go-diff/diffmatchpatch"
)

// Enabled reports whether the backup subsystem is active. Default on; set
// FLOW_BACKUP_ENABLED=0/false/off to disable entirely (a global no-op).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_BACKUP_ENABLED"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// defaultBranch is the branch the markdown working tree lives on. Fixed so the
// remote push (remote.go) has a stable ref.
const defaultBranch = "main"

// gitDirName is the SEPARATED git directory under the flow root. It is
// deliberately NOT ".git": a discoverable .git at the flow root would make every
// adhoc task workspace (~/.flow/tasks/<slug>/workspace/) appear "inside a repo",
// breaking flow's worktree detection. With a separated gitdir + worktree pointed
// at the root, no .git is ever written into ~/.flow, so `git rev-parse` from any
// subdir finds nothing and flow's repo detection is unaffected.
const gitDirName = ".backupgit"

// backupAuthor is the self-contained commit identity, so commits never depend on
// (or mutate) the user's global git config.
func backupAuthor() *object.Signature {
	return &object.Signature{Name: "flow backup", Email: "flow-backup@localhost", When: time.Now()}
}

// gitDir returns the separated git directory path for a flow root.
func gitDir(root string) string { return filepath.Join(root, gitDirName) }

// isRepo reports whether the backup repo has been initialized under root.
func isRepo(root string) bool {
	_, err := os.Stat(filepath.Join(gitDir(root), "HEAD"))
	return err == nil
}

// openRepo opens the backup repo (separated gitdir + worktree at root) for
// read/stage operations. Returns git.ErrRepositoryNotExists if uninitialized.
func openRepo(root string) (*git.Repository, error) {
	if !isRepo(root) {
		return nil, git.ErrRepositoryNotExists
	}
	storer := filesystem.NewStorage(osfs.New(gitDir(root)), cache.NewObjectLRUDefault())
	return git.Open(storer, osfs.New(root))
}

// openOrInitRepo opens the backup repo, initializing the separated gitdir on
// first use with the default branch.
func openOrInitRepo(root string) (*git.Repository, error) {
	if isRepo(root) {
		return openRepo(root)
	}
	if err := os.MkdirAll(gitDir(root), 0o755); err != nil {
		return nil, fmt.Errorf("flowbackup: mkdir gitdir: %w", err)
	}
	storer := filesystem.NewStorage(osfs.New(gitDir(root)), cache.NewObjectLRUDefault())
	repo, err := git.Init(storer, osfs.New(root))
	if err != nil {
		return nil, fmt.Errorf("flowbackup: init repo: %w", err)
	}
	// go-git writes a `.git` link file (gitdir: .backupgit) into the worktree.
	// Remove it: we never rely on git's worktree discovery (openRepo always
	// rebuilds the storer+worktree pair by hand), and a discoverable `.git` at
	// the flow root would make adhoc task workspaces look like repos.
	removeDotGitLink(root)
	// go-git defaults HEAD to refs/heads/master; point it at our branch before
	// the first commit.
	_ = storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(defaultBranch)))
	return repo, nil
}

// removeDotGitLink deletes a stray `.git` pointer FILE at the flow root (the one
// go-git's Init/Clone writes for a separated gitdir). It only removes a regular
// file, never a real `.git` directory.
func removeDotGitLink(root string) {
	p := filepath.Join(root, ".git")
	if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
		_ = os.Remove(p)
	}
}

// EnsureRepo makes root a git repo with the whitelist .gitignore and at least
// one (empty) baseline commit so HEAD exists. Idempotent. Called lazily by
// Checkpoint so existing installs get a repo without re-running `flow init`.
// Committing the working tree is the job of Checkpoint, not this function.
func EnsureRepo(root string) error {
	if root == "" {
		return fmt.Errorf("flowbackup: empty root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("flowbackup: mkdir root: %w", err)
	}
	repo, err := openOrInitRepo(root)
	if err != nil {
		return err
	}
	// Defensively drop any stray `.git` link file (e.g. if a go-git op recreated
	// it) so the flow root never looks like a discoverable repo.
	removeDotGitLink(root)
	if err := writeGitignore(root); err != nil {
		return err
	}
	if _, err := repo.Head(); err != nil {
		// No commits yet — create an empty baseline so Log/Show/Restore have a HEAD.
		wt, werr := repo.Worktree()
		if werr != nil {
			return fmt.Errorf("flowbackup: worktree: %w", werr)
		}
		if _, cerr := wt.Commit("flow backup: initial baseline", &git.CommitOptions{
			Author:            backupAuthor(),
			AllowEmptyCommits: true,
		}); cerr != nil {
			return fmt.Errorf("flowbackup: baseline commit: %w", cerr)
		}
	}
	return nil
}

// Checkpoint commits the current state of the curated markdown with the given
// reason. Returns committed=true when a new commit was made, false when nothing
// changed (no empty commits). Best-effort and serialized by an advisory lock.
func Checkpoint(root, reason string) (committed bool, err error) {
	if !Enabled() {
		return false, nil
	}
	unlock, err := acquireLock(root)
	if err != nil {
		return false, err
	}
	defer unlock()
	return checkpointLocked(root, reason)
}

// checkpointLocked is the commit body, assuming the lock is held. Reused by
// Restore to avoid re-locking.
func checkpointLocked(root, reason string) (bool, error) {
	if err := EnsureRepo(root); err != nil {
		return false, err
	}
	repo, err := openRepo(root)
	if err != nil {
		return false, fmt.Errorf("flowbackup: open repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("flowbackup: worktree: %w", err)
	}

	// Stage ALL changes in a single pass (add/modify/delete), respecting
	// .gitignore. The blacklist .gitignore leaves only curated markdown
	// non-ignored, so flow.db, tokens, logs, caches, and agent sessions are
	// skipped (not even hashed). This is O(worktree-once); adding files
	// one-by-one was O(n^2) — ~74s on a real ~/.flow with ~1100 markdown files.
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return false, fmt.Errorf("flowbackup: stage: %w", err)
	}

	msg := fmt.Sprintf("%s — %s", strings.TrimSpace(reason), time.Now().UTC().Format(time.RFC3339))
	if _, err := wt.Commit(msg, &git.CommitOptions{Author: backupAuthor()}); err != nil {
		if errors.Is(err, git.ErrEmptyCommit) {
			return false, nil // nothing changed
		}
		return false, fmt.Errorf("flowbackup: commit: %w", err)
	}
	return true, nil
}

// GC is a no-op: go-git has no repack/gc, and curated markdown produces only
// tiny loose objects, so the repo stays small. (A human may run `git gc` on the
// root manually if ever desired.)
func GC(root string) error { return nil }

// Commit is one entry in the backup history.
type Commit struct {
	Rev     string `json:"rev"`     // full sha
	Short   string `json:"short"`   // abbreviated sha
	When    string `json:"when"`    // RFC3339 author date
	Subject string `json:"subject"` // commit message subject (the checkpoint reason)
}

// Log returns the most-recent-first checkpoint history, optionally limited to a
// single tracked file (relpath relative to root). limit <= 0 means no cap.
func Log(root, relpath string, limit int) ([]Commit, error) {
	if !isRepo(root) {
		return nil, nil
	}
	repo, err := openRepo(root)
	if err != nil {
		return nil, err
	}
	opts := &git.LogOptions{}
	if rel := normalizeRel(relpath); rel != "" {
		opts.FileName = &rel
	}
	iter, err := repo.Log(opts)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var commits []Commit
	err = iter.ForEach(func(c *object.Commit) error {
		commits = append(commits, Commit{
			Rev:     c.Hash.String(),
			Short:   c.Hash.String()[:12],
			When:    c.Author.When.UTC().Format(time.RFC3339),
			Subject: strings.SplitN(c.Message, "\n", 2)[0],
		})
		if limit > 0 && len(commits) >= limit {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return commits, err
	}
	return commits, nil
}

// errStop is a sentinel used to break out of a commit-iter ForEach early.
var errStop = errors.New("stop")

// Show returns the contents of a tracked file at a given revision.
func Show(root, rev, relpath string) ([]byte, error) {
	if !isRepo(root) {
		return nil, fmt.Errorf("flowbackup: no repo at %s", root)
	}
	repo, err := openRepo(root)
	if err != nil {
		return nil, err
	}
	commit, err := resolveCommit(repo, rev)
	if err != nil {
		return nil, err
	}
	f, err := commit.File(normalizeRel(relpath))
	if err != nil {
		return nil, fmt.Errorf("flowbackup: %s@%s: %w", normalizeRel(relpath), rev, err)
	}
	contents, err := f.Contents()
	if err != nil {
		return nil, err
	}
	return []byte(contents), nil
}

// Diff returns a readable line-level diff of a tracked file between its content
// at rev and the current on-disk file.
func Diff(root, rev, relpath string) (string, error) {
	if !isRepo(root) {
		return "", fmt.Errorf("flowbackup: no repo at %s", root)
	}
	rel := normalizeRel(relpath)
	var oldStr string
	if b, err := Show(root, rev, rel); err == nil {
		oldStr = string(b)
	}
	newStr := ""
	if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
		newStr = string(b)
	}
	return lineDiff(oldStr, newStr), nil
}

// Restore rolls a single tracked file back to its content at rev. The current
// state is checkpointed first (so the restore is itself reversible), the file is
// rewritten from rev, and the result is checkpointed.
func Restore(root, relpath, rev string) error {
	if !Enabled() {
		return fmt.Errorf("flowbackup: disabled")
	}
	if !isRepo(root) {
		return fmt.Errorf("flowbackup: no repo at %s", root)
	}
	rel := normalizeRel(relpath)
	unlock, err := acquireLock(root)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := checkpointLocked(root, "pre-restore "+rel); err != nil {
		return err
	}
	content, err := Show(root, rev, rel)
	if err != nil {
		return err
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("flowbackup: mkdir for restore: %w", err)
	}
	if err := os.WriteFile(dst, content, 0o644); err != nil {
		return fmt.Errorf("flowbackup: write restored file: %w", err)
	}
	if _, err := checkpointLocked(root, fmt.Sprintf("restore %s@%s", rel, shortRev(rev))); err != nil {
		return err
	}
	return nil
}

// CommitCount returns the number of checkpoints in history (0 if no repo).
func CommitCount(root string) int {
	commits, err := Log(root, "", 0)
	if err != nil {
		return 0
	}
	return len(commits)
}

// trackedPaths returns the repo-relative paths tracked at HEAD (empty when there
// are no commits yet beyond an empty baseline).
func trackedPaths(repo *git.Repository) ([]string, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, nil // no HEAD yet
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	var paths []string
	err = tree.Files().ForEach(func(f *object.File) error {
		paths = append(paths, f.Name)
		return nil
	})
	return paths, err
}

func resolveCommit(repo *git.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("flowbackup: resolve %q: %w", rev, err)
	}
	return repo.CommitObject(*h)
}

// lineDiff renders a +/- line-level diff between old and new text.
func lineDiff(oldStr, newStr string) string {
	d := dmp.New()
	c1, c2, lines := d.DiffLinesToChars(oldStr, newStr)
	diffs := d.DiffCharsToLines(d.DiffMain(c1, c2, false), lines)
	var b strings.Builder
	for _, df := range diffs {
		prefix := " "
		switch df.Type {
		case dmp.DiffInsert:
			prefix = "+"
		case dmp.DiffDelete:
			prefix = "-"
		}
		for _, ln := range strings.SplitAfter(df.Text, "\n") {
			if ln == "" {
				continue
			}
			b.WriteString(prefix + strings.TrimSuffix(ln, "\n") + "\n")
		}
	}
	return b.String()
}

// normalizeRel cleans a caller-supplied relpath to a forward-slash repo path,
// stripping any leading separators so it's always relative to the repo root.
func normalizeRel(relpath string) string {
	p := filepath.ToSlash(strings.TrimSpace(relpath))
	return strings.TrimPrefix(p, "/")
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
