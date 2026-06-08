// internal/steering/session.go
package steering

import (
	"context"
	"crypto/rand"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// classifierExec runs `claude` with the given args and returns stdout. The one
// mockable seam for the session pool — tests inject a fake that records args.
type classifierExec func(ctx context.Context, args []string) (string, error)

func defaultClassifierExec(ctx context.Context, args []string) (string, error) {
	out, err := exec.CommandContext(ctx, "claude", args...).Output()
	if err != nil {
		return "", commandError("steering: pooled classifier claude", err, out)
	}
	return string(out), nil
}

// classifierPool manages reusable, primed claude sessions for the cheap
// classifier stages. One session per mode; the heavy framing (instructions +
// task index) is sent once at creation and reused across turns via --resume,
// so each subsequent call sends only the compact payload.
type classifierPool struct {
	maxTurns int
	ttl      time.Duration
	now      func() time.Time
	newID    func() string
	exec     classifierExec

	execMu   sync.Mutex
	mu       sync.Mutex
	sessions map[string]*sessionSlot
}

type sessionSlot struct {
	id        string
	turns     int
	startedAt time.Time
	primeKey  string
}

func newClassifierPool(maxTurns int, ttl time.Duration) *classifierPool {
	if maxTurns <= 0 {
		maxTurns = 40
	}
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	return &classifierPool{
		maxTurns: maxTurns, ttl: ttl,
		now: time.Now, newID: randomUUID, exec: defaultClassifierExec,
		sessions: map[string]*sessionSlot{},
	}
}

// run executes one classifier turn for mode. prime is the heavy framing sent
// only when (re)creating the session; payload is the compact per-call text
// sent every turn. primeKey rotates the session when the primed context
// changes (pass a stable string for static primes).
func (p *classifierPool) run(ctx context.Context, mode, prime, payload, primeKey string) (string, error) {
	p.execMu.Lock()
	defer p.execMu.Unlock()

	p.mu.Lock()
	slot := p.sessions[mode]
	fresh := slot == nil ||
		slot.turns >= p.maxTurns ||
		p.now().Sub(slot.startedAt) >= p.ttl ||
		slot.primeKey != primeKey
	var args []string
	if fresh {
		slot = &sessionSlot{id: p.newID(), startedAt: p.now(), primeKey: primeKey}
		p.sessions[mode] = slot
		args = []string{"-p", prime + "\n\n" + payload, "--model", classifierModel(), "--dangerously-skip-permissions", "--session-id", slot.id}
	} else {
		args = []string{"-p", payload, "--model", classifierModel(), "--dangerously-skip-permissions", "--resume", slot.id}
	}
	id := slot.id
	p.mu.Unlock()

	out, err := p.exec(ctx, args)

	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sessions[mode]
	if err != nil {
		if cur != nil && cur.id == id { // only reset if still our session
			delete(p.sessions, mode)
		}
		return "", err
	}
	if cur != nil && cur.id == id {
		cur.turns++
	}
	return out, nil
}

// randomUUID returns a random v4 UUID string (8-4-4-4-12 hex), required by
// `claude --session-id`. Uses crypto/rand.
func randomUUID() string {
	// 16 random bytes, set version (4) and variant (10xx) bits, format as UUID.
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
