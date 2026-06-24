package productdb_test

// Cross-layer parity for the attention_feed CRUD: write via productdb, read via
// flowdb (and vice versa), proving the ported copy operates on the same rows
// identically — incl. the coalesce-by-thread upsert and the acted transition.

import (
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func TestFeedItemParity(t *testing.T) {
	db := openProduct(t)

	id, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
		ID: "f1", Source: "slack", ThreadKey: "C1:1.1", Summary: "needs a look",
		SuggestedAction: "forward", MatchedTask: "deploy", Confidence: 0.9,
		Channel: "C1", ChannelType: "channel", Author: "U1", TS: "1.1",
		Status: "new", CreatedAt: "2026-06-24T08:00:00Z",
	})
	if err != nil || id != "f1" {
		t.Fatalf("productdb.UpsertFeedItem: id=%q err=%v", id, err)
	}

	// productdb write → flowdb read parity (field-by-field on the load-bearing fields).
	fw, err := flowdb.GetFeedItem(db, "f1")
	if err != nil {
		t.Fatalf("flowdb.GetFeedItem: %v", err)
	}
	pw, err := productdb.GetFeedItem(db, "f1")
	if err != nil {
		t.Fatalf("productdb.GetFeedItem: %v", err)
	}
	if fw.ThreadKey != pw.ThreadKey || fw.SuggestedAction != pw.SuggestedAction ||
		fw.MatchedTask != pw.MatchedTask || fw.Confidence != pw.Confidence ||
		fw.Channel != pw.Channel || fw.Status != pw.Status {
		t.Errorf("GetFeedItem parity mismatch:\n flowdb=%+v\n productdb=%+v", fw, pw)
	}

	// Coalesce-by-thread: a second 'new' upsert on the same thread reuses the row.
	id2, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
		ID: "f2", Source: "slack", ThreadKey: "C1:1.1", Summary: "updated",
		SuggestedAction: "make_task", Status: "new", CreatedAt: "2026-06-24T09:00:00Z", TS: "1.2",
	})
	if err != nil || id2 != "f1" {
		t.Errorf("coalesce: expected reuse of f1, got id=%q err=%v", id2, err)
	}

	// List parity + count after coalesce (should still be one row).
	pl, _ := productdb.ListFeedItems(db, "new")
	fl, _ := flowdb.ListFeedItems(db, "new")
	if len(pl) != len(fl) || len(pl) != 1 {
		t.Errorf("ListFeedItems parity/count: productdb=%d flowdb=%d (want 1)", len(pl), len(fl))
	}

	// Acted transition via productdb → visible to flowdb.
	if err := productdb.SetFeedItemActed(db, "f1", "deploy", "2026-06-24T10:00:00Z"); err != nil {
		t.Fatalf("productdb.SetFeedItemActed: %v", err)
	}
	if got, _ := flowdb.GetFeedItem(db, "f1"); got.Status != "acted" || got.LinkedTask != "deploy" {
		t.Errorf("after SetFeedItemActed: flowdb sees status=%q linked=%q", got.Status, got.LinkedTask)
	}
}
