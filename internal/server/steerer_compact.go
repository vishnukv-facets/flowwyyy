package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
	"flow/internal/steering"
)

const (
	defaultSteererCompactInterval = time.Minute
	defaultSteererCompactIdle     = 8 * time.Minute
	defaultSteererCompactCooldown = 30 * time.Minute
	defaultSteererCompactPct      = 60.0
	steererCompactPrompt          = "/compact"
)

type steererCompactWorker struct {
	srv *Server

	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	compactedAt map[string]time.Time
}

func newSteererCompactWorker(srv *Server) *steererCompactWorker {
	return &steererCompactWorker{srv: srv, compactedAt: map[string]time.Time{}}
}

func steererCompactInterval() time.Duration {
	return envDurationDefault("FLOW_STEERER_COMPACT_INTERVAL", defaultSteererCompactInterval)
}

func steererCompactIdle() time.Duration {
	return envDurationDefault("FLOW_STEERER_COMPACT_IDLE", defaultSteererCompactIdle)
}

func steererCompactCooldown() time.Duration {
	return envDurationDefault("FLOW_STEERER_COMPACT_COOLDOWN", defaultSteererCompactCooldown)
}

func steererCompactThresholdPct() float64 {
	raw := strings.TrimSpace(os.Getenv("FLOW_STEERER_COMPACT_PCT"))
	if raw == "" {
		return defaultSteererCompactPct
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return defaultSteererCompactPct
	}
	return v
}

func (w *steererCompactWorker) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go w.loop(ctx)
}

func (w *steererCompactWorker) stop() {
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.cancel = nil
	w.done = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (w *steererCompactWorker) loop(ctx context.Context) {
	defer close(w.done)
	interval := steererCompactInterval()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.tick(time.Now())
			if ni := steererCompactInterval(); ni != interval {
				interval = ni
				tick.Reset(ni)
			}
		}
	}
}

func (w *steererCompactWorker) tick(now time.Time) {
	if w == nil || w.srv == nil || w.srv.cfg.DB == nil || w.srv.terminals == nil || w.srv.transcripts == nil {
		return
	}
	if !steering.SteererSessionsEnabled() {
		return
	}
	chats, err := flowdb.ListChats(w.srv.cfg.DB, flowdb.ChatFilter{})
	if err != nil {
		return
	}
	absRoot, err := w.srv.absFlowRoot()
	if err != nil {
		return
	}
	for _, ch := range chats {
		w.sweepOne(ch, absRoot, now)
	}
}

func (w *steererCompactWorker) sweepOne(ch *flowdb.Chat, absRoot string, now time.Time) {
	if ch == nil || ch.Origin != "steerer" || !w.srv.terminals.running(ch.Slug) {
		return
	}
	if !ch.SessionID.Valid || strings.TrimSpace(ch.SessionID.String) == "" {
		return
	}
	provider, err := flowdb.NormalizeSessionProvider(ch.Provider)
	if err != nil {
		return
	}
	path, err := resolveSessionJSONLPath(&flowdb.Task{
		Slug: ch.Slug, WorkDir: absRoot, SessionProvider: provider,
		SessionID: sql.NullString{String: strings.TrimSpace(ch.SessionID.String), Valid: true},
	})
	if err != nil || path == "" {
		return
	}
	entry, err := w.srv.transcripts.get(path)
	if err != nil {
		return
	}
	tokensUsed, tokensMax := steererCompactUsage(provider, entry.usage)
	w.mu.Lock()
	last := w.compactedAt[ch.Slug]
	w.mu.Unlock()
	if !shouldCompact(now, entry.mtime, last, tokensUsed, tokensMax, steererCompactThresholdPct(), steererCompactIdle(), steererCompactCooldown()) {
		return
	}
	if err := w.srv.terminals.wakeTask(ch.Slug, steererCompactPrompt); err != nil {
		fmt.Fprintf(os.Stderr, "steerer compact: wake %s: %v\n", ch.Slug, err)
		return
	}
	w.mu.Lock()
	w.compactedAt[ch.Slug] = now
	w.mu.Unlock()
}

func steererCompactUsage(provider string, stats transcriptUsageStats) (int, int) {
	used := stats.TokensUsed
	max := stats.TokensMax
	if max <= 0 && strings.TrimSpace(stats.Model) != "" {
		max = contextWindowForModel(provider, stats.Model)
	}
	if max <= 0 {
		max = contextWindowForProvider(provider)
	}
	if used > max {
		max = used
	}
	return used, max
}

func shouldCompact(now, mtime, compactedAt time.Time, tokensUsed, tokensMax int, thresholdPct float64, idle, cooldown time.Duration) bool {
	if tokensUsed <= 0 || tokensMax <= 0 || thresholdPct <= 0 {
		return false
	}
	if now.Sub(mtime) < idle {
		return false
	}
	if !compactedAt.IsZero() && now.Sub(compactedAt) < cooldown {
		return false
	}
	occupancy := float64(tokensUsed) / float64(tokensMax) * 100
	return occupancy >= thresholdPct
}
