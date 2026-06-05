package flowdb

import "testing"

func TestAttentionFeedInsertAndList(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	item := FeedItem{
		ID:              "f1",
		Source:          "slack",
		ThreadKey:       "C1:100.1",
		Summary:         "Customer asks for rollout date",
		SuggestedAction: "make_task",
		MatchedTask:     "kong-split",
		Urgency:         "urgent",
		IsVIP:           true,
		Confidence:      0.9,
		Draft:           "On it.",
		Reason:          "names operator",
		ContextJSON:     `{"k":"v"}`,
		Status:          "new",
		CreatedAt:       "2026-06-05T10:00:00Z",
	}
	id, err := UpsertFeedItem(db, item)
	if err != nil {
		t.Fatalf("UpsertFeedItem: %v", err)
	}
	if id != "f1" {
		t.Fatalf("id = %q, want f1", id)
	}

	got, err := ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ID != "f1" || got[0].MatchedTask != "kong-split" || !got[0].IsVIP || got[0].Confidence != 0.9 {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func TestAttentionFeedCoalescesByThreadKey(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	first := FeedItem{ID: "a", Source: "slack", ThreadKey: "C1:200.1", SuggestedAction: "reply", Summary: "first", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := UpsertFeedItem(db, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second := FeedItem{ID: "b", Source: "slack", ThreadKey: "C1:200.1", SuggestedAction: "make_task", Summary: "updated", Status: "new", CreatedAt: "2026-06-05T10:05:00Z"}
	id, err := UpsertFeedItem(db, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id != "a" {
		t.Errorf("coalesced id = %q, want existing id a", id)
	}

	got, err := ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (coalesced)", len(got))
	}
	if got[0].Summary != "updated" || got[0].SuggestedAction != "make_task" {
		t.Errorf("expected coalesced row to carry new fields, got %+v", got[0])
	}
}

func TestAttentionFeedSetStatus(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if _, err := UpsertFeedItem(db, FeedItem{ID: "x", Source: "slack", ThreadKey: "C1:300.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := SetFeedItemStatus(db, "x", "dismissed", "2026-06-05T11:00:00Z"); err != nil {
		t.Fatalf("SetFeedItemStatus: %v", err)
	}
	if n, _ := ListFeedItems(db, "new"); len(n) != 0 {
		t.Errorf("new count = %d, want 0", len(n))
	}
	d, _ := ListFeedItems(db, "dismissed")
	if len(d) != 1 || d[0].ActedAt != "2026-06-05T11:00:00Z" {
		t.Errorf("dismissed = %+v", d)
	}
}

func TestResolveOpenFeedItemsByThread(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	// Two 'new' cards on the same thread, one on a different thread, one already
	// dismissed on the target thread.
	mustUpsert := func(id, thread, status string) {
		t.Helper()
		if _, err := UpsertFeedItem(db, FeedItem{ID: id, Source: "slack", ThreadKey: thread, SuggestedAction: "reply", Status: status, CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// UpsertFeedItem coalesces 'new' rows by thread_key, so the same-thread pair
	// must use distinct threads to both stay 'new'; instead seed one new + one
	// dismissed on the target thread and a new on another thread.
	mustUpsert("a", "C1:900.1", "new")
	mustUpsert("b", "C1:900.1", "dismissed") // already resolved — must be untouched
	mustUpsert("c", "C2:900.2", "new")       // different thread — must be untouched

	n, err := ResolveOpenFeedItemsByThread(db, "C1:900.1", "2026-06-05T12:00:00Z")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n != 1 {
		t.Fatalf("resolved = %d, want 1 (only the open card on the thread)", n)
	}
	got, _ := GetFeedItem(db, "a")
	if got.Status != "acted" || got.ActedAt != "2026-06-05T12:00:00Z" {
		t.Errorf("card a = %+v, want acted with stamped acted_at", got)
	}
	if other, _ := GetFeedItem(db, "c"); other.Status != "new" {
		t.Errorf("card c (other thread) status = %q, want new", other.Status)
	}

	// Empty thread key is a safe no-op.
	if n, err := ResolveOpenFeedItemsByThread(db, "", "2026-06-05T12:00:00Z"); err != nil || n != 0 {
		t.Errorf("empty thread key: n=%d err=%v, want 0,nil", n, err)
	}
}

func TestFeedItemLinkedTaskRoundTrip(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if _, err := UpsertFeedItem(db, FeedItem{
		ID: "lt1", Source: "slack", ThreadKey: "C1:500.1", SuggestedAction: "make_task",
		LinkedTask: "att-c1-500-1", Status: "acted", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := GetFeedItem(db, "lt1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.LinkedTask != "att-c1-500-1" {
		t.Errorf("LinkedTask = %q, want att-c1-500-1", got.LinkedTask)
	}
	list, _ := ListFeedItems(db, "acted")
	if len(list) != 1 || list[0].LinkedTask != "att-c1-500-1" {
		t.Errorf("ListFeedItems LinkedTask round-trip mismatch: %+v", list)
	}
}

func TestSetFeedItemActed(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if _, err := UpsertFeedItem(db, FeedItem{ID: "act1", Source: "slack", ThreadKey: "C1:600.1", SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SetFeedItemActed(db, "act1", "att-c1-600-1", "2026-06-05T11:00:00Z"); err != nil {
		t.Fatalf("SetFeedItemActed: %v", err)
	}
	got, err := GetFeedItem(db, "act1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" {
		t.Errorf("Status = %q, want acted", got.Status)
	}
	if got.ActedAt != "2026-06-05T11:00:00Z" {
		t.Errorf("ActedAt = %q, want stamped", got.ActedAt)
	}
	if got.LinkedTask != "att-c1-600-1" {
		t.Errorf("LinkedTask = %q, want att-c1-600-1", got.LinkedTask)
	}
	// Missing id → error.
	if err := SetFeedItemActed(db, "nope", "x", "2026-06-05T11:00:00Z"); err == nil {
		t.Error("SetFeedItemActed on a missing id must error")
	}
}

func TestFeedItemSourceContextRoundTrip(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	in := FeedItem{
		ID: "sc1", Source: "slack", ThreadKey: "C1:700.1", SuggestedAction: "reply",
		Channel: "C700", ChannelType: "channel", Author: "U_BOB", TS: "700.1",
		TeamID: "T123", URL: "https://example.slack.com/archives/C700/p7001",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := UpsertFeedItem(db, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := GetFeedItem(db, "sc1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Channel != "C700" || got.ChannelType != "channel" || got.Author != "U_BOB" ||
		got.TS != "700.1" || got.TeamID != "T123" || got.URL != "https://example.slack.com/archives/C700/p7001" {
		t.Errorf("GetFeedItem source-context round-trip mismatch: %+v", got)
	}

	list, _ := ListFeedItems(db, "new")
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	l := list[0]
	if l.Channel != "C700" || l.ChannelType != "channel" || l.Author != "U_BOB" ||
		l.TS != "700.1" || l.TeamID != "T123" || l.URL != "https://example.slack.com/archives/C700/p7001" {
		t.Errorf("ListFeedItems source-context round-trip mismatch: %+v", l)
	}

	// Coalescing onto an existing 'new' row must also refresh the source context.
	upd := FeedItem{
		ID: "sc2", Source: "slack", ThreadKey: "C1:700.1", SuggestedAction: "make_task",
		Channel: "C700", ChannelType: "channel", Author: "U_CAROL", TS: "700.2",
		TeamID: "T123", URL: "https://example.slack.com/archives/C700/p7002",
		Status: "new", CreatedAt: "2026-06-05T10:05:00Z",
	}
	id, err := UpsertFeedItem(db, upd)
	if err != nil {
		t.Fatalf("coalesce upsert: %v", err)
	}
	if id != "sc1" {
		t.Fatalf("coalesced id = %q, want sc1", id)
	}
	got2, _ := GetFeedItem(db, "sc1")
	if got2.Author != "U_CAROL" || got2.TS != "700.2" || got2.URL != "https://example.slack.com/archives/C700/p7002" {
		t.Errorf("coalesce did not refresh source context: %+v", got2)
	}
}

func TestGetFeedItem(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	in := FeedItem{ID: "g1", Source: "slack", ThreadKey: "C1:1.1", Summary: "hi", SuggestedAction: "make_task", MatchedTask: "kong-split", Confidence: 0.7, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := UpsertFeedItem(db, in); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := GetFeedItem(db, "g1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.ID != "g1" || got.MatchedTask != "kong-split" || got.SuggestedAction != "make_task" || got.Confidence != 0.7 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if _, err := GetFeedItem(db, "nope"); err == nil {
		t.Error("GetFeedItem on a missing id must return an error")
	}
}
