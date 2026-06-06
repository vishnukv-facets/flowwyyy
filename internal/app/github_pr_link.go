package app

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/ghpr"
	"flow/internal/ghref"
)

// openPRURLForBranch is the fork-safe + worktree-safe "open PR for this dir's
// branch" lookup, shared with the live monitor poll. Package var so tests can
// stub it.
var openPRURLForBranch = ghpr.OpenURLForBranch

// linkTaskToCurrentBranchPR tags the task with the open PR on its branch, so
// `flow done` leaves the task tracking its PR (reviews/merges keep flowing).
//
// It inspects the task's dedicated WORKTREE when it has one — that's the
// checkout actually on the task branch. Using work_dir for a worktree task
// would inspect the repo root, which usually sits on `main` with no PR, and
// silently link nothing (the bug this guards against).
func linkTaskToCurrentBranchPR(db *sql.DB, task *flowdb.Task) error {
	if db == nil || task == nil {
		return nil
	}
	dir := strings.TrimSpace(task.WorktreePath.String)
	if dir == "" {
		dir = strings.TrimSpace(task.WorkDir)
	}
	if dir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	url, err := openPRURLForBranch(ctx, dir)
	if err != nil || url == "" {
		return err
	}
	tag, ok := ghref.PRTagFromURL(url)
	if !ok {
		return nil
	}
	return flowdb.AddTaskTag(db, task.Slug, tag)
}
