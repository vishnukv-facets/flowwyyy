// Package ghpr resolves the open GitHub pull request for a local checkout's
// current branch, in a way that is safe for both git worktrees and forks.
//
// It is shared by the CLI close-out path (flow done) and the server's live
// monitor poll, which both must answer "is there an open PR for the branch
// checked out in this directory?" — and must answer it the same way.
package ghpr

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"flow/internal/ghref"
	"flow/internal/workdirreg"
)

// OpenURLForBranch returns the URL of the open PR whose head is dir's current
// branch, or "" when there is none.
//
// It targets dir's ORIGIN repo explicitly via `gh pr list --repo … --head
// <branch>`. This is the fork-safe path: a bare `gh pr view` resolves the base
// repo ambiguously when an `upstream` remote is also present and looks for the
// PR on the WRONG repo (the upstream), returning "no pull requests found" even
// though the PR exists on the fork. Listing by head against the origin repo
// avoids that guess. Worktree-safe because the remote and branch are both read
// from dir — for a task that is its dedicated worktree checkout, whose `.git`
// is a pointer file (see workdirreg.DetectGitRemote).
func OpenURLForBranch(ctx context.Context, dir string) (string, error) {
	repo, ok := ghref.RepoFromRemote(workdirreg.DetectGitRemote(dir))
	if !ok || repo == "" {
		return "", nil
	}
	branch, err := currentBranch(ctx, dir)
	if err != nil || branch == "" {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--repo", repo, "--head", branch, "--state", "open",
		"--json", "url", "--limit", "1")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var rows []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return "", err
	}
	return strings.TrimSpace(rows[0].URL), nil
}

func currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
