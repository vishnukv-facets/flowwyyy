package server

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// persistedFloating is one adhoc floating session stored on disk so the tray
// survives a flow-server restart. The PTY itself lives in a tmux session that
// outlives the server; we only need to remember the launch + metadata to
// reattach and re-list it.
type persistedFloating struct {
	Launch terminalLaunch      `json:"launch"`
	Meta   floatingSessionMeta `json:"meta"`
}

func (h *terminalHub) floatingStorePath() string {
	if h.server == nil || h.server.cfg.FlowRoot == "" {
		return ""
	}
	return filepath.Join(h.server.cfg.FlowRoot, "floating-sessions.json")
}

// persistFloatingLocked snapshots the floating registry to disk. The caller
// MUST hold h.mu. Best-effort: a persistence failure must never break a live
// session, so errors are swallowed — the in-memory registry stays authoritative.
func (h *terminalHub) persistFloatingLocked() {
	path := h.floatingStorePath()
	if path == "" {
		return
	}
	out := make([]persistedFloating, 0, len(h.floatingLaunches))
	for slug, launch := range h.floatingLaunches {
		out = append(out, persistedFloating{Launch: launch, Meta: h.floatingMeta[slug]})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// loadFloatingFromDisk repopulates the floating registry on boot, keeping only
// sessions whose underlying tmux session is still alive (reattachable). Dead
// ones are dropped so a restart never resurrects a finished adhoc session by
// re-running its original prompt. Rewrites the store to the surviving set.
func (h *terminalHub) loadFloatingFromDisk() {
	path := h.floatingStorePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // no store yet (the common case) — nothing to restore
	}
	var stored []persistedFloating
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	checkTmux := sharedTerminalAvailable()
	h.mu.Lock()
	for _, pf := range stored {
		slug := pf.Launch.Slug
		if slug == "" {
			continue
		}
		// Without tmux a restart kills the PTY, so a persisted session can't be
		// reattached; only keep ones whose shared session still exists.
		if checkTmux && !sharedTerminalHasSession(sharedTerminalSessionName(slug)) {
			continue
		}
		h.floatingLaunches[slug] = pf.Launch
		h.floatingMeta[slug] = pf.Meta
	}
	h.persistFloatingLocked()
	h.mu.Unlock()
}
