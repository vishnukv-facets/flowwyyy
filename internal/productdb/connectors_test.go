package productdb_test

// Cross-layer parity for the Bucket-F connector tables: write via productdb,
// read back via flowdb (and vice versa), proving both layers operate on the
// same physical rows identically. Product tables aren't in the core schema, so
// these tests call productdb.Ensure after flowdb.OpenDB to create them.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func openProduct(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := productdb.Ensure(db); err != nil {
		t.Fatalf("productdb.Ensure: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGitHubEventParity(t *testing.T) {
	db := openProduct(t)
	first, err := productdb.RecordGitHubEvent(db, productdb.GitHubEventLogEntry{
		EventKey: "pr:o/r#1:review_requested", EventKind: "pr_review_requested", TaskSlug: "", RawJSON: "{}",
	})
	if err != nil || !first {
		t.Fatalf("first RecordGitHubEvent: ok=%v err=%v", first, err)
	}
	// productdb write is visible to flowdb reader (and productdb's own reader).
	for _, name := range []string{"flowdb", "productdb"} {
		var has bool
		if name == "flowdb" {
			has, err = flowdb.HasGitHubEvent(db, "pr:o/r#1:review_requested")
		} else {
			has, err = productdb.HasGitHubEvent(db, "pr:o/r#1:review_requested")
		}
		if err != nil || !has {
			t.Errorf("%s.HasGitHubEvent: has=%v err=%v", name, has, err)
		}
	}
	dup, err := productdb.RecordGitHubEvent(db, productdb.GitHubEventLogEntry{EventKey: "pr:o/r#1:review_requested", EventKind: "pr_review_requested"})
	if err != nil || dup {
		t.Errorf("duplicate RecordGitHubEvent should be (false,nil): dup=%v err=%v", dup, err)
	}
}

func TestGitHubDeliveryParity(t *testing.T) {
	db := openProduct(t)
	first, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "d1", EventType: "pull_request", Action: "opened"})
	if err != nil || !first {
		t.Fatalf("first RecordGitHubDelivery: ok=%v err=%v", first, err)
	}
	if dup, _ := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{DeliveryID: "d1"}); dup {
		t.Errorf("redelivery should return false")
	}
	if err := productdb.FinishGitHubDelivery(db, "d1", "processed", "", 3); err != nil {
		t.Fatalf("FinishGitHubDelivery: %v", err)
	}
	// flowdb's health reader sees the productdb-written + finished delivery.
	h, err := flowdb.GitHubWebhookHealth(db)
	if err != nil {
		t.Fatalf("GitHubWebhookHealth: %v", err)
	}
	if h.Total != 1 || h.LastStatus != "processed" {
		t.Errorf("delivery state mismatch: total=%d status=%q", h.Total, h.LastStatus)
	}
}

func TestSteeringWatermarkParity(t *testing.T) {
	db := openProduct(t)
	if err := productdb.SetSteeringWatermark(db, "C1", "111.222", flowdb.NowISO()); err != nil {
		t.Fatalf("productdb.SetSteeringWatermark: %v", err)
	}
	// productdb write → flowdb read parity.
	got, err := flowdb.GetSteeringWatermark(db, "C1")
	if err != nil || got != "111.222" {
		t.Errorf("flowdb.GetSteeringWatermark = %q (err %v), want 111.222", got, err)
	}
	// upsert via flowdb → productdb read parity.
	if err := flowdb.SetSteeringWatermark(db, "C1", "999.000", flowdb.NowISO()); err != nil {
		t.Fatalf("flowdb.SetSteeringWatermark: %v", err)
	}
	if got, _ := productdb.GetSteeringWatermark(db, "C1"); got != "999.000" {
		t.Errorf("productdb.GetSteeringWatermark = %q, want 999.000", got)
	}
	// unknown channel → "".
	if got, err := productdb.GetSteeringWatermark(db, "nope"); err != nil || got != "" {
		t.Errorf("unknown channel = %q (err %v), want empty", got, err)
	}
}

func TestThreadCursorsParity(t *testing.T) {
	db := openProduct(t)
	now := flowdb.NowISO()
	for _, r := range []struct{ key, ts string }{{"slack:C1:1.1", "10.0"}, {"slack:C2:2.2", "20.0"}} {
		if _, err := db.Exec(`INSERT INTO attention_thread_state (thread_key,source,last_seen_ts,first_seen_at,updated_at) VALUES (?,?,?,?,?)`,
			r.key, "slack", r.ts, now, now); err != nil {
			t.Fatalf("seed thread state: %v", err)
		}
	}
	want, err := flowdb.ListRecentSlackThreadCursors(db, 10)
	if err != nil {
		t.Fatalf("flowdb.ListRecentSlackThreadCursors: %v", err)
	}
	got, err := productdb.ListRecentSlackThreadCursors(db, 10)
	if err != nil {
		t.Fatalf("productdb.ListRecentSlackThreadCursors: %v", err)
	}
	if len(want) != len(got) {
		t.Fatalf("cursor count mismatch: flowdb=%d productdb=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].ThreadKey != got[i].ThreadKey || want[i].LastSeenTS != got[i].LastSeenTS {
			t.Errorf("cursor %d mismatch: flowdb=%+v productdb=%+v", i, want[i], got[i])
		}
	}
}
