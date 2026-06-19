package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"flow/internal/schedule"
	"flow/internal/steering"
)

// kbDreamer is the KB hygiene ("dreaming") worker. On a slow cadence it (1) runs
// a headless agent that MOVES stale/superseded/incorrect KB entries into each
// file's "Pending removal" section (flagging, never deleting), and (2) runs a
// deterministic prune that permanently removes entries left flagged longer than
// maxAge. The operator curates in between (edit the file to keep/remove). The
// flagging is agent judgment; the removal is deterministic (parses the
// [flagged YYYY-MM-DD] marker), so "auto-remove" never depends on an LLM.
type kbDreamer struct {
	srv *Server

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// Observable state for the KB UI (separate lock: read from HTTP goroutines
	// while the loop goroutine writes). nextRun is owned by the scheduling loop;
	// lastRun/running/history are updated by tick() (scheduled or manual).
	stateMu sync.Mutex
	running bool
	lastRun time.Time
	nextRun time.Time
	history []KBDreamRecord // most-recent-first, capped at kbDreamHistoryCap
}

const (
	defaultKBDreamInterval = 24 * time.Hour
	defaultKBDreamMaxAge   = 30 * 24 * time.Hour // 30 days flagged → auto-remove
	kbDreamHistoryCap      = 12

	// kbDreamReeval bounds how long the scheduling loop sleeps before waking to
	// re-read settings and recompute the next run — so changing the cadence or
	// fixed time in Settings takes effect within this window, not only at the
	// next pass.
	kbDreamReeval = 5 * time.Minute
	// kbDreamCatchupDelay is how long after an overdue/missed slot the loop waits
	// before running the catch-up pass: long enough to clear startup churn, short
	// enough that restarting the server still tidies the KB promptly instead of
	// silently resetting the schedule.
	kbDreamCatchupDelay = 90 * time.Second
)

func newKBDreamer(srv *Server) *kbDreamer { return &kbDreamer{srv: srv} }

// kbDreamEnabled gates the whole hygiene feature (flagging AND auto-prune);
// default ON.
func kbDreamEnabled() bool { return envBoolDefaultServer("FLOW_KB_DREAM_ENABLED", true) }

func kbDreamInterval() time.Duration {
	return envDurationDefault("FLOW_KB_DREAM_INTERVAL", defaultKBDreamInterval)
}
func kbDreamMaxAge() time.Duration {
	return envDurationDefault("FLOW_KB_DREAM_MAX_AGE", defaultKBDreamMaxAge)
}

// kbDreamScheduleCron returns the canonical cron the dreamer fires on, a human
// label, and whether it came from an explicit FLOW_KB_DREAM_SCHEDULE phrase.
//
// The schedule reuses the SAME parser and next-fire engine as playbook schedules
// (internal/schedule): a phrase like "daily at 3am" / "every 6 hours" / "weekly",
// or a raw cron, sets a fixed cadence that survives restarts. When no phrase is
// set (or it doesn't parse) it falls back to the FLOW_KB_DREAM_INTERVAL duration,
// expressed as an "@every" cron so the identical next-fire math applies in both
// modes.
func kbDreamScheduleCron() (cron, label string, custom bool) {
	if raw := strings.TrimSpace(os.Getenv("FLOW_KB_DREAM_SCHEDULE")); raw != "" {
		if s, err := schedule.Parse(raw); err == nil {
			return s.Cron, schedule.Describe(s), true
		}
		// Unparseable phrase → fall back to the interval below rather than stalling.
	}
	return "@every " + kbDreamInterval().String(), "", false
}

func (d *kbDreamer) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return
	}
	// Restore the persisted last-run so a restart computes the next run from real
	// history (catching up a missed pass) instead of resetting the countdown.
	d.loadState()
	// Normalize existing KB files once at boot so any "Pending removal" section
	// sits at the top immediately, without waiting for the next dream pass.
	if kbDreamEnabled() {
		d.repositionPendingRemoval()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	d.setNextRun(d.computeNextRun(time.Now()))
	go d.loop(ctx)
}

func (d *kbDreamer) stop() {
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

func (d *kbDreamer) loop(ctx context.Context) {
	defer close(d.done)
	for {
		next := d.computeNextRun(time.Now())
		d.setNextRun(next)
		// Sleep until the next run, but never longer than the re-eval window so a
		// settings change (cadence / fixed time) is picked up promptly.
		wait := max(min(time.Until(next), kbDreamReeval), 0)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if !time.Now().Before(next) {
				d.tick(ctx) // tick() persists last-run on completion
			}
			// Otherwise it was just a re-eval wake; the loop recomputes next from
			// (possibly changed) settings.
		}
	}
}

// computeNextRun returns when the next dream pass should fire. It advances the
// schedule from the persisted last-run (the catch-up anchor): schedule.Next gives
// the first cron occurrence strictly after the last run, so a fixed-time schedule
// resumes at its clock time and an interval resumes at last-run + interval — in
// both cases the schedule is computed from real history, not reset to "now +
// interval" on every restart. An overdue/missed slot schedules a prompt catch-up
// (fire once on the next check), mirroring the playbook scheduler.
func (d *kbDreamer) computeNextRun(now time.Time) time.Time {
	cron, _, _ := kbDreamScheduleCron()
	base := d.getLastRun()
	if base.IsZero() {
		base = now // no history yet → schedule forward from now
	}
	next, err := schedule.Next(cron, base)
	if err != nil {
		// Defensive: an unexpected bad cron must not stall the worker.
		return now.Add(kbDreamInterval())
	}
	if !next.After(now) {
		return now.Add(kbDreamCatchupDelay) // overdue across downtime → catch up once
	}
	return next
}

// triggerDream runs a dream pass out of band (operator-initiated from the KB
// UI). It is a no-op if a pass is already running. Returns false if it could
// not start (already running or disabled).
func (d *kbDreamer) triggerDream() bool {
	if d.srv == nil || !kbDreamEnabled() {
		return false
	}
	d.stateMu.Lock()
	if d.running {
		d.stateMu.Unlock()
		return false
	}
	d.stateMu.Unlock()
	go d.tick(context.Background())
	return true
}

func (d *kbDreamer) tick(ctx context.Context) {
	if d.srv == nil || !kbDreamEnabled() {
		return
	}
	root := strings.TrimSpace(d.srv.cfg.FlowRoot)
	if root == "" {
		return
	}
	// Guard against overlapping passes (the scheduled tick and a manual trigger
	// racing). Whoever wins flips running; the loser bails.
	d.stateMu.Lock()
	if d.running {
		d.stateMu.Unlock()
		return
	}
	d.running = true
	d.stateMu.Unlock()

	start := time.Now()
	rec := KBDreamRecord{Status: "ok"}
	// 1) Flagging pass (agent judgment): move newly-stale entries into each
	//    file's Pending removal section. Sequential before the prune so the prune
	//    sees the agent's output.
	kbDir := filepath.Join(root, "kb")
	if _, err := steering.DreamKBViaAgent(ctx, kbDir); err != nil {
		fmt.Fprintf(os.Stderr, "kb dreamer: dream pass: %v\n", err)
		rec.Status = "error"
		rec.Detail = truncate(err.Error(), 300)
		// fall through — still run the deterministic prune below
	}
	// 2) Reposition (deterministic): move each file's Pending-removal section to
	//    the top so flagged entries surface first, wherever the agent wrote it.
	d.repositionPendingRemoval()
	// 3) Prune pass (deterministic): permanently remove entries flagged longer
	//    than maxAge.
	rec.Pruned = d.pruneStaleKBFiles(time.Now(), kbDreamMaxAge())
	rec.At = start.UTC().Format(time.RFC3339)
	rec.DurationMs = time.Since(start).Milliseconds()

	d.stateMu.Lock()
	d.running = false
	d.lastRun = time.Now()
	d.history = append([]KBDreamRecord{rec}, d.history...)
	if len(d.history) > kbDreamHistoryCap {
		d.history = d.history[:kbDreamHistoryCap]
	}
	d.stateMu.Unlock()
	// Persist last-run so the schedule survives a restart (a manual "Dream now"
	// also lands here, so its run counts toward the next scheduled pass).
	d.persistState()
}

// getLastRun / setNextRun / getNextRun guard the observable state shared with the
// HTTP goroutines.
func (d *kbDreamer) getLastRun() time.Time {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.lastRun
}

func (d *kbDreamer) setNextRun(t time.Time) {
	d.stateMu.Lock()
	d.nextRun = t
	d.stateMu.Unlock()
}

// kbDreamPersistState is the on-disk shape of the dreamer's schedule state.
type kbDreamPersistState struct {
	LastRun string `json:"last_run"`
}

func (d *kbDreamer) statePath() string {
	root := strings.TrimSpace(d.srv.cfg.FlowRoot)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "cache", "kb_dream_state.json")
}

// loadState restores the persisted last-run into memory. Best-effort: a
// missing/corrupt file just means "no prior run recorded".
func (d *kbDreamer) loadState() {
	path := d.statePath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st kbDreamPersistState
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(st.LastRun)); err == nil {
		d.stateMu.Lock()
		d.lastRun = t
		d.stateMu.Unlock()
	}
}

// persistState writes the last-run timestamp atomically. Best-effort.
func (d *kbDreamer) persistState() {
	path := d.statePath()
	if path == "" {
		return
	}
	lastRun := d.getLastRun()
	if lastRun.IsZero() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(kbDreamPersistState{LastRun: lastRun.UTC().Format(time.RFC3339)})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// dreamStatus returns the observable dreamer state for the KB UI.
func (d *kbDreamer) dreamStatus() KBDreamStatus {
	_, scheduleLabel, _ := kbDreamScheduleCron()
	// Compute the next run LIVE from the current schedule + last-run, rather than
	// the loop's cached nextRun (which only refreshes on the ≤5m re-eval). Without
	// this, saving a new schedule or running "Dream now" leaves the UI showing the
	// stale previous cadence (e.g. the old 24h interval) until the loop catches up.
	// computeNextRun locks stateMu internally, so call it before taking the lock.
	var nextRun time.Time
	if kbDreamEnabled() {
		nextRun = d.computeNextRun(time.Now())
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	st := KBDreamStatus{
		Enabled:    kbDreamEnabled(),
		Running:    d.running,
		IntervalMs: kbDreamInterval().Milliseconds(),
		MaxAgeDays: int(kbDreamMaxAge().Hours() / 24),
		Schedule:   scheduleLabel,
		History:    append([]KBDreamRecord(nil), d.history...),
	}
	if !d.lastRun.IsZero() {
		st.LastRunAt = d.lastRun.UTC().Format(time.RFC3339)
	}
	if !nextRun.IsZero() {
		st.NextRunAt = nextRun.UTC().Format(time.RFC3339)
	}
	return st
}

// pruneStaleKBFiles rewrites each KB file, deleting Pending-removal bullets whose
// [flagged YYYY-MM-DD] date is older than maxAge. Best-effort per file. Returns
// the total number of bullets removed across all files.
func (d *kbDreamer) pruneStaleKBFiles(now time.Time, maxAge time.Duration) int {
	total := 0
	for _, path := range kbFiles(d.srv.cfg.FlowRoot) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pruned, removed := pruneExpiredPendingRemoval(string(raw), now, maxAge)
		if removed == 0 {
			continue
		}
		if err := os.WriteFile(path, []byte(pruned), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "kb dreamer: prune write %s: %v\n", path, err)
			continue
		}
		total += removed
		fmt.Fprintf(os.Stderr, "kb dreamer: pruned %d expired entr%s from %s\n", removed, plural(removed), filepath.Base(path))
	}
	return total
}

// KBDreamStatus is the observable state of the KB "dreaming" hygiene worker,
// surfaced on the Knowledge page so the operator can see when the next pass
// runs and what recent passes did.
type KBDreamStatus struct {
	Enabled    bool            `json:"enabled"`
	Running    bool            `json:"running"`
	IntervalMs int64           `json:"interval_ms"`
	MaxAgeDays int             `json:"max_age_days"`
	Schedule   string          `json:"schedule,omitempty"` // human schedule label when a custom FLOW_KB_DREAM_SCHEDULE is set; "" = plain interval cadence
	LastRunAt  string          `json:"last_run_at,omitempty"`
	NextRunAt  string          `json:"next_run_at,omitempty"`
	History    []KBDreamRecord `json:"history"`
}

// KBDreamRecord is one completed dream pass.
type KBDreamRecord struct {
	At         string `json:"at"`
	Status     string `json:"status"` // "ok" | "error"
	Pruned     int    `json:"pruned"`
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

// flaggedBulletRe matches a Pending-removal bullet carrying its flagged date,
// e.g. "- [flagged 2026-06-14] old fact — why: superseded". The marker is unique
// to the Pending-removal section, so matching it anywhere in the file is safe.
var flaggedBulletRe = regexp.MustCompile(`^\s*-\s*\[flagged (\d{4}-\d{2}-\d{2})\]`)

// pruneExpiredPendingRemoval returns content with every flagged bullet older than
// maxAge removed, plus the count removed. Lines that don't match, or whose date
// is unparseable / within maxAge, are preserved verbatim (headings included).
func pruneExpiredPendingRemoval(content string, now time.Time, maxAge time.Duration) (string, int) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	removed := 0
	for _, line := range lines {
		if m := flaggedBulletRe.FindStringSubmatch(line); m != nil {
			if flaggedAt, err := time.Parse("2006-01-02", m[1]); err == nil && now.Sub(flaggedAt) > maxAge {
				removed++
				continue
			}
		}
		out = append(out, line)
	}
	if removed == 0 {
		return content, 0
	}
	return strings.Join(out, "\n"), removed
}

// pendingRemovalHeadingRe matches the "Pending removal" section heading at any
// level and regardless of the leading warning emoji, so the operator's manual
// purge clears the section the dreamer writes (## ⚠️ Pending removal) as well as
// any hand-written variant.
var pendingRemovalHeadingRe = regexp.MustCompile(`(?i)^#{1,6}\s+.*pending removal\s*$`)

// pendingRemovalSpan returns the [start,end) line range of the Pending-removal
// section: the heading line plus the run of FLAGGED bullets and blank lines that
// follow it, stopping at the first non-flagged, non-blank line (live content) or
// the next heading. ok is false when there is no heading.
//
// This is the SAFE extent and the load-bearing invariant of the whole feature:
// the span NEVER includes a live (non-flagged) entry, so moving or purging it
// cannot delete real KB facts — even when the section sits at the top of the file
// with live entries directly below it (the layout that caused data loss when the
// boundary was naively "until the next heading").
func pendingRemovalSpan(lines []string) (start, end int, ok bool) {
	start = -1
	for i, ln := range lines {
		if pendingRemovalHeadingRe.MatchString(ln) {
			start = i
			break
		}
	}
	if start == -1 {
		return 0, 0, false
	}
	end = start + 1
	for end < len(lines) {
		t := strings.TrimSpace(lines[end])
		if t == "" { // blank inside the section
			end++
			continue
		}
		if flaggedBulletRe.MatchString(lines[end]) { // a flagged entry
			end++
			continue
		}
		break // live content or next heading → section ends here
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end-- // don't claim trailing blank lines that separate from live content
	}
	return start, end, true
}

// stripAllPendingRemoval removes the "Pending removal" section — the heading and
// the flagged bullets under it — regardless of age (the operator's explicit
// "clear all flagged now"; pruneExpiredPendingRemoval is the date-gated auto
// variant). It removes ONLY flagged entries: any live content is preserved even
// if it sits immediately below the section. Returns the rewritten content and the
// count of flagged bullets removed. Content with no section round-trips unchanged.
func stripAllPendingRemoval(content string) (string, int) {
	lines := strings.Split(content, "\n")
	start, end, ok := pendingRemovalSpan(lines)
	if !ok {
		return content, 0
	}
	removed := 0
	for _, ln := range lines[start:end] {
		if flaggedBulletRe.MatchString(ln) {
			removed++
		}
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	return strings.Join(out, "\n"), removed
}

// purgeAllPendingRemoval strips the "Pending removal" section from every KB file
// immediately (the operator's "clean up flagged" action). Best-effort per file.
// Returns the total flagged bullets removed across all files.
func (d *kbDreamer) purgeAllPendingRemoval() int {
	total := 0
	for _, path := range kbFiles(d.srv.cfg.FlowRoot) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		stripped, removed := stripAllPendingRemoval(string(raw))
		if stripped == string(raw) {
			continue // no Pending-removal section in this file
		}
		if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "kb dreamer: purge write %s: %v\n", path, err)
			continue
		}
		total += removed
		fmt.Fprintf(os.Stderr, "kb dreamer: purged pending-removal section from %s (%d flagged)\n", filepath.Base(path), removed)
	}
	return total
}

// movePendingRemovalToTop relocates the "Pending removal" section to just after
// the file's first H1 heading (or the very top if there is none), so flagged
// entries are the first thing the operator sees. Returns the rewritten content
// and whether anything moved. Idempotent: a section already at the top returns
// changed=false, so re-running the dream pass never churns the file.
func movePendingRemovalToTop(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, line := range lines {
		if pendingRemovalHeadingRe.MatchString(line) {
			start = i
			break
		}
	}
	if start == -1 {
		return content, false // no section to move
	}
	// Desired insert index: right after the first H1 and its blank line.
	insertAfterH1 := func(ls []string) int {
		for i, line := range ls {
			if strings.HasPrefix(line, "# ") {
				j := i + 1
				if j < len(ls) && strings.TrimSpace(ls[j]) == "" {
					j++ // keep the blank line that follows the title
				}
				return j
			}
		}
		return 0 // no H1 → very top
	}
	if start == insertAfterH1(lines) {
		return content, false // already at the top
	}
	// Extract the SAFE span (heading + flagged bullets only) — never live content.
	_, end, _ := pendingRemovalSpan(lines)
	section := append([]string{}, lines[start:end]...)
	rest := append(append([]string{}, lines[:start]...), lines[end:]...)
	at := insertAfterH1(rest)
	out := make([]string, 0, len(rest)+len(section)+1)
	out = append(out, rest[:at]...)
	out = append(out, section...)
	out = append(out, "") // one blank separating the section from what follows
	out = append(out, rest[at:]...)
	joined := strings.Join(out, "\n")
	if joined == content {
		return content, false
	}
	return joined, true
}

// repositionPendingRemoval moves the Pending-removal section to the top of every
// KB file. Best-effort per file; runs as part of the dream pass so flagged
// entries surface first without the operator hand-editing.
func (d *kbDreamer) repositionPendingRemoval() {
	for _, path := range kbFiles(d.srv.cfg.FlowRoot) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		moved, changed := movePendingRemovalToTop(string(raw))
		if !changed {
			continue
		}
		if err := os.WriteFile(path, []byte(moved), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "kb dreamer: reposition write %s: %v\n", path, err)
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
