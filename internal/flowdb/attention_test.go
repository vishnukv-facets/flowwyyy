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

func TestAttentionFeedCoalescesDismissedThreadAndReopens(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	first := FeedItem{
		ID:              "old",
		Source:          "slack",
		ThreadKey:       "C1:250.1",
		Summary:         "first message",
		SuggestedAction: "digest_only",
		Status:          "new",
		CreatedAt:       "2026-06-05T10:00:00Z",
	}
	if _, err := UpsertFeedItem(db, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := SetFeedItemStatus(db, "old", "dismissed", "2026-06-05T10:01:00Z"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	next := FeedItem{
		ID:              "new",
		Source:          "slack",
		ThreadKey:       "C1:250.1",
		Summary:         "thread now has a concrete rollout plan",
		SuggestedAction: "forward",
		MatchedTask:     "coinswitch-task",
		Confidence:      0.82,
		ContextJSON:     `{"summary":"collated thread context"}`,
		Status:          "new",
		CreatedAt:       "2026-06-05T10:05:00Z",
	}
	id, err := UpsertFeedItem(db, next)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id != "old" {
		t.Fatalf("coalesced id = %q, want old", id)
	}

	got, err := GetFeedItem(db, "old")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "new" {
		t.Errorf("Status = %q, want reopened new", got.Status)
	}
	if got.ActedAt != "" {
		t.Errorf("ActedAt = %q, want cleared after reopen", got.ActedAt)
	}
	if got.Summary != next.Summary || got.SuggestedAction != "forward" || got.MatchedTask != "coinswitch-task" {
		t.Errorf("coalesced row did not carry refreshed decision: %+v", got)
	}
	if got.ContextJSON != next.ContextJSON {
		t.Errorf("ContextJSON = %q, want refreshed context", got.ContextJSON)
	}
	list, err := ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(list) != 1 || list[0].ID != "old" {
		t.Fatalf("new feed rows = %+v, want one reopened old row", list)
	}
}

func TestAttentionFeedDismissedNotResurrectedBySameMessage(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	// Surface a message, then the operator dismisses it.
	first := FeedItem{
		ID: "m1", Source: "slack", ThreadKey: "C9:400.1", TS: "1780985380.421019",
		Summary: "can you please join the meet", SuggestedAction: "reply",
		Status: "new", CreatedAt: "2026-06-09T06:12:00Z",
	}
	if _, err := UpsertFeedItem(db, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := SetFeedItemStatus(db, "m1", "dismissed", "2026-06-09T06:20:00Z"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// The SAME message is re-observed an hour later (verdict-cache TTL lapsed,
	// backfill replay) and re-triaged. It must NOT resurrect the dismissed card.
	resurface := first
	resurface.ID = "m2"
	resurface.CreatedAt = "2026-06-09T07:15:00Z"
	id, surfaced, err := UpsertFeedItemSurfaced(db, resurface)
	if err != nil {
		t.Fatalf("resurface upsert: %v", err)
	}
	if surfaced {
		t.Errorf("surfaced = true, want false: a dismissal must survive re-observation of the same message")
	}
	if id != "m1" {
		t.Errorf("id = %q, want the existing dismissed row m1", id)
	}
	if got, _ := GetFeedItem(db, "m1"); got.Status != "dismissed" {
		t.Errorf("Status = %q, want still dismissed", got.Status)
	}
	if n, _ := ListFeedItems(db, "new"); len(n) != 0 {
		t.Errorf("new feed rows = %d, want 0 (card stays dismissed)", len(n))
	}

	// Genuinely newer thread activity (a strictly newer ts) DOES reopen the card.
	newer := first
	newer.ID = "m3"
	newer.TS = "1780989999.000000"
	newer.Summary = "follow-up reply with new context"
	newer.CreatedAt = "2026-06-09T08:30:00Z"
	_, surfaced2, err := UpsertFeedItemSurfaced(db, newer)
	if err != nil {
		t.Fatalf("newer upsert: %v", err)
	}
	if !surfaced2 {
		t.Errorf("surfaced = false, want true: newer thread activity should reopen the card")
	}
	if got, _ := GetFeedItem(db, "m1"); got.Status != "new" || got.Summary != newer.Summary {
		t.Errorf("after newer activity, row = {status:%q summary:%q}, want reopened with refreshed context", got.Status, got.Summary)
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

func TestListOpenClubCandidates(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	seed := func(id, channel, threadKey, status, createdAt string) {
		t.Helper()
		if _, err := UpsertFeedItem(db, FeedItem{
			ID: id, Source: "slack", ThreadKey: threadKey, SuggestedAction: "reply",
			Summary: id, Channel: channel, ChannelType: "im", Status: status, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Three open cards in the target DM, one dismissed, one in another channel,
	// and one open card older than the window.
	seed("c1", "D1", "D1:100.1", "new", "2026-06-05T10:00:00Z")
	seed("c2", "D1", "D1:200.1", "new", "2026-06-05T10:05:00Z")
	seed("c3", "D1", "D1:300.1", "new", "2026-06-05T10:10:00Z")
	seed("dismissed", "D1", "D1:400.1", "new", "2026-06-05T10:12:00Z")
	if err := SetFeedItemStatus(db, "dismissed", "dismissed", "2026-06-05T10:13:00Z"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	seed("other", "D2", "D2:100.1", "new", "2026-06-05T10:06:00Z")
	seed("stale", "D1", "D1:050.1", "new", "2026-06-04T10:00:00Z")

	// Incoming standalone message is D1:300.1 (c3); it must be excluded from its
	// own candidate set. Window cutoff drops "stale"; channel filter drops
	// "other"; status filter drops "dismissed".
	got, err := ListOpenClubCandidates(db, "D1", "D1:300.1", "2026-06-05T09:00:00Z", 10)
	if err != nil {
		t.Fatalf("ListOpenClubCandidates: %v", err)
	}
	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	// Newest-first, excluding c3 (the incoming itself).
	want := []string{"c2", "c1"}
	if len(ids) != len(want) {
		t.Fatalf("candidate ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("candidate ids = %v, want %v", ids, want)
		}
	}

	// limit caps the result count (newest kept).
	capped, err := ListOpenClubCandidates(db, "D1", "", "2026-06-05T09:00:00Z", 2)
	if err != nil {
		t.Fatalf("ListOpenClubCandidates capped: %v", err)
	}
	if len(capped) != 2 || capped[0].ID != "c3" || capped[1].ID != "c2" {
		t.Fatalf("capped = %v, want [c3 c2]", feedIDs(capped))
	}

	// An empty channel can never club.
	none, err := ListOpenClubCandidates(db, "", "", "2026-06-05T09:00:00Z", 10)
	if err != nil {
		t.Fatalf("ListOpenClubCandidates empty channel: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty channel candidates = %v, want none", feedIDs(none))
	}
}

func feedIDs(items []FeedItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestAttentionHandoffCreateExpireAndLatest(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if _, err := UpsertFeedItem(db, FeedItem{
		ID: "hf1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "forward",
		MatchedTask: "owner-task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	h, err := CreateAttentionHandoff(db, AttentionHandoff{
		FeedItemID:       "hf1",
		Sender:           "attention-router",
		Receiver:         "owner-task",
		Context:          "Summary: rollout question",
		RequestedVerdict: "accept_or_decline",
		RequestedAt:      "2026-06-05T10:01:00Z",
		ExpiresAt:        "2026-06-05T10:31:00Z",
	})
	if err != nil {
		t.Fatalf("CreateAttentionHandoff: %v", err)
	}
	if h.ID == "" {
		t.Fatal("handoff id should be generated")
	}
	if h.Status != "pending" {
		t.Fatalf("Status = %q, want pending", h.Status)
	}
	got, ok, err := LatestAttentionHandoffForFeed(db, "hf1")
	if err != nil {
		t.Fatalf("LatestAttentionHandoffForFeed: %v", err)
	}
	if !ok || got.ID != h.ID || got.Receiver != "owner-task" || got.Context == "" {
		t.Fatalf("latest handoff mismatch: ok=%v got=%+v want id %s", ok, got, h.ID)
	}

	expired, err := ExpireAttentionHandoffs(db, "2026-06-05T10:31:01Z")
	if err != nil {
		t.Fatalf("ExpireAttentionHandoffs: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	got, err = GetAttentionHandoff(db, h.ID)
	if err != nil {
		t.Fatalf("GetAttentionHandoff: %v", err)
	}
	if got.Status != "timeout" || got.RespondedAt == "" {
		t.Fatalf("expired handoff = %+v, want timeout with responded_at", got)
	}
	item, _ := GetFeedItem(db, "hf1")
	if item.Status != "new" {
		t.Fatalf("timed-out handoff must not mark feed acted, got status %q", item.Status)
	}
}

func TestRespondAttentionHandoffRejectsMalformedVerdict(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	h, err := CreateAttentionHandoff(db, AttentionHandoff{
		FeedItemID: "hf2", Sender: "attention-router", Receiver: "owner-task",
		Context: "context", RequestedVerdict: "accept_or_decline",
		RequestedAt: "2026-06-05T10:00:00Z", ExpiresAt: "2026-06-05T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("CreateAttentionHandoff: %v", err)
	}
	if _, err := RespondAttentionHandoff(db, h.ID, "maybe", "ambiguous", "2026-06-05T10:05:00Z"); err == nil {
		t.Fatal("malformed verdict should error")
	}
	got, err := GetAttentionHandoff(db, h.ID)
	if err != nil {
		t.Fatalf("GetAttentionHandoff: %v", err)
	}
	if got.Status != "pending" || got.Reason != "" || got.RespondedAt != "" {
		t.Fatalf("malformed response should leave handoff pending, got %+v", got)
	}
}
