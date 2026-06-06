package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
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

// ghOpenPRsForIssue returns the numbers of OPEN pull requests in the issue's
// own repo that are cross-referenced from the issue (i.e. a PR that mentions or
// closes it) and authored by one of the self logins. Package var so tests can
// stub it.
//
// Closed/merged PRs are excluded so linking never resurrects a dead PR (its
// merge/close is surfaced once by pollTrackedPRComments and would otherwise
// re-fire here). The self-login filter rejects strangers' PRs that merely
// mention the issue — the operator's agent authors its PRs as the operator.
var ghOpenPRsForIssue = func(ctx context.Context, owner, repo string, number int, selfLogins []string) ([]int, error) {
	endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/timeline", owner, repo, number)
	// --paginate so a cross-reference buried past the first 100 timeline events
	// is still found; -X GET because any -f turns `gh api` into a POST.
	out, err := exec.CommandContext(ctx, "gh", "api", "--paginate", "-X", "GET", endpoint, "-f", "per_page=100").Output()
	if err != nil {
		return nil, err
	}
	var events []struct {
		Event  string `json:"event"`
		Source struct {
			Issue struct {
				Number      int             `json:"number"`
				State       string          `json:"state"`
				PullRequest json.RawMessage `json:"pull_request"`
				User        githubUser      `json:"user"`
				Repository  struct {
					FullName string `json:"full_name"`
				} `json:"repository"`
			} `json:"issue"`
		} `json:"source"`
	}
	if err := json.Unmarshal(out, &events); err != nil {
		return nil, err
	}
	self := map[string]bool{}
	for _, l := range selfLogins {
		if v := strings.ToLower(strings.TrimSpace(l)); v != "" {
			self[v] = true
		}
	}
	sameRepo := strings.ToLower(owner + "/" + repo)
	seen := map[int]bool{}
	var prs []int
	for _, e := range events {
		if e.Event != "cross-referenced" {
			continue
		}
		src := e.Source.Issue
		if len(src.PullRequest) == 0 || string(src.PullRequest) == "null" {
			continue // cross-ref from a plain issue, not a PR
		}
		if src.Number <= 0 || seen[src.Number] {
			continue
		}
		if !strings.EqualFold(src.State, "open") {
			continue // only track live PRs; merged/closed handled elsewhere
		}
		// A cross-reference can originate in another repo; our tag is scoped to
		// the issue's repo, so only link same-repo PRs. Absent repository means
		// same-repo in GitHub's timeline payload.
		if fn := strings.TrimSpace(src.Repository.FullName); fn != "" && !strings.EqualFold(fn, sameRepo) {
			continue
		}
		if !self[strings.ToLower(strings.TrimSpace(src.User.Login))] {
			continue // someone else's PR that merely mentions the issue
		}
		seen[src.Number] = true
		prs = append(prs, src.Number)
	}
	return prs, nil
}

func hasGitHubPRTag(tags []string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(strings.TrimSpace(tag), "gh-pr:") {
			return true
		}
	}
	return false
}
