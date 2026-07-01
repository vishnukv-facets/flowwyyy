package steering

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// stubActionIO swaps the shell-out vars and records calls.
type spawnRec struct{ name, slug, brief, project string }
type tellRec struct{ slug, msg string }
type tagRec struct{ slug, tag string }
type forwardRec struct {
	slug string
	item flowdb.FeedItem
	msg  string
}

// stubbedTags records taskTagger calls for the most recent stubActionIO setup,
// so tests can assert source-thread tagging without changing the helper's return
// signature (used by 11 call sites). Reset on each stubActionIO call.
var stubbedTags []tagRec
var stubbedForwards []forwardRec

func stubActionIO(t *testing.T) (*[]spawnRec, *[]tellRec) {
	t.Helper()
	var spawns []spawnRec
	var tells []tellRec
	stubbedTags = nil
	stubbedForwards = nil
	oldSpawn, oldTell, oldTag, oldHandoff, oldForward := taskSpawner, taskTeller, taskTagger, taskHandoffRequester, taskForwarder
	taskSpawner = func(_ context.Context, name, slug, brief, project string) error {
		spawns = append(spawns, spawnRec{name, slug, brief, project})
		return nil
	}
	taskTeller = func(_ context.Context, slug, msg string) error {
		tells = append(tells, tellRec{slug, msg})
		return nil
	}
	taskHandoffRequester = func(_ context.Context, slug, msg, _ string) error {
		tells = append(tells, tellRec{slug, msg})
		return nil
	}
	taskTagger = func(_ context.Context, slug, tag string) error {
		stubbedTags = append(stubbedTags, tagRec{slug, tag})
		return nil
	}
	taskForwarder = func(_ context.Context, _ *sql.DB, slug string, item flowdb.FeedItem, msg string) error {
		stubbedForwards = append(stubbedForwards, forwardRec{slug: slug, item: item, msg: msg})
		return nil
	}
	t.Cleanup(func() {
		taskSpawner, taskTeller, taskTagger, taskHandoffRequester, taskForwarder = oldSpawn, oldTell, oldTag, oldHandoff, oldForward
	})
	return &spawns, &tells
}

func seedSteeringTask(t *testing.T, db *sql.DB, slug string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'high', ?, 'claude', ?, ?)`,
		slug, slug, t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
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

func TestMakeTaskFromFeedIncludesContextPack(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	spawns, _ := stubActionIO(t)

	item := flowdb.FeedItem{
		ID:              "mkctx",
		Source:          "slack",
		ThreadKey:       "D1:200.1",
		Summary:         "Omendra needs a release guardrail decision",
		SuggestedAction: "make_task",
		ContextJSON: `{
			"parent": {
				"kind": "message",
				"author": "U_OMENDRA",
				"text": "destroy not allowed before validation"
			},
			"messages": [{
				"kind": "reply",
				"author": "U_OMENDRA",
				"text": "This is blocking the migration release."
			}]
		}`,
		Status:    "new",
		CreatedAt: "2026-06-05T10:00:00Z",
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
	brief := (*spawns)[0].brief
	for _, want := range []string{
		"Source context",
		"destroy not allowed before validation",
		"This is blocking the migration release.",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("spawned task brief missing %q:\n%s", want, brief)
		}
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

func TestMakeTaskFromFeedRecordsFeedback(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "fb-make", Source: "slack", ThreadKey: "C_eng:1700000000.000300", Channel: "C_eng",
		ChannelType: "channel", Author: "U_OWNER", Summary: "create task", SuggestedAction: "make_task",
		Confidence: 0.88, Draft: "Possible reply text.", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	got, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: item.ID})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("feedback rows = %d, want 1", len(got))
	}
	if got[0].SuggestedAction != "make_task" || got[0].FinalAction != "make_task" || got[0].Outcome != "approved" {
		t.Errorf("feedback action mismatch: %+v", got[0])
	}
	if got[0].DraftEditDelta != "" {
		t.Errorf("make-task feedback should not record draft delta, got %q", got[0].DraftEditDelta)
	}
}

func TestMakeTaskFromFeedRecordsWorkstreamOwner(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "mk-owner", Source: "slack", ThreadKey: "D1:100.0", Channel: "D1", ChannelType: "im",
		Summary: "cert-manager IRSA migration", SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	ws, ok, err := flowdb.AttentionWorkstreamByThreadKey(db, "D1:100.0")
	if err != nil || !ok {
		t.Fatalf("workstream ok=%v err=%v", ok, err)
	}
	if ws.OwnerTaskSlug != FeedTaskSlug(item) {
		t.Fatalf("owner = %q, want %q", ws.OwnerTaskSlug, FeedTaskSlug(item))
	}
}

func TestMakeTaskFromFeedRecordsWorkEvent(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "we-make", Source: "slack", ThreadKey: "C_eng:1700000000.000400", Channel: "C_eng",
		ChannelType: "channel", Author: "U_OWNER", TS: "1700000000.000400",
		URL:     "https://example.slack.com/archives/C_eng/p1700000000000400",
		Summary: "please make a task", SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	seedSteeringTask(t, db, FeedTaskSlug(item))
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := MakeTaskFromFeed(context.Background(), db, item); err != nil {
		t.Fatalf("MakeTaskFromFeed: %v", err)
	}
	rows, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "attention_make_task", TaskSlug: FeedTaskSlug(item)})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("attention_make_task rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.EventID != "attention:we-make:make_task" || got.Source != "slack" || got.ExternalID != "we-make" || got.ExternalURL != item.URL {
		t.Fatalf("attention_make_task provenance = %+v", got)
	}
	if got.ActorKind != "operator" || got.ActorID != "attention-router" {
		t.Fatalf("attention_make_task actor = %q/%q", got.ActorKind, got.ActorID)
	}
}

func TestDismissFeedRecordsFeedback(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	item := flowdb.FeedItem{
		ID: "fb-dismiss", Source: "slack", ThreadKey: "C_noise:1.1", Channel: "C_noise",
		ChannelType: "channel", Author: "U_BOT", SuggestedAction: "reply",
		Confidence: 0.82, Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := DismissFeed(db, item.ID); err != nil {
		t.Fatalf("DismissFeed: %v", err)
	}
	got, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: item.ID})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("feedback rows = %d, want 1", len(got))
	}
	if got[0].FinalAction != "dismiss" || got[0].Outcome != "dismissed" || got[0].Channel != "C_noise" {
		t.Errorf("dismiss feedback mismatch: %+v", got[0])
	}
}

func TestMergeFeedDuplicatesRecordsWorkstreamAliases(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	for _, item := range []flowdb.FeedItem{
		{
			ID: "keep", Source: "slack", ThreadKey: "D1:100.0", SuggestedAction: "make_task",
			Summary: "cert-manager IRSA migration", Channel: "D1", ChannelType: "im", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
		},
		{
			ID: "dupe", Source: "slack", ThreadKey: "D1:110.0", SuggestedAction: "make_task",
			Summary: "cert-manager smoke timeout", Channel: "D1", ChannelType: "im", Status: "new", CreatedAt: "2026-06-05T10:01:00Z",
		},
	} {
		if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	n, err := MergeFeedDuplicates(db, "keep", []string{"dupe"})
	if err != nil {
		t.Fatalf("MergeFeedDuplicates: %v", err)
	}
	if n != 1 {
		t.Fatalf("merged = %d, want 1", n)
	}
	ws, ok, err := flowdb.AttentionWorkstreamByThreadKey(db, "D1:110.0")
	if err != nil || !ok {
		t.Fatalf("duplicate alias ok=%v err=%v", ok, err)
	}
	if ws.CanonicalThreadKey != "D1:100.0" || ws.CanonicalFeedItemID != "keep" {
		t.Fatalf("workstream = %+v, want canonical D1:100.0 keep", ws)
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
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "r1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 {
		t.Fatalf("feedback rows = %d, want 1", len(fb))
	}
	if fb[0].FinalAction != "send_reply" || fb[0].Outcome != "approved" || fb[0].DraftEditDelta == "" {
		t.Errorf("reply feedback mismatch: %+v", fb[0])
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
	stubActionIO(t)

	item := flowdb.FeedItem{ID: "f2", Source: "slack", ThreadKey: "C1:200.1", Summary: "rel q", MatchedTask: "kong-split", SuggestedAction: "forward", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}
	if len(stubbedForwards) != 1 || stubbedForwards[0].slug != "kong-split" {
		t.Fatalf("taskForwarder = %+v, want one call to kong-split", stubbedForwards)
	}
	if !strings.Contains(stubbedForwards[0].msg, "C1:200.1") {
		t.Errorf("forward message should reference the source thread: %q", stubbedForwards[0].msg)
	}
	// The forwarded briefing nudges the receiving session to lift any durable fact
	// from the external event into the KB (the "make forwards smarter" change).
	if !strings.Contains(stubbedForwards[0].msg, "capture it to the KB") {
		t.Errorf("forward message should prompt durable-fact KB capture: %q", stubbedForwards[0].msg)
	}
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("forwarded item should be 'acted'")
	}
}

func TestForwardFeedRecordsWorkEvent(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	seedSteeringTask(t, db, "kong-split")

	item := flowdb.FeedItem{
		ID: "we-forward", Source: "slack", ThreadKey: "C1:200.2", Channel: "C1",
		ChannelType: "channel", Author: "U_ASKER", TS: "200.2",
		URL:     "https://example.slack.com/archives/C1/p2002",
		Summary: "forward this context", MatchedTask: "kong-split", SuggestedAction: "forward",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}

	rows, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "attention_forward", TaskSlug: "kong-split"})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("attention_forward rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.EventID != "attention:we-forward:forward" || got.Source != "slack" || got.ExternalID != "we-forward" || got.ExternalURL != item.URL {
		t.Fatalf("attention_forward provenance = %+v", got)
	}
	if got.ActorKind != "operator" || got.ActorID != "attention-router" {
		t.Fatalf("attention_forward actor = %q/%q", got.ActorKind, got.ActorID)
	}
}

func TestForwardFeedIncludesContextPack(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID:              "fctx",
		Source:          "slack",
		ThreadKey:       "D1:200.1",
		Summary:         "phase plan shared",
		MatchedTask:     "coinswitch-task",
		SuggestedAction: "forward",
		ContextJSON: `{
			"parent": {
				"text": "file: PHASE2-PHASE3-EXECUTION-PLAN.md\n\n# CSX Phase Plan\n\nDMS setup must start today.\n\nSecurity report: no high-risk code indicators found in fetched content."
			}
		}`,
		Status:    "new",
		CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}
	if len(stubbedForwards) != 1 {
		t.Fatalf("taskForwarder calls = %+v, want one", stubbedForwards)
	}
	msg := stubbedForwards[0].msg
	for _, want := range []string{
		"Source context",
		"untrusted",
		"DMS setup must start today.",
		"Security report: no high-risk code indicators found in fetched content.",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("forward message missing %q:\n%s", want, msg)
		}
	}
}

func TestFeedForwardInboxEventPreservesSourceAttribution(t *testing.T) {
	at := time.Date(2026, 6, 8, 17, 30, 0, 0, time.UTC)
	item := flowdb.FeedItem{
		ID:          "src1",
		Source:      "slack",
		ThreadKey:   "D03LH2RCZMG:1780916901.021529",
		Channel:     "D03LH2RCZMG",
		ChannelType: "im",
		Author:      "U03LK2CCE68",
		TS:          "1780916901.021529",
		TeamID:      "T123",
		URL:         "slack://channel?team=T123&id=D03LH2RCZMG&message=1780916901021529",
		Summary:     "Ishaan shared the Phase 2 plan",
	}

	ev := feedForwardInboxEvent(item, feedForwardMessage(item)+"\nForwarded source context", at)

	if ev.Kind != "attention_forward" {
		t.Fatalf("kind = %q, want attention_forward", ev.Kind)
	}
	if ev.ChannelType != "slack" || ev.Channel != "D03LH2RCZMG" || ev.UserID != "U03LK2CCE68" {
		t.Fatalf("source attribution lost: %+v", ev)
	}
	if ev.ThreadTS != "1780916901.021529" || ev.TS != "1780916901.021529" {
		t.Fatalf("thread identity lost: ts=%q thread_ts=%q", ev.TS, ev.ThreadTS)
	}
	if !strings.Contains(ev.Text, "Original slack sender: U03LK2CCE68") ||
		!strings.Contains(ev.Text, "Reply target: slack thread D03LH2RCZMG:1780916901.021529") ||
		!strings.Contains(ev.Text, "Forwarded source context") {
		t.Fatalf("forward text missing source guidance:\n%s", ev.Text)
	}
}

func TestForwardFeedWritesSourceAttributedInboxEvent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("FLOW_UI_URL", "http://127.0.0.1:1")
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	now := "2026-06-08T17:40:00Z"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, priority, work_dir, session_provider, session_id, created_at, updated_at)
		 VALUES (?, ?, 'in-progress', 'regular', 'high', ?, 'claude', 'session-1', ?, ?)`,
		"coinswitch-task", "coinswitch slack thread", root, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	item := flowdb.FeedItem{
		ID:              "fw-src",
		Source:          "slack",
		ThreadKey:       "D03LH2RCZMG:1780916901.021529",
		Channel:         "D03LH2RCZMG",
		ChannelType:     "im",
		Author:          "U03LK2CCE68",
		TS:              "1780916901.021529",
		Summary:         "Ishaan shared the Phase 2 plan",
		MatchedTask:     "coinswitch-task",
		SuggestedAction: "forward",
		Status:          "new",
		CreatedAt:       now,
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	if err := ForwardFeed(context.Background(), db, item); err != nil {
		t.Fatalf("ForwardFeed: %v", err)
	}

	entries, err := monitor.ReadInboxEntries("coinswitch-task")
	if err != nil {
		t.Fatalf("ReadInboxEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	ev := entries[0].Event
	if entries[0].Meta.Source != "slack" || !entries[0].Meta.Actionable {
		t.Fatalf("meta = %+v, want actionable slack", entries[0].Meta)
	}
	if ev.Kind != "attention_forward" || ev.UserID != "U03LK2CCE68" || ev.Channel != "D03LH2RCZMG" || ev.ThreadTS != "1780916901.021529" {
		t.Fatalf("source-attributed event not preserved: %+v", ev)
	}
	if !strings.Contains(ev.Text, "it was not authored by the operator") ||
		!strings.Contains(ev.Text, "Reply target: slack thread D03LH2RCZMG:1780916901.021529") {
		t.Fatalf("forward guidance missing from event text:\n%s", ev.Text)
	}
}

func TestRequestHandoffSendsCorrelationAndLeavesFeedNew(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "hr1", Source: "slack", ThreadKey: "C1:handoff", Summary: "Is this part of your rollout task?",
		SuggestedAction: "forward", MatchedTask: "rollout-task", Reason: "same customer thread",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	h, err := RequestHandoff(context.Background(), db, item, "attention-router")
	if err != nil {
		t.Fatalf("RequestHandoff: %v", err)
	}
	if h.ID == "" || h.Receiver != "rollout-task" || h.Sender != "attention-router" {
		t.Fatalf("handoff metadata = %+v, want id/sender/receiver", h)
	}
	if len(*tells) != 1 || (*tells)[0].slug != "rollout-task" {
		t.Fatalf("task tell calls = %+v, want one message to rollout-task", *tells)
	}
	for _, want := range []string{h.ID, "Sender: attention-router", "Receiver: rollout-task", "Requested verdict: accept or decline with reason", "flow attention handoff accept", "flow attention handoff decline", "Is this part of your rollout task?"} {
		if !strings.Contains((*tells)[0].msg, want) {
			t.Errorf("handoff request missing %q:\n%s", want, (*tells)[0].msg)
		}
	}
	got, _ := flowdb.GetFeedItem(db, "hr1")
	if got.Status != "new" || got.LinkedTask != "" {
		t.Fatalf("request must leave feed item open, got %+v", got)
	}
}

func TestRequestHandoffReusesPendingForSameReceiver(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	_, tells := stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "hr-reuse", Source: "slack", ThreadKey: "C1:handoff-reuse",
		Summary: "same pending handoff", SuggestedAction: "forward", MatchedTask: "rollout-task",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	first, err := RequestHandoff(context.Background(), db, item, "attention-router")
	if err != nil {
		t.Fatalf("first RequestHandoff: %v", err)
	}
	second, err := RequestHandoff(context.Background(), db, item, "attention-router")
	if err != nil {
		t.Fatalf("second RequestHandoff: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second handoff id = %q, want existing %q", second.ID, first.ID)
	}
	if len(*tells) != 1 {
		t.Fatalf("task tell calls = %+v, want one", *tells)
	}
}

func TestRequestHandoffDeliveryFailureRemovesPendingHandoff(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)
	taskHandoffRequester = func(_ context.Context, _, _, _ string) error {
		return errors.New("tell failed")
	}

	item := flowdb.FeedItem{
		ID: "hf1", Source: "slack", ThreadKey: "C1:handoff-fail", Summary: "needs owner confirmation",
		SuggestedAction: "forward", MatchedTask: "rollout-task", Status: "new",
		CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	if _, err := RequestHandoff(context.Background(), db, item, "attention-router"); err == nil {
		t.Fatal("RequestHandoff should report delivery failure")
	}
	if _, ok, err := flowdb.LatestAttentionHandoffForFeed(db, item.ID); err != nil || ok {
		t.Fatalf("failed delivery should remove pending handoff, ok=%v err=%v", ok, err)
	}
	got, _ := flowdb.GetFeedItem(db, item.ID)
	if got.Status != "new" || got.LinkedTask != "" {
		t.Fatalf("failed delivery must leave feed retryable, got %+v", got)
	}
}

func TestRespondHandoffAcceptMarksFeedActed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "ha1", Source: "slack", ThreadKey: "C1:accept", Summary: "belongs here",
		SuggestedAction: "forward", MatchedTask: "owner-task", Status: "new",
		CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	h, err := RequestHandoff(context.Background(), db, item, "attention-router")
	if err != nil {
		t.Fatalf("RequestHandoff: %v", err)
	}

	got, err := RespondHandoff(context.Background(), db, h.ID, "accept", "this is our deployment thread")
	if err != nil {
		t.Fatalf("RespondHandoff accept: %v", err)
	}
	if got.Status != "accepted" || got.Reason != "this is our deployment thread" {
		t.Fatalf("accepted handoff = %+v", got)
	}
	feed, _ := flowdb.GetFeedItem(db, "ha1")
	if feed.Status != "acted" || feed.LinkedTask != "owner-task" {
		t.Fatalf("accepted handoff should mark feed acted/linked, got %+v", feed)
	}
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "ha1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "confirm_handoff" || fb[0].Outcome != "approved" {
		t.Fatalf("accept feedback mismatch: %+v", fb)
	}
}

func TestRespondHandoffDeclineLeavesFeedNew(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubActionIO(t)

	item := flowdb.FeedItem{
		ID: "hd1", Source: "slack", ThreadKey: "C1:decline", Summary: "probably not ours",
		SuggestedAction: "forward", MatchedTask: "owner-task", Status: "new",
		CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	h, err := RequestHandoff(context.Background(), db, item, "attention-router")
	if err != nil {
		t.Fatalf("RequestHandoff: %v", err)
	}

	got, err := RespondHandoff(context.Background(), db, h.ID, "decline", "this belongs to support-triage")
	if err != nil {
		t.Fatalf("RespondHandoff decline: %v", err)
	}
	if got.Status != "declined" || got.Reason != "this belongs to support-triage" {
		t.Fatalf("declined handoff = %+v", got)
	}
	feed, _ := flowdb.GetFeedItem(db, "hd1")
	if feed.Status != "new" || feed.LinkedTask != "" {
		t.Fatalf("declined handoff should leave feed open, got %+v", feed)
	}
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "hd1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "confirm_handoff" || fb[0].Outcome != "declined" {
		t.Fatalf("decline feedback mismatch: %+v", fb)
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

func TestApplyActionAutoCaptureKBSkipsOperatorFeedback(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })

	item := flowdb.FeedItem{ID: "kb1", Source: "slack", ThreadKey: "C1:1.1", Summary: "durable org fact", SuggestedAction: "capture_kb", Confidence: 0.90, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pol := AutonomyPolicy{ActionCaptureKB: {Enabled: true, Threshold: 0.75}}
	if err := ApplyActionAuto(context.Background(), db, item, ActionCaptureKB, t.TempDir(), pol, 0.90); err != nil {
		t.Fatalf("ApplyActionAuto capture_kb: %v", err)
	}
	got, err := flowdb.GetFeedItem(db, "kb1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" {
		t.Errorf("confirmed auto-capture should mark the card acted, got %q", got.Status)
	}
	// The invariant: an autonomous outcome must NEVER write an attention_feedback
	// row, or the ConfidenceCalibrator would learn from the steerer agreeing with
	// itself and inflate the very confidence that gated the action.
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 0 {
		t.Errorf("autonomous capture_kb wrote %d feedback rows, want 0 (calibration must stay operator-only)", len(fb))
	}
}

func TestApplyActionAutoDismissDigestOnly(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	item := flowdb.FeedItem{ID: "fy1", Source: "slack", ThreadKey: "C1:2.1", Summary: "fyi", SuggestedAction: "digest_only", Confidence: 0.90, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pol := AutonomyPolicy{ActionDigestOnly: {Enabled: true, Threshold: 0.85}}
	if err := ApplyActionAuto(context.Background(), db, item, ActionDigestOnly, "", pol, 0.90); err != nil {
		t.Fatalf("ApplyActionAuto digest_only: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("auto-dismiss should resolve the FYI card to 'dismissed'")
	}
	fb, _ := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{})
	if len(fb) != 0 {
		t.Errorf("autonomous dismiss wrote %d feedback rows, want 0", len(fb))
	}
}

func TestApplyActionAutoDeniedLeavesCardUntouched(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	item := flowdb.FeedItem{ID: "d1", Source: "slack", ThreadKey: "C1:3.1", SuggestedAction: "capture_kb", Confidence: 0.99, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// DefaultAutonomy is all-off — even a 0.99 gate confidence is denied.
	if err := ApplyActionAuto(context.Background(), db, item, ActionCaptureKB, t.TempDir(), DefaultAutonomy(), 0.99); err != ErrAutonomyDenied {
		t.Fatalf("auto capture under default policy should be ErrAutonomyDenied, got %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 1 {
		t.Errorf("denied auto-act must leave the card 'new'")
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
