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

// TestHandleRemoteDeviceDelete verifies the local delete handler removes a
// device row entirely (the fix for revoked devices piling up with no way to
// clear them) and validates input.
func TestHandleRemoteDeviceDelete(t *testing.T) {
	s := newTestServer(t)
	now := flowdb.NowISO()
	exp := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := flowdb.InsertRemoteDevice(s.cfg.DB, "dev1", "iPhone", "hashAAA", now, exp); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// Missing id → 400, nothing deleted.
	rec := httptest.NewRecorder()
	s.handleRemoteDeviceDelete(rec, httptest.NewRequest("POST", "/api/remote/devices/delete", strings.NewReader(`{}`)))
	if rec.Code != 400 {
		t.Fatalf("empty id: got %d want 400", rec.Code)
	}

	// Valid id → 200, row gone from the list.
	rec = httptest.NewRecorder()
	s.handleRemoteDeviceDelete(rec, httptest.NewRequest("POST", "/api/remote/devices/delete", strings.NewReader(`{"id":"dev1"}`)))
	if rec.Code != 200 {
		t.Fatalf("delete: got %d body=%s", rec.Code, rec.Body.String())
	}
	list, _ := flowdb.ListRemoteDevices(s.cfg.DB)
	if len(list) != 0 {
		t.Fatalf("expected 0 devices after delete, got %d", len(list))
	}
}

func TestRemoteForbiddenRPCPath(t *testing.T) {
	for _, p := range []string{"/api/remote/pair-code", "/api/remote/devices", "/api/remote/devices/revoke", "/api/remote/devices/delete", "/api/remote/enable", "/api/remote/status"} {
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

// TestInjectRemoteFlagAddsMarkerNoToken verifies the remote-shell HTML
// transform directly (no dependency on the embedded bundle): it inserts the
// window.__FLOW_REMOTE__ marker before </head> and never references the
// session-token global. The marker assertion lives here — not in the
// handler test below — because handleRemoteStatic only injects it when
// static/index.html is present in the embed, which CI's `go test` does not
// build (the bundle is gitignored / produced by `make ui`).
func TestInjectRemoteFlagAddsMarkerNoToken(t *testing.T) {
	in := []byte("<!doctype html><html><head><title>x</title></head><body></body></html>")
	out := string(injectRemoteFlag(in))
	if !strings.Contains(out, "__FLOW_REMOTE__") {
		t.Fatal("injectRemoteFlag must add the window.__FLOW_REMOTE__ marker")
	}
	if strings.Contains(out, "__FLOW_TOKEN__") {
		t.Fatal("remote shell must not reference the session-token global")
	}
}

// TestRemoteStaticDoesNotLeakSessionToken asserts the security-critical
// property: a remotely-served response NEVER contains the shared session
// token. This holds whether handleRemoteStatic serves real HTML (local, with
// a built bundle) or a 500 fallback (CI, no bundle) — neither must carry the
// token. (The remote-flag marker is verified in the test above.)
func TestRemoteStaticDoesNotLeakSessionToken(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleRemoteStatic(rec, httptest.NewRequest("GET", "/", nil))
	if s.sessionToken != "" && strings.Contains(rec.Body.String(), s.sessionToken) {
		t.Fatal("remote static must NOT embed the shared session token")
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
