package productdb_test

// Tests for productdb.Open — flowwyyy's two-binary DB entry point. The
// load-bearing invariants: it creates every Bucket-F table (6 core-gap + 13
// product) AND deliberately does NOT create any Bucket-O table (those are owned
// by official flow's `flow init`), it is idempotent, and the search_docs FTS
// triggers it declares actually route inserts to the FTS index.

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func tableSet(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

func TestOpenCreatesBucketFNotBucketO(t *testing.T) {
	db, err := productdb.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tables := tableSet(t, db)

	// Bucket F — flowwyyy owns these; Open must create them.
	bucketF := []string{
		// 6 core-gap
		"brain_runs", "task_dependencies", "task_links", "agent_runtime_states",
		"pending_wakes", "search_docs", "search_docs_fts", "search_docs_tx_fts",
		// 13 product
		"attention_feed", "attention_feedback", "attention_handoffs", "attention_thread_state",
		"steering_trace", "steering_mutes", "steering_watermark", "chats", "kb_capture",
		"remote_devices", "pending_sends", "github_event_log", "github_webhook_deliveries",
	}
	var missing []string
	for _, tbl := range bucketF {
		if !tables[tbl] {
			missing = append(missing, tbl)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Open did not create Bucket-F tables: %v", missing)
	}

	// Bucket O — official flow owns these; Open must NOT declare them.
	for _, tbl := range []string{"tasks", "projects", "playbooks", "owners", "workdirs", "task_tags", "schema_meta"} {
		if tables[tbl] {
			t.Errorf("Open created Bucket-O table %q — official flow owns it, flowwyyy must not re-declare it", tbl)
		}
	}
}

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")
	db1, err := productdb.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()
	db2, err := productdb.Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotency): %v", err)
	}
	db2.Close()
}

func TestOpenSearchDocsFTSWired(t *testing.T) {
	db, err := productdb.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Insert a non-transcript doc → the AFTER INSERT trigger must mirror it into
	// search_docs_fts (proving the trigger + FTS table were declared correctly).
	if _, err := db.Exec(
		`INSERT INTO search_docs (doc_key, scope, entity_type, entity_slug, title, source_path, source_mtime, content, updated_at)
		 VALUES ('task/x/brief','brief','task','x','Ship the thing','/tmp/x','2026-06-24T00:00:00Z','deploy oauth rollout','2026-06-24T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert search_doc: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM search_docs_fts WHERE search_docs_fts MATCH 'oauth'`).Scan(&n); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if n != 1 {
		t.Errorf("search_docs_fts match for 'oauth' = %d, want 1 (trigger did not mirror the row)", n)
	}
}

// TestCoreGapSchemaParity proves productdb.Open declares the 6 core-gap tables
// with the SAME column structure flowdb does, so whichever binary creates them
// first, the shared flow.db schema is identical. Compares PRAGMA table_info
// (name+type+notnull+pk) — robust to cosmetic DDL whitespace differences.
func TestCoreGapSchemaParity(t *testing.T) {
	fdb, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("flowdb.OpenDB: %v", err)
	}
	defer fdb.Close()
	pdb, err := productdb.Open(filepath.Join(t.TempDir(), "prod.db"))
	if err != nil {
		t.Fatalf("productdb.Open: %v", err)
	}
	defer pdb.Close()

	for _, tbl := range []string{"brain_runs", "task_dependencies", "task_links", "agent_runtime_states", "pending_wakes", "search_docs"} {
		f := tableInfo(t, fdb, tbl)
		p := tableInfo(t, pdb, tbl)
		if f != p {
			t.Errorf("core-gap table %q schema diverged:\n flowdb=%s\n productdb=%s", tbl, f, p)
		}
	}
}

func tableInfo(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name+":"+ctype+":nn"+itoa(notnull)+":pk"+itoa(pk))
	}
	sort.Strings(cols)
	return joinCols(cols)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

func joinCols(cols []string) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += "|"
		}
		out += c
	}
	return out
}
