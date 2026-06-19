package flowbackup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// remoteName is the single offsite remote the backup repo pushes to.
const remoteName = "origin"

// dbBranch is the branch holding ONLY the latest db snapshot, force-pushed as a
// single orphan commit each time so the remote stays bounded (no binary history).
const dbBranch = "flow-db"

// dbSnapshotFile is the path of the snapshot blob within the db branch tree.
const dbSnapshotFile = "flow.db.gz"

// RemoteConfigured reports whether an offsite remote is set on the backup repo.
func RemoteConfigured(root string) bool { return RemoteURL(root) != "" }

// RemoteURL returns the configured offsite remote URL, or "".
func RemoteURL(root string) string {
	if !isRepo(root) {
		return ""
	}
	repo, err := openRepo(root)
	if err != nil {
		return ""
	}
	r, err := repo.Remote(remoteName)
	if err != nil || r == nil {
		return ""
	}
	urls := r.Config().URLs
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}

// SetRemote configures (or replaces) the offsite remote. The URL must point at a
// PRIVATE repository — the KB carries personal/org facts.
func SetRemote(root, url string) error {
	if err := EnsureRepo(root); err != nil {
		return err
	}
	repo, err := openRepo(root)
	if err != nil {
		return err
	}
	_ = repo.DeleteRemote(remoteName) // replace if present
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{strings.TrimSpace(url)}}); err != nil {
		return fmt.Errorf("flowbackup: set remote: %w", err)
	}
	return nil
}

// ClearRemote removes the offsite remote.
func ClearRemote(root string) error {
	if !isRepo(root) {
		return nil
	}
	repo, err := openRepo(root)
	if err != nil {
		return err
	}
	if err := repo.DeleteRemote(remoteName); err != nil && !errors.Is(err, git.ErrRemoteNotFound) {
		return err
	}
	return nil
}

// Push sends the markdown branch to the offsite remote. No-op when no remote is
// configured. Best-effort: returns nil when already up to date.
func Push(root string) error {
	if !Enabled() || !RemoteConfigured(root) {
		return nil
	}
	repo, err := openRepo(root)
	if err != nil {
		return err
	}
	auth, err := authFor(RemoteURL(root))
	if err != nil {
		return err
	}
	err = repo.Push(&git.PushOptions{
		RemoteName: remoteName,
		Auth:       auth,
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + defaultBranch + ":refs/heads/" + defaultBranch)},
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// PushDBSnapshot force-pushes the gzipped snapshot at snapPath to the dbBranch as
// a single orphan commit, so the remote retains only the newest db (bounded).
func PushDBSnapshot(root, snapPath string) error {
	if !Enabled() || !RemoteConfigured(root) || snapPath == "" {
		return nil
	}
	repo, err := openRepo(root)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(snapPath)
	if err != nil {
		return fmt.Errorf("flowbackup: read snapshot: %w", err)
	}
	commitHash, err := writeSingleFileCommit(repo, dbSnapshotFile, data, "flow backup: db snapshot "+time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	ref := plumbing.NewBranchReferenceName(dbBranch)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, commitHash)); err != nil {
		return fmt.Errorf("flowbackup: set db branch ref: %w", err)
	}
	auth, err := authFor(RemoteURL(root))
	if err != nil {
		return err
	}
	err = repo.Push(&git.PushOptions{
		RemoteName: remoteName,
		Auth:       auth,
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + ref.String() + ":" + ref.String())},
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// writeSingleFileCommit creates an orphan (parentless) commit whose tree holds a
// single file, and returns its hash. Used for the bounded db-snapshot branch.
func writeSingleFileCommit(repo *git.Repository, name string, content []byte, message string) (plumbing.Hash, error) {
	store := repo.Storer
	// Blob.
	blob := store.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	_ = w.Close()
	blobHash, err := store.SetEncodedObject(blob)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	// Tree with one entry.
	tree := &object.Tree{Entries: []object.TreeEntry{{Name: name, Mode: 0o100644, Hash: blobHash}}}
	treeObj := store.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, err
	}
	treeHash, err := store.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	// Orphan commit.
	sig := backupAuthor()
	commit := &object.Commit{Author: *sig, Committer: *sig, Message: message, TreeHash: treeHash}
	commitObj := store.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, err
	}
	return store.SetEncodedObject(commitObj)
}

// authFor resolves a transport auth method from a remote URL. Local paths and
// file:// need none; https uses a token from the environment; ssh uses the
// ssh-agent. Returns a clear error when ssh-agent isn't available.
func authFor(url string) (transport.AuthMethod, error) {
	u := strings.TrimSpace(url)
	switch {
	case u == "", strings.HasPrefix(u, "/"), strings.HasPrefix(u, "."), strings.HasPrefix(u, "file://"):
		return nil, nil // local — no auth
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		tok := backupToken()
		if tok == "" && strings.Contains(u, "github.com") {
			tok = ghToken() // fall back to the gh CLI's token for github.com
		}
		if tok != "" {
			return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
		}
		return nil, nil // try unauthenticated; a private repo will fail with a clear git error
	default: // assume ssh (git@host:owner/repo, ssh://...)
		user := "git"
		if i := strings.Index(u, "@"); i > 0 && !strings.ContainsAny(u[:i], "/:") {
			user = u[:i]
		}
		a, err := gitssh.NewSSHAgentAuth(user)
		if err != nil {
			return nil, fmt.Errorf("flowbackup: ssh auth via agent failed (%w); start ssh-agent with your key, or use an https remote with FLOW_BACKUP_TOKEN set", err)
		}
		return a, nil
	}
}

// backupToken returns a token for https remotes, from the first set of the
// common env vars.
func backupToken() string {
	for _, k := range []string{"FLOW_BACKUP_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Clone restores a backup repo from a remote into root, using a separated gitdir
// so no `.git` link is left at the flow root. The markdown working tree
// (kb + briefs/updates) is checked out. Intended for new-laptop restore.
func Clone(root, url string) error {
	if err := os.MkdirAll(gitDir(root), 0o755); err != nil {
		return fmt.Errorf("flowbackup: mkdir gitdir for clone: %w", err)
	}
	storer := filesystem.NewStorage(osfs.New(gitDir(root)), cache.NewObjectLRUDefault())
	auth, err := authFor(url)
	if err != nil {
		return err
	}
	_, err = git.Clone(storer, osfs.New(root), &git.CloneOptions{
		URL:           strings.TrimSpace(url),
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(defaultBranch),
		SingleBranch:  true,
	})
	if err != nil {
		return fmt.Errorf("flowbackup: clone: %w", err)
	}
	removeDotGitLink(root)
	return nil
}

// FetchDBSnapshotBytes fetches the dbBranch from the configured remote and
// returns the gzipped snapshot bytes. Returns (nil, nil) when the remote has no
// db branch.
func FetchDBSnapshotBytes(root string) ([]byte, error) {
	if !RemoteConfigured(root) {
		return nil, nil
	}
	repo, err := openRepo(root)
	if err != nil {
		return nil, err
	}
	auth, err := authFor(RemoteURL(root))
	if err != nil {
		return nil, err
	}
	remoteRef := "refs/remotes/" + remoteName + "/" + dbBranch
	err = repo.Fetch(&git.FetchOptions{
		RemoteName: remoteName,
		Auth:       auth,
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/" + dbBranch + ":" + remoteRef)},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if errors.Is(err, git.NoMatchingRefSpecError{}) {
			return nil, nil // no db branch on the remote
		}
		return nil, fmt.Errorf("flowbackup: fetch db branch: %w", err)
	}
	ref, err := repo.Reference(plumbing.ReferenceName(remoteRef), true)
	if err != nil {
		return nil, nil // no db branch
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	f, err := commit.File(dbSnapshotFile)
	if err != nil {
		return nil, fmt.Errorf("flowbackup: db snapshot file missing on remote: %w", err)
	}
	rc, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
