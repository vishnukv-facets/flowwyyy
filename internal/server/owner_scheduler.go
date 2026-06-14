package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ownerScheduler is the in-process heartbeat that drives due owner ticks while
// `flow ui serve` is up — the owner twin of playbookScheduler. Each tick it
// shells out to `flow <CommandPath> owner tick-due`, which finds owners whose
// next wake has passed and fires each as an autonomous tick. The CLI owns all
// due-detection, the overlap guard (skip an owner whose prior tick is still
// live), and advancing next_wake; the server is only the cron driver, mirroring
// how createPlaybook and other mutations shell out to the flow binary rather
// than duplicating the command surface in the server.
//
// Why this exists: the `flow owner tick-due` entry point shipped, but nothing
// called it on a cadence — only playbooks had an in-process scheduler — so an
// owner whose next_wake went "due" never fired and only ran when ticked by hand
// in the UI. A boot tick also catches up owners that came due while the server
// was down or the laptop was asleep.
//
// A `flow owner tick-due` CLI entry remains available for host cron/launchd when
// the server isn't running; the two never conflict because tick-due is
// idempotent (it advances next_wake atomically and an overlap guard skips an
// owner whose prior tick is still live).
type ownerScheduler struct {
	srv      *Server
	interval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const defaultOwnerSchedulerInterval = 60 * time.Second

// newOwnerScheduler returns a scheduler, or nil when disabled
// (FLOW_OWNER_SCHEDULER=off) or when there's no flow binary to invoke.
func newOwnerScheduler(srv *Server) *ownerScheduler {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLOW_OWNER_SCHEDULER")), "off") {
		return nil
	}
	if srv == nil || strings.TrimSpace(srv.cfg.CommandPath) == "" {
		return nil
	}
	interval := defaultOwnerSchedulerInterval
	if v := strings.TrimSpace(os.Getenv("FLOW_OWNER_SCHEDULER_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	return &ownerScheduler{srv: srv, interval: interval}
}

func (o *ownerScheduler) start() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	o.cancel = cancel
	o.done = done
	// Pass the channel into loop rather than letting it read o.done: stop() nils
	// o.done, and a `defer close(o.done)` in loop would evaluate that field — if
	// stop() races ahead before loop's first line, it would close(nil) and panic.
	go o.loop(ctx, done)
}

func (o *ownerScheduler) stop() {
	o.mu.Lock()
	cancel := o.cancel
	done := o.done
	o.cancel = nil
	o.done = nil
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (o *ownerScheduler) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	// Tick immediately on boot so owners that came due while the server was down
	// (or the laptop was asleep) fire once on startup (catch-up), then cadence.
	o.tick(ctx)
	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			o.tick(ctx)
		}
	}
}

func (o *ownerScheduler) tick(ctx context.Context) {
	if o.srv == nil || strings.TrimSpace(o.srv.cfg.CommandPath) == "" {
		return
	}
	// Bound a single sweep so a wedged invocation can't pin the loop forever.
	// tick-due only detects due owners and launches each tick detached (Setsid),
	// so it returns quickly; the ceiling is a backstop, not the normal duration.
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, o.srv.cfg.CommandPath, "owner", "tick-due")
	cmd.Env = append(os.Environ(), "FLOW_ROOT="+o.srv.cfg.FlowRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: owner tick-due: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
}
