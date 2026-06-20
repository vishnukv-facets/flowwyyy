package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
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
