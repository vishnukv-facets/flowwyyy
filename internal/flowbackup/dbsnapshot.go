package flowbackup

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (registered as "sqlite")
)

// defaultDBKeep is how many db snapshots are retained by rotation.
const defaultDBKeep = 14

// dbSnapshotDir returns <root>/backups/db, where rotated db snapshots live.
func dbSnapshotDir(root string) string { return filepath.Join(root, "backups", "db") }

func dbKeep() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_BACKUP_DB_KEEP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultDBKeep
}

// SnapshotDB produces a compact, consistent backup of flow.db with the
// regenerable full-text-search index excluded, gzips it under
// <root>/backups/db/flow-<RFC3339>.db.gz, rotates old snapshots, and returns the
// new snapshot path.
//
// Why exclude FTS: ~438MB of the live 476MB db is the `search_docs*` family —
// derived data rebuilt from briefs/updates/memories/transcripts by the indexer.
// Dropping it in the *copy* (the live db is never touched) yields a ~30-40MB
// snapshot that still holds every source-of-truth table. On restore the index
// is rebuilt locally.
//
// Returns ("", nil) when there is no flow.db to snapshot.
func SnapshotDB(root string) (string, error) {
	if !Enabled() {
		return "", nil
	}
	dbPath := filepath.Join(root, "flow.db")
	if _, err := os.Stat(dbPath); err != nil {
		return "", nil // nothing to back up yet
	}
	if err := os.MkdirAll(dbSnapshotDir(root), 0o755); err != nil {
		return "", fmt.Errorf("flowbackup: mkdir db snapshot dir: %w", err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	rawPath := filepath.Join(dbSnapshotDir(root), ".snap-"+ts+".db")
	_ = os.Remove(rawPath) // VACUUM INTO requires the target not exist

	// 1. Consistent online copy via VACUUM INTO (read-only w.r.t. the live db).
	if err := vacuumInto(dbPath, rawPath); err != nil {
		return "", err
	}
	defer os.Remove(rawPath)

	// 2. Strip the regenerable FTS from the copy and compact it.
	if err := stripFTS(rawPath); err != nil {
		return "", err
	}

	// 3. Gzip to the final snapshot path.
	finalPath := filepath.Join(dbSnapshotDir(root), "flow-"+ts+".db.gz")
	if err := gzipFile(rawPath, finalPath); err != nil {
		return "", err
	}

	rotateDBSnapshots(root, dbKeep())
	return finalPath, nil
}

// vacuumInto runs `VACUUM INTO <dst>` against the database at src. The path is
// embedded as an escaped SQL string literal (VACUUM INTO does not accept bound
// parameters for the filename on all backends).
func vacuumInto(src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("flowbackup: open db for snapshot: %w", err)
	}
	defer db.Close()
	lit := "'" + strings.ReplaceAll(dst, "'", "''") + "'"
	if _, err := db.Exec("VACUUM INTO " + lit); err != nil {
		return fmt.Errorf("flowbackup: VACUUM INTO: %w", err)
	}
	return nil
}

// stripFTS drops the regenerable search index from a snapshot copy and compacts
// it. Operates on the COPY only — never the live db. Triggers are dropped first,
// then the FTS virtual tables (which take their shadow tables with them), then
// the search_docs content table.
func stripFTS(path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("flowbackup: open snapshot to strip FTS: %w", err)
	}
	defer db.Close()

	// Triggers first.
	if err := dropMatching(db, "trigger", "SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'search_docs%'"); err != nil {
		return err
	}
	// FTS virtual tables (CREATE VIRTUAL TABLE ...) — dropping these removes
	// their shadow tables automatically.
	if err := dropMatching(db, "table", "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'search_docs%' AND sql LIKE 'CREATE VIRTUAL TABLE%'"); err != nil {
		return err
	}
	// The plain content table, if it survives independently.
	if _, err := db.Exec("DROP TABLE IF EXISTS search_docs"); err != nil {
		return fmt.Errorf("flowbackup: drop search_docs: %w", err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("flowbackup: vacuum snapshot: %w", err)
	}
	return nil
}

// dropMatching drops every object whose name is returned by the query.
func dropMatching(db *sql.DB, kind, query string) error {
	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("flowbackup: list %ss: %w", kind, err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := db.Exec(fmt.Sprintf("DROP %s IF EXISTS %q", strings.ToUpper(kind), n)); err != nil {
			return fmt.Errorf("flowbackup: drop %s %s: %w", kind, n, err)
		}
	}
	return nil
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("flowbackup: open raw snapshot: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("flowbackup: create gz snapshot: %w", err)
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		return fmt.Errorf("flowbackup: gzip snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("flowbackup: close gzip: %w", err)
	}
	return nil
}

// rotateDBSnapshots keeps only the most recent keep snapshots, deleting older
// flow-*.db.gz files. Best-effort.
func rotateDBSnapshots(root string, keep int) {
	snaps := listDBSnapshots(root)
	if len(snaps) <= keep {
		return
	}
	for _, p := range snaps[keep:] {
		_ = os.Remove(p)
	}
}

// listDBSnapshots returns snapshot paths most-recent-first (lexical sort on the
// timestamped name is chronological).
func listDBSnapshots(root string) []string {
	entries, err := os.ReadDir(dbSnapshotDir(root))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "flow-") && strings.HasSuffix(e.Name(), ".db.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(dbSnapshotDir(root), n)
	}
	return out
}

// LatestDBSnapshot returns the newest db snapshot path, or "" if none exist.
func LatestDBSnapshot(root string) string {
	snaps := listDBSnapshots(root)
	if len(snaps) == 0 {
		return ""
	}
	return snaps[0]
}

// DBSnapshotCount returns how many rotated db snapshots are on disk.
func DBSnapshotCount(root string) int { return len(listDBSnapshots(root)) }
