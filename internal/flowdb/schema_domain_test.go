package flowdb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func tableExists(db *sql.DB, name string) bool {
	var n string
	return db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n) == nil
}

// TestCoreSchemaOmitsProductTables asserts the core schema seam: a DB opened
// with NO product migration set registered creates the core tables but none of
// the product (flowwyyy) tables. This is the invariant that lets the future
// core `flow` binary — which never imports internal/productdb — run on a
// core-only DB. The flowdb test binary normally has the product set registered
// (via the external-test blank import in db_export_test.go), so this test
// temporarily clears registeredSets to simulate a core-only binary.
func TestCoreSchemaOmitsProductTables(t *testing.T) {
	saved := registeredSets
	registeredSets = nil
	defer func() { registeredSets = saved }()

	db, err := OpenDB(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	for _, core := range []string{"projects", "tasks", "playbooks", "owners", "search_docs", "brain_runs"} {
		if !tableExists(db, core) {
			t.Errorf("core table %q missing from core-only DB", core)
		}
	}
	for _, product := range []string{"attention_feed", "steering_trace", "chats", "github_event_log", "remote_devices"} {
		if tableExists(db, product) {
			t.Errorf("product table %q present in core-only DB (should be omitted)", product)
		}
	}
}
