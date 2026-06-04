package monitor

import (
	"context"
	"errors"
	"testing"

	"flow/internal/flowdb"
)

func TestLinkInProgressTaskPRsTagsWorktreePR(t *testing.T) {
	db := dispatcherTestDB(t)
	wt := t.TempDir()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, worktree_path, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'in-progress', 'high', ?, ?, 'default', 'claude', 'sess-wt', ?, ?, ?)`,
		"wt-task", "worktree task", t.TempDir(), wt, now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	orig := ghPRURLForWorktree
	defer func() { ghPRURLForWorktree = orig }()
	ghPRURLForWorktree = func(_ context.Context, dir string) (string, error) {
		if dir == wt {
			return "https://github.com/vishnukv-facets/flow-manager/pull/4", nil
		}
		return "", errors.New("no pr")
	}

	linkInProgressTaskPRs(context.Background(), db)

	tags, err := flowdb.GetTaskTags(db, "wt-task")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	if !hasGitHubPRTag(tags) {
		t.Fatalf("expected a gh-pr: tag after linking, got %v", tags)
	}
}

func TestLinkInProgressIssuePRsTagsLinkedPRs(t *testing.T) {
	db := dispatcherTestDB(t)
	now := flowdb.NowISO()
	// Task created by self-assigning an issue: tracks the issue, no worktree
	// branch that matches the PR head (the PRs are opened from sub-branches).
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES ('issue-task', 'issue task', 'in-progress', 'high', ?, 'default', 'claude', 'sess-issue', ?, ?, ?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "issue-task", "gh-issue:facets-cloud/raptor#139"); err != nil {
		t.Fatalf("seed issue tag: %v", err)
	}

	orig := ghOpenPRsForIssue
	defer func() { ghOpenPRsForIssue = orig }()
	var gotOwner, gotRepo string
	var gotNumber int
	ghOpenPRsForIssue = func(_ context.Context, owner, repo string, number int, _ []string) ([]int, error) {
		gotOwner, gotRepo, gotNumber = owner, repo, number
		return []int{156, 158}, nil // one issue, several PRs
	}

	linkInProgressIssuePRs(context.Background(), db, []string{"vishnukv-facets"})

	if gotOwner != "facets-cloud" || gotRepo != "raptor" || gotNumber != 139 {
		t.Fatalf("queried wrong issue: %s/%s#%d", gotOwner, gotRepo, gotNumber)
	}
	tags, err := flowdb.GetTaskTags(db, "issue-task")
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	want := map[string]bool{
		"gh-issue:facets-cloud/raptor#139": false, // still tracked
		"gh-pr:facets-cloud/raptor#156":    false,
		"gh-pr:facets-cloud/raptor#158":    false,
	}
	for _, tag := range tags {
		if _, ok := want[tag]; ok {
			want[tag] = true
		}
	}
	for tag, found := range want {
		if !found {
			t.Fatalf("expected tag %q after linking, got %v", tag, tags)
		}
	}
}

func TestLinkInProgressIssuePRsSkipsTasksWithoutIssueTag(t *testing.T) {
	db := dispatcherTestDB(t)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES ('plain', 'plain', 'in-progress', 'high', ?, 'default', 'claude', 'sess-plain', ?, ?, ?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	orig := ghOpenPRsForIssue
	defer func() { ghOpenPRsForIssue = orig }()
	calls := 0
	ghOpenPRsForIssue = func(_ context.Context, _, _ string, _ int, _ []string) ([]int, error) {
		calls++
		return nil, nil
	}

	linkInProgressIssuePRs(context.Background(), db, []string{"vishnukv-facets"})

	if calls != 0 {
		t.Fatalf("ghOpenPRsForIssue called %d times; want 0 (no gh-issue tag)", calls)
	}
}

func TestLinkInProgressTaskPRsSkipsNoWorktreeAndAlreadyTagged(t *testing.T) {
	db := dispatcherTestDB(t)
	now := flowdb.NowISO()
	// (a) in-progress task WITHOUT a worktree → never calls gh, never tags.
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES ('no-wt', 'no worktree', 'in-progress', 'high', ?, 'default', 'claude', 'sess-nowt', ?, ?, ?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed no-wt: %v", err)
	}
	// (b) already-tagged worktree task → must not re-call gh.
	wt := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, worktree_path, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES ('tagged', 'tagged', 'in-progress', 'high', ?, ?, 'default', 'claude', 'sess-tagged', ?, ?, ?)`,
		t.TempDir(), wt, now, now, now,
	); err != nil {
		t.Fatalf("seed tagged: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "tagged", "gh-pr:vishnukv-facets/flow-manager#9"); err != nil {
		t.Fatalf("pretag: %v", err)
	}

	orig := ghPRURLForWorktree
	defer func() { ghPRURLForWorktree = orig }()
	calls := 0
	ghPRURLForWorktree = func(_ context.Context, _ string) (string, error) {
		calls++
		return "", errors.New("should not be called")
	}

	linkInProgressTaskPRs(context.Background(), db)

	if calls != 0 {
		t.Fatalf("gh pr view called %d times; want 0 (no-worktree skipped, already-tagged skipped)", calls)
	}
}
