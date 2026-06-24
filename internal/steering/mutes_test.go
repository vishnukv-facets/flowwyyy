package steering

import (
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func TestMuteAndSweep(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	mk := func(id, channel, author string) {
		t.Helper()
		if _, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
			ID: id, Source: "slack", ThreadKey: "C:" + id, Channel: channel, Author: author,
			SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	mk("a", "C_noise", "U1") // matches channel mute
	mk("b", "C_noise", "U2") // matches channel mute
	mk("c", "C_other", "U3") // different channel — untouched

	n, err := MuteAndSweep(db, productdb.MuteScopeChannel, "C_noise")
	if err != nil {
		t.Fatalf("MuteAndSweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept = %d, want 2", n)
	}
	// The mute is recorded for future Stage 0 drops.
	mutes, _ := productdb.ListSteeringMutes(db)
	if !mutes.Channels["C_noise"] {
		t.Error("channel mute not recorded")
	}
	// Open cards on the muted channel are dismissed; the other remains.
	if it, _ := productdb.GetFeedItem(db, "a"); it.Status != "dismissed" {
		t.Errorf("card a = %q, want dismissed", it.Status)
	}
	if it, _ := productdb.GetFeedItem(db, "c"); it.Status != "new" {
		t.Errorf("card c = %q, want new (other channel)", it.Status)
	}
}
