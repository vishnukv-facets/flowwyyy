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

// playbookScheduler is the in-process heartbeat that drives scheduled playbook
// runs while `flow ui serve` is up. Each tick it shells out to
// `flow <CommandPath> playbook tick-due`, which finds due playbooks and fires
// each as an autonomous run. The CLI owns all due-detection, overlap, and
// firing logic — the server is only a cron driver, mirroring how createPlaybook
// and other mutations shell out to the flow binary rather than duplicating the
// command surface in the server.
//
// A `flow playbook tick-due` CLI entry remains available for host cron/launchd
// when the server isn't running; the two never conflict because tick-due is
// idempotent (it advances next_fire_at atomically and an overlap guard skips a
// playbook whose prior run is still live).
type playbookScheduler struct {
	srv      *Server
	interval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const defaultPlaybookSchedulerInterval = 60 * time.Second

// newPlaybookScheduler returns a scheduler, or nil when disabled
// (FLOW_PLAYBOOK_SCHEDULER=off) or when there's no flow binary to invoke.
func newPlaybookScheduler(srv *Server) *playbookScheduler {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FLOW_PLAYBOOK_SCHEDULER")), "off") {
		return nil
	}
	if srv == nil || strings.TrimSpace(srv.cfg.CommandPath) == "" {
		return nil
	}
	interval := defaultPlaybookSchedulerInterval
	if v := strings.TrimSpace(os.Getenv("FLOW_PLAYBOOK_SCHEDULER_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	return &playbookScheduler{srv: srv, interval: interval}
}

func (p *playbookScheduler) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	// Pass the channel into loop rather than reading p.done there: stop() nils
	// p.done, so a racing `defer close(p.done)` could close(nil) and panic.
	go p.loop(ctx, done)
}

func (p *playbookScheduler) stop() {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (p *playbookScheduler) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	// Tick immediately on boot so schedules that came due while the server was
	// down fire once on startup (catch-up), then follow the cadence.
	p.tick(ctx)
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.tick(ctx)
		}
	}
}

func (p *playbookScheduler) tick(ctx context.Context) {
	if p.srv == nil || strings.TrimSpace(p.srv.cfg.CommandPath) == "" {
		return
	}
	// Bound a single tick so a wedged invocation can't pin the loop forever.
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, p.srv.cfg.CommandPath, "playbook", "tick-due")
	cmd.Env = append(os.Environ(), "FLOW_ROOT="+p.srv.cfg.FlowRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: playbook tick-due: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
}
