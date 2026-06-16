package steering

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

func surfaceTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSurfaceCardRejectsForeignThreadKey(t *testing.T) {
	db := surfaceTestDB(t)

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "100.1",
		ThreadKey:   "C1:999.9",
		Action:      "digest_only",
		Summary:     "hello",
		Confidence:  0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" {
		t.Fatalf("want a surfaced card, got surfaced=%v id=%q", surfaced, id)
	}
	got, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.ThreadKey != "C1:100.1" {
		t.Errorf("foreign key should fall back to raw C1:100.1, got %q", got.ThreadKey)
	}
}

func TestSurfaceCardMergesIntoOpenCard(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "a1",
		Source:          "slack",
		ThreadKey:       "C1:50.0",
		SuggestedAction: "digest_only",
		Summary:         "repo access for dynamodb",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U1",
		TS:              "50.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "60.0",
		ThreadKey:   "C1:50.0",
		Action:      "digest_only",
		Summary:     "list the repo names",
		Confidence:  0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced {
		t.Fatalf("want surfaced")
	}
	if id != "a1" {
		t.Errorf("want merge into existing card a1, got new id %q", id)
	}
}

func TestSurfaceCardContextOnlyDoesNotSurface(t *testing.T) {
	db := surfaceTestDB(t)

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "70.0",
		Action:      "digest_only",
		Summary:     "operator's own note",
		ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if surfaced || id != "" {
		t.Errorf("context_only must not surface, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("context_only should not create feed items, got %+v", items)
	}
}
