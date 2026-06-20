package flowbackup

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v5"
	_ "modernc.org/sqlite"
)

// TestRemoteRoundTrip exercises the full offsite + new-laptop restore flow
// against a LOCAL bare remote — hermetic, no network or auth. It mirrors:
// configure remote → push markdown + db snapshot → clone into a fresh root →
// fetch + decompress the db snapshot.
func TestRemoteRoundTrip(t *testing.T) {
	// Source install.
	src := t.TempDir()
	write(t, filepath.Join(src, "kb", "org.md"), "# org\n\n- durable fact\n")
	write(t, filepath.Join(src, "tasks", "t", "brief.md"), "# task brief\n")
	if _, err := Checkpoint(src, "seed"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// A tiny db to snapshot.
	makeTestDB(t, filepath.Join(src, "flow.db"))
	snap, err := SnapshotDB(src)
	if err != nil || snap == "" {
		t.Fatalf("SnapshotDB: snap=%q err=%v", snap, err)
	}

	// Bare remote (acts like a private GitHub repo, but on local disk).
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	if err := SetRemote(src, bare); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if !RemoteConfigured(src) || RemoteURL(src) != bare {
		t.Fatalf("remote not configured correctly: %q", RemoteURL(src))
	}
	if err := Push(src); err != nil {
		t.Fatalf("Push markdown: %v", err)
	}
	if err := PushDBSnapshot(src, snap); err != nil {
		t.Fatalf("PushDBSnapshot: %v", err)
	}

	// New laptop: clone into a fresh root.
	dst := t.TempDir()
	if err := Clone(dst, bare); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// No .git link must be left at the restored root.
	if _, err := os.Lstat(filepath.Join(dst, ".git")); err == nil {
		t.Fatal("clone left a .git link at the restored root")
	}
	// Markdown materialized.
	got, err := os.ReadFile(filepath.Join(dst, "kb", "org.md"))
	if err != nil || string(got) != "# org\n\n- durable fact\n" {
		t.Fatalf("kb/org.md not restored: got=%q err=%v", string(got), err)
	}
	if _, err := os.Stat(filepath.Join(dst, "tasks", "t", "brief.md")); err != nil {
		t.Fatalf("task brief not restored: %v", err)
	}

	// DB snapshot fetched + decompresses to a valid sqlite db.
	gz, err := FetchDBSnapshotBytes(dst)
	if err != nil {
		t.Fatalf("FetchDBSnapshotBytes: %v", err)
	}
	if len(gz) == 0 {
		t.Fatal("expected db snapshot bytes from remote")
	}
	dbPath := filepath.Join(t.TempDir(), "restored.db")
	if err := gunzipBytes(t, gz, dbPath); err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	rdb, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer rdb.Close()
	var n string
	if err := rdb.QueryRow(`SELECT name FROM tasks WHERE slug='x'`).Scan(&n); err != nil {
		t.Fatalf("restored db missing metadata: %v", err)
	}
	if n != "demo" {
		t.Fatalf("restored metadata = %q, want demo", n)
	}
}

func TestClearRemote(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(t.TempDir(), "r.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if err := SetRemote(root, bare); err != nil {
		t.Fatal(err)
	}
	if !RemoteConfigured(root) {
		t.Fatal("expected remote configured")
	}
	if err := ClearRemote(root); err != nil {
		t.Fatal(err)
	}
	if RemoteConfigured(root) {
		t.Fatal("expected remote cleared")
	}
}

func makeTestDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tasks (slug TEXT, name TEXT)`)
	mustExec(t, db, `INSERT INTO tasks VALUES ('x','demo')`)
	mustExec(t, db, `CREATE TABLE search_docs (doc_key TEXT, content TEXT)`)
	mustExec(t, db, `INSERT INTO search_docs VALUES ('k','big content')`)
}

func gunzipBytes(t *testing.T, gz []byte, dst string) error {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, zr)
	return err
}
