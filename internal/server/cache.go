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

func (c *ttlCache[K, V]) reset() {
	c.mu.Lock()
	c.items = map[K]ttlEntry[V]{}
	c.mu.Unlock()
}

// liveSnapshotCache memoizes the agent-session `ps` scan. Multiple call sites
// inside one SSE tick (buildUIData, BuildTaskViews, uiAgent dependents) all
// hit the same cached value instead of each forking their own `ps`.
type liveSnapshotCache struct {
	ttl time.Duration
	mu  sync.Mutex
	val atomic.Pointer[liveSnapshotEntry]
}

type liveSnapshotEntry struct {
	sessions  map[string]bool
	err       error
	expiresAt time.Time
}

func newLiveSnapshotCache(ttl time.Duration) *liveSnapshotCache {
	return &liveSnapshotCache{ttl: ttl}
}

func (c *liveSnapshotCache) load(now time.Time, refresh func() (map[string]bool, error)) (map[string]bool, error) {
	// Fast path: lock-free read of the atomic pointer.
	if entry := c.val.Load(); entry != nil && now.Before(entry.expiresAt) {
		return entry.sessions, entry.err
	}
	// Slow path: serialize refreshes so we don't fork N parallel `ps` calls
	// when several handlers race after expiry.
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.val.Load(); entry != nil && now.Before(entry.expiresAt) {
		return entry.sessions, entry.err
	}
	sessions, err := refresh()
	c.val.Store(&liveSnapshotEntry{
		sessions:  sessions,
		err:       err,
		expiresAt: now.Add(c.ttl),
	})
	return sessions, err
}

func (c *liveSnapshotCache) reset() {
	c.val.Store(nil)
}

// uiCaches bundles per-server TTL caches for the read-mostly data that
// buildUIData would otherwise recompute (and re-fork) for every task on every
// SSE tick. Per-server means each `New(Config{})` in tests gets a fresh cache
// so test isolation is preserved.
type uiCaches struct {
	live        *liveSnapshotCache
	gitBranch   *ttlCache[string, string]
	gitBranches *ttlCache[string, []string]
	gitDiff     *ttlCache[string, gitDiffSnapshot]
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
		live: newLiveSnapshotCache(1500 * time.Millisecond),
		// Branches and diffs change on user action (switch-branch,
		// commits, edits) but rarely within one UI refresh. 5s TTL means
		// a freshly-switched branch shows within the next two SSE ticks
		// without exception; explicit invalidate on switch-branch makes
		// it instant.
		gitBranch:   newTTLCache[string, string](5 * time.Second),
		gitBranches: newTTLCache[string, []string](5 * time.Second),
		gitDiff:     newTTLCache[string, gitDiffSnapshot](5 * time.Second),
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
