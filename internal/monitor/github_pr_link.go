package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/ghref"
	"flow/internal/workdirreg"
)

// ghPRURLForWorktree returns the URL of the open PR whose head is the worktree's
// current branch, or "" when there is none.
//
// It targets the worktree's ORIGIN repo explicitly via `gh pr list --repo …
// --head <branch>`. This is the fork-safe path: a bare `gh pr view` resolves
// the base repo ambiguously when an `upstream` remote is also present and looks
// for the PR on the WRONG repo (the upstream), returning "no pull requests
// found" even though the PR exists on the fork. Listing by head against the
// origin repo avoids that guess entirely. Package var so tests can stub it.
var ghPRURLForWorktree = func(ctx context.Context, dir string) (string, error) {
	repo, ok := ghref.RepoFromRemote(workdirreg.DetectGitRemote(dir))
	if !ok || repo == "" {
		return "", nil
	}
	branch, err := gitCurrentBranch(ctx, dir)
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

func gitCurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// linkInProgressTaskPRs tags in-progress tasks with the PR opened on their
// branch so the monitor starts tracking it.
//
// The CLI's linkTaskToCurrentBranchPR runs this only at `flow done`, which is
// too late for live monitoring: a PR raised WHILE the agent works (the common
// case) wouldn't be polled — its comments, reviews, and head updates would
// never reach the task until completion. Running the same linking each GitHub
// poll cycle for untagged in-progress tasks closes that gap: a PR opened
// mid-task gets tagged within one cycle, after which trackedGitHubPRs polls it
// normally.
//
// Gated to tasks with a worktree: that's both the correctness signal (the PR
// lookup must run against the task's OWN branch checkout, not a shared work_dir
// on main) and a cost bound (only dedicated-branch tasks shell out, and
// already-tagged tasks are skipped, so the work is self-limiting).
func linkInProgressTaskPRs(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Status: "in-progress"})
	if err != nil {
		return
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		dir := strings.TrimSpace(t.WorktreePath.String)
		if dir == "" {
			continue // no isolated branch checkout — nothing reliable to link
		}
		tags, err := flowdb.GetTaskTags(db, t.Slug)
		if err != nil || hasGitHubPRTag(tags) {
			continue // already tracked
		}
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		url, err := ghPRURLForWorktree(cctx, dir)
		cancel()
		if err != nil || url == "" {
			continue // no PR on this branch yet — quietly skip
		}
		tag, ok := ghref.PRTagFromURL(url)
		if !ok {
			continue
		}
		_ = flowdb.AddTaskTag(db, t.Slug, tag)
		_ = flowdb.AddTaskTag(db, t.Slug, "github")
	}
}

func hasGitHubPRTag(tags []string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(strings.TrimSpace(tag), "gh-pr:") {
			return true
		}
	}
	return false
}
