package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The WebSocket-RPC bridge lets the web UI run every data read and every
// mutation over a single WebSocket instead of HTTP fetch(), satisfying the
// "all connections over websockets" contract. Each client frame names an
// /api/* method+path (plus an optional JSON body or base64 file uploads);
// the bridge replays it through the existing apiHandler() mux via an
// in-memory ResponseWriter and ships the recorded response back, correlated
// by id. No handler logic is duplicated — the REST surface is the RPC
// surface.

// rpcMaxFrameBytes caps a single inbound frame. Generous because attachment
// uploads arrive base64-encoded (≈4/3 overhead) and the action handler
// already enforces the real 50 MiB attachment ceiling downstream.
const rpcMaxFrameBytes = 96 << 20

// rpcRequest is one client→server call. Files (base64) take precedence over
// Form, which takes precedence over Body, mirroring how the HTTP handlers
// distinguish multipart, form, and JSON requests.
type rpcRequest struct {
	ID     string            `json:"id"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Body   json.RawMessage   `json:"body,omitempty"`
	// Text is a raw (non-JSON) request body — used by the markdown brief
	// PUT endpoints, which read the body bytes verbatim. ContentType
	// overrides the header (defaults to text/markdown).
	Text        *string           `json:"text,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Form        map[string]string `json:"form,omitempty"`
	Files       []rpcFile         `json:"files,omitempty"`
}

type rpcFile struct {
	Field       string `json:"field"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        string `json:"data"` // base64-encoded bytes
}

// rpcResponse is one server→client reply. JSON responses are embedded raw in
// JSON so the client parses once; everything else (markdown briefs, the
// ui-data.js bootstrap) rides in Text.
type rpcResponse struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Status      int             `json:"status"`
	ContentType string          `json:"content_type,omitempty"`
	JSON        json.RawMessage `json:"json,omitempty"`
	Text        string          `json:"text,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// rpcRecorder is a minimal http.ResponseWriter that captures a handler's
// status, headers, and body in memory. Lighter than httptest and keeps a
// test helper out of the production import graph.
type rpcRecorder struct {
	status      int
	wroteHeader bool
	header      http.Header
	body        bytes.Buffer
}

func newRPCRecorder() *rpcRecorder {
	return &rpcRecorder{status: http.StatusOK, header: make(http.Header)}
}

func (r *rpcRecorder) Header() http.Header { return r.header }

func (r *rpcRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
}

func (r *rpcRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.body.Write(b)
}

func (s *Server) handleRPCWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeWSHandshake(w, r) {
		return
	}
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(rpcMaxFrameBytes)

	var writeMu sync.Mutex
	writeFrame := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return conn.WriteJSON(v)
	}

	// Greet so the client knows the channel is live and which server it's
	// talking to (handy when reconnecting to a restarted binary).
	_ = writeFrame(map[string]any{
		"type":           "ready",
		"server_version": s.cfg.Version,
		"flow_root":      s.cfg.FlowRoot,
	})

	done := make(chan struct{})
	var closeOnce sync.Once
	stop := func() { closeOnce.Do(func() { close(done) }) }
	defer stop()

	// Heartbeat: keep the socket warm and detect dead peers. The browser
	// answers control pings transparently.
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				writeMu.Lock()
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(data, &req); err != nil {
			_ = writeFrame(rpcResponse{Type: "rpc", Status: http.StatusBadRequest, Error: "malformed rpc frame"})
			continue
		}
		// Dispatch concurrently so a slow handler (e.g. a search reindex)
		// never head-of-line-blocks other in-flight requests on the same
		// socket. Writes stay serialized by writeMu.
		go func(req rpcRequest) {
			resp := s.dispatchRPC(req)
			_ = writeFrame(resp)
		}(req)
	}
}

func (s *Server) dispatchRPC(req rpcRequest) rpcResponse {
	resp := rpcResponse{Type: "rpc", ID: req.ID}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	path := strings.TrimSpace(req.Path)
	if path == "" || !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// The RPC channel only fronts the data plane. Refuse anything that isn't
	// an /api/* route so frames can't reach the websocket upgrade or static
	// handlers.
	if !strings.HasPrefix(path, "/api/") {
		resp.Status = http.StatusNotFound
		resp.Error = "rpc path must be under /api/"
		return resp
	}

	body, contentType, err := buildRPCBody(req)
	if err != nil {
		resp.Status = http.StatusBadRequest
		resp.Error = err.Error()
		return resp
	}

	httpReq, err := http.NewRequest(method, path, body)
	if err != nil {
		resp.Status = http.StatusBadRequest
		resp.Error = err.Error()
		return resp
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpReq.Header.Set("X-Flow-Transport", "ws-rpc")
	httpReq.RemoteAddr = "ws-rpc"

	rec := newRPCRecorder()
	s.apiHandler().ServeHTTP(rec, httpReq)

	ct := rec.header.Get("Content-Type")
	resp.Status = rec.status
	resp.ContentType = ct
	switch {
	case strings.Contains(ct, "application/json"):
		raw := rec.body.Bytes()
		if len(raw) == 0 {
			raw = []byte("null")
		}
		resp.JSON = json.RawMessage(append([]byte(nil), raw...))
	default:
		resp.Text = rec.body.String()
	}
	return resp
}

// buildRPCBody reconstructs the request body for the replayed HTTP request.
// File uploads are rebuilt into a real multipart/form-data body so the
// existing multipart handlers (attachments, create-flow images) work
// unchanged; plain form fields become urlencoded; otherwise a JSON body is
// passed straight through.
func buildRPCBody(req rpcRequest) (io.Reader, string, error) {
	if len(req.Files) > 0 {
		buf := &bytes.Buffer{}
		mw := multipart.NewWriter(buf)
		for key, value := range req.Form {
			if err := mw.WriteField(key, value); err != nil {
				return nil, "", err
			}
		}
		for _, file := range req.Files {
			decoded, err := base64.StdEncoding.DecodeString(file.Data)
			if err != nil {
				return nil, "", err
			}
			field := strings.TrimSpace(file.Field)
			if field == "" {
				field = "files"
			}
			name := strings.TrimSpace(file.Name)
			if name == "" {
				name = "upload"
			}
			part, err := mw.CreateFormFile(field, name)
			if err != nil {
				return nil, "", err
			}
			if _, err := part.Write(decoded); err != nil {
				return nil, "", err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, "", err
		}
		return buf, mw.FormDataContentType(), nil
	}

	if len(req.Form) > 0 {
		values := url.Values{}
		for key, value := range req.Form {
			values.Set(key, value)
		}
		return strings.NewReader(values.Encode()), "application/x-www-form-urlencoded", nil
	}

	if req.Text != nil {
		ct := strings.TrimSpace(req.ContentType)
		if ct == "" {
			ct = "text/markdown; charset=utf-8"
		}
		return strings.NewReader(*req.Text), ct, nil
	}

	if len(req.Body) > 0 {
		return bytes.NewReader(req.Body), "application/json", nil
	}

	return nil, "", nil
}
