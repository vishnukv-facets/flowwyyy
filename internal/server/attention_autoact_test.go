package server

import (
	"flow/internal/productdb"
	"testing"
)

// Autonomy is off by default — a surfaced session card must NOT auto-forward or
// auto-send, even at confidence 0.99. This is the safety property: the session
// path only acts when the operator has opted in.
func TestAutoActOnSurfacedCardOffByDefault(t *testing.T) {
	t.Setenv("FLOW_STEERING_AUTONOMY", "") // default: every action disabled
	s, db := attentionTestServer(t)
	cards := []productdb.FeedItem{
		{ID: "fwd", Source: "slack", ThreadKey: "C1:1.1", Summary: "s", SuggestedAction: "forward", MatchedTask: "some-task", Confidence: 0.99, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"},
		{ID: "rep", Source: "slack", ThreadKey: "C2:1.1", Summary: "s", SuggestedAction: "reply", Draft: "hi", Confidence: 0.99, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"},
	}
	for _, c := range cards {
		if _, err := productdb.UpsertFeedItem(db, c); err != nil {
			t.Fatalf("seed %s: %v", c.ID, err)
		}
		item, err := productdb.GetFeedItem(db, c.ID)
		if err != nil {
			t.Fatalf("get %s: %v", c.ID, err)
		}
		s.autoActOnSurfacedCard(item) // forward path is synchronous; reply gate denies (off)
		got, err := productdb.GetFeedItem(db, c.ID)
		if err != nil {
			t.Fatalf("re-get %s: %v", c.ID, err)
		}
		if got.Status != "new" {
			t.Fatalf("autonomy off must leave %s untouched; status=%q want new", c.ID, got.Status)
		}
	}
}

// With forward autonomy opted in above the card's confidence, a session-surfaced
// forward card auto-forwards to its matched task (status → acted) — the gap that
// left 90% cards stranded in the queue.
func TestAutoActOnSurfacedCardAutoForwardsWhenOptedIn(t *testing.T) {
	t.Setenv("FLOW_STEERING_AUTONOMY", `{"forward":{"enabled":true,"threshold":0.85}}`)
	s, db := attentionTestServer(t)
	now := "2026-06-05T10:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, session_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"csx-test", "CSX migration", "in-progress", "high", t.TempDir(), "claude", "2fa45058-ab12-4d78-b944-81a7a1a482ae", now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
		ID: "f90", Source: "slack", ThreadKey: "C1:1.1", Summary: "forward me",
		SuggestedAction: "forward", MatchedTask: "csx-test", Confidence: 0.90,
		Status: "new", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	item, err := productdb.GetFeedItem(db, "f90")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	s.autoActOnSurfacedCard(item)
	got, err := productdb.GetFeedItem(db, "f90")
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got.Status != "acted" {
		t.Fatalf("opted-in forward at 0.90 ≥ 0.85 must auto-forward; status=%q want acted", got.Status)
	}
}
