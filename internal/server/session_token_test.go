package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// authedTestHandler wraps s.Handler() so every request it serves carries the
// data-plane session token (the credential the browser UI normally gets from
// window.__FLOW_TOKEN__). Handler-logic tests use this to exercise the real
// Handler() — middleware included — without threading the token through each
// httptest request. The dedicated auth tests below deliberately do NOT use it,
// so the gate itself stays covered. Production never uses this.
func authedTestHandler(s *Server) http.Handler {
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sessionTokenHeader) == "" {
			r.Header.Set(sessionTokenHeader, s.sessionToken)
		}
		h.ServeHTTP(w, r)
	})
}

func TestCheckLocalWSOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"empty origin rejected", "", "127.0.0.1:8787", false},
		{"exact match ok", "http://127.0.0.1:8787", "127.0.0.1:8787", true},
		{"localhost exact ok", "http://localhost:8787", "localhost:8787", true},
		{"substring bypass rejected", "http://127.0.0.1:8787.evil.com", "127.0.0.1:8787", false},
		{"different host rejected", "http://evil.com", "127.0.0.1:8787", false},
		{"port mismatch rejected", "http://127.0.0.1:9999", "127.0.0.1:8787", false},
		{"garbage origin rejected", "::::not a url", "127.0.0.1:8787", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws/events", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := checkLocalWSOrigin(r); got != tc.want {
				t.Fatalf("checkLocalWSOrigin(origin=%q, host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

func TestRequestCrossOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		refer  string
		host   string
		want   bool
	}{
		{"no headers is not cross-origin", "", "", "127.0.0.1:8787", false},
		{"same origin", "http://127.0.0.1:8787", "", "127.0.0.1:8787", false},
		{"cross origin", "http://evil.com", "", "127.0.0.1:8787", true},
		{"substring is cross origin", "http://127.0.0.1:8787.evil.com", "", "127.0.0.1:8787", true},
		{"cross referer when no origin", "", "http://evil.com/page", "127.0.0.1:8787", true},
		{"same referer when no origin", "", "http://127.0.0.1:8787/x", "127.0.0.1:8787", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/actions", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.refer != "" {
				r.Header.Set("Referer", tc.refer)
			}
			if got := requestCrossOrigin(r); got != tc.want {
				t.Fatalf("requestCrossOrigin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidSessionToken(t *testing.T) {
	s := &Server{sessionToken: "secret-token"}
	mk := func(setup func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/actions", nil)
		setup(r)
		return r
	}
	if !s.validSessionToken(mk(func(r *http.Request) { r.Header.Set(sessionTokenHeader, "secret-token") })) {
		t.Fatal("valid header token should pass")
	}
	if !s.validSessionToken(mk(func(r *http.Request) { r.URL.RawQuery = "token=secret-token" })) {
		t.Fatal("valid query token should pass")
	}
	if s.validSessionToken(mk(func(r *http.Request) { r.Header.Set(sessionTokenHeader, "wrong") })) {
		t.Fatal("wrong token must fail")
	}
	if s.validSessionToken(mk(func(r *http.Request) {})) {
		t.Fatal("missing token must fail")
	}
	// Fail closed when no token was minted.
	empty := &Server{sessionToken: ""}
	if empty.validSessionToken(mk(func(r *http.Request) { r.Header.Set(sessionTokenHeader, "") })) {
		t.Fatal("server with no token must reject everything")
	}
}

func TestApiRouteNeedsToken(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/actions", true},
		{http.MethodPut, "/api/tasks/x/brief", true},
		{http.MethodDelete, "/api/owners/x", true},
		{http.MethodGet, "/api/tasks", false},
		{http.MethodGet, "/api/fs/entries", true}, // sensitive read — gated regardless of method
		{http.MethodPost, "/api/fs/mkdir", true},
		{http.MethodPost, "/api/github/webhook", false},  // HMAC-authed, exempt
		{http.MethodPost, "/api/clickup/webhook", false}, // HMAC-authed, exempt
		{http.MethodPost, "/api/hooks/agent", false},     // localhost hook, exempt
		{http.MethodPost, "/api/inbox/notify", false},    // localhost wake poke, exempt
	}
	for _, tc := range cases {
		if got := apiRouteNeedsToken(tc.method, tc.path); got != tc.want {
			t.Errorf("apiRouteNeedsToken(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestInjectSessionToken(t *testing.T) {
	html := []byte("<html><head><title>x</title></head><body></body></html>")
	out := string(injectSessionToken(html, "abc123"))
	if !strings.Contains(out, `window.__FLOW_TOKEN__="abc123";`) {
		t.Fatalf("token not injected: %s", out)
	}
	if !strings.Contains(out, "<title>x</title>") || !strings.Contains(out, "</head>") {
		t.Fatalf("original head clobbered: %s", out)
	}
	// Empty token is a no-op.
	if got := injectSessionToken(html, ""); string(got) != string(html) {
		t.Fatal("empty token should leave html unchanged")
	}
}

// TestDataPlaneAuthMiddleware exercises the real Handler() (no test wrapper) to
// verify the P0-1 gate end to end.
func TestDataPlaneAuthMiddleware(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test", CommandPath: "/bin/false"})
	h := s.Handler()

	do := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	t.Run("state-changing POST without token is 403", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(`{"kind":"x"}`))
		if rec := do(r); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("state-changing POST with token passes the gate", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(`{"kind":"definitely-not-real"}`))
		r.Header.Set(sessionTokenHeader, s.sessionToken)
		rec := do(r)
		// Past the gate: the action handler rejects the unknown kind with 400,
		// never the auth 403.
		if rec.Code == http.StatusForbidden {
			t.Fatalf("authorized request was rejected by the gate: %s", rec.Body.String())
		}
	})

	t.Run("cross-origin POST is 403 even with token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(`{"kind":"x"}`))
		r.Header.Set(sessionTokenHeader, s.sessionToken)
		r.Header.Set("Origin", "http://evil.com")
		if rec := do(r); rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin status = %d, want 403", rec.Code)
		}
	})

	t.Run("GET read needs no token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		if rec := do(r); rec.Code == http.StatusForbidden {
			t.Fatalf("GET /api/health should not require a token: %s", rec.Body.String())
		}
	})

	t.Run("webhook POST is exempt from the token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/github/webhook", strings.NewReader("{}"))
		rec := do(r)
		// May fail HMAC (503/401) but must not be blocked by the session-token gate.
		if strings.Contains(rec.Body.String(), "session token") {
			t.Fatalf("webhook should be exempt from the token gate: %s", rec.Body.String())
		}
	})

	t.Run("clickup webhook POST is exempt from the token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/clickup/webhook", strings.NewReader("{}"))
		rec := do(r)
		if strings.Contains(rec.Body.String(), "session token") {
			t.Fatalf("ClickUp webhook should be exempt from the token gate: %s", rec.Body.String())
		}
	})
}

// TestWSHandshakeAuth dials a real /ws/events socket to confirm the strict
// origin + token gate on the live upgrade path.
func TestWSHandshakeAuth(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test", CommandPath: "/bin/false"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	wsBase := "ws://" + host + "/ws/events"
	origin := "http://" + host

	dial := func(rawURL string, header http.Header) (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial(rawURL, header)
	}

	t.Run("missing token rejected", func(t *testing.T) {
		_, resp, err := dial(wsBase, http.Header{"Origin": {origin}})
		if err == nil {
			t.Fatal("handshake without token should fail")
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("want 403, got resp=%v err=%v", resp, err)
		}
	})

	t.Run("empty origin rejected even with token", func(t *testing.T) {
		_, _, err := dial(wsBase+"?token="+url.QueryEscape(s.sessionToken), http.Header{})
		if err == nil {
			t.Fatal("handshake with empty Origin should fail")
		}
	})

	t.Run("cross origin rejected even with token", func(t *testing.T) {
		_, resp, err := dial(wsBase+"?token="+url.QueryEscape(s.sessionToken), http.Header{"Origin": {"http://evil.com"}})
		if err == nil {
			t.Fatal("cross-origin handshake should fail")
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("want 403, got resp=%v err=%v", resp, err)
		}
	})

	t.Run("valid origin and token upgrades", func(t *testing.T) {
		conn, _, err := dial(wsBase+"?token="+url.QueryEscape(s.sessionToken), http.Header{"Origin": {origin}})
		if err != nil {
			t.Fatalf("valid handshake should succeed: %v", err)
		}
		conn.Close()
	})
}
