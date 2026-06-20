package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"flow/internal/flowdb"
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
