package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

const (
	// remoteDeviceTokenTTL bounds how long a QR-paired device token is valid.
	// After this the phone must re-pair (scan a fresh QR from the laptop), which
	// caps a lost phone's exposure at 12h even before the operator revokes it.
	remoteDeviceTokenTTL = 12 * time.Hour
	// pairingCodeTTL is the window to scan the QR before the code expires.
	pairingCodeTTL = 5 * time.Minute
)

// mintRemoteToken returns a fresh 256-bit token as hex, or "" if crypto/rand
// fails (callers treat "" as failure and refuse to pair, i.e. fail closed).
func mintRemoteToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// hashRemoteToken returns the SHA-256 hex of a token. Only the hash is stored;
// the presented token is hashed before the DB lookup so no secret-dependent
// string comparison happens in the app.
func hashRemoteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// pairingStore holds short-lived, single-use pairing codes in memory. Codes are
// not persisted — a server restart drops pending codes, which is acceptable for
// a 5-minute window. Methods are safe for concurrent use.
type pairingStore struct {
	mu    sync.Mutex
	codes map[string]time.Time // code -> expiry
}

func newPairingStore() *pairingStore {
	return &pairingStore{codes: make(map[string]time.Time)}
}

func (p *pairingStore) createAt(now time.Time) (string, time.Time) {
	code := mintRemoteToken()
	exp := now.Add(pairingCodeTTL)
	p.mu.Lock()
	p.codes[code] = exp
	// Opportunistic GC of expired codes so the map can't grow unbounded.
	for c, e := range p.codes {
		if now.After(e) {
			delete(p.codes, c)
		}
	}
	p.mu.Unlock()
	return code, exp
}

// redeemAt consumes a code: returns true exactly once, only if the code exists
// and has not expired. The code is deleted on any matched lookup (single-use).
func (p *pairingStore) redeemAt(code string, now time.Time) bool {
	if code == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.codes[code]
	if !ok {
		return false
	}
	delete(p.codes, code)
	return !now.After(exp)
}

// remoteFlagHeader marks an upgraded/forwarded request as having arrived over
// the remote (device-token) surface. handleRPCWebSocket reads it to deny
// device-management RPC paths to remote clients (see rpc_bridge.go).
const remoteFlagHeader = "X-Flow-Remote"

// validRemoteDeviceToken extracts a device token (X-Flow-Session-Token header or
// ?token= query — the same transport the browser uses for the session token),
// hashes it, looks up the device, and accepts it only when the row exists, is
// not revoked, and has not expired. Best-effort last-seen touch. Fails closed.
func (s *Server) validRemoteDeviceToken(r *http.Request) (*flowdb.RemoteDevice, bool) {
	if s == nil || s.cfg.DB == nil {
		return nil, false
	}
	got := strings.TrimSpace(r.Header.Get(sessionTokenHeader))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got == "" {
		return nil, false
	}
	dev, err := flowdb.GetRemoteDeviceByTokenHash(s.cfg.DB, hashRemoteToken(got))
	if err != nil || dev == nil {
		return nil, false
	}
	if dev.RevokedAt.Valid {
		return nil, false
	}
	exp, err := time.Parse(time.RFC3339, dev.ExpiresAt)
	if err != nil || time.Now().After(exp) {
		return nil, false
	}
	_ = flowdb.TouchRemoteDeviceLastSeen(s.cfg.DB, dev.ID, flowdb.NowISO())
	return dev, true
}

// remoteAuth gates the remote-app surface. On a valid device token it marks the
// request remote and INJECTS the shared session token (header + ?token=) so the
// existing WS/RPC handlers — which check the session token via
// authorizeWSHandshake — work unchanged. The session token is injected only
// into the server-side request; it is never sent back to the client.
func (s *Server) remoteAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.validRemoteDeviceToken(r); !ok {
			writeError(w, errors.New("missing or invalid device token"), http.StatusForbidden)
			return
		}
		r.Header.Set(remoteFlagHeader, "1")
		r.Header.Set(sessionTokenHeader, s.sessionToken)
		q := r.URL.Query()
		q.Set("token", s.sessionToken)
		r.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r)
	})
}
