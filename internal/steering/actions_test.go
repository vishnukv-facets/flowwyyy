package steering

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

// stubActionIO swaps the shell-out vars and records calls.
type spawnRec struct{ name, slug, brief, project string }
type tellRec struct{ slug, msg string }
type tagRec struct{ slug, tag string }

// stubbedTags records taskTagger calls for the most recent stubActionIO setup,
// so tests can assert source-thread tagging without changing the helper's return
// signature (used by 11 call sites). Reset on each stubActionIO call.
var stubbedTags []tagRec

func stubActionIO(t *testing.T) (*[]spawnRec, *[]tellRec) {
	t.Helper()
	var spawns []spawnRec
	var tells []tellRec
	stubbedTags = nil
	oldSpawn, oldTell, oldTag := taskSpawner, taskTeller, taskTagger
	taskSpawner = func(_ context.Context, name, slug, brief, project string) error {
		spawns = append(spawns, spawnRec{name, slug, brief, project})
		return nil
	}
	taskTeller = func(_ context.Context, slug, msg string) error {
		tells = append(tells, tellRec{slug, msg})
		return nil
	}
	taskTagger = func(_ context.Context, slug, tag string) error {
		stubbedTags = append(stubbedTags, tagRec{slug, tag})
		return nil
	}
	t.Cleanup(func() { taskSpawner, taskTeller, taskTagger = oldSpawn, oldTell, oldTag })
	return &spawns, &tells
}

func TestMakeTaskFromFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "f1", Source: "slack", ThreadKey: "C1:100.1", Summary: "Customer wants rollout date",
		SuggestedAction: "make_task", SuggestedProject: "goniyo", Reason: "names operator",
		Draft: "Targeting Friday.", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("taskSpawner calls = %d, want 1", len(*spawns))
	}
	got := (*spawns)[0]
	if got.project != "goniyo" {
		t.Errorf("project = %q, want goniyo", got.project)
	}
	if !strings.Contains(got.brief, "Customer wants rollout date") || !strings.Contains(got.brief, "C1:100.1") {
		t.Errorf("brief should embed summary + thread key:\n%s", got.brief)
	}
	if !strings.HasPrefix(got.slug, "att-") {
		t.Errorf("slug = %q, want att- prefix", got.slug)
	}
	// feed row marked acted
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("feed item should be 'acted', got %d acted rows", len(items))
	}
}

func TestFeedTrackingTag(t *testing.T) {
	cases := []struct {
		name string
		item flowdb.FeedItem
		want string
	}{
		{"slack thread", flowdb.FeedItem{Source: "slack", ThreadKey: "C_eng:1700000000.000100"}, "slack-thread:C_eng:1700000000.000100"},
		{"github pr composite", flowdb.FeedItem{Source: "github", ThreadKey: "owner/repo:gh-pr:owner/repo#550"}, "gh-pr:owner/repo#550"},
		{"github issue composite", flowdb.FeedItem{Source: "github", ThreadKey: "owner/repo:gh-issue:owner/repo#7"}, "gh-issue:owner/repo#7"},
		{"github no link tag → empty", flowdb.FeedItem{Source: "github", ThreadKey: "owner/repo:weird"}, ""},
		{"empty thread key → empty", flowdb.FeedItem{Source: "slack", ThreadKey: ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedTrackingTag(tc.item); got != tc.want {
				t.Errorf("feedTrackingTag = %q, want %q", got, tc.want)
			}
		})
	}
}

// MakeTaskFromFeed must tag the spawned task with its source thread so a later
// reply on that thread routes home (the Samarthya loop-closing fix).
func TestMakeTaskFromFeedTagsSourceThread(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "tt1", Source: "slack", ThreadKey: "C_eng:1700000000.000100", Summary: "grant access",
		SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	if len(stubbedTags) != 1 {
		t.Fatalf("taskTagger calls = %d, want 1", len(stubbedTags))
	}
	if stubbedTags[0].tag != "slack-thread:C_eng:1700000000.000100" {
		t.Errorf("tag = %q, want slack-thread:C_eng:1700000000.000100", stubbedTags[0].tag)
	}
	if stubbedTags[0].slug != FeedTaskSlug(item) {
		t.Errorf("tagged slug = %q, want %q", stubbedTags[0].slug, FeedTaskSlug(item))
	}
}

// Regression: retrying Send reply when the task already exists must NOT spawn a
// duplicate (which fails on UNIQUE tasks.slug) — it injects into the existing task.
func TestInjectReplyToTaskRecordsUpdate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root) // so monitor.TaskDir resolves the updates/ path
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "r1", Source: "slack", ThreadKey: "C_eng:1700000000.000200", Channel: "C_eng",
		Summary: "rollout date?", SuggestedAction: "send_reply", MatchedTask: "rollout-task",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	if err := InjectReplyToTask(context.Background(), db, item, "Targeting Friday.", "rollout-task", "keep it warm"); err != nil {
		t.Fatalf("InjectReplyToTask: %v", err)
	}

	// The reply must be injected into the task's inbox AND recorded as a durable
	// update so the task's agent knows what went out on the thread.
	if len(*tells) != 1 || (*tells)[0].slug != "rollout-task" {
		t.Errorf("must inject the reply via tell into rollout-task, got %+v", *tells)
	}
	updatesDir := filepath.Join(root, "tasks", "rollout-task", "updates")
	entries, err := os.ReadDir(updatesDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one update file in %s, got %v (err %v)", updatesDir, entries, err)
	}
	body, err := os.ReadFile(filepath.Join(updatesDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read update: %v", err)
	}
	for _, want := range []string{"Targeting Friday.", "C_eng:1700000000.000200", "keep it warm"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("update note missing %q:\n%s", want, body)
		}
	}
	if fi, _ := flowdb.GetFeedItem(db, "r1"); fi.Status != "acted" || fi.LinkedTask != "rollout-task" {
		t.Errorf("feed row = status %q linked %q, want acted/rollout-task", fi.Status, fi.LinkedTask)
	}
}

func TestSlackSendSessionPrompt(t *testing.T) {
	item := flowdb.FeedItem{Source: "slack", ThreadKey: "C9:1.2", Channel: "C9"}
	doneCmd := "flow attention sent r9 --close-floating send-abc"
	p := SlackSendSessionPrompt(item, "  ship it  ", "", doneCmd)
	for _, want := range []string{"Slack MCP", "C9:1.2", "ship it", doneCmd, "ONLY when"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// With operator instructions, the prompt must tell the agent to apply them.
	pi := SlackSendSessionPrompt(item, "ship it", "make it shorter", doneCmd)
	if !strings.Contains(pi, "make it shorter") || !strings.Contains(pi, "APPLY") {
		t.Errorf("instructed prompt missing revision guidance:\n%s", pi)
	}
}

func TestMakeReplyTaskFromFeedIdempotent(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, tells := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "idem1", Source: "slack", ThreadKey: "C_eng:1700000000.000100", Summary: "reply please",
		SuggestedAction: "send_reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	// Pre-create the task that FeedTaskSlug would target (simulates a prior action).
	slug := FeedTaskSlug(item)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES (?, 'existing', 'backlog', 'high', ?, 'default', 'claude', ?, ?, ?)`,
		slug, t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed existing task: %v", err)
	}

	got, err := MakeReplyTaskFromFeed(context.Background(), db, item, "thanks!")
	if err != nil {
		t.Fatalf("MakeReplyTaskFromFeed: %v", err)
	}
	if got != slug {
		t.Errorf("returned slug = %q, want existing %q", got, slug)
	}
	if len(*spawns) != 0 {
		t.Errorf("must NOT spawn when task exists, spawned %d", len(*spawns))
	}
	if len(*tells) != 1 || (*tells)[0].slug != slug {
		t.Errorf("must inject the reply via tell into %q, got %+v", slug, *tells)
	}
	if fi, _ := flowdb.GetFeedItem(db, "idem1"); fi.Status != "acted" || fi.LinkedTask != slug {
		t.Errorf("feed row = status %q linked %q, want acted/%s", fi.Status, fi.LinkedTask, slug)
	}
}

func TestMakeTaskFromFeedLinksTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "lk1", Source: "slack", ThreadKey: "C1:700.1", Summary: "do a thing",
		SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	got, err := flowdb.GetFeedItem(db, "lk1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" {
		t.Errorf("Status = %q, want acted", got.Status)
	}
	if got.LinkedTask != FeedTaskSlug(item) {
		t.Errorf("LinkedTask = %q, want %q", got.LinkedTask, FeedTaskSlug(item))
	}
}

func TestForwardFeedLinksMatchedTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{ID: "fl1", Source: "slack", ThreadKey: "C1:800.1", MatchedTask: "kong-split", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}
	got, err := flowdb.GetFeedItem(db, "fl1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" || got.LinkedTask != "kong-split" {
		t.Errorf("forwarded item = status %q linked %q, want acted/kong-split", got.Status, got.LinkedTask)
	}
}

func TestForwardFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{ID: "f2", Source: "slack", ThreadKey: "C1:200.1", Summary: "rel q", MatchedTask: "kong-split", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}
	if len(*tells) != 1 || (*tells)[0].slug != "kong-split" {
		t.Fatalf("taskTeller = %+v, want one call to kong-split", *tells)
	}
	if !strings.Contains((*tells)[0].msg, "C1:200.1") {
		t.Errorf("forward message should reference the source thread: %q", (*tells)[0].msg)
	}
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("forwarded item should be 'acted'")
	}
}

func TestForwardRequiresMatchedTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	item := flowdb.FeedItem{ID: "f3", Source: "slack", ThreadKey: "C1:300.1", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err == nil {
		t.Error("forward without matched_task must error")
	}
}

func TestDismissFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	item := flowdb.FeedItem{ID: "f4", Source: "slack", ThreadKey: "C1:400.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := DismissFeed(db, "f4"); err != nil {
		t.Fatalf("DismissFeed: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("item should be dismissed")
	}
}

func TestApplyActionManualBypassesGate(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g1", Source: "slack", ThreadKey: "C1:1.1", Summary: "s", SuggestedAction: "make_task", Confidence: 0.1, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// manual=true, even with all-off DefaultAutonomy and low confidence → executes.
	if err := ApplyAction(context.Background(), db, item, ActionMakeTask, DefaultAutonomy(), true); err != nil {
		t.Fatalf("manual ApplyAction: %v", err)
	}
	if len(*spawns) != 1 {
		t.Errorf("manual action should execute regardless of gate, spawns=%d", len(*spawns))
	}
}

func TestApplyActionAutonomousDenied(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g2", Source: "slack", ThreadKey: "C1:2.1", SuggestedAction: "make_task", Confidence: 0.99, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// manual=false + DefaultAutonomy (all disabled) → denied, no execution.
	err = ApplyAction(context.Background(), db, item, ActionMakeTask, DefaultAutonomy(), false)
	if err != ErrAutonomyDenied {
		t.Fatalf("autonomous make_task under default policy should be ErrAutonomyDenied, got %v", err)
	}
	if len(*spawns) != 0 {
		t.Errorf("denied action must NOT execute, spawns=%d", len(*spawns))
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 1 {
		t.Errorf("denied action must leave the feed row untouched ('new')")
	}
}

func TestApplyActionAutonomousAllowed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{ID: "g3", Source: "slack", ThreadKey: "C1:3.1", Summary: "s", SuggestedAction: "make_task", Confidence: 0.95, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	policy := AutonomyPolicy{ActionMakeTask: {Enabled: true, Threshold: 0.80}}
	if err := ApplyAction(context.Background(), db, item, ActionMakeTask, policy, false); err != nil {
		t.Fatalf("autonomous allowed ApplyAction: %v", err)
	}
	if len(*spawns) != 1 {
		t.Errorf("allowed autonomous action should execute, spawns=%d", len(*spawns))
	}
}

func TestInjectReplyToTask(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "r1", Source: "slack", ThreadKey: "C1:900.1", Summary: "wants ETA",
		SuggestedAction: "send_reply", MatchedTask: "gh-task", Draft: "soon",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := InjectReplyToTask(context.Background(), db, item, "hello there", "gh-task", "make it warmer"); err != nil {
		t.Fatalf("InjectReplyToTask: %v", err)
	}
	if len(*tells) != 1 || (*tells)[0].slug != "gh-task" {
		t.Fatalf("taskTeller = %+v, want one call to gh-task", *tells)
	}
	if !strings.Contains((*tells)[0].msg, "hello there") {
		t.Errorf("inject message should embed the reply text: %q", (*tells)[0].msg)
	}
	if !strings.Contains((*tells)[0].msg, "make it warmer") {
		t.Errorf("inject message should embed operator instructions: %q", (*tells)[0].msg)
	}
	got, err := flowdb.GetFeedItem(db, "r1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" || got.LinkedTask != "gh-task" {
		t.Errorf("feed row = status %q linked %q, want acted/gh-task", got.Status, got.LinkedTask)
	}
}

func TestMakeReplyTaskFromFeed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "r2", Source: "slack", ThreadKey: "C1:950.1", Summary: "needs reply",
		SuggestedAction: "send_reply", SuggestedProject: "goniyo", Channel: "C1",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	slug, err := MakeReplyTaskFromFeed(context.Background(), db, item, "ship it Friday")
	if err != nil {
		t.Fatalf("MakeReplyTaskFromFeed: %v", err)
	}
	if slug != FeedTaskSlug(item) {
		t.Errorf("returned slug = %q, want %q", slug, FeedTaskSlug(item))
	}
	if len(*spawns) != 1 {
		t.Fatalf("taskSpawner calls = %d, want 1", len(*spawns))
	}
	got := (*spawns)[0]
	if got.slug != FeedTaskSlug(item) {
		t.Errorf("spawn slug = %q, want %q", got.slug, FeedTaskSlug(item))
	}
	if !strings.Contains(got.brief, "ship it Friday") {
		t.Errorf("brief should embed the reply text:\n%s", got.brief)
	}
	if !strings.Contains(got.brief, "Post the reply") {
		t.Errorf("brief should instruct to post the reply:\n%s", got.brief)
	}
	row, err := flowdb.GetFeedItem(db, "r2")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if row.Status != "acted" || row.LinkedTask != FeedTaskSlug(item) {
		t.Errorf("feed row = status %q linked %q, want acted/%s", row.Status, row.LinkedTask, FeedTaskSlug(item))
	}
}

func TestApplyActionUnsupported(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	item := flowdb.FeedItem{ID: "g4", Source: "slack", ThreadKey: "C1:4.1", SuggestedAction: "reply", Confidence: 0.9, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// reply/afk_reply are outward sends — not implemented until P2. Manual or not, ApplyAction errors.
	if err := ApplyAction(context.Background(), db, item, ActionReply, DefaultAutonomy(), true); err == nil {
		t.Error("reply action is unsupported in P1.3 and must error")
	}
}
