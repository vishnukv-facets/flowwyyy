package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

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
}

const (
	defaultKBDreamInterval = 24 * time.Hour
	defaultKBDreamMaxAge   = 30 * 24 * time.Hour // 30 days flagged → auto-remove
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

func (d *kbDreamer) start() {
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
	interval := kbDreamInterval()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.tick(ctx)
			if ni := kbDreamInterval(); ni != interval {
				interval = ni
				tick.Reset(ni)
			}
		}
	}
}

func (d *kbDreamer) tick(ctx context.Context) {
	if d.srv == nil || !kbDreamEnabled() {
		return
	}
	root := strings.TrimSpace(d.srv.cfg.FlowRoot)
	if root == "" {
		return
	}
	// 1) Flagging pass (agent judgment): move newly-stale entries into each
	//    file's Pending removal section. Sequential before the prune so the prune
	//    sees the agent's output.
	kbDir := filepath.Join(root, "kb")
	if _, err := steering.DreamKBViaAgent(ctx, kbDir); err != nil {
		fmt.Fprintf(os.Stderr, "kb dreamer: dream pass: %v\n", err)
		// fall through — still run the deterministic prune below
	}
	// 2) Prune pass (deterministic): permanently remove entries flagged longer
	//    than maxAge.
	d.pruneStaleKBFiles(time.Now(), kbDreamMaxAge())
}

// pruneStaleKBFiles rewrites each KB file, deleting Pending-removal bullets whose
// [flagged YYYY-MM-DD] date is older than maxAge. Best-effort per file.
func (d *kbDreamer) pruneStaleKBFiles(now time.Time, maxAge time.Duration) {
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
		fmt.Fprintf(os.Stderr, "kb dreamer: pruned %d expired entr%s from %s\n", removed, plural(removed), filepath.Base(path))
	}
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

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
