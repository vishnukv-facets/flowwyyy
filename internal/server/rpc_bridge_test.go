package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// The WS-RPC bridge is the UI's entire data plane, so guard its dispatch
// contract directly (no websocket needed — dispatchRPC is the core).
func TestDispatchRPCRoutesThroughAPIHandler(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, Version: "test", CommandPath: "/bin/false"})

	t.Run("GET json endpoint returns embedded JSON", func(t *testing.T) {
		resp := srv.dispatchRPC(rpcRequest{ID: "1", Method: "GET", Path: "/api/health"})
		if resp.ID != "1" || resp.Status != 200 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if !strings.Contains(resp.ContentType, "application/json") {
			t.Fatalf("content type = %q", resp.ContentType)
		}
		var body struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(resp.JSON, &body); err != nil {
			t.Fatalf("decode json: %v (raw %s)", err, resp.JSON)
		}
		if !body.OK || body.Version != "test" {
			t.Fatalf("health body = %+v", body)
		}
		if resp.Text != "" {
			t.Fatalf("json response should not populate Text: %q", resp.Text)
		}
	})

	t.Run("non-api path is refused", func(t *testing.T) {
		resp := srv.dispatchRPC(rpcRequest{ID: "2", Method: "GET", Path: "/etc/passwd"})
		if resp.Status != 404 || resp.Error == "" {
			t.Fatalf("expected 404 refusal, got %+v", resp)
		}
	})

	t.Run("POST forwards JSON body to the action handler", func(t *testing.T) {
		resp := srv.dispatchRPC(rpcRequest{
			ID:     "3",
			Method: "POST",
			Path:   "/api/actions",
			Body:   json.RawMessage(`{"kind":"definitely-not-a-real-action"}`),
		})
		// Body reached runAction, which rejects unknown kinds with a JSON error.
		if resp.Status != 400 {
			t.Fatalf("status = %d, want 400 (resp %+v)", resp.Status, resp)
		}
		if !strings.Contains(string(resp.JSON), "definitely-not-a-real-action") {
			t.Fatalf("action error not echoed: %s", resp.JSON)
		}
	})

	t.Run("raw text body uses given content type", func(t *testing.T) {
		// buildRPCBody is the half that turns a frame into an HTTP body; verify
		// the raw-text branch (used by markdown brief PUTs) directly.
		text := "# hi\n"
		reader, ct, err := buildRPCBody(rpcRequest{Text: &text})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ct, "text/markdown") {
			t.Fatalf("content type = %q", ct)
		}
		buf := make([]byte, 16)
		n, _ := reader.Read(buf)
		if string(buf[:n]) != text {
			t.Fatalf("body = %q", buf[:n])
		}
	})

	t.Run("base64 files become a multipart body", func(t *testing.T) {
		_, ct, err := buildRPCBody(rpcRequest{
			Form:  map[string]string{"kind": "create-flow"},
			Files: []rpcFile{{Field: "images", Name: "a.png", Data: "AAAA"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Fatalf("content type = %q, want multipart/form-data", ct)
		}
	})
}
