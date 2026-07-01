package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/ghpr"
	"flow/internal/ghref"
)

// ghPRURLForWorktree returns the URL of the open PR whose head is the worktree's
// current branch, or "" when there is none. The fork-safe, worktree-safe lookup
// lives in internal/ghpr so the CLI close-out path (flow done) resolves PRs the
// same way. Package var so tests can stub it.
var ghPRURLForWorktree = ghpr.OpenURLForBranch

// linkInProgressTaskPRs tags in-progress tasks with the PR opened on their
// branch so the monitor starts tracking it.
//
// The CLI's linkTaskToCurrentBranchPR runs this only at `flow done`, which is
// too late for live monitoring: a PR raised WHILE the agent works (the common
// case) wouldn't be polled — its comments, reviews, and head updates would
// never reach the task until completion. Running the same linking each GitHub
// poll cycle for untagged in-progress tasks closes that gap: a PR opened
// mid-task gets tagged within one cycle, after which the GitHub monitor polls
// it normally.
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

// linkInProgressIssuePRs tags in-progress issue-tracking tasks with the open
// PR(s) opened to resolve their issue, so the monitor polls those PRs' reviews
// and comments too.
//
// This complements linkInProgressTaskPRs, which links by matching the
// worktree's CURRENT branch to a PR head. That branch match misses the common
// case where the agent splits one issue into several PRs opened from
// sub-branches (e.g. flow/gh-139-bugs-5-6) that differ from the worktree's own
// branch (flow/gh-issue-…-139): no PR has the worktree's head, so nothing is
// linked and the PR's reviews never reach the task. The issue NUMBER is the
// stable anchor here, so we resolve PRs via GitHub's issue→PR cross-references
// instead of the local branch.
//
// Unlike the branch linker this does NOT stop at the first gh-pr: tag — one
// issue commonly spawns multiple PRs, and each must be tracked. AddTaskTag is
// idempotent (PK on task_slug+tag, INSERT OR IGNORE), so re-linking an
// already-tracked PR every cycle is a no-op. Cost is bounded: only in-progress
// tasks that carry a gh-issue: tag shell out, one call per tracked issue.
func linkInProgressIssuePRs(ctx context.Context, db *sql.DB, selfLogins []string) {
	if db == nil || len(selfLogins) == 0 {
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
		tags, err := flowdb.GetTaskTags(db, t.Slug)
		if err != nil {
			continue
		}
		for _, tag := range tags {
			issue, ok := parseGitHubRefTag(tag, "gh-issue:")
			if !ok {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			prs, err := ghOpenPRsForIssue(cctx, issue.Owner, issue.Repo, issue.Number, selfLogins)
			cancel()
			if err != nil {
				continue // transient gh failure — try again next cycle
			}
			for _, n := range prs {
				// Owner/repo come from the (already lowercased) gh-issue tag, so
				// the derived gh-pr tag is lowercase and routes via findTaskByGitHubTag.
				prTag := fmt.Sprintf("gh-pr:%s/%s#%d", issue.Owner, issue.Repo, n)
				_ = flowdb.AddTaskTag(db, t.Slug, prTag)
				_ = flowdb.AddTaskTag(db, t.Slug, "github")
			}
		}
	}
}

// ghOpenPRsForIssue resolves open PRs cross-referencing an issue via the native
// SDK (see sdkOpenPRsForIssue). Package var so tests can stub it.
var ghOpenPRsForIssue = sdkOpenPRsForIssue

func hasGitHubPRTag(tags []string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(strings.TrimSpace(tag), "gh-pr:") {
			return true
		}
	}
	return false
}
