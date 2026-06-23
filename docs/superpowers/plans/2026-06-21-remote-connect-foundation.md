# Remote Connect — Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator reach Mission Control and drive their live Claude/Codex sessions from a phone over the existing zrok public ingress, gated by per-device tokens that expire 12h after a QR pairing — with the GitHub webhook ingress untouched.

**Architecture:** A composite public handler serves the unchanged GitHub-webhook/OAuth mux *plus* a separate, device-token-gated remote-app mux on the same zrok share, only when remote access is enabled. The remote-app mux serves the existing static PWA bundle and the existing `/ws/*` handlers behind a single new middleware (`remoteAuth`) that validates a device token and swaps in the shared session token so every existing terminal/RPC/events handler works unchanged. Device tokens live in a new `remote_devices` table; pairing codes are short-lived in-memory.

**Tech Stack:** Go (`net/http`, `crypto/rand`, `crypto/sha256`, `modernc.org/sqlite`), React + TypeScript + Vite (xterm.js), zrok SDK.

## Global Constraints

- **No CGO.** Pure Go SQLite driver (`modernc.org/sqlite`) only.
- **Timestamps:** RFC3339 strings everywhere (`flowdb.NowISO()`); never Unix ints.
- **Flag parsing:** `flag.FlagSet` with `ContinueOnError` via `flagSet()` if any CLI surface is touched.
- **Tests:** table-driven; real SQLite in a temp dir (no DB mocks); external processes mocked via package function vars. `make test` must stay fast (no network, no sleeps — inject `now time.Time` for time-dependent logic).
- **Device-token TTL:** exactly `12 * time.Hour`. **Pairing-code TTL:** `5 * time.Minute`.
- **Fail closed:** any missing/malformed/expired/revoked credential is rejected.
- **The shared session token must NEVER be sent to a remote client.** Remotely-served HTML must not embed `window.__FLOW_TOKEN__`.
- **Rebuild after editing the embedded skill or the UI:** `make ui` (frontend) / `make build` (binary).

## File Structure

**New files:**
- `internal/flowdb/remote_devices.go` — `RemoteDevice` model + CRUD.
- `internal/flowdb/remote_devices_test.go` — CRUD tests.
- `internal/server/remote_auth.go` — token mint/hash, TTL consts, pairing-code store, `validRemoteDeviceToken`, `remoteAuth` middleware, rate limiter.
- `internal/server/remote_auth_test.go` — auth/pairing/limiter tests.
- `internal/server/remote_handlers.go` — remote pairing handler + local device-management handlers.
- `internal/server/remote_handlers_test.go` — handler tests.
- `internal/server/ui/public/manifest.webmanifest` — PWA manifest.
- `internal/server/ui/public/sw.js` — service worker (shell cache).
- `internal/server/ui/src/lib/devicetoken.ts` — device-token localStorage helper + remote-mode detection.
- `internal/server/ui/src/screens/RemoteAccessSettings.tsx` — Settings panel (enable, QR pairing, device list).

**Modified files:**
- `internal/flowdb/schema.go` — add `remote_devices` CREATE TABLE to `schemaDDL`.
- `internal/server/types.go` — add `pairing`, `remoteLimiter` fields to `Server`.
- `internal/server/server.go` — register local `/api/remote/*` routes; add `remoteAppMux`, `publicIngressHandler`, `handleRemoteStatic`.
- `internal/server/session_token.go` — `apiRouteNeedsToken` always-token for `/api/remote/` subtree.
- `internal/server/rpc_bridge.go` — thread a `remote` flag into `dispatchRPC`; deny device-management paths for remote connections.
- `internal/server/ingress.go` — relax `zrokManager.start()` gate for remote access; `remoteAccessEnabled()` helper.
- `internal/app/serve.go` (or `ListenAndServe`) — serve `publicIngressHandler()` over zrok instead of bare `ingressMux()`.
- `internal/server/ui/index.html` — link manifest + register service worker.
- `internal/server/ui/src/lib/wsurl.ts` — use device token in remote mode.
- `internal/server/ui/src/lib/rpc.ts` — token source in remote mode + pair-on-load bootstrap.
- `internal/server/ui/src/app.tsx` — mount `RemoteAccessSettings` under Settings.
- `internal/server/ui/package.json` — add `qrcode` dependency.

---

### Task 1: `remote_devices` table + flowdb CRUD

**Files:**
- Modify: `internal/flowdb/schema.go` (add to `schemaDDL` const, near the other `CREATE TABLE` blocks)
- Create: `internal/flowdb/remote_devices.go`
- Test: `internal/flowdb/remote_devices_test.go`

**Interfaces:**
- Produces: `RemoteDevice` struct; `InsertRemoteDevice(db *sql.DB, id, label, tokenHash, createdAt, expiresAt string) error`; `GetRemoteDeviceByTokenHash(db *sql.DB, tokenHash string) (*RemoteDevice, error)`; `ListRemoteDevices(db *sql.DB) ([]*RemoteDevice, error)`; `RevokeRemoteDevice(db *sql.DB, id, now string) error`; `TouchRemoteDeviceLastSeen(db *sql.DB, id, now string) error`.

- [ ] **Step 1: Add the schema.** In `internal/flowdb/schema.go`, append this block inside the `schemaDDL` backtick string (after an existing table, before the closing backtick):

```sql
CREATE TABLE IF NOT EXISTS remote_devices (
    id            TEXT PRIMARY KEY,
    label         TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    last_seen_at  TEXT,
    revoked_at    TEXT
);
```

- [ ] **Step 2: Write the failing test.** Create `internal/flowdb/remote_devices_test.go`:

```go
package flowdb

import "testing"

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRemoteDeviceCRUD(t *testing.T) {
	db := openTestDB(t)
	now := NowISO()
	exp := time.Now().Add(12 * time.Hour).Format(time.RFC3339)
	if err := InsertRemoteDevice(db, "dev1", "iPhone", "hashAAA", now, exp); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := GetRemoteDeviceByTokenHash(db, "hashAAA")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "dev1" || got.Label != "iPhone" || got.ExpiresAt != exp {
		t.Fatalf("unexpected device: %+v", got)
	}
	list, err := ListRemoteDevices(db)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if err := RevokeRemoteDevice(db, "dev1", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err = GetRemoteDeviceByTokenHash(db, "hashAAA")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if !got.RevokedAt.Valid {
		t.Fatalf("expected revoked_at set after revoke")
	}
}
```

Add the imports `"database/sql"` and `"time"` to the test file's import block.

- [ ] **Step 3: Run test to verify it fails.**

Run: `go test -run TestRemoteDeviceCRUD ./internal/flowdb/`
Expected: FAIL — `InsertRemoteDevice` / `RemoteDevice` undefined.

- [ ] **Step 4: Write the implementation.** Create `internal/flowdb/remote_devices.go`:

```go
package flowdb

import (
	"database/sql"
	"fmt"
)

// RemoteDevice is a phone (or other client) paired for remote access. Its token
// is stored only as a SHA-256 hex hash; the plaintext is shown to the device
// once at pairing and never persisted.
type RemoteDevice struct {
	ID         string
	Label      string
	TokenHash  string
	CreatedAt  string
	ExpiresAt  string
	LastSeenAt sql.NullString
	RevokedAt  sql.NullString
}

const RemoteDeviceCols = "id, label, token_hash, created_at, expires_at, last_seen_at, revoked_at"

func ScanRemoteDevice(row interface{ Scan(dest ...any) error }) (*RemoteDevice, error) {
	var d RemoteDevice
	err := row.Scan(&d.ID, &d.Label, &d.TokenHash, &d.CreatedAt, &d.ExpiresAt, &d.LastSeenAt, &d.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func InsertRemoteDevice(db *sql.DB, id, label, tokenHash, createdAt, expiresAt string) error {
	_, err := db.Exec(
		`INSERT INTO remote_devices (`+RemoteDeviceCols+`) VALUES (?, ?, ?, ?, ?, NULL, NULL)`,
		id, label, tokenHash, createdAt, expiresAt)
	if err != nil {
		return fmt.Errorf("insert remote device %s: %w", id, err)
	}
	return nil
}

// GetRemoteDeviceByTokenHash returns the device row for a token hash, or
// sql.ErrNoRows. It does NOT filter revoked/expired rows — the caller decides,
// so validation logic lives in one place (see validRemoteDeviceToken).
func GetRemoteDeviceByTokenHash(db *sql.DB, tokenHash string) (*RemoteDevice, error) {
	row := db.QueryRow("SELECT "+RemoteDeviceCols+" FROM remote_devices WHERE token_hash = ?", tokenHash)
	return ScanRemoteDevice(row)
}

func ListRemoteDevices(db *sql.DB) ([]*RemoteDevice, error) {
	rows, err := db.Query("SELECT " + RemoteDeviceCols + " FROM remote_devices ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list remote devices: %w", err)
	}
	defer rows.Close()
	var out []*RemoteDevice
	for rows.Next() {
		d, err := ScanRemoteDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan remote device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func RevokeRemoteDevice(db *sql.DB, id, now string) error {
	_, err := db.Exec("UPDATE remote_devices SET revoked_at = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("revoke remote device %s: %w", id, err)
	}
	return nil
}

func TouchRemoteDeviceLastSeen(db *sql.DB, id, now string) error {
	_, err := db.Exec("UPDATE remote_devices SET last_seen_at = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("touch remote device %s: %w", id, err)
	}
	return nil
}
```

- [ ] **Step 5: Run test to verify it passes.**

Run: `go test -run TestRemoteDeviceCRUD ./internal/flowdb/`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/flowdb/schema.go internal/flowdb/remote_devices.go internal/flowdb/remote_devices_test.go
git commit -m "feat(flowdb): remote_devices table + CRUD for paired remote access"
```

---

### Task 2: Remote token + pairing-code primitives

**Files:**
- Create: `internal/server/remote_auth.go`
- Test: `internal/server/remote_auth_test.go`

**Interfaces:**
- Produces: `const remoteDeviceTokenTTL = 12 * time.Hour`; `const pairingCodeTTL = 5 * time.Minute`; `mintRemoteToken() string`; `hashRemoteToken(token string) string`; `pairingStore` with `newPairingStore() *pairingStore`, `(*pairingStore) createAt(now time.Time) (code string, expiresAt time.Time)`, `(*pairingStore) redeemAt(code string, now time.Time) bool`.

- [ ] **Step 1: Write the failing test.** Create `internal/server/remote_auth_test.go`:

```go
package server

import (
	"testing"
	"time"
)

func TestHashRemoteTokenDeterministic(t *testing.T) {
	if hashRemoteToken("abc") != hashRemoteToken("abc") {
		t.Fatal("hash not deterministic")
	}
	if hashRemoteToken("abc") == hashRemoteToken("abd") {
		t.Fatal("distinct inputs collided")
	}
	if len(hashRemoteToken("abc")) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(hashRemoteToken("abc")))
	}
}

func TestMintRemoteTokenLength(t *testing.T) {
	if len(mintRemoteToken()) != 64 {
		t.Fatalf("expected 64 hex chars")
	}
}

func TestPairingStoreSingleUse(t *testing.T) {
	ps := newPairingStore()
	now := time.Unix(1_700_000_000, 0)
	code, _ := ps.createAt(now)
	if !ps.redeemAt(code, now) {
		t.Fatal("first redeem should succeed")
	}
	if ps.redeemAt(code, now) {
		t.Fatal("second redeem must fail (single-use)")
	}
}

func TestPairingStoreExpiry(t *testing.T) {
	ps := newPairingStore()
	now := time.Unix(1_700_000_000, 0)
	code, _ := ps.createAt(now)
	if ps.redeemAt(code, now.Add(pairingCodeTTL+time.Second)) {
		t.Fatal("expired code must not redeem")
	}
}

func TestPairingStoreUnknown(t *testing.T) {
	ps := newPairingStore()
	if ps.redeemAt("nope", time.Unix(1_700_000_000, 0)) {
		t.Fatal("unknown code must not redeem")
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `go test -run 'TestHashRemoteToken|TestMintRemoteToken|TestPairingStore' ./internal/server/`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Write the implementation.** Create `internal/server/remote_auth.go`:

```go
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// remoteDeviceTokenTTL bounds how long a QR-paired device token is valid.
	// After this the phone must re-pair (scan a fresh QR from the laptop), which
	// caps a lost phone's exposure at 12h even before the operator revokes it.
	remoteDeviceTokenTTL = 12 * time.Hour
	// pairingCodeTTL is the window to scan the QR before the code expires.
	pairingCodeTTL = 5 * time.Minute
)

// mintRemoteToken returns a fresh 256-bit token as hex, or "" if crypto/rand
// fails (callers treat "" as failure and refuse to pair, i.e. fail closed).
func mintRemoteToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// hashRemoteToken returns the SHA-256 hex of a token. Only the hash is stored;
// the presented token is hashed before the DB lookup so no secret-dependent
// string comparison happens in the app.
func hashRemoteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// pairingStore holds short-lived, single-use pairing codes in memory. Codes are
// not persisted — a server restart drops pending codes, which is acceptable for
// a 5-minute window. Methods are safe for concurrent use.
type pairingStore struct {
	mu    sync.Mutex
	codes map[string]time.Time // code -> expiry
}

func newPairingStore() *pairingStore {
	return &pairingStore{codes: make(map[string]time.Time)}
}

func (p *pairingStore) createAt(now time.Time) (string, time.Time) {
	code := mintRemoteToken()
	exp := now.Add(pairingCodeTTL)
	p.mu.Lock()
	p.codes[code] = exp
	// Opportunistic GC of expired codes so the map can't grow unbounded.
	for c, e := range p.codes {
		if now.After(e) {
			delete(p.codes, c)
		}
	}
	p.mu.Unlock()
	return code, exp
}

// redeemAt consumes a code: returns true exactly once, only if the code exists
// and has not expired. The code is deleted on any matched lookup (single-use).
func (p *pairingStore) redeemAt(code string, now time.Time) bool {
	if code == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.codes[code]
	if !ok {
		return false
	}
	delete(p.codes, code)
	return !now.After(exp)
}
```

- [ ] **Step 4: Run test to verify it passes.**

Run: `go test -run 'TestHashRemoteToken|TestMintRemoteToken|TestPairingStore' ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/server/remote_auth.go internal/server/remote_auth_test.go
git commit -m "feat(server): remote token mint/hash + single-use pairing-code store"
```

---

### Task 3: Device-token validation + `remoteAuth` middleware

**Files:**
- Modify: `internal/server/types.go` (add `pairing *pairingStore` field to `Server`, initialized in `New`)
- Modify: `internal/server/remote_auth.go` (add validation + middleware)
- Test: `internal/server/remote_auth_test.go`

**Interfaces:**
- Consumes: `flowdb.GetRemoteDeviceByTokenHash`, `flowdb.TouchRemoteDeviceLastSeen`, `s.cfg.DB`, `s.sessionToken`, `sessionTokenHeader`.
- Produces: `(s *Server) validRemoteDeviceToken(r *http.Request) (*flowdb.RemoteDevice, bool)`; `(s *Server) remoteAuth(next http.Handler) http.Handler`; `const remoteFlagHeader = "X-Flow-Remote"`.

- [ ] **Step 1: Add the struct field.** In `internal/server/types.go`, add to the `Server` struct (near `sessionToken`):

```go
	// pairing holds short-lived, single-use remote-access pairing codes.
	// Always non-nil after New().
	pairing *pairingStore
```

In `New(...)` (server.go / wherever `&Server{...}` is constructed), set `pairing: newPairingStore(),`.

- [ ] **Step 2: Write the failing test.** Append to `internal/server/remote_auth_test.go`:

```go
func insertTestDevice(t *testing.T, s *Server, token string, expiresAt time.Time, revoked bool) {
	t.Helper()
	now := flowdb.NowISO()
	if err := flowdb.InsertRemoteDevice(s.cfg.DB, "dev-"+token[:6], "test", hashRemoteToken(token), now, expiresAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if revoked {
		_ = flowdb.RevokeRemoteDevice(s.cfg.DB, "dev-"+token[:6], now)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := flowdb.OpenDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(Config{DB: db, FlowRoot: t.TempDir()})
}

func TestValidRemoteDeviceToken(t *testing.T) {
	s := newTestServer(t)
	good := mintRemoteToken()
	insertTestDevice(t, s, good, time.Now().Add(time.Hour), false)
	expired := mintRemoteToken()
	insertTestDevice(t, s, expired, time.Now().Add(-time.Hour), false)
	revoked := mintRemoteToken()
	insertTestDevice(t, s, revoked, time.Now().Add(time.Hour), true)

	cases := []struct {
		name  string
		token string
		ok    bool
	}{
		{"valid", good, true},
		{"expired", expired, false},
		{"revoked", revoked, false},
		{"unknown", mintRemoteToken(), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/ws/rpc?token="+c.token, nil)
			_, ok := s.validRemoteDeviceToken(r)
			if ok != c.ok {
				t.Fatalf("got ok=%v want %v", ok, c.ok)
			}
		})
	}
}

func TestRemoteAuthInjectsSessionToken(t *testing.T) {
	s := newTestServer(t)
	good := mintRemoteToken()
	insertTestDevice(t, s, good, time.Now().Add(time.Hour), false)

	var sawToken, sawFlag string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.URL.Query().Get("token")
		sawFlag = r.Header.Get(remoteFlagHeader)
		w.WriteHeader(200)
	})
	h := s.remoteAuth(next)

	// valid device token -> next runs, session token injected, remote flag set
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/rpc?token="+good, nil))
	if rec.Code != 200 {
		t.Fatalf("valid token: got %d", rec.Code)
	}
	if sawToken != s.sessionToken {
		t.Fatalf("session token not injected for downstream handler")
	}
	if sawFlag != "1" {
		t.Fatalf("remote flag not set")
	}

	// bad device token -> 403, next never runs
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/rpc?token=bogus", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad token: got %d want 403", rec.Code)
	}
}
```

Add imports `"net/http"`, `"net/http/httptest"`, and the module path for `flowdb` (match an existing server test's import, e.g. `"<module>/internal/flowdb"`).

- [ ] **Step 3: Run test to verify it fails.**

Run: `go test -run 'TestValidRemoteDeviceToken|TestRemoteAuth' ./internal/server/`
Expected: FAIL — `validRemoteDeviceToken` / `remoteAuth` / `remoteFlagHeader` undefined.

- [ ] **Step 4: Write the implementation.** Append to `internal/server/remote_auth.go` (add `"net/http"`, `"net/url"`, `"strings"`, and the `flowdb` import):

```go
// remoteFlagHeader marks an upgraded/forwarded request as having arrived over
// the remote (device-token) surface. handleRPCWebSocket reads it to deny
// device-management RPC paths to remote clients (see rpc_bridge.go).
const remoteFlagHeader = "X-Flow-Remote"

// validRemoteDeviceToken extracts a device token (X-Flow-Session-Token header or
// ?token= query — the same transport the browser uses for the session token),
// hashes it, looks up the device, and accepts it only when the row exists, is
// not revoked, and has not expired. Best-effort last-seen touch. Fails closed.
func (s *Server) validRemoteDeviceToken(r *http.Request) (*flowdb.RemoteDevice, bool) {
	if s == nil || s.cfg.DB == nil {
		return nil, false
	}
	got := strings.TrimSpace(r.Header.Get(sessionTokenHeader))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got == "" {
		return nil, false
	}
	dev, err := flowdb.GetRemoteDeviceByTokenHash(s.cfg.DB, hashRemoteToken(got))
	if err != nil || dev == nil {
		return nil, false
	}
	if dev.RevokedAt.Valid {
		return nil, false
	}
	exp, err := time.Parse(time.RFC3339, dev.ExpiresAt)
	if err != nil || time.Now().After(exp) {
		return nil, false
	}
	_ = flowdb.TouchRemoteDeviceLastSeen(s.cfg.DB, dev.ID, flowdb.NowISO())
	return dev, true
}

// remoteAuth gates the remote-app surface. On a valid device token it marks the
// request remote and INJECTS the shared session token (header + ?token=) so the
// existing WS/RPC handlers — which check the session token via
// authorizeWSHandshake — work unchanged. The session token is injected only
// into the server-side request; it is never sent back to the client.
func (s *Server) remoteAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.validRemoteDeviceToken(r); !ok {
			writeError(w, errors.New("missing or invalid device token"), http.StatusForbidden)
			return
		}
		r.Header.Set(remoteFlagHeader, "1")
		r.Header.Set(sessionTokenHeader, s.sessionToken)
		q := r.URL.Query()
		q.Set("token", s.sessionToken)
		r.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r)
	})
}
```

Add `"errors"` to the import block (and remove `"net/url"` if unused — gofmt/`go vet` will flag).

- [ ] **Step 5: Run test to verify it passes.**

Run: `go test -run 'TestValidRemoteDeviceToken|TestRemoteAuth' ./internal/server/`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/server/types.go internal/server/remote_auth.go internal/server/remote_auth_test.go internal/server/server.go
git commit -m "feat(server): device-token validation + remoteAuth token-swap middleware"
```

---

### Task 4: Rate limiter for pairing + failed auth

**Files:**
- Modify: `internal/server/remote_auth.go` (add limiter type) and `internal/server/types.go` (add `remoteLimiter *rateLimiter` field, init in `New`)
- Test: `internal/server/remote_auth_test.go`

**Interfaces:**
- Produces: `rateLimiter` with `newRateLimiter(max int, window time.Duration) *rateLimiter` and `(*rateLimiter) allowAt(key string, now time.Time) bool`.

- [ ] **Step 1: Write the failing test.** Append to `internal/server/remote_auth_test.go`:

```go
func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		if !rl.allowAt("1.2.3.4", now) {
			t.Fatalf("attempt %d should be allowed", i)
		}
	}
	if rl.allowAt("1.2.3.4", now) {
		t.Fatal("4th attempt in window must be blocked")
	}
	if !rl.allowAt("1.2.3.4", now.Add(time.Minute+time.Second)) {
		t.Fatal("attempt after window must be allowed")
	}
	if !rl.allowAt("5.6.7.8", now) {
		t.Fatal("different key must have its own budget")
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `go test -run TestRateLimiter ./internal/server/`
Expected: FAIL — `newRateLimiter` undefined.

- [ ] **Step 3: Write the implementation.** Append to `internal/server/remote_auth.go`:

```go
// rateLimiter is a fixed-window per-key limiter used on the pairing-redemption
// endpoint and on failed device-token validations to resist brute force over
// the public URL. now is injected for testability.
type rateLimiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	counters map[string]*rlWindow
}

type rlWindow struct {
	start time.Time
	count int
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, counters: make(map[string]*rlWindow)}
}

func (rl *rateLimiter) allowAt(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	w := rl.counters[key]
	if w == nil || now.Sub(w.start) >= rl.window {
		rl.counters[key] = &rlWindow{start: now, count: 1}
		return true
	}
	if w.count >= rl.max {
		return false
	}
	w.count++
	return true
}
```

In `internal/server/types.go` add to `Server`:

```go
	// remoteLimiter throttles pairing redemption + failed device-token auth.
	// Always non-nil after New().
	remoteLimiter *rateLimiter
```

In `New(...)` set `remoteLimiter: newRateLimiter(10, time.Minute),`.

- [ ] **Step 4: Run test to verify it passes.**

Run: `go test -run TestRateLimiter ./internal/server/`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/server/remote_auth.go internal/server/types.go internal/server/server.go
git commit -m "feat(server): fixed-window rate limiter for remote pairing + auth"
```

---

### Task 5: Remote pairing handler + local device-management handlers

**Files:**
- Create: `internal/server/remote_handlers.go`
- Modify: `internal/server/server.go` (register local routes in `registerAPIRoutes`)
- Modify: `internal/server/session_token.go` (`apiRouteNeedsToken`: always token for `/api/remote/` subtree)
- Modify: `internal/server/ingress.go` (add `remoteAccessEnabled()` helper)
- Test: `internal/server/remote_handlers_test.go`

**Interfaces:**
- Consumes: `s.pairing`, `s.remoteLimiter`, `flowdb.*RemoteDevice*`, `s.publicBaseURL()`, `clientIP(r)`.
- Produces handlers: `handleRemotePair` (remote, unauth), `handleRemotePairCode`, `handleRemoteDevices`, `handleRemoteDeviceRevoke`, `handleRemoteStatus`, `handleRemoteEnable`, `handleRemoteDisable` (all local); helper `remoteAccessEnabled() bool`; helper `clientIP(r *http.Request) string`.

- [ ] **Step 1: Add the env helper.** In `internal/server/ingress.go`, add near `zrokAutoStart()`:

```go
// remoteAccessEnabled reports whether the operator turned on the remote-access
// (phone PWA) surface. Persisted in config.json as FLOW_REMOTE_ACCESS and
// repopulated into the env on boot, mirroring zrokAutoStart/githubWebhookSecret.
func remoteAccessEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_REMOTE_ACCESS")))
	return v == "1" || v == "true" || v == "on"
}
```

- [ ] **Step 2: Gate `/api/remote/` behind the token.** In `internal/server/session_token.go`, in `apiRouteNeedsToken`, add before the method switch (right after the `tokenExemptAPIPath` check):

```go
	// All remote-access management endpoints are localhost-only operator
	// actions (pairing, enable/disable, device list/revoke) — require the
	// session token regardless of method so a stray same-origin GET can't read
	// the device list over direct HTTP. The remote-mux pairing-REDEMPTION
	// endpoint lives on a different mux and never reaches this gate.
	if strings.HasPrefix(path, "/api/remote/") {
		return true
	}
```

- [ ] **Step 3: Write the failing test.** Create `internal/server/remote_handlers_test.go`:

```go
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleRemotePairHappyPath(t *testing.T) {
	s := newTestServer(t)
	code, _ := s.pairing.createAt(time.Now())

	body := strings.NewReader(`{"code":"` + code + `","label":"iPhone"}`)
	req := httptest.NewRequest("POST", "/api/remote/pair", body)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	s.handleRemotePair(rec, req)

	if rec.Code != 200 {
		t.Fatalf("pair: got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("expected token in response: %s", rec.Body.String())
	}
	list, _ := flowdb.ListRemoteDevices(s.cfg.DB)
	if len(list) != 1 || list[0].Label != "iPhone" {
		t.Fatalf("device not persisted: %+v", list)
	}
}

func TestHandleRemotePairBadCode(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/remote/pair", strings.NewReader(`{"code":"nope","label":"x"}`))
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	s.handleRemotePair(rec, req)
	if rec.Code != 403 {
		t.Fatalf("bad code: got %d want 403", rec.Code)
	}
}
```

Add the `flowdb` import.

- [ ] **Step 4: Run test to verify it fails.**

Run: `go test -run TestHandleRemotePair ./internal/server/`
Expected: FAIL — `handleRemotePair` undefined.

- [ ] **Step 5: Write the implementation.** Create `internal/server/remote_handlers.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"<module>/internal/flowdb" // match the module path used by other server files
)

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleRemotePair (REMOTE mux, unauthenticated, rate-limited) redeems a pairing
// code and mints a 12h device token. This is the ONLY remote endpoint that does
// not require a device token — it is how a device obtains its first one.
func (s *Server) handleRemotePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.remoteLimiter.allowAt(clientIP(r), time.Now()) {
		writeError(w, errors.New("too many attempts"), http.StatusTooManyRequests)
		return
	}
	var body struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if !s.pairing.redeemAt(strings.TrimSpace(body.Code), time.Now()) {
		writeError(w, errors.New("invalid or expired pairing code"), http.StatusForbidden)
		return
	}
	token := mintRemoteToken()
	if token == "" {
		writeError(w, errors.New("token generation failed"), http.StatusInternalServerError)
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = "Paired device"
	}
	now := time.Now()
	expires := now.Add(remoteDeviceTokenTTL)
	id := mintRemoteToken()[:16]
	if err := flowdb.InsertRemoteDevice(s.cfg.DB, id, label, hashRemoteToken(token),
		now.Format(time.RFC3339), expires.Format(time.RFC3339)); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"token":      token,
		"device_id":  id,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// handleRemotePairCode (LOCAL) mints a pairing code + QR URL for the laptop UI.
func (s *Server) handleRemotePairCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !remoteAccessEnabled() {
		writeError(w, errors.New("enable remote access first"), http.StatusConflict)
		return
	}
	base := s.publicBaseURL()
	if base == "" {
		writeError(w, errors.New("public ingress not ready yet"), http.StatusServiceUnavailable)
		return
	}
	code, exp := s.pairing.createAt(time.Now())
	writeJSON(w, map[string]any{
		"code":       code,
		"expires_at": exp.Format(time.RFC3339),
		"pair_url":   strings.TrimRight(base, "/") + "/?pair=" + code,
	})
}

// handleRemoteDevices (LOCAL) lists paired devices.
func (s *Server) handleRemoteDevices(w http.ResponseWriter, r *http.Request) {
	list, err := flowdb.ListRemoteDevices(s.cfg.DB)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	type view struct {
		ID, Label, CreatedAt, ExpiresAt, LastSeenAt string
		Revoked                                     bool
	}
	out := make([]view, 0, len(list))
	for _, d := range list {
		out = append(out, view{
			ID: d.ID, Label: d.Label, CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt,
			LastSeenAt: d.LastSeenAt.String, Revoked: d.RevokedAt.Valid,
		})
	}
	writeJSON(w, map[string]any{"devices": out})
}

// handleRemoteDeviceRevoke (LOCAL) revokes one device by id.
func (s *Server) handleRemoteDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeError(w, errors.New("device id required"), http.StatusBadRequest)
		return
	}
	if err := flowdb.RevokeRemoteDevice(s.cfg.DB, body.ID, flowdb.NowISO()); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRemoteStatus (LOCAL) reports the toggle state + public URL.
func (s *Server) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"enabled":    remoteAccessEnabled(),
		"public_url": s.publicBaseURL(),
	})
}
```

Replace `<module>` with the real module path (read it from `go.mod`; reuse the exact import other `internal/server` files use for `internal/flowdb`). If `writeJSON` does not already exist in the package, use the existing JSON-writing helper this package uses for handler responses (grep for how `handleHealth` writes its body) instead of inventing one.

- [ ] **Step 6: Register the local routes.** In `internal/server/server.go` `registerAPIRoutes`, add:

```go
	mux.HandleFunc("/api/remote/pair-code", s.handleRemotePairCode)
	mux.HandleFunc("/api/remote/devices", s.handleRemoteDevices)
	mux.HandleFunc("/api/remote/devices/revoke", s.handleRemoteDeviceRevoke)
	mux.HandleFunc("/api/remote/status", s.handleRemoteStatus)
	mux.HandleFunc("/api/remote/enable", s.handleRemoteEnable)
	mux.HandleFunc("/api/remote/disable", s.handleRemoteDisable)
```

(`handleRemoteEnable`/`handleRemoteDisable` are implemented in Task 7; add stub handlers returning 501 now so the package compiles, then replace in Task 7.)

- [ ] **Step 7: Run test to verify it passes.**

Run: `go test -run TestHandleRemotePair ./internal/server/`
Expected: PASS.

- [ ] **Step 8: Commit.**

```bash
git add internal/server/remote_handlers.go internal/server/server.go internal/server/session_token.go internal/server/ingress.go internal/server/remote_handlers_test.go
git commit -m "feat(server): remote pairing + local device-management endpoints"
```

---

### Task 6: Composite public handler, leak-free remote static, RPC denylist

**Files:**
- Modify: `internal/server/server.go` (add `handleRemoteStatic`, `remoteAppMux`, `publicIngressHandler`)
- Modify: `internal/server/rpc_bridge.go` (thread `remote` flag, deny device-management paths)
- Test: `internal/server/remote_handlers_test.go`

**Interfaces:**
- Consumes: `s.ingressMux()`, `s.handleStatic`, `s.remoteAuth`, `s.handleTerminalWebSocket`, `s.handleFloatingTerminalWebSocket`, `s.handleRPCWebSocket`, `s.handleEventWebSocket`, `s.handleRemotePair`, `injectSessionToken`, `staticFS`, `remoteAccessEnabled`.
- Produces: `(s *Server) publicIngressHandler() http.Handler`; `(s *Server) remoteAppMux() http.Handler`; `(s *Server) handleRemoteStatic(w, r)`; `remoteForbiddenRPCPath(path string) bool`.

- [ ] **Step 1: Write the failing test.** Append to `internal/server/remote_handlers_test.go`:

```go
func TestRemoteForbiddenRPCPath(t *testing.T) {
	for _, p := range []string{"/api/remote/pair-code", "/api/remote/devices", "/api/remote/enable", "/api/remote/status"} {
		if !remoteForbiddenRPCPath(p) {
			t.Fatalf("%s must be forbidden for remote RPC", p)
		}
	}
	for _, p := range []string{"/api/tasks", "/api/overview", "/api/kb"} {
		if remoteForbiddenRPCPath(p) {
			t.Fatalf("%s must be allowed for remote RPC", p)
		}
	}
}

func TestRemoteStaticDoesNotLeakSessionToken(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleRemoteStatic(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), s.sessionToken) && s.sessionToken != "" {
		t.Fatal("remote static must NOT embed the shared session token")
	}
	if !strings.Contains(rec.Body.String(), "__FLOW_REMOTE__") {
		t.Fatal("remote static must mark the page as remote")
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `go test -run 'TestRemoteForbiddenRPCPath|TestRemoteStaticDoesNotLeak' ./internal/server/`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement the composite handler + leak-free static.** Add to `internal/server/server.go`:

```go
// remoteForbiddenRPCPath reports whether an /api path is a localhost-only
// operator action that a remote (device-token) client must NOT reach over the
// /ws/rpc bridge. The remote surface can drive sessions but can never pair new
// devices, toggle remote access, or read/revoke the device list.
func remoteForbiddenRPCPath(path string) bool {
	return strings.HasPrefix(path, "/api/remote/")
}

// handleRemoteStatic serves the embedded PWA shell for the REMOTE surface. It is
// identical to handleStatic EXCEPT it never injects the shared session token —
// the phone authenticates with its own device token from localStorage. It marks
// the page remote so the client uses the device-token transport.
func (s *Server) handleRemoteStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
	if path == "." || path == "" {
		path = "index.html"
	}
	data, err := staticFS.ReadFile("static/" + path)
	if err != nil {
		data, err = staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "static assets unavailable", http.StatusInternalServerError)
			return
		}
		path = "index.html"
	}
	if ctype := mime.TypeByExtension(filepath.Ext(path)); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-store")
	if path == "index.html" {
		data = injectRemoteFlag(data)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// injectRemoteFlag inserts window.__FLOW_REMOTE__ = true before </head> so the
// PWA knows to authenticate with its stored device token, not __FLOW_TOKEN__.
func injectRemoteFlag(html []byte) []byte {
	tag := []byte("<script>window.__FLOW_REMOTE__=true;</script></head>")
	return []byte(strings.Replace(string(html), "</head>", string(tag), 1))
}

// remoteAppMux is the device-token-gated app surface served over zrok when
// remote access is enabled. Only /api/remote/pair is reachable without a device
// token (rate-limited; how a device gets its first token). All data flows over
// the device-gated /ws/rpc — no general /api/* is exposed on this mux.
func (s *Server) remoteAppMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/pair", s.handleRemotePair)
	mux.Handle("/ws/terminal", s.remoteAuth(http.HandlerFunc(s.handleTerminalWebSocket)))
	mux.Handle("/ws/floating-terminal", s.remoteAuth(http.HandlerFunc(s.handleFloatingTerminalWebSocket)))
	mux.Handle("/ws/rpc", s.remoteAuth(http.HandlerFunc(s.handleRPCWebSocket)))
	mux.Handle("/ws/events", s.remoteAuth(http.HandlerFunc(s.handleEventWebSocket)))
	mux.HandleFunc("/", s.handleRemoteStatic)
	return mux
}

// publicIngressHandler is what the zrok share serves. The GitHub webhook + OAuth
// mux is always served unchanged; the remote app is served only when remote
// access is enabled, otherwise app paths 404.
func (s *Server) publicIngressHandler() http.Handler {
	ingress := s.ingressMux()
	app := s.remoteAppMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/github/webhook", githubSetupCallbackPath, slackOAuthCallbackPath:
			ingress.ServeHTTP(w, r)
			return
		}
		if !remoteAccessEnabled() {
			http.NotFound(w, r)
			return
		}
		app.ServeHTTP(w, r)
	})
}
```

Ensure `internal/server/server.go` imports `"mime"` and `"path/filepath"` (already used by `handleStatic`).

- [ ] **Step 4: Enforce the RPC denylist.** In `internal/server/rpc_bridge.go`:

In `handleRPCWebSocket`, after the upgrade, read the remote flag once:

```go
	remote := r.Header.Get(remoteFlagHeader) == "1"
```

Pass `remote` into the per-frame dispatch goroutine and into `dispatchRPC`. Change the dispatch call site from `resp := s.dispatchRPC(req)` to `resp := s.dispatchRPC(req, remote)`, and change the signature:

```go
func (s *Server) dispatchRPC(req rpcRequest, remote bool) rpcResponse {
	if remote && remoteForbiddenRPCPath(req.Path) {
		return rpcResponse{Type: "rpc", ID: req.ID, Status: http.StatusForbidden,
			Error: "not permitted from a remote device"}
	}
	// ... existing body unchanged ...
}
```

- [ ] **Step 4b: Remote WebSocket origin gate (carry-forward from Task 3).** The shared WS upgrader's `checkLocalWSOrigin` rejects any handshake whose `Origin` host ≠ `r.Host`. A remote phone's PWA is served from the zrok public URL, and through the zrok proxy the `Host` the backend sees may NOT equal the public origin — which would 403 a *valid* device token at the origin gate. Give the remote path a dedicated, origin-aware upgrader. (The device token validated by `remoteAuth` is the real auth; this is defense-in-depth against cross-origin handshakes.)

In `internal/server/session_token.go` (next to `checkLocalWSOrigin`):

```go
// checkRemoteWSOrigin is the origin gate for the remote (device-token) WS
// surface. The PWA is served from the zrok public URL, so a remote handshake's
// Origin is that public host. Accept when the Origin host matches r.Host (zrok
// preserved Host) OR the configured public base URL host (zrok rewrote Host).
// Empty/unparseable Origin is rejected, same as the local gate.
func (s *Server) checkRemoteWSOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if base := s.publicBaseURL(); base != "" {
		if bu, err := url.Parse(base); err == nil && strings.EqualFold(u.Host, bu.Host) {
			return true
		}
	}
	return false
}
```

Add a per-request upgrader selector in `internal/server/terminal_bridge.go` (near the `terminalUpgrader` var):

```go
// wsUpgrader returns the WebSocket upgrader for a request: the strict local
// origin gate by default, or the remote-aware gate when remoteAuth marked the
// request remote (X-Flow-Remote). Both share every other upgrader setting.
func (s *Server) wsUpgrader(r *http.Request) websocket.Upgrader {
	if r.Header.Get(remoteFlagHeader) == "1" {
		return websocket.Upgrader{CheckOrigin: s.checkRemoteWSOrigin}
	}
	return terminalUpgrader
}
```

Then in each of the four WS handlers — `handleTerminalWebSocket`, `handleFloatingTerminalWebSocket` (terminal_bridge.go), `handleRPCWebSocket` (rpc_bridge.go), `handleEventWebSocket` (events_hub.go) — replace the `terminalUpgrader.Upgrade(w, r, nil)` call with `s.wsUpgrader(r).Upgrade(w, r, nil)`. No other handler logic changes. Confirm `session_token.go` imports `net/url` (`checkLocalWSOrigin` already uses it).

Add a test to `internal/server/remote_handlers_test.go`:

```go
func TestCheckRemoteWSOrigin(t *testing.T) {
	s := newTestServer(t)
	mk := func(origin, host string) *http.Request {
		r := httptest.NewRequest("GET", "/ws/rpc", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if !s.checkRemoteWSOrigin(mk("https://h.example", "h.example")) {
		t.Fatal("same-host origin should be allowed")
	}
	if s.checkRemoteWSOrigin(mk("", "h.example")) {
		t.Fatal("empty origin must be rejected")
	}
	if s.checkRemoteWSOrigin(mk("https://evil.example", "h.example")) {
		t.Fatal("cross-origin handshake must be rejected")
	}
}
```

- [ ] **Step 5: Run tests to verify they pass.**

Run: `go test -run 'TestRemoteForbiddenRPCPath|TestRemoteStaticDoesNotLeak|TestCheckRemoteWSOrigin' ./internal/server/ && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 6: Commit.**

```bash
git add internal/server/server.go internal/server/rpc_bridge.go internal/server/terminal_bridge.go internal/server/events_hub.go internal/server/session_token.go internal/server/remote_handlers_test.go
git commit -m "feat(server): composite public ingress, leak-free remote static, RPC denylist, remote WS origin gate"
```

---

### Task 7: Enable/disable lifecycle + zrok start-gate relaxation

**Files:**
- Modify: `internal/server/ingress.go` (`zrokManager.start()` gate)
- Modify: `internal/server/server.go` or `internal/app/serve.go` (serve `publicIngressHandler()`; wire enable/disable)
- Modify: `internal/server/remote_handlers.go` (replace the Task 5 stubs)

**Interfaces:**
- Consumes: `loadConfigFile`, `saveConfigFile`, `s.configPath()`, `s.zrok`, `s.ensureZrokIngressCredentials()`, `zrokManager.start()`, `os.Setenv`.
- Produces: `handleRemoteEnable`, `handleRemoteDisable`.

- [ ] **Step 1: Serve the composite handler over zrok.** In `ListenAndServe` (where `s.zrok.handler = s.ingressMux()` is set per the verbatim code), change to:

```go
	if s.zrok != nil {
		s.zrok.handler = s.publicIngressHandler()
		s.ensureZrokIngressCredentials()
		s.zrok.start()
		defer s.zrok.stop()
	}
```

- [ ] **Step 2: Relax the start gate for remote access.** In `internal/server/ingress.go` `zrokManager.start()`, change the webhook-secret hard error so it does not block when remote access is the driver:

```go
	if githubWebhookSecret() == "" && !remoteAccessEnabled() {
		m.setErr(errors.New("GitHub webhook secret required before public ingress can start"))
		return
	}
```

- [ ] **Step 3: Implement enable/disable (replace the Task 5 stubs).** In `internal/server/remote_handlers.go`:

```go
func (s *Server) handleRemoteEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if activeIngressProvider() != ingressProviderZrok || !zrokAutoStart() {
		writeError(w, errors.New("set up public ingress (zrok) first — see Connectors"), http.StatusConflict)
		return
	}
	s.setRemoteAccessConfig(true)
	s.ensureZrokIngressCredentials()
	if s.zrok != nil {
		s.zrok.start() // idempotent — no-op if already serving
	}
	writeJSON(w, map[string]any{"enabled": true, "public_url": s.publicBaseURL()})
}

func (s *Server) handleRemoteDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.setRemoteAccessConfig(false)
	// The zrok share stays up to keep serving the GitHub webhook; the composite
	// handler now 404s all app paths because remoteAccessEnabled() is false.
	writeJSON(w, map[string]any{"enabled": false})
}

// setRemoteAccessConfig persists FLOW_REMOTE_ACCESS to config.json and the env,
// mirroring ensureZrokIngressCredentials' load/save pattern.
func (s *Server) setRemoteAccessConfig(on bool) {
	val := "0"
	if on {
		val = "1"
	}
	os.Setenv("FLOW_REMOTE_ACCESS", val)
	path := s.configPath()
	if path == "" {
		return
	}
	cfg := loadConfigFile(path)
	cfg["FLOW_REMOTE_ACCESS"] = val
	_ = saveConfigFile(path, cfg)
}
```

Add `"os"` to the imports if not present.

- [ ] **Step 4: Verify build + existing tests.**

Run: `go build ./... && go test ./internal/server/ ./internal/flowdb/`
Expected: clean build, all PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/server/ingress.go internal/server/remote_handlers.go internal/app/serve.go
git commit -m "feat(server): remote-access enable/disable + zrok start for remote driver"
```

---

### Task 8: PWA manifest + service worker + index.html wiring

**Files:**
- Create: `internal/server/ui/public/manifest.webmanifest`
- Create: `internal/server/ui/public/sw.js`
- Modify: `internal/server/ui/index.html`

**Interfaces:** Produces installable PWA assets copied to `static/` by Vite's default `public/` handling.

- [ ] **Step 1: Create the manifest.** `internal/server/ui/public/manifest.webmanifest`:

```json
{
  "name": "flow · operator console",
  "short_name": "flow",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#0b0b0d",
  "theme_color": "#0b0b0d",
  "icons": [
    { "src": "/flow-mark.svg", "type": "image/svg+xml", "sizes": "any", "purpose": "any maskable" }
  ]
}
```

- [ ] **Step 2: Create the service worker.** `internal/server/ui/public/sw.js`:

```js
// Shell-only cache. Mission Control is entirely live over WebSocket, so there is
// no offline DATA mode — the service worker exists for installability and a fast
// cold start of the static shell. Network-first, cache fallback for navigations.
const CACHE = 'flow-shell-v1'
self.addEventListener('install', (e) => { self.skipWaiting() })
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k)))))
})
self.addEventListener('fetch', (e) => {
  const req = e.request
  if (req.method !== 'GET' || new URL(req.url).pathname.startsWith('/ws')) return
  e.respondWith(
    fetch(req).then((res) => {
      const copy = res.clone()
      caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {})
      return res
    }).catch(() => caches.match(req).then((m) => m || caches.match('/')))
  )
})
```

- [ ] **Step 3: Wire into index.html.** In `internal/server/ui/index.html`, add inside `<head>` (after the viewport meta):

```html
    <link rel="manifest" href="/manifest.webmanifest" />
```

And before `</body>` (after the module script):

```html
    <script>
      if ('serviceWorker' in navigator) {
        window.addEventListener('load', () => navigator.serviceWorker.register('/sw.js').catch(() => {}))
      }
    </script>
```

- [ ] **Step 4: Build and verify the assets are embedded.**

Run: `make ui && ls internal/server/static/manifest.webmanifest internal/server/static/sw.js`
Expected: both files exist under `static/`.

- [ ] **Step 5: Commit.**

```bash
git add internal/server/ui/public/manifest.webmanifest internal/server/ui/public/sw.js internal/server/ui/index.html internal/server/static
git commit -m "feat(ui): PWA manifest + shell service worker + install wiring"
```

---

### Task 9: Device-token transport + pair-on-load bootstrap

**Files:**
- Create: `internal/server/ui/src/lib/devicetoken.ts`
- Modify: `internal/server/ui/src/lib/wsurl.ts`
- Modify: `internal/server/ui/src/lib/rpc.ts` (or `main.tsx` bootstrap — wherever the socket first connects)

**Interfaces:**
- Produces: `isRemoteMode(): boolean`, `getDeviceToken(): string | null`, `setDeviceToken(t: string): void`, `clearDeviceToken(): void`, `authToken(): string`, `deviceLabel(): string`, `maybePairFromUrl(): Promise<void>`.

- [ ] **Step 1: Create the device-token helper.** `internal/server/ui/src/lib/devicetoken.ts`:

```ts
// In remote (PWA-over-zrok) mode the page is served WITHOUT window.__FLOW_TOKEN__
// (the shared session token must never leave the laptop). Instead the device
// stores its own 12h token, obtained by redeeming a pairing code from the QR.
const KEY = 'flow.device.token'

declare global {
  interface Window {
    __FLOW_TOKEN__?: string
    __FLOW_REMOTE__?: boolean
  }
}

export function isRemoteMode(): boolean {
  return window.__FLOW_REMOTE__ === true
}

export function getDeviceToken(): string | null {
  try { return localStorage.getItem(KEY) } catch { return null }
}
export function setDeviceToken(t: string): void {
  try { localStorage.setItem(KEY, t) } catch { /* ignore */ }
}
export function clearDeviceToken(): void {
  try { localStorage.removeItem(KEY) } catch { /* ignore */ }
}

// authToken is the token to put on /ws/* URLs and RPC. Remote mode uses the
// stored device token; local mode uses the injected session token.
export function authToken(): string {
  if (isRemoteMode()) return getDeviceToken() ?? ''
  return window.__FLOW_TOKEN__ ?? ''
}

// deviceLabel derives a friendly, human-readable device label from the
// user-agent so the laptop's paired-devices list reads "iPad" / "iPhone" /
// "Android phone" rather than a raw UA string. Best-effort; falls back to a
// generic label. The operator can tell at a glance which device is paired.
export function deviceLabel(): string {
  const ua = navigator.userAgent
  if (/iPad/i.test(ua) || (/Macintosh/i.test(ua) && navigator.maxTouchPoints > 1)) return 'iPad'
  if (/iPhone/i.test(ua)) return 'iPhone'
  if (/Android/i.test(ua)) return /Mobile/i.test(ua) ? 'Android phone' : 'Android tablet'
  if (/Macintosh/i.test(ua)) return 'Mac'
  if (/Windows/i.test(ua)) return 'Windows PC'
  return 'Paired device'
}

// maybePairFromUrl redeems a ?pair=<code> query param (the QR target) into a
// device token, stores it, and strips the param from the URL. No-op otherwise.
export async function maybePairFromUrl(): Promise<void> {
  const url = new URL(window.location.href)
  const code = url.searchParams.get('pair')
  if (!code) return
  const res = await fetch('/api/remote/pair', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code, label: deviceLabel() }),
  })
  if (res.ok) {
    const data = await res.json()
    if (data.token) setDeviceToken(data.token)
  }
  url.searchParams.delete('pair')
  window.history.replaceState({}, '', url.toString())
}
```

- [ ] **Step 2: Use the token in wsurl.** In `internal/server/ui/src/lib/wsurl.ts`, replace the token source (currently `window.__FLOW_TOKEN__`) with `authToken()` imported from `./devicetoken`. The URL construction is otherwise unchanged (`?token=<authToken()>`).

- [ ] **Step 3: Pair before first connect.** In `internal/server/ui/src/main.tsx` (the app entry, before the RPC client first connects / before `createRoot(...).render`), add:

```ts
import { maybePairFromUrl } from './lib/devicetoken'

await maybePairFromUrl()
```

If the entry is not already async, wrap the render in `maybePairFromUrl().finally(() => { /* existing render */ })` so a paired token is stored before the socket opens.

- [ ] **Step 4: Build and verify.**

Run: `make ui`
Expected: build succeeds (no TS errors).

- [ ] **Step 5: Commit.**

```bash
git add internal/server/ui/src/lib/devicetoken.ts internal/server/ui/src/lib/wsurl.ts internal/server/ui/src/main.tsx internal/server/static
git commit -m "feat(ui): device-token transport + QR pair-on-load bootstrap"
```

---

### Task 10: Settings "Remote access" panel (enable + QR + device list)

**Files:**
- Modify: `internal/server/ui/package.json` (add `qrcode`)
- Create: `internal/server/ui/src/screens/RemoteAccessSettings.tsx`
- Modify: `internal/server/ui/src/screens/Settings.tsx` (mount the panel) — confirm the real Settings file path/name with `Glob` first.

**Interfaces:**
- Consumes: `rpc.request({method, path, body, timeoutMs})` returning `{status, json, error}` (verbatim from `lib/rpc.ts`); `qrcode` package.
- Produces: `<RemoteAccessSettings />` React component.

- [ ] **Step 1: Add the QR dependency.**

Run: `cd internal/server/ui && pnpm add qrcode && pnpm add -D @types/qrcode`
Expected: `qrcode` appears in `package.json` dependencies.

- [ ] **Step 2: Create the panel.** `internal/server/ui/src/screens/RemoteAccessSettings.tsx`:

```tsx
import { useEffect, useState } from 'react'
import QRCode from 'qrcode'
import { rpc } from '../lib/rpc'

interface Device { ID: string; Label: string; CreatedAt: string; ExpiresAt: string; LastSeenAt: string; Revoked: boolean }

export function RemoteAccessSettings() {
  const [enabled, setEnabled] = useState(false)
  const [publicUrl, setPublicUrl] = useState('')
  const [devices, setDevices] = useState<Device[]>([])
  const [qr, setQr] = useState<string>('')
  const [pairUrl, setPairUrl] = useState('')
  const [err, setErr] = useState('')

  async function refresh() {
    const st = await rpc.request({ method: 'GET', path: '/api/remote/status' })
    const s = (st.json ?? {}) as { enabled?: boolean; public_url?: string }
    setEnabled(!!s.enabled); setPublicUrl(s.public_url ?? '')
    const dl = await rpc.request({ method: 'GET', path: '/api/remote/devices' })
    setDevices(((dl.json ?? {}) as { devices?: Device[] }).devices ?? [])
  }
  useEffect(() => { refresh() }, [])

  async function toggle(on: boolean) {
    setErr('')
    const r = await rpc.request({ method: 'POST', path: on ? '/api/remote/enable' : '/api/remote/disable' })
    if (r.status >= 400) { setErr((r.json as any)?.error || r.error || 'failed'); return }
    await refresh()
  }

  async function addDevice() {
    setErr(''); setQr(''); setPairUrl('')
    const r = await rpc.request({ method: 'POST', path: '/api/remote/pair-code' })
    if (r.status >= 400) { setErr((r.json as any)?.error || r.error || 'failed'); return }
    const url = (r.json as any).pair_url as string
    setPairUrl(url)
    setQr(await QRCode.toDataURL(url, { margin: 1, width: 240 }))
  }

  async function revoke(id: string) {
    await rpc.request({ method: 'POST', path: '/api/remote/devices/revoke', body: { id } })
    await refresh()
  }

  return (
    <section className="card">
      <h3>Remote access</h3>
      <p>Reach Mission Control and your live sessions from your phone. Device tokens expire 12h after pairing.</p>
      {err && <div className="error">{err}</div>}
      <label>
        <input type="checkbox" checked={enabled} onChange={(e) => toggle(e.target.checked)} /> Enable remote access
      </label>
      {enabled && publicUrl && <p>Public URL: <code>{publicUrl}</code></p>}
      {enabled && <button onClick={addDevice}>Add device (show QR)</button>}
      {qr && (
        <div className="pair-qr">
          <img src={qr} alt="Pairing QR" width={240} height={240} />
          <p>Scan within 5 minutes. <code>{pairUrl}</code></p>
        </div>
      )}
      <h4>Paired devices</h4>
      <ul>
        {devices.map((d) => (
          <li key={d.ID}>
            {d.Label} — expires {d.ExpiresAt}{d.Revoked ? ' (revoked)' : ''}
            {!d.Revoked && <button onClick={() => revoke(d.ID)}>Revoke</button>}
          </li>
        ))}
      </ul>
    </section>
  )
}
```

- [ ] **Step 3: Mount it.** In the Settings screen (confirm the file with `Glob internal/server/ui/src/screens/Settings.tsx` or similar), import and render `<RemoteAccessSettings />` within the settings page body, following the existing section/card pattern used by neighboring settings blocks.

- [ ] **Step 4: Build and verify.**

Run: `make ui`
Expected: build succeeds; `qrcode` bundles without a CDN.

- [ ] **Step 5: Commit.**

```bash
git add internal/server/ui/package.json internal/server/ui/pnpm-lock.yaml internal/server/ui/src/screens/RemoteAccessSettings.tsx internal/server/ui/src/screens/Settings.tsx internal/server/static
git commit -m "feat(ui): Settings remote-access panel — enable, QR pairing, device list/revoke"
```

---

### Task 11: Full build, test, and architecture-doc update

**Files:**
- Modify: `CLAUDE.md` (architecture map — note the remote-access surface)

- [ ] **Step 1: Full build + test.**

Run: `make build && make test`
Expected: binary builds; all tests PASS.

- [ ] **Step 2: Manual smoke (operator).** With zrok ingress configured: start `flow ui serve`, open Settings → Remote access → Enable → Add device, scan the QR on a phone on a *different* network, confirm the PWA loads, pairs, lists sessions, and a terminal attaches and accepts input. Then revoke the device and confirm the phone's next request 403s.

- [ ] **Step 3: Update the architecture doc.** In `CLAUDE.md`, under the `internal/server` description, add one line noting the remote-access surface (composite public ingress + per-device tokens) so the map stays honest.

- [ ] **Step 4: Commit.**

```bash
git add CLAUDE.md
git commit -m "docs: note remote-access surface in architecture map"
```

---

## Self-Review

**Spec coverage:**
- Component 1 (separate opt-in ingress) → Tasks 6 (composite handler), 7 (enable/disable + start gate). ✓
- Component 2 (per-device tokens, 12h expiry, pairing, revocation, hashed-at-rest, origin) → Tasks 1, 2, 3, 5. ✓
- Component 3 (PWA manifest + service worker) → Task 8. ✓
- Security gates 1–11 → opt-in (7), bounded mux (6), pairing-from-localhost (5: `/api/remote/` token-gated + pair-code requires localhost; 6: pair-code not on remote mux), single-use code (2), hashed tokens (1,2), 12h expiry (1,3), fail-closed (3,5,6), rate-limit (4,5), origin (3 — relies on existing `checkLocalWSOrigin`, naturally satisfied), revocation+audit (1,5,10), defense-in-depth (inherent). ✓
- Laptop shows which device type is paired (phone/iPad/etc.) + revoke from laptop only → Task 9 `deviceLabel()` (friendly UA-derived label), Task 5 `handleRemoteDevices`/`handleRemoteDeviceRevoke` (localhost-gated), Task 6 RPC denylist (a phone cannot reach the device list/revoke), Task 10 list + Revoke button. ✓
- RPC device-management leak (discovered during planning) → Task 6 denylist. ✓
- Session-token leak via remote static (discovered during planning) → Task 6 `handleRemoteStatic`. ✓
- Responsive Mission Control (Component 4) → explicitly DEFERRED to a follow-on plan (noted in Goal/scope). ✓ (Not a gap — out of scope for this foundation plan.)

**Placeholder scan:** `<module>` in Task 5 is a real instruction to substitute the go.mod module path (explicitly flagged), not a code placeholder. `writeJSON`/`writeError` are assumed existing package helpers — Task 5 instructs verifying against `handleHealth`. No "TBD"/"implement later".

**Type consistency:** `RemoteDevice` fields used in Tasks 3/5/10 match the struct in Task 1. `hashRemoteToken`/`mintRemoteToken`/`pairingStore.createAt`/`redeemAt` signatures consistent across Tasks 2/3/5. `remoteFlagHeader`/`remoteForbiddenRPCPath`/`remoteAccessEnabled` consistent across Tasks 3/5/6/7. `authToken()`/`isRemoteMode()` consistent across Tasks 9/10.

**Note for the implementer:** `New(...)` is modified in Tasks 3 and 4 (add `pairing` and `remoteLimiter` init). Confirm the exact `&Server{...}` literal in `server.go` `New` before editing; both fields initialize to non-nil there.
