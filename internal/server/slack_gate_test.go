package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func postJSONTo(t *testing.T, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// The external-channel send gate: a send to a channel outside the operator's org
// must be PARKED (202, pending row) not posted; an internal send goes straight
// out; and the operator approving a parked send posts it.
func TestSlackSendExternalGate(t *testing.T) {
	root := t.TempDir()
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	s := New(Config{DB: db, FlowRoot: root})

	// Observe the real sender without hitting Slack.
	var sent []string
	origText := slackTextSendFn
	slackTextSendFn = func(channel, threadTS, text, identity string) error {
		sent = append(sent, channel+"|"+text)
		return nil
	}
	defer func() { slackTextSendFn = origText }()

	// Seed the gate verdict cache so classification needs no Slack API call.
	s.slackExtCache.set("C_EXT", slackExtVerdict{external: true, reason: "Slack Connect: external organization present", label: "#partner", resolved: true})
	s.slackExtCache.set("C_INT", slackExtVerdict{external: false, resolved: true})

	// External → gated.
	rec := postJSONTo(t, s.handleSlackSend, "/api/slack/send", map[string]any{
		"channel": "C_EXT", "thread_ts": "1.1", "text": "hi partner", "as": "user",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("external send: code=%d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if len(sent) != 0 {
		t.Fatalf("external send must NOT post directly; sent=%v", sent)
	}
	pending, _ := productdb.ListPendingSends(db, "pending")
	if len(pending) != 1 || pending[0].Channel != "C_EXT" || pending[0].Text != "hi partner" {
		t.Fatalf("expected one pending external send; got %+v", pending)
	}
	pendID := pending[0].ID

	// Internal → posts immediately.
	rec = postJSONTo(t, s.handleSlackSend, "/api/slack/send", map[string]any{
		"channel": "C_INT", "thread_ts": "2.2", "text": "hi team",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("internal send: code=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(sent) != 1 || sent[0] != "C_INT|hi team" {
		t.Fatalf("internal send should post; sent=%v", sent)
	}

	// Operator approves the parked external send → it posts and is marked sent.
	rec = postJSONTo(t, s.handleSlackPendingDecide, "/api/slack/pending/decide", map[string]any{
		"id": pendID, "action": "send",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: code=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(sent) != 2 || sent[1] != "C_EXT|hi partner" {
		t.Fatalf("approval must post the parked message; sent=%v", sent)
	}
	if rest, _ := productdb.ListPendingSends(db, "pending"); len(rest) != 0 {
		t.Fatalf("approved send should leave the pending queue; got %d", len(rest))
	}
}
