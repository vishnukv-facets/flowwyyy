package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file locks down the unauthenticated local data plane (audit finding
// P0-1). Two layers work together:
//
//  1. Strict same-origin enforcement. The old WS CheckOrigin accepted an empty
//     Origin and used a substring match (`strings.Contains(origin, r.Host)`),
//     so a non-browser client OR a page served from 127.0.0.1:8787.evil.com
//     sailed through. checkLocalWSOrigin rejects empty Origin and requires an
//     exact host match; requestCrossOrigin applies the same idea to direct
//     /api/* HTTP requests so a page the operator merely visits can't drive the
//     data plane via fetch()/forms either.
//
//  2. A localhost-minted session token. A non-browser local process can forge
//     Origin, so origin checks alone don't stop it. The token — minted with
//     crypto/rand at boot, never leaving the loopback host — gates every WS
//     handshake and every state-changing /api/* route. The browser UI reads it
//     from window.__FLOW_TOKEN__ (injected into index.html, unreadable
//     cross-origin); trusted local CLIs read it from a 0600 file under FlowRoot.

// sessionTokenHeader carries the data-plane session token on direct HTTP
// requests. A custom header is CSRF-safe: a cross-site simple request can't set
// it, and adding it forces a CORS preflight cross-origin JS can't satisfy.
const sessionTokenHeader = "X-Flow-Session-Token"

// SessionTokenFileName is the 0600 file under FlowRoot where the running server
// writes its minted token. Trusted local CLIs (flow wait / slack send /
// attention sent) read it to authenticate to token-gated routes and the WS
// handshake. Exported so internal/app can locate it without re-deriving.
const SessionTokenFileName = ".ui-session-token"

// mintSessionToken returns a fresh 256-bit token as hex. On the (catastrophic)
// event that crypto/rand fails, it returns "" — which makes validSessionToken
// reject everything, i.e. the data plane fails closed rather than open.
func mintSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func sessionTokenFilePath(flowRoot string) string {
	root := strings.TrimSpace(flowRoot)
	if root == "" {
		return ""
	}
	return filepath.Join(root, SessionTokenFileName)
}

// writeSessionTokenFile persists the minted token (0600) so trusted local CLIs
// can authenticate. Best-effort: a write failure only means those CLIs fall
// back to their unauthenticated behavior (e.g. flow tell still lands the
// message on disk; the wake poke just gets a 403).
func (s *Server) writeSessionTokenFile() {
	path := sessionTokenFilePath(s.cfg.FlowRoot)
	if path == "" || s.sessionToken == "" {
		return
	}
	if err := os.WriteFile(path, []byte(s.sessionToken), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write session token file: %v\n", err)
	}
}

// validSessionToken reports whether the request carries the server's session
// token, in the X-Flow-Session-Token header or (for WS handshakes, where
// browsers can't set custom headers) the `token` query parameter. Fails closed
// when no token was minted.
func (s *Server) validSessionToken(r *http.Request) bool {
	if s == nil || s.sessionToken == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get(sessionTokenHeader))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.sessionToken)) == 1
}

// checkLocalWSOrigin is the WebSocket upgrade origin gate (shared by every
// /ws/* endpoint via terminalUpgrader). It REJECTS an empty Origin — every
// browser sends one on a WS handshake, so a missing Origin is a non-browser
// client trying to ride the data plane — and requires url.Parse(origin).Host to
// EXACTLY equal r.Host (no substring). This is the headline P0-1 fix.
func checkLocalWSOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// requestCrossOrigin reports whether a direct HTTP request carries an Origin (or
// Referer) from a different host than r.Host. A truthful Origin that mismatches
// means a page the operator visited is trying to drive the data plane — reject.
// A request with no Origin/Referer is not browser-driven (CLI, agent hook,
// server-to-server); it passes this gate and is constrained by the token gate.
func requestCrossOrigin(r *http.Request) bool {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return true
		}
		return !strings.EqualFold(u.Host, r.Host)
	}
	// Fall back to Referer when Origin is absent (some browsers omit Origin on
	// same-origin GETs but still send Referer on cross-site navigations).
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return true
		}
		return !strings.EqualFold(u.Host, r.Host)
	}
	return false
}

// tokenExemptAPIPath lists the /api/* routes that must NOT require the session
// token because their only callers can't supply it. They are still protected
// from the browser threat by the cross-origin gate (requestCrossOrigin), and
// each carries its own auth or is low-impact:
//   - /api/github/webhook   — external GitHub deliveries; HMAC-authenticated.
//   - /api/hooks/agent       — localhost agent-event hook, fired on every tool
//     use by flow-spawned (and ambient) sessions; records runtime state only.
//   - /api/inbox/notify      — localhost wake poke from `flow tell` and the
//     in-process steerer; the durable write already happened on disk.
//   - OAuth/setup callbacks  — GET with their own state-nonce validation.
func tokenExemptAPIPath(path string) bool {
	switch path {
	case "/api/github/webhook", "/api/hooks/agent", "/api/inbox/notify",
		slackOAuthCallbackPath, githubSetupCallbackPath:
		return true
	}
	return false
}

// apiRouteNeedsToken reports whether a direct-HTTP /api/* request must present
// the session token: every state-changing method, plus the filesystem browser
// (which enumerates and creates arbitrary paths) regardless of method. The
// browser UI never reaches these over direct HTTP — it uses the token-gated
// /ws/rpc bridge — so this is invisible to the UI and only constrains
// non-browser callers.
func apiRouteNeedsToken(method, path string) bool {
	if tokenExemptAPIPath(path) {
		return false
	}
	// All remote-access management endpoints are localhost-only operator
	// actions (pairing, enable/disable, device list/revoke) — require the
	// session token regardless of method so a stray same-origin GET can't read
	// the device list over direct HTTP. The remote-mux pairing-REDEMPTION
	// endpoint lives on a different mux and never reaches this gate.
	if strings.HasPrefix(path, "/api/remote/") {
		return true
	}
	if path == "/api/fs/entries" || path == "/api/fs/mkdir" {
		return true
	}
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// dataPlaneAuth wraps the public HTTP handler. It guards only /api/* — static
// assets and /ws/* upgrades enforce their own checks (the upgrader's
// checkLocalWSOrigin plus an in-handler token check). The /ws/rpc bridge
// dispatches through the separate, unwrapped apiHandler(), so RPC calls are
// trusted post-handshake and never hit this middleware.
func (s *Server) dataPlaneAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if requestCrossOrigin(r) {
			writeError(w, errors.New("cross-origin request rejected"), http.StatusForbidden)
			return
		}
		if apiRouteNeedsToken(r.Method, r.URL.Path) && !s.validSessionToken(r) {
			writeError(w, errors.New("missing or invalid session token"), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeWSHandshake is the per-handler token gate for /ws/* endpoints. The
// origin half is enforced by the upgrader's checkLocalWSOrigin; this rejects a
// handshake lacking a valid token before the connection is upgraded. Browsers
// can't set custom headers on a WS handshake, so the token rides the `token`
// query parameter (see validSessionToken).
func (s *Server) authorizeWSHandshake(w http.ResponseWriter, r *http.Request) bool {
	if s.validSessionToken(r) {
		return true
	}
	writeError(w, errors.New("missing or invalid session token"), http.StatusForbidden)
	return false
}

// injectSessionToken inserts the minted token into served index.html as a
// global the SPA reads (window.__FLOW_TOKEN__). A cross-origin page can't read
// this response body (same-origin policy), and can't open the WS anyway
// (checkLocalWSOrigin), so inlining is safe. No-op when the token is empty or no
// insertion point is found.
func injectSessionToken(html []byte, token string) []byte {
	if token == "" {
		return html
	}
	tag := []byte("<script>window.__FLOW_TOKEN__=" + strconv.Quote(token) + ";</script>")
	if i := bytes.Index(html, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(html)+len(tag))
		out = append(out, html[:i]...)
		out = append(out, tag...)
		out = append(out, html[i:]...)
		return out
	}
	return append(tag, html...)
}
