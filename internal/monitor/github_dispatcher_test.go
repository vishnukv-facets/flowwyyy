package monitor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func seedGitHubTask(t *testing.T, slug string, db *sql.DB, tag string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'high', ?, 'default', 'claude', ?, ?, ?)`,
		slug, "seeded github task", t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed github task %s: %v", slug, err)
	}
	if err := flowdb.AddTaskTag(db, slug, tag); err != nil {
		t.Fatalf("tag %s: %v", tag, err)
	}
}

// TestFindTaskByGitHubTag_PrefersLiveWorkingTaskOverStub guards the inbox
// "Open session" / routing bug: when both the real working task and an
// auto-created gh-pr-* stub carry the same PR tag, routing must land on the
// live, session-backed task — not the stub, which (sorting first by slug) used
// to win the naive first-match.
func TestFindTaskByGitHubTag_PrefersLiveWorkingTaskOverStub(t *testing.T) {
	db := dispatcherTestDB(t)
	const tag = "gh-pr:vishnukv-facets/flow-manager#12"

	// Stub the dispatcher auto-created earlier; its slug sorts BEFORE the real
	// task ("g" < "m"), so it would win a first-by-slug match.
	seedGitHubTask(t, "gh-pr-vishnukv-facets-flow-manager-12", db, tag)

	// Real working task: in-progress, with a captured session + worktree.
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, worktree_path, session_id, session_provider, permission_mode, status_changed_at, created_at, updated_at)
		 VALUES (?, 'real', 'in-progress', 'high', ?, ?, 'sess-123', 'claude', 'default', ?, ?, ?)`,
		"mc-task-tree-startability", t.TempDir(), t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed working task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "mc-task-tree-startability", tag); err != nil {
		t.Fatalf("tag working task: %v", err)
	}

	d := NewGitHubDispatcher(db, nil)
	slug, found, err := d.findTaskByGitHubTag(tag)
	if err != nil || !found {
		t.Fatalf("findTaskByGitHubTag found=%v err=%v", found, err)
	}
	if slug != "mc-task-tree-startability" {
		t.Fatalf("routed to %q, want the live working task (not the gh-pr-* stub)", slug)
	}
}

func TestGitHubDispatcher_PRReviewRequestCreatesTask(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, tags, opens, restore := stubDispatcherIO(t)
	defer restore()

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Title:    "Add GitHub integration",
		Body:     "Please review the new polling path.",
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
		Author:   "octo",
		BaseRef:  "main",
		HeadRef:  "feature/github",
		HeadSHA:  "abc123",
		Labels:   []string{"flow:codex"},
		EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
		RawJSON:  `{"number":42}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(*spawns))
	}
	spawn := (*spawns)[0]
	if spawn.Slug != "gh-pr-facets-cloud-flow-manager-42" {
		t.Fatalf("slug = %q", spawn.Slug)
	}
	if spawn.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", spawn.Provider)
	}
	for _, want := range []string{
		"Pull request: Facets-cloud/flow-manager#42",
		"base: main",
		"head: feature/github",
		"https://github.com/Facets-cloud/flow-manager/pull/42",
	} {
		if !strings.Contains(spawn.Brief, want) {
			t.Fatalf("brief missing %q\n%s", want, spawn.Brief)
		}
	}
	gotTags := map[string]bool{}
	for _, c := range *tags {
		gotTags[c.Tag] = true
	}
	for _, want := range []string{"github", "gh-pr:Facets-cloud/flow-manager#42"} {
		if !gotTags[want] {
			t.Fatalf("missing tag %q from %v", want, gotTags)
		}
	}
	if len(*opens) != 0 {
		t.Fatalf("autoopen off should suppress opens: %v", *opens)
	}
	seen, err := flowdb.HasGitHubEvent(db, gitHubPRHeadEventKey("Facets-cloud", "flow-manager", 42, "abc123"))
	if err != nil {
		t.Fatalf("HasGitHubEvent: %v", err)
	}
	if !seen {
		t.Fatal("initial PR task creation should mark the current head SHA as already seen")
	}
}

func TestGitHubDispatcher_SecondPREventAppendsWithoutDuplicateTask(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	second := GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Title:    "Add GitHub integration",
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
		EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
	}

	if err := d.Dispatch(context.Background(), second); err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	if len(*spawns) != 0 {
		t.Fatalf("spawn count = %d, want 0", len(*spawns))
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
}

func TestGitHubDispatcher_ReviewCommentAppendsToTrackedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")
	if _, err := db.Exec(`UPDATE tasks SET status='done', updated_at=? WHERE slug='tracked-pr'`, flowdb.NowISO()); err != nil {
		t.Fatalf("seed done status: %v", err)
	}

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventPRReviewComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    42,
		CommentID: "PRRC_kwDOAAABBB",
		Author:    "reviewer",
		Body:      "Please tighten the idempotency test.",
		URL:       "https://github.com/Facets-cloud/flow-manager/pull/42#discussion_r1",
		EventKey:  "review-comment:PRRC_kwDOAAABBB",
		RawJSON:   `{"node_id":"PRRC_kwDOAAABBB"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	if entries[0].Event.Kind != string(GitHubEventPRReviewComment) || entries[0].Event.Text != ev.Body {
		t.Fatalf("entry event = %+v", entries[0].Event)
	}
}

func TestGitHubDispatcher_SteererOwnedRoutingSkipsLegacyTaskPipeline(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")
	observer := &fakeMessageObserver{}

	d := NewGitHubDispatcher(db, nil)
	d.Steerer = observer
	d.SteererOwnsRouting = func() bool { return true }
	ev := GitHubEvent{
		Kind:      GitHubEventPRReviewComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    42,
		CommentID: "PRRC_owned",
		Author:    "reviewer",
		Body:      "Please tighten the idempotency test.",
		URL:       "https://github.com/Facets-cloud/flow-manager/pull/42#discussion_r1",
		EventKey:  "review-comment:PRRC_owned",
		RawJSON:   `{"node_id":"PRRC_owned"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(observer.events) != 1 || observer.events[0].Text != ev.Body {
		t.Fatalf("steerer events = %+v, want exactly the GitHub comment", observer.events)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("GitHub event should not be appended directly when steerer owns routing; entries=%+v", entries)
	}
	seen, err := flowdb.HasGitHubEvent(db, ev.EventKeyValue())
	if err != nil {
		t.Fatalf("HasGitHubEvent: %v", err)
	}
	if !seen {
		t.Fatal("steerer-owned GitHub event should still be recorded for poller dedupe")
	}
}

// Regression: a new comment on an ARCHIVED but still-open PR must route to the
// existing task (append to its inbox), NOT spawn a duplicate. Archiving only
// declutters the active list; routing still tracks the thread.
func TestGitHubDispatcher_ReviewCommentRoutesToArchivedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "archived-pr", db, "gh-pr:Facets-cloud/flow-manager#77")
	if _, err := db.Exec(`UPDATE tasks SET archived_at=?, updated_at=? WHERE slug='archived-pr'`, flowdb.NowISO(), flowdb.NowISO()); err != nil {
		t.Fatalf("archive task: %v", err)
	}

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRReviewComment, Owner: "Facets-cloud", Repo: "flow-manager", Number: 77,
		CommentID: "PRRC_arch", Author: "reviewer", Body: "feedback addressed; re-review please",
		URL: "https://github.com/Facets-cloud/flow-manager/pull/77#discussion_r9", EventKey: "review-comment:PRRC_arch",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 0 {
		t.Errorf("must NOT spawn a duplicate task for an archived PR, spawned %d", len(*spawns))
	}
	entries, err := ReadInboxEntries("archived-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("archived PR inbox entries = %d, want 1 (routed to existing task)", len(entries))
	}
}

func TestGitHubDispatcher_ChangesRequestedReviewReopensTrackedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")
	if _, err := db.Exec(`UPDATE tasks SET status='done', updated_at=? WHERE slug='tracked-pr'`, flowdb.NowISO()); err != nil {
		t.Fatalf("seed done status: %v", err)
	}

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewChangesRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Author:   "reviewer",
		Body:     "Please add a regression test.",
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42#pullrequestreview-44",
		EventKey: "review:PRR_kwDOAAABBB",
		RawJSON:  `{"node_id":"PRR_kwDOAAABBB"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	if entries[0].Event.Kind != string(GitHubEventPRReviewChangesRequested) || !entries[0].Meta.Actionable {
		t.Fatalf("entry = %+v", entries[0])
	}
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "backlog" {
		t.Fatalf("status = %q, want backlog after changes requested", task.Status)
	}
}

func TestGitHubDispatcher_ApprovedReviewAppendsWithoutReopeningTrackedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")
	if _, err := db.Exec(`UPDATE tasks SET status='done', updated_at=? WHERE slug='tracked-pr'`, flowdb.NowISO()); err != nil {
		t.Fatalf("seed done status: %v", err)
	}

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewApproved,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Author:   "reviewer",
		Body:     "Looks good.",
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42#pullrequestreview-45",
		EventKey: "review:PRR_kwDOAAACCC",
		RawJSON:  `{"node_id":"PRR_kwDOAAACCC"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	// Approval is actionable now (the agent should wake to proceed, e.g. merge)
	// but it must still NOT reopen a task the user already marked done.
	if entries[0].Event.Kind != string(GitHubEventPRReviewApproved) || !entries[0].Meta.Actionable {
		t.Fatalf("entry = %+v (approved review should be actionable so the session wakes)", entries[0])
	}
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "done" {
		t.Fatalf("status = %q, want done after approved review", task.Status)
	}
}

func TestGitHubDispatcher_IssueAssignmentCreatesTask(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, tags, _, restore := stubDispatcherIO(t)
	defer restore()

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventIssueAssigned,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    7,
		Title:     "Document GitHub polling",
		Body:      "Add docs for the new env vars.",
		URL:       "https://github.com/Facets-cloud/flow-manager/issues/7",
		Author:    "octo",
		Labels:    []string{"docs"},
		Milestone: "v1",
		EventKey:  "issue:Facets-cloud/flow-manager#7:assigned",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(*spawns))
	}
	if (*spawns)[0].Slug != "gh-issue-facets-cloud-flow-manager-7" {
		t.Fatalf("slug = %q", (*spawns)[0].Slug)
	}
	gotTags := map[string]bool{}
	for _, c := range *tags {
		gotTags[c.Tag] = true
	}
	if !gotTags["gh-issue:Facets-cloud/flow-manager#7"] {
		t.Fatalf("missing issue tag from %v", gotTags)
	}
}

func TestGitHubDispatcher_MentionedOpensSessionInvolvedNotifyOnly(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "1")
	db := dispatcherTestDB(t)
	spawns, _, opens, restore := stubDispatcherIO(t)
	defer restore()
	d := NewGitHubDispatcher(db, nil)

	mention := GitHubEvent{
		Kind: GitHubEventIssueMentioned, Owner: "Facets-cloud", Repo: "flow-manager", Number: 80,
		Title: "mention me", URL: "https://github.com/Facets-cloud/flow-manager/issues/80", Author: "octo",
		EventKey: "issue:Facets-cloud/flow-manager#80:mentioned",
	}
	involved := GitHubEvent{
		Kind: GitHubEventIssueInvolved, Owner: "Facets-cloud", Repo: "flow-manager", Number: 81,
		Title: "fyi", URL: "https://github.com/Facets-cloud/flow-manager/issues/81", Author: "octo",
		EventKey: "issue:Facets-cloud/flow-manager#81:involved",
	}
	if err := d.Dispatch(context.Background(), mention); err != nil {
		t.Fatalf("Dispatch mention: %v", err)
	}
	if err := d.Dispatch(context.Background(), involved); err != nil {
		t.Fatalf("Dispatch involved: %v", err)
	}

	if len(*spawns) != 2 {
		t.Fatalf("both discovery kinds should create a task; spawns=%d", len(*spawns))
	}
	if len(*opens) != 1 || (*opens)[0] != "gh-issue-facets-cloud-flow-manager-80" {
		t.Fatalf("opens = %v; want only the mentioned item (#80) to auto-open a session", *opens)
	}
}

func TestGitHubDispatcher_DuplicateEventKeySuppressesAppend(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventPRReviewComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    42,
		CommentID: "PRRC_kwDOAAABBB",
		Body:      "same comment",
		EventKey:  "review-comment:PRRC_kwDOAAABBB",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch duplicate: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate event should append once, got %d", len(entries))
	}
}

func TestGitHubDispatcher_PRTopLevelCommentAppendsToTrackedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventPRComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    42,
		CommentID: "IC_kwDOAAABBB",
		Author:    "reviewer",
		Body:      "Could you also address the bullets in the description?",
		URL:       "https://github.com/Facets-cloud/flow-manager/pull/42#issuecomment-1",
		EventKey:  "issue-comment:IC_kwDOAAABBB",
		RawJSON:   `{"node_id":"IC_kwDOAAABBB"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	if entries[0].Event.Kind != string(GitHubEventPRComment) {
		t.Fatalf("kind = %q", entries[0].Event.Kind)
	}
	// Top-level comments are the most common reviewer reply channel — they
	// must be actionable so the same-session monitor wakes the bound task.
	if !entries[0].Meta.Actionable {
		t.Fatalf("meta = %+v, want actionable=true", entries[0].Meta)
	}
	if entries[0].Event.Text != ev.Body || entries[0].Event.URL != ev.URL {
		t.Fatalf("event payload lost: %+v", entries[0].Event)
	}
}

func TestGitHubDispatcher_IssueCommentAppendsToTrackedIssue(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-issue", db, "gh-issue:Facets-cloud/flow-manager#7")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventIssueComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    7,
		CommentID: "IC_kwDOAAACCC",
		Author:    "reporter",
		Body:      "Any update on this?",
		URL:       "https://github.com/Facets-cloud/flow-manager/issues/7#issuecomment-2",
		EventKey:  "issue-comment:IC_kwDOAAACCC",
		RawJSON:   `{"node_id":"IC_kwDOAAACCC"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-issue")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	if entries[0].Event.Kind != string(GitHubEventIssueComment) {
		t.Fatalf("kind = %q", entries[0].Event.Kind)
	}
	if !entries[0].Meta.Actionable {
		t.Fatalf("meta = %+v, want actionable=true", entries[0].Meta)
	}
}

func TestGitHubDispatcher_DuplicateIssueCommentEventKeySuppressesAppend(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:      GitHubEventPRComment,
		Owner:     "Facets-cloud",
		Repo:      "flow-manager",
		Number:    42,
		CommentID: "IC_kwDOAAABBB",
		Body:      "same top-level comment",
		EventKey:  "issue-comment:IC_kwDOAAABBB",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch duplicate: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate event should append once, got %d", len(entries))
	}
}

func TestGitHubDispatcher_HeadUpdatedAppendsToTrackedPR(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRHeadUpdated,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		HeadRef:  "feature/github",
		HeadSHA:  "abc123",
		Body:     "Pull request head changed; review again.",
		EventKey: "pr-head:Facets-cloud/flow-manager#42:abc123",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 || entries[0].Event.Kind != string(GitHubEventPRHeadUpdated) {
		t.Fatalf("entries = %#v", entries)
	}
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "backlog" {
		t.Fatalf("status = %q, want backlog after new PR head", task.Status)
	}
}

func TestGitHubDispatcher_ClosedPRAppendsAndStaysActionableWithoutMarkingDone(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRClosed,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Body:     "Pull request Facets-cloud/flow-manager#42 was closed without merging.",
		EventKey: "pr-closed:Facets-cloud/flow-manager#42",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// Close-without-merge is ambiguous; the agent decides. Status must be left
	// untouched (unlike merge, which auto-marks done).
	if task.Status != "backlog" {
		t.Fatalf("status = %q, want backlog (close must not auto-mark done)", task.Status)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 || entries[0].Event.Kind != string(GitHubEventPRClosed) {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[0].Meta.Actionable {
		t.Fatal("pr_closed inbox entry must be actionable so the live session wakes")
	}
}

func TestGitHubDispatcher_MergedPRDeliversToSessionWithoutAutoClose(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind:     GitHubEventPRMerged,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		HeadSHA:  "abc123",
		Body:     "Pull request merged.",
		EventKey: "pr-merged:Facets-cloud/flow-manager#42",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// A merge delivers an actionable event to the session and lets the AGENT
	// decide how to close (final steps, post an update, mark done). flow no
	// longer server-side auto-closes — mirrors pr_closed.
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "backlog" {
		t.Fatalf("status = %q, want backlog (merge must not auto-close; the agent decides)", task.Status)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 || entries[0].Event.Kind != string(GitHubEventPRMerged) {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[0].Meta.Actionable {
		t.Fatal("pr_merged inbox entry must be actionable so the live session wakes")
	}
}

// TestGitHubDispatcher_AutonomyDeliversMergeToLinkedSession is the regression
// for the reported bug: with steering autonomy owning routing, a PR merge must
// still reach the linked task's inbox (actionable wake) so the session learns of
// it and the agent decides how to close. Before the fix the dispatcher handed
// the event only to the steerer's relevance cascade — which drops bare lifecycle
// notifications — so the session never woke and the task was never closed.
func TestGitHubDispatcher_AutonomyDeliversMergeToLinkedSession(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")

	obs := &recordingObserver{}
	d := NewGitHubDispatcher(db, nil)
	d.Steerer = obs
	d.SteererOwnsRouting = func() bool { return true } // steering autonomy on

	ev := GitHubEvent{
		Kind:     GitHubEventPRMerged,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Body:     "Pull request merged.",
		EventKey: "pr-merged:Facets-cloud/flow-manager#42",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 || entries[0].Event.Kind != string(GitHubEventPRMerged) {
		t.Fatalf("merge not delivered to linked task inbox in autonomy mode: %#v", entries)
	}
	if !entries[0].Meta.Actionable {
		t.Fatal("delivered merge must be actionable so the session wakes")
	}
	// The steerer still observes the event (feed/trace) — delivery is additive.
	if len(obs.events) != 1 {
		t.Fatalf("steerer should still observe the event, got %d", len(obs.events))
	}
	// Agent decides the close: no server-side mark-done.
	task, err := flowdb.GetTask(db, "tracked-pr")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status == "done" {
		t.Fatal("autonomy merge must not auto-close; the agent decides")
	}
}

// --- legacy (non-autonomy) involvement gate ---------------------------------
// The webhook is a firehose (org-wide install delivers every repo's PRs). The
// legacy dispatcher must not spawn a task for a PR/issue the operator isn't
// involved in — only when they're a participant (author/assignee/reviewer),
// @-mentioned, or the PR/issue is already tracked.

func TestGitHubDispatcher_DropsUninvolvedNewPR(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat-self")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	d := NewGitHubDispatcher(db, nil) // non-autonomy: no steerer ownership
	ev := GitHubEvent{
		Kind: GitHubEventPRReviewRequested, Owner: "Facets-cloud", Repo: "agent-factory", Number: 1285,
		Title: "Azure migration", Author: "srikxcipher",
		Participants: []string{"srikxcipher", "anujhydrabadi"}, // operator absent
		EventKey:     "gh-pr:Facets-cloud/agent-factory#1285:pr_review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 0 {
		t.Fatalf("uninvolved PR must not spawn a task, spawned %d", len(*spawns))
	}
}

func TestGitHubDispatcher_CreatesTaskWhenOperatorIsRequestedReviewer(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat-self")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRReviewRequested, Owner: "o", Repo: "r", Number: 5,
		Author: "alice", Participants: []string{"alice", "octocat-self"},
		EventKey: "gh-pr:o/r#5:pr_review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("operator as requested reviewer must spawn a task, spawned %d", len(*spawns))
	}
}

func TestGitHubDispatcher_CreatesTaskWhenOperatorMentionedInComment(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat-self")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRComment, Owner: "o", Repo: "r", Number: 5,
		Author: "alice", Body: "hey @octocat-self can you take a look?",
		Participants: []string{"alice"},
		EventKey:     "issue-comment:c-mention",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("operator @-mentioned must spawn a task, spawned %d", len(*spawns))
	}
}

func TestGitHubDispatcher_DropsUninvolvedComment(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat-self")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRComment, Owner: "o", Repo: "r", Number: 5,
		Author: "alice", Body: "looks good to me",
		Participants: []string{"alice", "bob"},
		EventKey:     "issue-comment:c-uninvolved",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 0 {
		t.Fatalf("uninvolved comment must not spawn a task, spawned %d", len(*spawns))
	}
}

func TestGitHubDispatcher_ProcessesTrackedPRRegardlessOfInvolvement(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat-self")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:o/r#5")
	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRComment, Owner: "o", Repo: "r", Number: 5,
		Author: "alice", Body: "looks good", Participants: []string{"alice"},
		EventKey: "issue-comment:c-tracked",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-pr")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("a comment on a TRACKED PR must reach its inbox regardless of involvement, got %d", len(entries))
	}
}

func TestGitHubDispatcher_FailsOpenWhenSelfLoginsUnset(t *testing.T) {
	t.Setenv("FLOW_GH_SELF_LOGINS", "") // identity unknown → can't gate → fail open
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()
	d := NewGitHubDispatcher(db, nil)
	ev := GitHubEvent{
		Kind: GitHubEventPRReviewRequested, Owner: "o", Repo: "r", Number: 5,
		Author: "alice", Participants: []string{"alice", "bob"},
		EventKey: "gh-pr:o/r#5:pr_review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("fail-open (no self logins) must preserve task creation, spawned %d", len(*spawns))
	}
}

// recordingObserver captures every InboundEvent handed to it (the steering
// cascade implements MessageObserver in production).
type recordingObserver struct {
	events []InboundEvent
	err    error
}

func (o *recordingObserver) Observe(_ context.Context, ev InboundEvent) error {
	o.events = append(o.events, ev)
	return o.err
}

// TestGitHubDispatcher_RoutesNewEventToSteerer asserts a NEW github event is
// also handed to the steerer (trace-only parallel) AND that the existing task
// pipeline still runs. The steerer sees the github-shaped InboundEvent.
func TestGitHubDispatcher_RoutesNewEventToSteerer(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	obs := &recordingObserver{}
	d := NewGitHubDispatcher(db, nil)
	d.Steerer = obs
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		Title:    "Add GitHub integration",
		Body:     "Please review.",
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
		Author:   "octo",
		EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// Existing pipeline still ran (a task was spawned).
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1 (pipeline must still run)", len(*spawns))
	}
	// Steerer observed exactly one github-shaped event.
	if len(obs.events) != 1 {
		t.Fatalf("steerer observed %d events, want 1", len(obs.events))
	}
	got := obs.events[0]
	if got.ChannelType != "github" || got.Channel != "Facets-cloud/flow-manager" {
		t.Errorf("observed event = %+v, want github o/r shape", got)
	}
	if got.URL != "https://github.com/Facets-cloud/flow-manager/pull/42" {
		t.Errorf("observed URL = %q", got.URL)
	}
}

// TestGitHubDispatcher_SteererErrorDoesNotBreakPipeline confirms the steerer
// call is best-effort: a steerer error is logged, never returned, and the task
// pipeline still completes.
func TestGitHubDispatcher_SteererErrorDoesNotBreakPipeline(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, _, _, restore := stubDispatcherIO(t)
	defer restore()

	obs := &recordingObserver{err: errors.New("steerer down")}
	d := NewGitHubDispatcher(db, nil)
	d.Steerer = obs
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
		Author:   "octo",
		EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch must not return the steerer error: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1 (pipeline must run despite steerer error)", len(*spawns))
	}
}

// TestGitHubDispatcher_DedupSuppressesSteerer confirms an event suppressed by
// the HasGitHubEvent dedup is NOT re-observed by the steerer (observe-once).
func TestGitHubDispatcher_DedupSuppressesSteerer(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()

	obs := &recordingObserver{}
	d := NewGitHubDispatcher(db, nil)
	d.Steerer = obs
	ev := GitHubEvent{
		Kind:     GitHubEventPRReviewRequested,
		Owner:    "Facets-cloud",
		Repo:     "flow-manager",
		Number:   42,
		URL:      "https://github.com/Facets-cloud/flow-manager/pull/42",
		Author:   "octo",
		EventKey: "pr:Facets-cloud/flow-manager#42:review_requested",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	if err := d.Dispatch(context.Background(), ev); err != nil { // same event key → deduped
		t.Fatalf("second Dispatch: %v", err)
	}
	if len(obs.events) != 1 {
		t.Errorf("steerer observed %d events, want 1 (dedup must suppress the repeat)", len(obs.events))
	}
}
