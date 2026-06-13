package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

// kbDistiller periodically captures durable knowledge from LIVE agent sessions
// (in-progress tasks + chats) into the KB, mid-flight — complementing the
// close-out sweep that only fires on `flow done`.
//
// It reuses the existing capture mechanism rather than inventing a parallel one:
// it WAKES THE SESSION ITSELF with a §4.10 KB-checkpoint instruction (the same
// rules the close-out sweep applies), so the agent — which already holds the
// whole conversation in context — distills and writes ~/.flow/kb/*.md. It never
// spawns a separate process to re-read the transcript.
//
// Two hard guarantees keep it non-intrusive and cheap:
//   - NEVER wake a working agent. A session is only woken once its transcript
//     has been QUIET for >= idle (the jsonl mtime is the universal activity
//     signal: a working agent appends continuously; an idle one waiting at the
//     prompt goes stale). So we never inject mid-turn.
//   - NEVER re-mine or loop. A per-session cursor (last transcript byte offset
//     we requested a capture through) + a cooldown gate mean an unchanged or
//     recently-swept session is skipped — and the checkpoint turn's own output
//     can't immediately re-trigger another checkpoint.
type kbDistiller struct {
	srv      *Server
	minDelta int64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	defaultKBDistillInterval = 5 * time.Minute
	defaultKBDistillIdle     = 8 * time.Minute
	defaultKBDistillCooldown = 30 * time.Minute
	// minKBDistillDelta is the minimum new transcript bytes since the last
	// capture worth waking a session for — skips trivial deltas.
	minKBDistillDelta = 600
)

func newKBDistiller(srv *Server) *kbDistiller {
	return &kbDistiller{srv: srv, minDelta: minKBDistillDelta}
}

// The cadence knobs are read LIVE (each tick / gate check) rather than cached at
// construction, so changing them in the settings UI takes effect within one tick
// without a server restart.
func kbDistillInterval() time.Duration {
	return envDurationDefault("FLOW_KB_DISTILL_INTERVAL", defaultKBDistillInterval)
}
func kbDistillIdle() time.Duration {
	return envDurationDefault("FLOW_KB_DISTILL_IDLE", defaultKBDistillIdle)
}
func kbDistillCooldown() time.Duration {
	return envDurationDefault("FLOW_KB_DISTILL_COOLDOWN", defaultKBDistillCooldown)
}

// kbDistillEnabled gates the whole worker; default ON. The capture spends tokens
// (it drives the live agent), so it's a toggle, but the idle+cooldown+activity
// gates keep idle/short sessions free.
func kbDistillEnabled() bool {
	return envBoolDefaultServer("FLOW_KB_DISTILL_ENABLED", true)
}

func (d *kbDistiller) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	go d.loop(ctx)
}

func (d *kbDistiller) stop() {
	d.mu.Lock()
	cancel := d.cancel
	done := d.done
	d.cancel = nil
	d.done = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (d *kbDistiller) loop(ctx context.Context) {
	defer close(d.done)
	interval := kbDistillInterval()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.tick(ctx)
			// Honor a live change to the interval setting without a restart.
			if ni := kbDistillInterval(); ni != interval {
				interval = ni
				tick.Reset(ni)
			}
		}
	}
}

// kbCandidate is a session the distiller may sweep: a live in-progress task or a
// chat, with the bits needed to resolve its transcript.
type kbCandidate struct {
	slug      string
	kind      string // "task" | "chat"
	sessionID string
	task      *flowdb.Task // synthetic or real; carries the fields resolveSessionJSONLPath needs
}

func (d *kbDistiller) tick(ctx context.Context) {
	if d.srv == nil || d.srv.cfg.DB == nil || d.srv.terminals == nil {
		return
	}
	if !kbDistillEnabled() {
		return
	}
	now := time.Now()
	for _, c := range d.candidates() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		d.sweepOne(c, now)
	}
}

// candidates gathers live in-progress tasks and chats that have a session.
func (d *kbDistiller) candidates() []kbCandidate {
	db := d.srv.cfg.DB
	var out []kbCandidate

	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{Status: "in-progress"})
	if err == nil {
		for _, t := range tasks {
			if t == nil || !t.SessionID.Valid || strings.TrimSpace(t.SessionID.String) == "" {
				continue
			}
			out = append(out, kbCandidate{slug: t.Slug, kind: "task", sessionID: strings.TrimSpace(t.SessionID.String), task: t})
		}
	}

	// Chats: include archived-but-not-deleted (an archived chat whose session is
	// still alive should keep getting checkpoints — that subsumes the
	// "capture on archive" case continuously).
	chats, err := flowdb.ListChats(db, flowdb.ChatFilter{IncludeArchived: true})
	if err == nil {
		root := strings.TrimSpace(d.srv.cfg.FlowRoot)
		absRoot, aerr := filepath.Abs(root)
		for _, ch := range chats {
			if ch == nil || !ch.SessionID.Valid || strings.TrimSpace(ch.SessionID.String) == "" || aerr != nil {
				continue
			}
			provider, perr := flowdb.NormalizeSessionProvider(ch.Provider)
			if perr != nil {
				continue
			}
			out = append(out, kbCandidate{
				slug: ch.Slug, kind: "chat", sessionID: strings.TrimSpace(ch.SessionID.String),
				task: &flowdb.Task{
					Slug: ch.Slug, WorkDir: absRoot, SessionProvider: provider,
					SessionID: sql.NullString{String: strings.TrimSpace(ch.SessionID.String), Valid: true},
				},
			})
		}
	}
	return out
}

// sweepOne checks a single candidate against the gates and, if it qualifies,
// wakes the session with the §4.10 KB checkpoint and advances its cursor.
func (d *kbDistiller) sweepOne(c kbCandidate, now time.Time) {
	// Only LIVE sessions — a dead/finished one is captured by the close-out
	// sweep, and we must never resurrect a process just to scoop KB.
	if !d.srv.terminals.running(c.slug) && !d.srv.terminals.sharedRunning(c.slug) {
		return
	}
	path, err := resolveSessionJSONLPath(c.task)
	if err != nil || path == "" {
		return
	}
	entry, err := d.srv.transcripts.get(path)
	if err != nil {
		return
	}
	cur, _, _ := flowdb.GetKBCaptureCursor(d.srv.cfg.DB, c.sessionID)
	capturedAt := parseRFC3339OrZero(cur.CapturedAt)
	maxOffset := maxTranscriptByteOffset(entry.entries)
	if !kbShouldWake(now, entry.mtime, capturedAt, cur.Cursor, maxOffset, d.minDelta, kbDistillIdle(), kbDistillCooldown()) {
		return
	}
	if err := d.srv.wakeTaskForInboxNotify(c.slug, kbCheckpointPrompt(d.srv.cfg.FlowRoot)); err != nil {
		fmt.Fprintf(os.Stderr, "kb distiller: wake %s (%s): %v\n", c.slug, c.kind, err)
		return
	}
	// Optimistic cursor advance: we requested a capture through maxOffset. The
	// cooldown prevents the checkpoint turn's own output from re-triggering, and
	// the next eligible sweep advances past it. We don't read the agent's reply.
	if err := flowdb.UpsertKBCaptureCursor(d.srv.cfg.DB, flowdb.KBCaptureCursor{
		SessionID:  c.sessionID,
		Slug:       c.slug,
		Kind:       c.kind,
		Cursor:     maxOffset,
		CapturedAt: now.Format(time.RFC3339),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "kb distiller: cursor %s: %v\n", c.slug, err)
	}
}

// kbShouldWake is the pure gate: wake only when the session is idle (transcript
// quiet >= idle), past its cooldown, and has accumulated >= minDelta new
// transcript bytes since the last requested capture. A zero capturedAt means
// never swept (cooldown passes). A non-positive maxOffset means no transcript
// yet (nothing to capture).
func kbShouldWake(now, mtime, capturedAt time.Time, cursor, maxOffset, minDelta int64, idle, cooldown time.Duration) bool {
	if maxOffset <= 0 {
		return false
	}
	if now.Sub(mtime) < idle {
		return false // still active — never wake a working agent
	}
	if !capturedAt.IsZero() && now.Sub(capturedAt) < cooldown {
		return false // swept recently
	}
	return maxOffset-cursor >= minDelta
}

// kbCheckpointPrompt is injected into the idle live session. It reuses the flow
// skill's §4.10 KB capture rules — the same discipline the close-out sweep
// applies — so mid-flight capture writes the same kb/*.md in the same way. The
// real flow root is interpolated (matching the close-out sweep) so a non-default
// $FLOW_ROOT points the agent at the same kb/ the Knowledge screen reads.
func kbCheckpointPrompt(root string) string {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	if root == "" {
		root = "~/.flow"
	}
	return "[flow KB checkpoint — automated, run this silently, then go back to waiting]\n" +
		"Run a knowledge-base checkpoint per the flow skill's KB capture rules (§4.10). " +
		"If you haven't already loaded the flow skill this session, invoke it first. " +
		"Review our conversation since your last checkpoint and capture any DURABLE, surprising, " +
		"future-relevant facts about the user / their org / products / processes / business into " +
		root + "/kb/*.md — STRICT bar (default: write nothing), one dated bullet per insight, dedup " +
		"against existing entries, and update/upgrade in place any entry this conversation has made " +
		"stale. This is the same capture flow performs at task close-out. " +
		"Do NOT reply to me or announce this — write any KB files silently and then resume waiting."
}

func maxTranscriptByteOffset(entries []TranscriptEntry) int64 {
	var max int64
	for _, e := range entries {
		if e.ByteOffset > max {
			max = e.ByteOffset
		}
	}
	return max
}

func parseRFC3339OrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// envDurationDefault parses a Go duration (e.g. "5m") from env, falling back to
// def on empty/invalid.
func envDurationDefault(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// envBoolDefaultServer reads a boolean env var with a fallback. Recognized
// truthy: 1,true,yes,y,on; falsy: 0,false,no,n,off; anything else → fallback.
func envBoolDefaultServer(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
