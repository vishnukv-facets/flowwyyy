package server

import (
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
