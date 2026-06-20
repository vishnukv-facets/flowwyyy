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

// TestHandleRemoteDisableSetsFalse verifies that POST /api/remote/disable
// returns 200 and leaves remoteAccessEnabled() returning false.
func TestHandleRemoteDisableSetsFalse(t *testing.T) {
	// Seed env so we start with remote access on, then disable it.
	t.Setenv("FLOW_REMOTE_ACCESS", "1")
	s := newTestServer(t)

	req := httptest.NewRequest("POST", "/api/remote/disable", nil)
	rec := httptest.NewRecorder()
	s.handleRemoteDisable(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("disable: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if remoteAccessEnabled() {
		t.Fatal("remoteAccessEnabled() must be false after disable")
	}
	// Config file must also reflect the change.
	cfg := loadConfigFile(s.configPath())
	if v := cfg["FLOW_REMOTE_ACCESS"]; v != "0" {
		t.Fatalf("config FLOW_REMOTE_ACCESS: got %q want \"0\"", v)
	}
}

// TestHandleRemoteEnableConflictWhenNotZrok verifies that POST /api/remote/enable
// returns 409 when FLOW_INGRESS_PROVIDER is not zrok (or auto-start is off).
func TestHandleRemoteEnableConflictWhenNotZrok(t *testing.T) {
	// Ensure provider is not zrok.
	t.Setenv("FLOW_INGRESS_PROVIDER", "none")
	t.Setenv("FLOW_REMOTE_ACCESS", "0")
	s := newTestServer(t)

	req := httptest.NewRequest("POST", "/api/remote/enable", nil)
	rec := httptest.NewRecorder()
	s.handleRemoteEnable(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("enable without zrok: got %d want 409, body=%s", rec.Code, rec.Body.String())
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
