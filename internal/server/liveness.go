package server

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

// livenessReconciler is the periodic process-scan reconciler that catches
// sessions whose Stop / SessionEnd hook never fired (Claude crashes,
// network blip, agent killed by oom-killer, etc.). Without it the runtime
// state for a dead session shows stale "running" forever, which is the
// most annoying class of UI lies.
//
// It runs every reconcileInterval; per tick:
//  1. Snapshot the live Claude session IDs from `ps -axo pid,command`.
//  2. Snapshot the live Codex session IDs (last-modified Codex JSONL).
//  3. For each row in tasks where session_id IS NOT NULL and runtime
//     status is in {running, waiting}, if the session id is missing
//     from the live snapshot AND the runtime row hasn't been updated
//     recently, force status=dead.
//
// Reconciler errors are non-fatal — a tick that fails to ps the process
// list just logs and tries again next round. This is liveness telemetry,
// not a correctness boundary.
type livenessReconciler struct {
	srv      *Server
	interval time.Duration
	grace    time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	defaultReconcileInterval = 30 * time.Second
	// Grace window before forcing dead. A session can be ungracefully
	// torn down while the next hook is in-flight; this avoids flapping
	// the UI between running and dead when a hook arrives moments late.
	defaultReconcileGrace = 90 * time.Second
)

func newLivenessReconciler(srv *Server) *livenessReconciler {
	return &livenessReconciler{
		srv:      srv,
		interval: defaultReconcileInterval,
		grace:    defaultReconcileGrace,
	}
}

func (r *livenessReconciler) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.loop(ctx)
}

func (r *livenessReconciler) stop() {
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.cancel = nil
	r.done = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *livenessReconciler) loop(ctx context.Context) {
	defer close(r.done)
	// Tick immediately on start — the first reconcile is the one that
	// catches a session that crashed before the server came up. Then
	// follow the configured cadence.
	r.tick()
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.tick()
		}
	}
}

func (r *livenessReconciler) tick() {
	if r.srv == nil || r.srv.cfg.DB == nil {
		return
	}
	live, err := scanLiveSessions()
	if err != nil {
		// Process scan failed — likely a sandbox without ps. Skip this
		// tick rather than declaring everything dead.
		return
	}
	r.reconcileTasks(r.srv.cfg.DB, live)
}

// liveSessionSet captures both Claude and Codex sessions detected as
// live on this host. Claude sessions come from ps, Codex sessions come
// from a Codex-process scan; both end up as lower-case session ids.
type liveSessionSet struct {
	claude map[string]bool
	codex  map[string]bool
	// codexDirs holds the working dirs (-C) of live codex processes. Fresh
	// codex sessions carry no session id on the command line, so workdir is the
	// only reliable liveness signal for them.
	codexDirs map[string]bool
}

func (l liveSessionSet) has(provider, sessionID string) bool {
	sessionID = strings.ToLower(strings.TrimSpace(sessionID))
	if sessionID == "" {
		return false
	}
	switch provider {
	case "claude":
		return l.claude[sessionID]
	case "codex":
		return l.codex[sessionID]
	}
	return false
}

// codexDirLive reports whether a live codex process is running in dir (matched
// by its -C working dir / worktree path).
func (l liveSessionSet) codexDirLive(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	return l.codexDirs[filepath.Clean(dir)]
}

var reLiveClaudeSession = regexp.MustCompile(
	`(?:--session-id|--resume)[ =]([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12})`,
)

var reconcileScanner = scanProcesses

func scanProcesses() ([]byte, error) {
	return exec.Command("ps", "-axo", "pid,command").Output()
}

func scanLiveSessions() (liveSessionSet, error) {
	out, err := reconcileScanner()
	if err != nil {
		return liveSessionSet{}, err
	}
	set := liveSessionSet{
		claude:    map[string]bool{},
		codex:     map[string]bool{},
		codexDirs: map[string]bool{},
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "claude") {
			for _, m := range reLiveClaudeSession.FindAllStringSubmatch(line, -1) {
				if len(m) >= 2 {
					set.claude[strings.ToLower(m[1])] = true
				}
			}
		}
		if strings.Contains(line, "codex") {
			if strings.Contains(line, "resume") {
				for _, id := range anySessionUUIDs(line) {
					set.codex[strings.ToLower(id)] = true
				}
			}
			// Fresh codex sessions have no id on the command line; the working
			// dir (-C) is present for both fresh and resumed sessions.
			for _, dir := range codexWorkdirsInLine(line) {
				set.codexDirs[dir] = true
			}
		}
	}
	return set, nil
}

type liveTaskRow struct {
	slug         string
	provider     string
	sessionID    string
	workDir      string
	worktreePath string
}

func (r *livenessReconciler) reconcileTasks(db *sql.DB, live liveSessionSet) {
	// Tasks whose session is in-flight (status='in-progress') and have a
	// session_id are candidates. We skip backlog/done because those
	// already imply a non-live state — the reconciler shouldn't second-
	// guess workflow status.
	rows, err := db.Query(`
		SELECT slug, session_provider, session_id,
		       COALESCE(work_dir, ''), COALESCE(worktree_path, '')
		FROM tasks
		WHERE status = 'in-progress'
		  AND session_id IS NOT NULL
		  AND deleted_at IS NULL
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	var stale []liveTaskRow
	for rows.Next() {
		var row liveTaskRow
		if err := rows.Scan(&row.slug, &row.provider, &row.sessionID, &row.workDir, &row.worktreePath); err != nil {
			return
		}
		if live.has(row.provider, row.sessionID) {
			continue
		}
		// Fresh codex sessions have no id on the command line — match by their
		// working dir / worktree so an actively-running session isn't flipped to
		// dead.
		if row.provider == "codex" && (live.codexDirLive(row.workDir) || live.codexDirLive(row.worktreePath)) {
			continue
		}
		stale = append(stale, row)
	}
	if err := rows.Err(); err != nil {
		return
	}

	if len(stale) == 0 {
		return
	}
	cutoff := time.Now().Add(-r.grace).UTC().Format(time.RFC3339)
	for _, row := range stale {
		state, err := flowdb.AgentRuntimeStateBySessionID(db, row.provider, row.sessionID)
		if err != nil {
			continue
		}
		switch state.Status {
		case "dead", "idle", "released":
			// Already terminal — nothing to reconcile.
			continue
		}
		if state.UpdatedAt > cutoff {
			// Hook is still freshly active; give it a grace window
			// before we flip to dead.
			continue
		}
		_ = flowdb.UpsertAgentRuntimeState(db, flowdb.AgentRuntimeStateInput{
			Provider:  row.provider,
			SessionID: row.sessionID,
			TaskSlug:  row.slug,
			Status:    "dead",
			EventKind: "liveness_reconciled",
			Message:   "process not found in live scan",
			// Forcing seq=0 so the conditional upsert applies regardless
			// of what the hook last wrote — this row is authoritative
			// over any stale running event we beat to the punch.
			Seq:     0,
			RawJSON: "",
		})
		r.srv.publishLiveness(row.provider, row.sessionID, row.slug, "dead", "process not found")
	}
}
