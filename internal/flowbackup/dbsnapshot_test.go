package flowbackup

import (
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestSnapshotDBExcludesFTS builds a db with a regenerable search index plus
// real metadata, snapshots it, and asserts the snapshot drops search_docs* but
// keeps the metadata and is openable.
func TestSnapshotDBExcludesFTS(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "flow.db")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustExec(t, db, `CREATE TABLE tasks (slug TEXT PRIMARY KEY, name TEXT)`)
	mustExec(t, db, `INSERT INTO tasks VALUES ('kb-backup-safety','data safety')`)
	mustExec(t, db, `CREATE TABLE search_docs (doc_key TEXT, title TEXT, content TEXT)`)
	mustExec(t, db, `INSERT INTO search_docs VALUES ('t/x','title','lots of indexed content')`)
	// Best-effort FTS5 virtual table; skip the assertion if the build lacks it.
	hasFTS := false
	if _, err := db.Exec(`CREATE VIRTUAL TABLE search_docs_fts USING fts5(title, content)`); err == nil {
		hasFTS = true
		mustExec(t, db, `INSERT INTO search_docs_fts VALUES ('a','b')`)
	}
	db.Close()

	snapPath, err := SnapshotDB(root)
	if err != nil {
		t.Fatalf("SnapshotDB: %v", err)
	}
	if snapPath == "" {
		t.Fatal("expected a snapshot path")
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}

	// Decompress and open the snapshot.
	restored := filepath.Join(t.TempDir(), "restored.db")
	gunzip(t, snapPath, restored)
	sdb, err := sql.Open("sqlite", "file:"+restored)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer sdb.Close()

	// Metadata survives.
	var name string
	if err := sdb.QueryRow(`SELECT name FROM tasks WHERE slug='kb-backup-safety'`).Scan(&name); err != nil {
		t.Fatalf("metadata missing from snapshot: %v", err)
	}
	if name != "data safety" {
		t.Fatalf("metadata = %q, want %q", name, "data safety")
	}

	// FTS is gone.
	if tableExists(sdb, "search_docs") {
		t.Fatal("search_docs should be excluded from the snapshot")
	}
	if hasFTS && tableExists(sdb, "search_docs_fts") {
		t.Fatal("search_docs_fts should be excluded from the snapshot")
	}
}

func TestSnapshotDBNoDB(t *testing.T) {
	root := t.TempDir()
	p, err := SnapshotDB(root)
	if err != nil {
		t.Fatalf("SnapshotDB with no db: %v", err)
	}
	if p != "" {
		t.Fatalf("expected empty path when no flow.db, got %q", p)
	}
}

func TestRotateDBSnapshots(t *testing.T) {
	root := t.TempDir()
	dir := dbSnapshotDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"flow-20260101T000000Z.db.gz",
		"flow-20260102T000000Z.db.gz",
		"flow-20260103T000000Z.db.gz",
		"flow-20260104T000000Z.db.gz",
	}
	for _, n := range names {
		write(t, filepath.Join(dir, n), "x")
	}
	rotateDBSnapshots(root, 2)
	left := listDBSnapshots(root)
	if len(left) != 2 {
		t.Fatalf("expected 2 snapshots after rotation, got %d", len(left))
	}
	// The two newest must remain.
	if filepath.Base(left[0]) != "flow-20260104T000000Z.db.gz" {
		t.Fatalf("newest snapshot wrong: %s", left[0])
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}

func gunzip(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open gz: %v", err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatalf("gunzip copy: %v", err)
	}
}
