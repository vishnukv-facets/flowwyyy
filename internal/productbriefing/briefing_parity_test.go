package productbriefing_test

// Cross-layer parity: productbriefing (productdb-backed) must produce the EXACT
// same Briefing as core briefing (flowdb-backed) against the same shared DB.
// productbriefing is a port of internal/briefing with flowdb→productdb swapped;
// this pins that the port stays faithful as either side evolves. Seed via flowdb
// (the core writer), build both, compare the JSON-equivalent output.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"flow/internal/briefing"
	"flow/internal/flowdb"
	"flow/internal/productbriefing"
	"flow/internal/productdb"

	_ "flow/internal/productdbreg" // register product tables for flowdb.OpenDB
)

func TestBriefingParity(t *testing.T) {
	root := t.TempDir()
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)
	yesterday := now.Add(-20 * time.Hour).Format(time.RFC3339)

	// A high-priority startable backlog task (NextUp), a waiting task (NeedsYou),
	// and a recently-shipped task (Overnight) — exercises all three tiers.
	mustExec(t, db, `INSERT INTO projects (slug,name,status,priority,work_dir,created_at,updated_at) VALUES ('proj','Proj','active','high','/tmp/proj',?,?)`, yesterday, yesterday)
	mustExec(t, db, `INSERT INTO tasks (slug,name,project_slug,status,kind,priority,work_dir,permission_mode,session_provider,created_at,updated_at) VALUES ('ready','Ready task','proj','backlog','regular','high','/tmp/proj','auto','claude',?,?)`, yesterday, yesterday)
	mustExec(t, db, `INSERT INTO tasks (slug,name,project_slug,status,kind,priority,work_dir,permission_mode,session_provider,waiting_on,created_at,updated_at) VALUES ('blocked','Blocked task','proj','backlog','regular','medium','/tmp/proj','auto','claude','review from Sam',?,?)`, yesterday, yesterday)
	shipped := now.Add(-2 * time.Hour).Format(time.RFC3339)
	mustExec(t, db, `INSERT INTO tasks (slug,name,project_slug,status,kind,priority,work_dir,permission_mode,session_provider,created_at,updated_at,status_changed_at) VALUES ('done1','Shipped task','proj','done','regular','medium','/tmp/proj','auto','claude',?,?,?)`, yesterday, shipped, shipped)

	// A new attention feed item (Tier-1 NeedsYou) — a product table.
	if _, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
		ID: "f1", Source: "slack", ThreadKey: "C1:1.1", Summary: "needs a decision",
		SuggestedAction: "make_task", Status: "new", Urgency: "high", CreatedAt: yesterday, TS: "1.1",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	opts := briefing.Options{Now: now, Since: now.Add(-24 * time.Hour), Limit: 20}
	core, err := briefing.Build(db, root, opts)
	if err != nil {
		t.Fatalf("briefing.Build: %v", err)
	}
	prod, err := productbriefing.Build(db, root, productbriefing.Options{Now: opts.Now, Since: opts.Since, Limit: opts.Limit})
	if err != nil {
		t.Fatalf("productbriefing.Build: %v", err)
	}

	if cj, pj := mustJSON(t, core), mustJSON(t, prod); cj != pj {
		t.Errorf("briefing parity mismatch:\n core=%s\n prod=%s", cj, pj)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("seed exec: %v\n  query: %s", err, q)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
