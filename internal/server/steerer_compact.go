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

	"flow/internal/productdb"
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
	// forkedAt records when this worker auto-forked a chat Claude→Codex, so the
	// optional recovery path knows how long it's been on Codex (GAP-9). In-memory:
	// after a restart an already-forked chat won't auto-recover until it re-forks —
	// the operator can always switch back manually. ponytail: no DB column for an
	// off-by-default convenience timer.
	forkedAt map[string]time.Time
}

func newSteererCompactWorker(srv *Server) *steererCompactWorker {
	return &steererCompactWorker{srv: srv, compactedAt: map[string]time.Time{}, forkedAt: map[string]time.Time{}}
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
	chats, err := productdb.ListChats(w.srv.cfg.DB, productdb.ChatFilter{})
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

func (w *steererCompactWorker) sweepOne(ch *productdb.Chat, absRoot string, now time.Time) {
	if ch == nil || ch.Origin != "steerer" || !w.srv.terminals.running(ch.Slug) {
		return
	}
	if !ch.SessionID.Valid || strings.TrimSpace(ch.SessionID.String) == "" {
		return
	}
	provider, err := productdb.NormalizeSessionProvider(ch.Provider)
	if err != nil {
		return
	}
	path, err := resolveSessionJSONLPath(&productdb.Task{
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
	// Provider fork (GAP-9) runs before compact: an exhausted Claude session can't
	// be relieved by /compact, so escalate to the switch instead. If it forks (or
	// recovers), the slot was relaunched — skip compact this tick.
	if w.maybeForkSteererChat(ch, provider, entry, now) {
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

// maybeForkSteererChat applies the bidirectional provider-fork triggers for one
// running steerer chat (GAP-9): auto Claude→Codex on detected exhaustion, and —
// when recovery is enabled — auto Codex→Claude after a cooldown. Best-effort and
// gated by FLOW_STEERER_FORK_PROVIDER; the manual switch is the dependable path.
// Returns true if it switched the provider (the slot was relaunched). Never forks
// mid-turn (same idle gate as compact).
func (w *steererCompactWorker) maybeForkSteererChat(ch *productdb.Chat, provider string, entry *transcriptCacheEntry, now time.Time) bool {
	if entry == nil || !steererForkEnabled() {
		return false
	}
	if now.Sub(entry.mtime) < steererCompactIdle() {
		return false // mid-turn — never fork while the agent is working
	}
	switch provider {
	case "claude":
		if !recentSteererExhaustion(entry.entries) {
			return false
		}
		if err := w.srv.switchSteererProvider(ch.Slug, "codex"); err != nil {
			fmt.Fprintf(os.Stderr, "steerer fork %s claude→codex: %v\n", ch.Slug, err)
			return false
		}
		w.mu.Lock()
		w.forkedAt[ch.Slug] = now
		w.mu.Unlock()
		return true
	case "codex":
		w.mu.Lock()
		forkedAt := w.forkedAt[ch.Slug]
		w.mu.Unlock()
		if !shouldRecoverToClaude(now, forkedAt, steererForkRecoveryAfter(), steererForkRecoveryEnabled()) {
			return false
		}
		if err := w.srv.switchSteererProvider(ch.Slug, "claude"); err != nil {
			fmt.Fprintf(os.Stderr, "steerer recover %s codex→claude: %v\n", ch.Slug, err)
			return false
		}
		w.mu.Lock()
		delete(w.forkedAt, ch.Slug)
		w.mu.Unlock()
		return true
	}
	return false
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
