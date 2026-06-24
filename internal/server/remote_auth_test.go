package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func TestHashRemoteTokenDeterministic(t *testing.T) {
	// Hash the same input twice into distinct variables: a non-deterministic
	// hash would make h1 != h2. (Comparing two identical call expressions
	// directly trips staticcheck SA4000, so bind to vars first.)
	h1 := hashRemoteToken("abc")
	h2 := hashRemoteToken("abc")
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if h1 == hashRemoteToken("abd") {
		t.Fatal("distinct inputs collided")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h1))
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

func insertTestDevice(t *testing.T, s *Server, token string, expiresAt time.Time, revoked bool) {
	t.Helper()
	now := productdb.NowISO()
	if err := productdb.InsertRemoteDevice(s.cfg.DB, "dev-"+token[:6], "test", hashRemoteToken(token), now, expiresAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if revoked {
		_ = productdb.RevokeRemoteDevice(s.cfg.DB, "dev-"+token[:6], now)
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

// TestRemoteAuthRateLimitsFailedAuth verifies that repeated invalid device-token
// attempts from the same IP eventually hit 429, and that a valid token never
// consumes limiter budget (it still succeeds after the limiter is exhausted).
func TestRemoteAuthRateLimitsFailedAuth(t *testing.T) {
	s := newTestServer(t)
	// The limiter is created with max=10 in New (server.go). Exhaust it with
	// bogus tokens and confirm the Nth+1 call yields 429 not 403.
	limiterMax := 10

	good := mintRemoteToken()
	insertTestDevice(t, s, good, time.Now().Add(time.Hour), false)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := s.remoteAuth(next)

	// Exhaust the budget: each bad-token attempt should return 403.
	for i := 0; i < limiterMax; i++ {
		req := httptest.NewRequest("GET", "/ws/rpc?token=bogus", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: got %d want 403", i, rec.Code)
		}
	}

	// One more bad token from the same IP: must now be 429.
	req := httptest.NewRequest("GET", "/ws/rpc?token=bogus", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after budget exhausted: got %d want 429", rec.Code)
	}

	// A VALID token must still succeed — valid tokens never touch the limiter.
	req = httptest.NewRequest("GET", "/ws/rpc?token="+good, nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid token after limiter exhausted: got %d want 200", rec.Code)
	}
}

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
