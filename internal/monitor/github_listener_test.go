package monitor

import (
	"context"
	"testing"
	"time"

	"flow/internal/flowdb"
)

// Without a connected App, Start() must be a no-op: the webhook receiver
// handles live events and the PR-linker tick (which needs an installation
// token) is not scheduled.
func TestGitHubListener_StartNoOpWithoutApp(t *testing.T) {
	t.Setenv("FLOW_GH_APP_ID", "")
	t.Setenv("FLOW_GH_APP_PEM", "")
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "")

	l := NewGitHubListener(NewGitHubDispatcher(nil, nil))
	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if running {
		t.Fatal("listener should not run the linker tick without a connected App")
	}
	l.Stop()
}

// Even with valid App credentials, ingress=off disables the tick entirely.
func TestGitHubListener_StartNoOpWhenIngressOff(t *testing.T) {
	t.Setenv("FLOW_GH_TRANSPORT", "off")
	t.Setenv("FLOW_GH_APP_ID", "1")
	t.Setenv("FLOW_GH_APP_PEM", testRSAKeyPEM(t))
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "2")

	l := NewGitHubListener(NewGitHubDispatcher(nil, nil))
	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if running {
		t.Fatal("listener should not run the linker tick when ingress is off")
	}
	l.Stop()
}

// With an App connected and ingress on, the linker tick runs and tags an
// in-progress issue task with the cross-referenced open PR resolved via
// ghOpenPRsForIssue (stubbed here to avoid a real GitHub call).
func TestGitHubListener_LinkTickTagsIssueTaskWithPR(t *testing.T) {
	t.Setenv("FLOW_GH_TRANSPORT", "webhook")
	t.Setenv("FLOW_GH_APP_ID", "1")
	t.Setenv("FLOW_GH_APP_PEM", testRSAKeyPEM(t))
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "2")
	t.Setenv("FLOW_GH_SELF_LOGINS", "me")

	db := dispatcherTestDB(t)
	now := flowdb.NowISO()
	// In-progress task that tracks an issue (no worktree branch matching the
	// PR head — the PR is found via the issue's cross-references instead).
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, session_id, status_changed_at, created_at, updated_at)
		 VALUES ('issue-task', 'issue task', 'in-progress', 'high', ?, 'default', 'claude', 'sess-issue', ?, ?, ?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "issue-task", "gh-issue:o/r#7"); err != nil {
		t.Fatalf("seed issue tag: %v", err)
	}

	origIssue := ghOpenPRsForIssue
	origWorktree := ghPRURLForWorktree
	defer func() {
		ghOpenPRsForIssue = origIssue
		ghPRURLForWorktree = origWorktree
	}()
	ghOpenPRsForIssue = func(_ context.Context, _, _ string, _ int, _ []string) ([]int, error) {
		return []int{5}, nil
	}
	ghPRURLForWorktree = func(_ context.Context, _ string) (string, error) {
		return "", nil // no branch PR
	}
	// The linker tags via `flow update task --tag` exec (Bucket-O write, seam
	// §11). With no `flow` on PATH (e.g. CI) that exec fails and the tag never
	// lands; stub it to write directly to the test DB, mirroring
	// github_pr_link_test.go.
	origTag := tagFlowTask
	defer func() { tagFlowTask = origTag }()
	tagFlowTask = func(_ context.Context, slug, tag string) error {
		_, err := db.Exec(`INSERT OR IGNORE INTO task_tags (task_slug, tag, created_at) VALUES (?,?,?)`, slug, flowdb.NormalizeTag(tag), flowdb.NowISO())
		return err
	}

	l := NewGitHubListener(NewGitHubDispatcher(db, nil))
	l.linkInterval = 10 * time.Millisecond
	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	defer l.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tags, _ := flowdb.GetTaskTags(db, "issue-task")
		for _, tag := range tags {
			if tag == "gh-pr:o/r#5" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	tags, _ := flowdb.GetTaskTags(db, "issue-task")
	t.Fatalf("linker tick did not tag the issue task with gh-pr:o/r#5; tags=%v", tags)
}
