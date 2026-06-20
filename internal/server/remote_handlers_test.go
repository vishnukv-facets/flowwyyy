package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
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
