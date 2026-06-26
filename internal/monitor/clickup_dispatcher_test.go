package monitor

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func seedClickUpTask(t *testing.T, slug string, db *sql.DB, tag string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'high', ?, 'default', 'claude', ?, ?, ?)`,
		slug, "seeded clickup task", t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed clickup task %s: %v", slug, err)
	}
	if err := flowdb.AddTaskTag(db, slug, tag); err != nil {
		t.Fatalf("tag %s: %v", tag, err)
	}
}

func TestClickUpDispatcherCreatesTask(t *testing.T) {
	t.Setenv("FLOW_CLICKUP_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	spawns, tags, opens, restore := stubDispatcherIO(t)
	defer restore()

	d := NewClickUpDispatcher(db, nil)
	ev := ClickUpEvent{
		Kind:      ClickUpEventTaskAssigneeUpdated,
		TeamID:    "321",
		TaskID:    "cu-123",
		TaskName:  "Prepare launch checklist",
		TaskURL:   "https://app.clickup.com/t/cu-123",
		WebhookID: "wh-1",
		HistoryID: "hist-1",
		Author:    "Jane",
		Body:      "assignee_add changed to Vishnu",
		RawJSON:   `{"event":"taskAssigneeUpdated"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(*spawns))
	}
	spawn := (*spawns)[0]
	if spawn.Slug != "clickup-cu-123" {
		t.Fatalf("slug = %q", spawn.Slug)
	}
	for _, want := range []string{
		"ClickUp task: cu-123",
		"https://app.clickup.com/t/cu-123",
		"Prepare launch checklist",
		"assignee_add changed to Vishnu",
	} {
		if !strings.Contains(spawn.Brief, want) {
			t.Fatalf("brief missing %q\n%s", want, spawn.Brief)
		}
	}
	gotTags := map[string]bool{}
	for _, c := range *tags {
		gotTags[c.Tag] = true
	}
	for _, want := range []string{"clickup", "clickup-task:321:cu-123"} {
		if !gotTags[want] {
			t.Fatalf("missing tag %q from %v", want, gotTags)
		}
	}
	if len(*opens) != 0 {
		t.Fatalf("autoopen off should suppress opens: %v", *opens)
	}
}

func TestClickUpDispatcherAppendsToTrackedTaskAndDedupes(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedClickUpTask(t, "tracked-clickup", db, "clickup-task:321:cu-123")

	d := NewClickUpDispatcher(db, nil)
	ev := ClickUpEvent{
		Kind:      ClickUpEventTaskCommentPosted,
		TeamID:    "321",
		TaskID:    "cu-123",
		TaskURL:   "https://app.clickup.com/t/cu-123",
		WebhookID: "wh-1",
		HistoryID: "hist-1",
		Author:    "Sam",
		Body:      "Can you add a status update?",
		RawJSON:   `{"event":"taskCommentPosted"}`,
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("dispatch duplicate: %v", err)
	}
	entries, err := ReadInboxEntries("tracked-clickup")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate event should append once, got %d", len(entries))
	}
	if entries[0].Event.ChannelType != "clickup" || entries[0].Event.Text != ev.Body {
		t.Fatalf("entry = %+v", entries[0])
	}
	seen, err := flowdb.HasClickUpEvent(db, ev.EventKeyValue())
	if err != nil {
		t.Fatalf("HasClickUpEvent: %v", err)
	}
	if !seen {
		t.Fatal("ClickUp event should be recorded for dedupe")
	}
}

func TestClickUpDispatcherSteererOwnedRoutingSkipsLegacyTaskPipeline(t *testing.T) {
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedClickUpTask(t, "tracked-clickup", db, "clickup-task:321:cu-123")
	observer := &fakeMessageObserver{}

	d := NewClickUpDispatcher(db, nil)
	d.Steerer = observer
	d.SteererOwnsRouting = func() bool { return true }
	ev := ClickUpEvent{
		Kind:      ClickUpEventTaskCommentPosted,
		TeamID:    "321",
		TaskID:    "cu-123",
		TaskURL:   "https://app.clickup.com/t/cu-123",
		WebhookID: "wh-2",
		HistoryID: "hist-2",
		Author:    "Sam",
		Body:      "Please confirm this is still on track.",
	}
	if err := d.Dispatch(context.Background(), ev); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(observer.events) != 1 || observer.events[0].ChannelType != "clickup" || observer.events[0].Text != ev.Body {
		t.Fatalf("steerer events = %+v", observer.events)
	}
	entries, err := ReadInboxEntries("tracked-clickup")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("steerer-owned ClickUp event should not append directly; entries=%+v", entries)
	}
}
