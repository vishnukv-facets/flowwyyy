package server

import (
	"sync"
	"sync/atomic"
	"time"
)

// ttlCache is a small goroutine-safe TTL cache. Entries are checked against
// the TTL on every read; there's no background sweeper because the key spaces
// in this package are bounded (task slugs, workdir paths).
type ttlCache[K comparable, V any] struct {
	ttl   time.Duration
	mu    sync.Mutex
	items map[K]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value     V
	expiresAt time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, items: map[K]ttlEntry[V]{}}
}

func (c *ttlCache[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expiresAt) {
		var zero V
		return zero, false
	}
	return e.value, true
}

func (c *ttlCache[K, V]) set(key K, value V) {
	c.mu.Lock()
	c.items[key] = ttlEntry[V]{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *ttlCache[K, V]) invalidate(key K) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

// snapshotCache memoizes a single computed value for a TTL window and collapses
// concurrent rebuilds into one: an atomic-pointer fast path serves a fresh value
// lock-free, while a mutex serializes refreshes so a request storm after expiry
// forks only one rebuild. Used for the agent-session `ps` scan
// (snapshotCache[map[string]bool]) and the full buildUIData result
// (snapshotCache[uiData]) — both previously had their own near-identical type.
// store() publishes a known-fresh value immediately (e.g. the broadcast path
// after a mutation) instead of waiting out the TTL.
type snapshotCache[V any] struct {
	ttl time.Duration
	mu  sync.Mutex
	val atomic.Pointer[snapshotEntry[V]]
}

type snapshotEntry[V any] struct {
	value     V
	err       error
	expiresAt time.Time
}

func newSnapshotCache[V any](ttl time.Duration) *snapshotCache[V] {
	return &snapshotCache[V]{ttl: ttl}
}

func (c *snapshotCache[V]) load(now time.Time, refresh func() (V, error)) (V, error) {
	// Fast path: lock-free read of the atomic pointer.
	if e := c.val.Load(); e != nil && now.Before(e.expiresAt) {
		return e.value, e.err
	}
	// Slow path: serialize refreshes so a race after expiry forks only once.
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.val.Load(); e != nil && now.Before(e.expiresAt) {
		return e.value, e.err
	}
	v, err := refresh()
	c.val.Store(&snapshotEntry[V]{value: v, err: err, expiresAt: now.Add(c.ttl)})
	return v, err
}

// store records a freshly-built value (broadcast path serves a just-mutated
// state immediately rather than waiting out the TTL).
func (c *snapshotCache[V]) store(now time.Time, value V, err error) {
	c.val.Store(&snapshotEntry[V]{value: value, err: err, expiresAt: now.Add(c.ttl)})
}

// uiCaches bundles per-server TTL caches for the read-mostly data that
// buildUIData would otherwise recompute (and re-fork) for every task on every
// SSE tick. Per-server means each `New(Config{})` in tests gets a fresh cache
// so test isolation is preserved.
type uiCaches struct {
	live        *snapshotCache[map[string]bool]
	gitBranch   *ttlCache[string, string]
	gitBranches *ttlCache[string, []string]
	gitDiff     *ttlCache[string, gitDiffSnapshot]
	flowDBDiag  *flowDBDiagCache
	uiData      *snapshotCache[uiData]
	// autoAlive caches Signal(0) liveness probes for auto-run supervisor pids.
	// TTL 15s matches U2: only rows with stored status='running' are probed.
	autoAlive *ttlCache[int, bool]
}

type gitDiffSnapshot struct {
	diff  uiDiff
	files []uiDiffFile
}

func newUICaches() *uiCaches {
	return &uiCaches{
		// `ps` scan changes when sessions come or go — refreshing it every
		// 1.5s keeps the live indicator within a tick of reality while
		// collapsing concurrent buildUIData / BuildTaskViews calls into one
		// fork per window.
		live: newSnapshotCache[map[string]bool](1500 * time.Millisecond),
		// Branches and diffs change on user action (switch-branch,
		// commits, edits) but rarely within one UI refresh. The 30s TTL
		// is a safety net — explicit invalidate on switch-branch makes
		// updates instant; the TTL just bounds staleness if an action
		// path forgets to invalidate. Kept generous to minimize git
		// fork()/wait4() pressure on the SSE refresh loop.
		gitBranch:   newTTLCache[string, string](30 * time.Second),
		gitBranches: newTTLCache[string, []string](30 * time.Second),
		gitDiff:     newTTLCache[string, gitDiffSnapshot](30 * time.Second),
		// flow.db diagnostics full-scan the database (quick_check, dbstat,
		// search_docs). They cannot run per SSE tick; serve them stale and
		// refresh in the background. See flowDBDiagCache.
		flowDBDiag: newFlowDBDiagCache(flowDBDiagCacheTTL, defaultFlowDBQuickCheckTimeout, flowDBDiagQuickCheckTimeout),
		// Full ui-data rebuilds are requested on every change notification and
		// from every open tab; collapse concurrent rebuilds and serve a snapshot
		// up to one broadcast-debounce-window (250ms) old.
		uiData: newSnapshotCache[uiData](250 * time.Millisecond),
		// Auto-run pid liveness: 15s TTL, probes only stored-'running' rows.
		autoAlive: newTTLCache[int, bool](15 * time.Second),
	}
}

func (c *uiCaches) invalidateWorkdir(dir string) {
	if c == nil || dir == "" {
		return
	}
	c.gitBranch.invalidate(dir)
	c.gitDiff.invalidate(dir)
	// gitBranches is keyed by dir+current branch, so wipe by prefix.
	c.gitBranches.mu.Lock()
	for k := range c.gitBranches.items {
		if len(k) >= len(dir) && k[:len(dir)] == dir {
			delete(c.gitBranches.items, k)
		}
	}
	c.gitBranches.mu.Unlock()
}

// flowDBDiagCache serves flow.db storage diagnostics on the hot SSE path
// without ever running their O(database size) scans there. A warm entry is
// returned instantly (lock-free); a stale entry is served as-is while ONE
// background goroutine refreshes it; only a cold cache computes synchronously
// — that case (first paint, or just after compact invalidation) is exactly
// when a caller needs authoritative numbers anyway. A generation counter
// discards a refresh that a concurrent invalidate has obsoleted (e.g. a
// pre-VACUUM scan landing after compact). quickShort/quickLong split the
// integrity-check budget: short when blocking a synchronous caller, generous
// in the background so a large database actually finishes verifying.
type flowDBDiagCache struct {
	ttl        time.Duration
	quickShort time.Duration
	quickLong  time.Duration

	mu         sync.Mutex // guards gen + val writes; val reads are lock-free
	gen        uint64
	val        atomic.Pointer[flowDBDiagEntry]
	refreshing atomic.Bool
}

type flowDBDiagEntry struct {
	diag      flowDBDiag
	path      string
	expiresAt time.Time
}

func newFlowDBDiagCache(ttl, quickShort, quickLong time.Duration) *flowDBDiagCache {
	return &flowDBDiagCache{ttl: ttl, quickShort: quickShort, quickLong: quickLong}
}

// load returns diagnostics for path. compute(quickTimeout) runs the actual
// scans; the cache decides which integrity budget to pass.
func (c *flowDBDiagCache) load(path string, now time.Time, compute func(time.Duration) flowDBDiag) flowDBDiag {
	if e := c.val.Load(); e != nil && e.path == path {
		if now.Before(e.expiresAt) {
			return e.diag // warm — zero database work on the hot path
		}
		c.refreshAsync(path, compute) // stale — serve now, rescan off-path
		return e.diag
	}
	// Cold: compute synchronously with the short integrity budget so we don't
	// stall the first paint. If integrity didn't finish ("not checked"), kick a
	// background refresh with the long budget so the badge self-heals to "ok".
	gen := c.capturedGen()
	diag := compute(c.quickShort)
	c.store(path, diag, gen)
	if diag.QuickCheck != "ok" {
		c.refreshAsync(path, compute)
	}
	return diag
}

func (c *flowDBDiagCache) refreshAsync(path string, compute func(time.Duration) flowDBDiag) {
	if c == nil || !c.refreshing.CompareAndSwap(false, true) {
		return // a refresh is already in flight
	}
	gen := c.capturedGen()
	go func() {
		defer c.refreshing.Store(false)
		c.store(path, compute(c.quickLong), gen)
	}()
}

// computeFresh recomputes synchronously, bypassing any cached or in-flight
// value, and primes the cache. Used by compact, which must publish post-VACUUM
// numbers immediately.
func (c *flowDBDiagCache) computeFresh(path string, compute func(time.Duration) flowDBDiag) flowDBDiag {
	c.invalidate()
	gen := c.capturedGen()
	diag := compute(c.quickShort)
	c.store(path, diag, gen)
	return diag
}

func (c *flowDBDiagCache) capturedGen() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *flowDBDiagCache) store(path string, diag flowDBDiag, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return // invalidated mid-compute — drop this now-stale result
	}
	c.val.Store(&flowDBDiagEntry{diag: diag, path: path, expiresAt: time.Now().Add(c.ttl)})
}

func (c *flowDBDiagCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.val.Store(nil)
}
