package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// uiFlowDB reports the on-disk size of flow.db plus cached storage
// diagnostics. Missing-file is not an error: the sidebar just shows "—" until
// `flow init` runs. The expensive diagnostics come from the hot-path cache
// (stale-while-revalidate), so this never full-scans the database on an SSE
// tick — the property that keeps `flow ui serve` off the CPU.
func (s *Server) uiFlowDB() uiFlowDB {
	out := s.statFlowDB()
	if out.Exists {
		applyFlowDBDiag(&out, s.cachedFlowDBDiag(out.Path, out.Bytes))
	}
	return out
}

// uiFlowDBFresh is uiFlowDB with an authoritative, synchronous diagnostics
// recompute that also primes the cache. Used by compact, which must read and
// publish post-VACUUM numbers immediately even if a background refresh races.
func (s *Server) uiFlowDBFresh() uiFlowDB {
	out := s.statFlowDB()
	if out.Exists {
		applyFlowDBDiag(&out, s.freshFlowDBDiag(out.Path, out.Bytes))
	}
	return out
}

// statFlowDB fills the cheap, always-available fields (path + on-disk size).
func (s *Server) statFlowDB() uiFlowDB {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return uiFlowDB{HumanSize: "—"}
	}
	path := filepath.Join(root, "flow.db")
	out := uiFlowDB{Path: path, DisplayPath: displayUIPath(path), HumanSize: "—"}
	info, err := os.Stat(path)
	if err != nil {
		return out
	}
	out.Exists = true
	out.Bytes = info.Size()
	out.HumanSize = humanByteSize(info.Size())
	return out
}

// applyFlowDBDiag copies cached diagnostics onto a stat-only uiFlowDB.
func applyFlowDBDiag(out *uiFlowDB, diag flowDBDiag) {
	out.PageSize = diag.PageSize
	out.PageCount = diag.PageCount
	out.FreePageCount = diag.FreePageCount
	out.ReclaimableBytes = diag.ReclaimableBytes
	out.UsedBytes = diag.UsedBytes
	out.UsedHumanSize = humanByteSize(diag.UsedBytes)
	out.ReclaimableHumanSize = humanByteSize(diag.ReclaimableBytes)
	out.CanCompact = diag.ReclaimableBytes > 0
	out.QuickCheck = diag.QuickCheck
	out.QuickCheckSource = diag.QuickCheckSource
	out.QuickCheckCheckedAt = diag.QuickCheckCheckedAt
	out.QuickCheckNote = diag.QuickCheckNote
	out.Objects = diag.Objects
	out.Documents = diag.Documents
	out.Error = diag.Error
	if diag.Error == "" {
		out.Explanation = "SQLite keeps deleted content as free pages inside flow.db until compaction; transcript full-text search can dominate storage because it stores searchable session text."
	}
}

// cachedFlowDBDiag serves diagnostics from the hot-path cache. A warm entry is
// returned with zero database work (a lock-free atomic read); a stale entry is
// served as-is while a single background goroutine rescans; only a cold cache
// computes synchronously. That is what keeps the SSE tick scan-free.
func (s *Server) cachedFlowDBDiag(path string, totalBytes int64) flowDBDiag {
	compute := func(quick time.Duration) flowDBDiag {
		return s.computeFlowDBDiag(path, totalBytes, quick)
	}
	if s == nil || s.caches == nil || path == "" {
		return compute(defaultFlowDBQuickCheckTimeout)
	}
	return s.caches.flowDBDiag.load(path, time.Now(), compute)
}

// freshFlowDBDiag recomputes diagnostics synchronously, bypassing any cached or
// in-flight value, and primes the cache with the result. Used by compact.
func (s *Server) freshFlowDBDiag(path string, totalBytes int64) flowDBDiag {
	compute := func(quick time.Duration) flowDBDiag {
		return s.computeFlowDBDiag(path, totalBytes, quick)
	}
	if s == nil || s.caches == nil || path == "" {
		return compute(defaultFlowDBQuickCheckTimeout)
	}
	return s.caches.flowDBDiag.computeFresh(path, compute)
}

// computeFlowDBDiag runs the O(database size) scans. Never call this on the hot
// path — go through cachedFlowDBDiag. quickTimeout bounds the integrity check:
// short on the synchronous first paint (don't stall the UI), generous on the
// background refresh so a large database actually finishes verifying.
func (s *Server) computeFlowDBDiag(path string, totalBytes int64, quickTimeout time.Duration) flowDBDiag {
	var diag flowDBDiag
	db, err := openFlowDBDiagnostic(path)
	if err != nil {
		diag.Error = err.Error()
		return diag
	}
	defer db.Close()
	pageSize, err := sqlitePragmaInt64(db, "page_size", 500*time.Millisecond)
	if err != nil {
		diag.Error = err.Error()
		return diag
	}
	pageCount, err := sqlitePragmaInt64(db, "page_count", 500*time.Millisecond)
	if err != nil {
		diag.Error = err.Error()
		return diag
	}
	freePageCount, err := sqlitePragmaInt64(db, "freelist_count", 500*time.Millisecond)
	if err != nil {
		diag.Error = err.Error()
		return diag
	}
	diag.PageSize = pageSize
	diag.PageCount = pageCount
	diag.FreePageCount = freePageCount
	diag.ReclaimableBytes = pageSize * freePageCount
	diag.UsedBytes = pageSize * (pageCount - freePageCount)
	if diag.UsedBytes < 0 {
		diag.UsedBytes = 0
	}
	// addFlowDBQuickCheck writes onto a uiFlowDB; use a scratch value and lift
	// the four quick-check fields out so its reuse logic stays intact.
	scratch := uiFlowDB{Path: path}
	s.addFlowDBQuickCheck(&scratch, db, quickTimeout)
	diag.QuickCheck = scratch.QuickCheck
	diag.QuickCheckSource = scratch.QuickCheckSource
	diag.QuickCheckCheckedAt = scratch.QuickCheckCheckedAt
	diag.QuickCheckNote = scratch.QuickCheckNote
	diag.Objects = sqliteTopObjects(db, totalBytes, 12, 2*time.Second)
	diag.Documents = sqliteSearchDocStats(db, time.Second)
	return diag
}

// addFlowDBQuickCheck fills the integrity fields. Integrity rarely changes, so
// a recent verified result is reused without rescanning the whole database;
// otherwise PRAGMA quick_check runs within quickTimeout. A "not checked" result
// means the scan didn't finish in budget — the background refresh re-runs it
// with the generous flowDBDiagQuickCheckTimeout, so the badge self-heals to a
// verified status without the user doing anything.
func (s *Server) addFlowDBQuickCheck(out *uiFlowDB, db *sql.DB, quickTimeout time.Duration) {
	now := time.Now()
	if cached, ok := s.recentFlowDBQuickCheck(out.Path, now); ok {
		out.QuickCheck = cached.Result
		out.QuickCheckSource = cached.Source
		out.QuickCheckCheckedAt = cached.CheckedAt.Format(time.RFC3339)
		out.QuickCheckNote = recentQuickCheckNote(cached.Source)
		return
	}
	timeout := quickTimeout
	if s.flowDBQuickCheckTimeout != 0 {
		timeout = s.flowDBQuickCheckTimeout
	}
	if timeout == 0 {
		timeout = defaultFlowDBQuickCheckTimeout
	}
	result := sqliteQuickCheck(db, timeout)
	out.QuickCheck = result
	switch result {
	case "ok":
		out.QuickCheckSource = "live"
		out.QuickCheckCheckedAt = now.Format(time.RFC3339)
		out.QuickCheckNote = "Live integrity check completed."
		s.rememberFlowDBQuickCheck(out.Path, result, "live", now)
	case "not checked":
		out.QuickCheckSource = "pending"
		out.QuickCheckNote = "Integrity check is running in the background; this refreshes to a verified result shortly."
	default:
		out.QuickCheckSource = "live"
		out.QuickCheckNote = "Live integrity check returned an error."
	}
}

func (s *Server) rememberFlowDBQuickCheck(path, result, source string, checkedAt time.Time) {
	if path == "" || result != "ok" || checkedAt.IsZero() {
		return
	}
	s.flowDBQuickCheckMu.Lock()
	defer s.flowDBQuickCheckMu.Unlock()
	s.flowDBQuickCheck = cachedFlowDBQuickCheck{
		Path:      path,
		Result:    result,
		Source:    source,
		CheckedAt: checkedAt,
	}
}

// recentFlowDBQuickCheck returns the last verified integrity result if it is
// still within flowDBQuickCheckCacheTTL, so callers can skip a fresh full-DB
// scan. Only "ok" results are ever remembered (see rememberFlowDBQuickCheck).
func (s *Server) recentFlowDBQuickCheck(path string, now time.Time) (cachedFlowDBQuickCheck, bool) {
	s.flowDBQuickCheckMu.Lock()
	cached := s.flowDBQuickCheck
	s.flowDBQuickCheckMu.Unlock()
	if cached.Path != path || cached.Result == "" || cached.CheckedAt.IsZero() {
		return cachedFlowDBQuickCheck{}, false
	}
	if now.Sub(cached.CheckedAt) > flowDBQuickCheckCacheTTL {
		return cachedFlowDBQuickCheck{}, false
	}
	return cached, true
}

func recentQuickCheckNote(source string) string {
	switch source {
	case "compact-precheck":
		return "Integrity was checked before compact; reused here without rescanning."
	default:
		return "Reusing a recent verified integrity check."
	}
}

func openFlowDBDiagnostic(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(100)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open diagnostics sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set diagnostics busy_timeout: %w", err)
	}
	return db, nil
}

func sqlitePragmaInt64(db *sql.DB, name string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var n int64
	if err := db.QueryRowContext(ctx, `PRAGMA `+name).Scan(&n); err != nil {
		return 0, fmt.Errorf("PRAGMA %s: %w", name, err)
	}
	return n, nil
}

func sqliteQuickCheck(db *sql.DB, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "not checked"
		}
		return "error: " + err.Error()
	}
	return result
}

func sqliteTopObjects(db *sql.DB, totalBytes int64, limit int, timeout time.Duration) []uiFlowDBObject {
	if limit <= 0 {
		limit = 12
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT d.name, COALESCE(s.type, 'internal') AS kind, COALESCE(SUM(d.pgsize), 0) AS bytes
		FROM dbstat d
		LEFT JOIN sqlite_schema s ON s.name = d.name
		GROUP BY d.name, kind
		ORDER BY bytes DESC, d.name ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []uiFlowDBObject{}
	for rows.Next() {
		var obj uiFlowDBObject
		if err := rows.Scan(&obj.Name, &obj.Kind, &obj.Bytes); err != nil {
			return out
		}
		obj.HumanSize = humanByteSize(obj.Bytes)
		if totalBytes > 0 {
			obj.Percent = float64(obj.Bytes) / float64(totalBytes) * 100
		}
		out = append(out, obj)
	}
	return out
}

func sqliteSearchDocStats(db *sql.DB, timeout time.Duration) []uiFlowDBDocStat {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT scope, entity_type, COUNT(*), COALESCE(SUM(LENGTH(content)), 0)
		FROM search_docs
		GROUP BY scope, entity_type
		ORDER BY COALESCE(SUM(LENGTH(content)), 0) DESC, scope ASC, entity_type ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []uiFlowDBDocStat{}
	for rows.Next() {
		var stat uiFlowDBDocStat
		if err := rows.Scan(&stat.Scope, &stat.EntityType, &stat.Count, &stat.ContentBytes); err != nil {
			return out
		}
		stat.HumanSize = humanByteSize(stat.ContentBytes)
		out = append(out, stat)
	}
	return out
}

func humanByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := "KMGTPE"[exp]
	if val >= 100 {
		return fmt.Sprintf("%.0f %ciB", val, suffix)
	}
	return fmt.Sprintf("%.1f %ciB", val, suffix)
}
