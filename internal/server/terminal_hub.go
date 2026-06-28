package server

import (
	"flow/internal/flowdb"
	"fmt"
	"strings"
	"sync"
	"time"
)

func (h *terminalHub) attach(slug string, cols, rows int) (*terminalSession, error) {
	h.mu.Lock()
	if sess := h.sessions[slug]; sess != nil && sess.running() {
		h.mu.Unlock()
		return sess, nil
	}
	h.mu.Unlock()

	unlock := h.lockLaunch(slug)
	defer unlock()

	h.mu.Lock()
	if sess := h.sessions[slug]; sess != nil && sess.running() {
		h.mu.Unlock()
		return sess, nil
	}
	h.mu.Unlock()

	launch, err := h.server.prepareTerminalLaunch(slug)
	if err != nil {
		return nil, err
	}
	sess, err := h.startSessionLocked(launch, cols, rows)
	if err != nil {
		if launch.Created {
			h.server.rollbackPreparedTerminalLaunch(launch)
		}
		return nil, err
	}
	h.mu.Lock()
	h.sessions[slug] = sess
	h.mu.Unlock()
	movedPaused := h.server != nil && h.server.markLaunchResumed(launch)
	if h.server != nil && h.server.inboxMonitors != nil {
		h.server.inboxMonitors.start(slug)
	}
	if movedPaused || (h.wakes != nil && h.wakes.has(slug)) {
		go func() {
			h.waitForSessionReady(slug, steererWakeStable, steererWakeTimeout)
			h.flushWakes(slug)
		}()
	}
	return sess, nil
}

func (h *terminalHub) attachBrowser(slug string, cols, rows int) (*terminalSession, bool, error) {
	h.mu.Lock()
	sess := h.sessions[slug]
	h.mu.Unlock()
	if sess != nil && sess.running() && sess.clientCount() > 0 {
		launch, err := h.server.prepareTerminalLaunch(slug)
		if err != nil {
			return nil, false, err
		}
		transient, err := h.startSessionLocked(launch, cols, rows)
		if err != nil {
			if launch.Created {
				h.server.rollbackPreparedTerminalLaunch(launch)
			}
			return nil, false, err
		}
		return transient, true, nil
	}
	sess, err := h.attach(slug, cols, rows)
	return sess, false, err
}

func (h *terminalHub) lockLaunch(slug string) func() {
	h.mu.Lock()
	if h.launchLocks == nil {
		h.launchLocks = map[string]*sync.Mutex{}
	}
	lock := h.launchLocks[slug]
	if lock == nil {
		lock = &sync.Mutex{}
		h.launchLocks[slug] = lock
	}
	h.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (h *terminalHub) registerFloatingLaunch(launch terminalLaunch, title string) floatingTerminalResponse {
	h.mu.Lock()
	h.floatingLaunches[launch.Slug] = launch
	if _, ok := h.floatingMeta[launch.Slug]; !ok {
		h.floatingMeta[launch.Slug] = floatingSessionMeta{
			Provider: launch.Provider,
			Title:    strings.TrimSpace(title),
			Created:  time.Now(),
		}
	}
	h.persistFloatingLocked()
	h.mu.Unlock()
	// Let the tray (sourced from the UiData snapshot) pick up the new session.
	if h.server != nil {
		h.server.publishUIChange("floating")
	}
	return floatingTerminalResponse{
		ID:       launch.Slug,
		Provider: launch.Provider,
		Title:    strings.TrimSpace(title),
	}
}

// floatingResponse returns the floatingTerminalResponse for a registered adhoc
// session so a reopen can hand the UI the same shape it got on launch, letting
// it reattach to a live (or registered-but-detached) session. ok is false when
// the slug is not in the floating registry (never launched, or already
// stopped). Title/provider come from the stored metadata, falling back to the
// launch when metadata is sparse.
func (h *terminalHub) floatingResponse(slug string) (floatingTerminalResponse, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	launch, ok := h.floatingLaunches[slug]
	if !ok {
		return floatingTerminalResponse{}, false
	}
	meta := h.floatingMeta[slug]
	provider := meta.Provider
	if provider == "" {
		provider = launch.Provider
	}
	return floatingTerminalResponse{
		ID:       slug,
		Provider: provider,
		Title:    strings.TrimSpace(meta.Title),
	}, true
}

// floatingSessions returns the current set of adhoc floating sessions for the
// tray, marking which ones currently have a live PTY attached. Order is left
// to the client (it sorts by created_at).
func (h *terminalHub) floatingSessions() []floatingSessionInfo {
	type snap struct {
		id, provider, title, created, sid string
		running                           bool
	}
	h.mu.Lock()
	snaps := make([]snap, 0, len(h.floatingLaunches))
	for id, launch := range h.floatingLaunches {
		meta := h.floatingMeta[id]
		provider := meta.Provider
		if provider == "" {
			provider = launch.Provider
		}
		sid := launch.SessionID
		running := false
		if sess := h.sessions[id]; sess != nil {
			running = sess.running()
			// Codex captures its real thread id after launch; prefer it so the
			// runtime-state lookup keys on the id the hook actually reports.
			if cap := strings.TrimSpace(sess.sessionID); cap != "" {
				sid = cap
			}
		}
		created := ""
		if !meta.Created.IsZero() {
			created = meta.Created.UTC().Format(time.RFC3339)
		}
		snaps = append(snaps, snap{id: id, provider: provider, title: meta.Title, created: created, sid: sid, running: running})
	}
	h.mu.Unlock()

	// Resolve waiting state outside the hub lock (it's a DB read). Adhoc
	// sessions have no task row, but the agent hook still records their runtime
	// state keyed by session_id, so "awaiting your input" is queryable here.
	out := make([]floatingSessionInfo, 0, len(snaps))
	for _, s := range snaps {
		waiting, why := false, ""
		if s.sid != "" && h.server != nil && h.server.cfg.DB != nil {
			if st, err := flowdb.AgentRuntimeStateBySessionID(h.server.cfg.DB, s.provider, s.sid); err == nil && st != nil && st.Status == "waiting" {
				waiting = true
				if st.Message.Valid {
					why = strings.TrimSpace(st.Message.String)
				}
			}
		}
		out = append(out, floatingSessionInfo{
			ID: s.id, Provider: s.provider, Title: s.title, Running: s.running,
			Waiting: waiting, WaitingWhy: why, Created: s.created,
		})
	}
	return out
}

// stopFloating terminates an adhoc floating session's PTY (if attached) and
// forgets its launch + metadata, so it disappears from the tray and can't be
// reattached. Returns true when there was something to stop. Mirrors stop()
// but also clears the floating registry. Publishes a UI change so the tray
// updates.
func (h *terminalHub) stopFloating(id string) bool {
	h.mu.Lock()
	_, known := h.floatingLaunches[id]
	delete(h.floatingLaunches, id)
	delete(h.floatingMeta, id)
	sess := h.sessions[id]
	delete(h.sessions, id)
	h.persistFloatingLocked()
	h.mu.Unlock()
	if sess != nil {
		if h.server != nil && h.server.inboxMonitors != nil {
			h.server.inboxMonitors.stop(id)
		}
		sess.terminate()
	}
	if (known || sess != nil) && h.server != nil {
		h.server.publishUIChange("floating")
	}
	return known || sess != nil
}

// startFloatingDetached starts a registered floating session's PTY WITHOUT a UI
// client attached, so its agent runs in the background whether or not the
// operator opens the window — used for ephemeral sends, where the reply must be
// posted regardless and the floating window is opt-in (a tray chip to watch).
// Output buffers into scrollback and replays when they later attach. No-op if it
// is already running. Default size matches the floating WS handler so a later
// attach doesn't reflow.
func (h *terminalHub) startFloatingDetached(id string) error {
	h.mu.Lock()
	if sess := h.sessions[id]; sess != nil && sess.running() {
		h.mu.Unlock()
		return nil
	}
	_, ok := h.floatingLaunches[id]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("floating terminal not found: %s", id)
	}

	unlock := h.lockLaunch(id)
	defer unlock()

	h.mu.Lock()
	if sess := h.sessions[id]; sess != nil && sess.running() {
		h.mu.Unlock()
		return nil
	}
	launch, ok := h.floatingLaunches[id]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("floating terminal not found: %s", id)
	}

	sess, err := h.startSessionLocked(launch, 120, 32)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if _, ok := h.floatingLaunches[id]; !ok {
		h.mu.Unlock()
		sess.terminate()
		return fmt.Errorf("floating terminal not found: %s", id)
	}
	h.sessions[id] = sess
	h.mu.Unlock()
	return nil
}

func (h *terminalHub) attachFloating(id string, cols, rows int) (*terminalSession, error) {
	h.mu.Lock()
	if sess := h.sessions[id]; sess != nil && sess.running() {
		h.mu.Unlock()
		return sess, nil
	}
	_, ok := h.floatingLaunches[id]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("floating terminal not found: %s", id)
	}

	unlock := h.lockLaunch(id)
	defer unlock()

	h.mu.Lock()
	if sess := h.sessions[id]; sess != nil && sess.running() {
		h.mu.Unlock()
		return sess, nil
	}
	launch, ok := h.floatingLaunches[id]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("floating terminal not found: %s", id)
	}

	sess, err := h.startSessionLocked(launch, cols, rows)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if _, ok := h.floatingLaunches[id]; !ok {
		h.mu.Unlock()
		sess.terminate()
		return nil, fmt.Errorf("floating terminal not found: %s", id)
	}
	h.sessions[id] = sess
	h.mu.Unlock()
	return sess, nil
}

func (h *terminalHub) attachFloatingBrowser(id string, cols, rows int) (*terminalSession, bool, error) {
	h.mu.Lock()
	sess := h.sessions[id]
	if sess != nil && sess.running() && sess.clientCount() > 0 {
		launch, ok := h.floatingLaunches[id]
		h.mu.Unlock()
		if !ok {
			return nil, false, fmt.Errorf("floating terminal not found: %s", id)
		}
		transient, err := h.startSessionLocked(launch, cols, rows)
		if err != nil {
			return nil, false, err
		}
		return transient, true, nil
	}
	h.mu.Unlock()
	sess, err := h.attachFloating(id, cols, rows)
	return sess, false, err
}

func (h *terminalHub) stop(slug string) error {
	h.mu.Lock()
	sess := h.sessions[slug]
	delete(h.sessions, slug)
	h.mu.Unlock()
	if sess != nil {
		if h.server != nil && h.server.inboxMonitors != nil {
			h.server.inboxMonitors.stop(slug)
		}
		sess.terminate()
	}
	if h.sharedRunningCache != nil {
		h.sharedRunningCache.invalidate(slug)
	}
	name := sharedTerminalSessionName(slug)
	if sharedTerminalHasSession(name) {
		if err := sharedTerminalKillSession(name); err != nil {
			return fmt.Errorf("stop shared terminal %s: %w", name, err)
		}
	}
	if sharedTerminalHasSession(name) {
		return fmt.Errorf("shared terminal %s is still running", name)
	}
	return nil
}

func (h *terminalHub) sendInput(slug, data string) error {
	h.mu.Lock()
	sess := h.sessions[slug]
	h.mu.Unlock()
	if sess == nil || !sess.running() {
		return fmt.Errorf("terminal session for %s is not running", slug)
	}
	return sess.write(data)
}
