package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// monitorPoller is the background goroutine that calls
// `monitor.Poller.Poll("all")` on a fixed cadence so `flow ui serve`
// continuously ingests GitHub signals without the operator
// having to remember to run `flow monitor poll` or hit the UI's Sync
// button. Mirrors livenessReconciler's start/stop lifecycle exactly so
// shutdown is clean and a stuck poll can't outlive the server.
//
// Config precedence (highest to lowest):
//
//  1. Explicit constructor argument from `flow ui serve --monitor-interval ...`
//  2. FLOW_MONITOR_POLL_INTERVAL env (Go duration string, e.g. "60s", "5m")
//  3. defaultMonitorInterval (60s) — keeps the recommended cadence even
//     when neither flag nor env is set, so `flow ui serve` "just works"
//     without configuration.
//
// Setting the interval to 0 (via flag or env) disables the poller — the
// server still serves but doesn't ingest until the operator manually
// triggers `flow monitor poll` or clicks Sync. Useful for tests, CI,
// machines with no internet. Slack ingest runs through Socket Mode instead.
type monitorPoller struct {
	srv      *Server
	interval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// defaultMonitorInterval keeps GitHub polling responsive while Slack moves
// through Socket Mode instead of conversations.history polling.
const defaultMonitorInterval = 60 * time.Second

// monitorPollIntervalEnv is the env var operators set to override the
// default interval without passing a CLI flag. Empty/unset = use the
// default. "0" or "0s" = disable. Anything Go's time.ParseDuration
// accepts is allowed.
const monitorPollIntervalEnv = "FLOW_MONITOR_POLL_INTERVAL"

// newMonitorPoller resolves the interval from (in order) the explicit
// arg, the env var, and the default. Pass interval=-1 to mean "no
// explicit value supplied; fall through to env/default"; pass 0 to
// mean "explicitly disabled". A returned poller with interval==0
// declines to start() — newMonitorPoller never returns nil.
func newMonitorPoller(srv *Server, interval time.Duration) *monitorPoller {
	resolved := interval
	if resolved < 0 {
		// Not explicitly set on the CLI → check env, fall back to default.
		if raw := strings.TrimSpace(os.Getenv(monitorPollIntervalEnv)); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
				resolved = d
			} else {
				fmt.Fprintf(os.Stderr, "warning: %s = %q is not a valid duration; using default %s\n",
					monitorPollIntervalEnv, raw, defaultMonitorInterval)
				resolved = defaultMonitorInterval
			}
		} else {
			resolved = defaultMonitorInterval
		}
	}
	return &monitorPoller{srv: srv, interval: resolved}
}

// start kicks off the background loop. No-op when interval==0 (the
// operator-disabled path) or when the poller is already running.
// Logs a one-line "starting" announcement to stderr so the operator
// has a visible signal that polling is active.
func (m *monitorPoller) start() {
	if m == nil || m.interval == 0 {
		fmt.Fprintln(os.Stderr, "monitor poller: disabled (interval=0)")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	fmt.Fprintf(os.Stderr, "monitor poller: started (interval=%s)\n", m.interval)
	go m.loop(ctx)
}

// stop shuts the loop down and blocks until it returns. Idempotent —
// calling stop on a never-started poller is a no-op. Safe to defer
// from ListenAndServe so SIGINT cleanly drains an in-flight poll.
func (m *monitorPoller) stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.done = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *monitorPoller) loop(ctx context.Context) {
	defer close(m.done)
	// Tick immediately on start so a fresh `flow ui serve` doesn't make
	// the operator wait one full interval before the first GitHub
	// pull. Matches livenessReconciler.loop's first-tick semantics.
	m.tick(ctx)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.tick(ctx)
		}
	}
}

// tick runs one Poll("all") with a per-tick timeout. Errors are logged
// but never returned — a single failed tick must not kill the loop. The
// per-tick timeout is the tick interval itself (or 90s, whichever is
// larger) to prevent a stuck GitHub call from cascading into back-to-back
// tick storms.
func (m *monitorPoller) tick(parent context.Context) {
	if m.srv == nil || m.srv.cfg.DB == nil {
		return
	}
	timeout := m.interval
	if timeout < 90*time.Second {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// Wire BOTH callbacks: OnSyncChange for the per-source "syncing now…"
	// indicator, OnNewEvent for fresh inbox items so the UI updates in
	// real time AND can fire macOS desktop notifications for needs-review
	// items.
	summaries, err := monitor.Poller{
		DB:           m.srv.cfg.DB,
		OnSyncChange: m.srv.publishMonitorSync,
		OnNewEvent:   m.srv.publishInboxItem,
	}.Poll(ctx, "all")
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor poller: poll error: %v\n", err)
		return
	}
	// Surface per-source summaries so the operator sees ingest activity
	// in `flow ui serve` logs. Tight format: one line per source. Skip
	// the noise of "0 events (0 new)" silence when nothing changed.
	for _, s := range summaries {
		if s.New == 0 && len(s.Errors) == 0 {
			logMonitorDiagnostics(s, false)
			continue
		}
		if len(s.Errors) > 0 {
			fmt.Fprintf(os.Stderr, "monitor poller: %s: %d events (%d new), errors: %v\n",
				s.Source, s.Events, s.New, s.Errors)
			logMonitorDiagnostics(s, true)
		} else {
			fmt.Fprintf(os.Stderr, "monitor poller: %s: %d events (%d new) at %s\n",
				s.Source, s.Events, s.New, flowdb.NowISO())
			logMonitorDiagnostics(s, false)
		}
	}
}

func logMonitorDiagnostics(s monitor.PollSummary, force bool) {
	if len(s.Diagnostics) == 0 {
		return
	}
	if !force && !monitorDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "monitor poller: %s diagnostics: %s\n", s.Source, strings.Join(s.Diagnostics, "; "))
}

func monitorDebugEnabled() bool {
	for _, key := range []string{"FLOW_MONITOR_DEBUG", "FLOW_SLACK_DEBUG"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}
