package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flow/internal/agenthooks"
	"flow/internal/agents"
	"flow/internal/flowdb"
	"flow/internal/steering"
	"flow/internal/workdirreg"
	"flow/internal/worktree"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// terminal scrollback defaults are intentionally bounded. tmux remains the
// source of authoritative pane history, while this in-process buffer is a replay
// fallback and a source for lightweight status snapshots.
const (
	defaultTerminalScrollbackBytes    = 128 * 1024 * 1024
	defaultTerminalScrollbackHeadroom = 1024 * 1024
	defaultTerminalReplayChunkBytes   = 256 * 1024
	maxTerminalScrollbackBytes        = 1024 * 1024 * 1024
)

func terminalScrollbackBytes() int {
	return positiveIntEnv("FLOW_TERMINAL_SCROLLBACK_BYTES", defaultTerminalScrollbackBytes, 1024*1024, maxTerminalScrollbackBytes)
}

func terminalScrollbackHeadroomBytes() int {
	return positiveIntEnv("FLOW_TERMINAL_SCROLLBACK_HEADROOM_BYTES", defaultTerminalScrollbackHeadroom, 64*1024, 16*1024*1024)
}

func terminalReplayChunkBytes() int {
	return positiveIntEnv("FLOW_TERMINAL_REPLAY_CHUNK_BYTES", defaultTerminalReplayChunkBytes, 16*1024, 4*1024*1024)
}

func positiveIntEnv(key string, def, minValue, maxValue int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minValue {
		return def
	}
	if maxValue > 0 && n > maxValue {
		return maxValue
	}
	return n
}

var terminalUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || strings.Contains(origin, r.Host)
	},
}

type terminalHub struct {
	server           *Server
	mu               sync.Mutex
	sessions         map[string]*terminalSession
	floatingLaunches map[string]terminalLaunch
	// floatingMeta carries the display metadata (title, provider, created)
	// for each registered adhoc floating session, so the tray can list them
	// independently of whether their PTY is currently attached.
	floatingMeta map[string]floatingSessionMeta
	launchLocks  map[string]*sync.Mutex
	// sharedRunningCache backs sharedRunning, which is invoked once per task
	// per SSE tick (every 2s). Each raw call forks `tmux has-session` — with
	// N tasks visible that's N forks per tick. The cache collapses repeats
	// within a 2.5s window; we explicitly invalidate on create/kill so the
	// UI never lies after a state change the user just triggered.
	sharedRunningCache *ttlCache[string, bool]
}

type terminalLaunch struct {
	Slug           string
	SessionID      string
	Provider       string
	PermissionMode string
	WorkDir        string
	Args           []string
	FreeAgent      bool
	Created        bool
	NeedsCapture   bool
	StartedAt      time.Time
}

// floatingSessionMeta is the display metadata for one registered adhoc
// floating (Ask Flow) session. Stored separately from terminalLaunch so the
// tray can render a session before/after its PTY is attached.
type floatingSessionMeta struct {
	Provider string
	Title    string
	Created  time.Time
}

type terminalSession struct {
	hub        *terminalHub
	slug       string
	sessionID  string
	provider   string
	workDir    string
	sharedName string
	cmd        *exec.Cmd
	tty        *os.File
	done       chan struct{}

	mu           sync.Mutex
	clients      map[*terminalClient]struct{}
	scrollback   []byte
	closed       bool
	exitStatus   string
	lastOutputAt time.Time
	resizeOwner  *terminalClient
	cols         int
	rows         int
}

type terminalClient struct {
	conn      *websocket.Conn
	send      chan terminalWSMessage
	done      chan struct{}
	closeOnce sync.Once
	cols      int
	rows      int
}

type terminalWSMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
}

func newTerminalHub(s *Server) *terminalHub {
	return &terminalHub{
		server:             s,
		sessions:           map[string]*terminalSession{},
		floatingLaunches:   map[string]terminalLaunch{},
		floatingMeta:       map[string]floatingSessionMeta{},
		launchLocks:        map[string]*sync.Mutex{},
		sharedRunningCache: newTTLCache[string, bool](2500 * time.Millisecond),
	}
}

func (s *Server) handleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if err := validateSlug(slug); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cols := intQueryDefault(r, "cols", 120)
	rows := intQueryDefault(r, "rows", 32)
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sess, transient, err := s.terminals.attachBrowser(slug, cols, rows)
	if err != nil {
		_ = conn.WriteJSON(terminalWSMessage{Type: "error", Message: err.Error()})
		_ = conn.Close()
		return
	}

	client := &terminalClient{conn: conn, send: make(chan terminalWSMessage, 128), done: make(chan struct{})}
	sess.addClient(client, true, cols, rows)

	go client.writeLoop()
	client.readLoop(sess)
	sess.removeClient(client)
	if transient {
		sess.detachBrowserAttach()
	}
}

func (s *Server) handleFloatingTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if err := validateSlug(id); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cols := intQueryDefault(r, "cols", 120)
	rows := intQueryDefault(r, "rows", 32)
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sess, transient, err := s.terminals.attachFloatingBrowser(id, cols, rows)
	if err != nil {
		_ = conn.WriteJSON(terminalWSMessage{Type: "error", Message: err.Error()})
		_ = conn.Close()
		return
	}

	client := &terminalClient{conn: conn, send: make(chan terminalWSMessage, 128), done: make(chan struct{})}
	sess.addClient(client, true, cols, rows)

	go client.writeLoop()
	client.readLoop(sess)
	sess.removeClient(client)
	if transient {
		sess.detachBrowserAttach()
	}
}

func intQueryDefault(r *http.Request, key string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return def
	}
	if value > 500 {
		return 500
	}
	return value
}

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
	if h.server != nil && h.server.inboxMonitors != nil {
		h.server.inboxMonitors.start(slug)
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
	launch, ok := h.floatingLaunches[id]
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
	launch, ok = h.floatingLaunches[id]
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
	launch, ok := h.floatingLaunches[id]
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
	launch, ok = h.floatingLaunches[id]
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

func (h *terminalHub) stop(slug string) {
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

func terminalPasteInput(prompt string) string {
	return "\x1b[200~" + prompt + "\x1b[201~\r"
}

func (h *terminalHub) wakeTask(slug, prompt string) error {
	// Paste the prompt WITHOUT a trailing newline, then submit with a separate,
	// delayed Enter. A \r in the same write as the bracketed-paste terminator
	// gets absorbed into the (usually multi-line) input buffer instead of
	// submitting — the prompt ends up sitting unsent in the box. Sending Enter
	// as a distinct keystroke, after the editor has exited paste mode, reliably
	// submits.
	paste := "\x1b[200~" + prompt + "\x1b[201~"
	if err := h.sendInput(slug, paste); err != nil {
		if _, aerr := h.attach(slug, 120, 32); aerr != nil {
			return aerr
		}
		if err := h.sendInput(slug, paste); err != nil {
			return err
		}
	}
	go h.submitAfterPaste(slug)
	return nil
}

// submitAfterPaste presses Enter shortly after a wake paste, once the input
// editor has had a beat to finish processing the paste and leave paste mode.
func (h *terminalHub) submitAfterPaste(slug string) {
	time.Sleep(250 * time.Millisecond)
	_ = h.sendInput(slug, "\r")
}

// wakeSharedTask injects a wake prompt straight into the task's detached tmux
// session via `tmux send-keys`, with NO browser PTY required. It returns true
// when a live tmux session was found and the paste was sent.
//
// Why this exists: agents run inside a persistent tmux session (see
// startSessionLocked) and the browser terminal is only a `tmux attach` bridge
// living in this server process's in-memory hub. After a `flow ui serve`
// restart the agent keeps running in tmux, but the hub is empty until the user
// re-opens the session — so terminalHub.running(slug) is false even though the
// agent is alive and waiting. Without this path, deliverInboxEvents would
// mistake the still-live tmux session for a "native" (user-owned) session,
// decline to inject, advance the inbox cursor, and silently drop the wake.
//
// Mirrors wakeTask: bracketed-paste the prompt (so a multi-line body lands in
// the editor as pasted text, not submitted line-by-line), then a separate,
// delayed Enter once the editor has left paste mode.
func (h *terminalHub) wakeSharedTask(slug, prompt string) bool {
	if !sharedTerminalAvailable() {
		return false
	}
	name := sharedTerminalSessionName(slug)
	if !sharedTerminalHasSession(name) {
		return false
	}
	paste := "\x1b[200~" + prompt + "\x1b[201~"
	if _, err := sharedTerminalCommand("send-keys", "-t", name, "-l", paste); err != nil {
		return false
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		_, _ = sharedTerminalCommand("send-keys", "-t", name, "Enter")
	}()
	return true
}

func (h *terminalHub) scrollbackText(slug string, limit int) (string, bool) {
	h.mu.Lock()
	sess := h.sessions[slug]
	h.mu.Unlock()
	if sess == nil {
		return "", false
	}
	sess.mu.Lock()
	data := append([]byte(nil), sess.scrollback...)
	sess.mu.Unlock()
	if len(data) == 0 {
		return "", true
	}
	if limit > 0 && len(data) > limit {
		data = data[len(data)-limit:]
	}
	return string(stripTerminalAltScreenControls(data)), true
}

func (h *terminalHub) lastOutputAt(slug string) (time.Time, bool) {
	h.mu.Lock()
	sess := h.sessions[slug]
	h.mu.Unlock()
	if sess == nil {
		return time.Time{}, false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.lastOutputAt, !sess.lastOutputAt.IsZero()
}

func (h *terminalHub) sharedSessionName(slug string) (string, bool) {
	h.mu.Lock()
	sess := h.sessions[slug]
	h.mu.Unlock()
	if sess == nil || !sess.running() {
		return "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.sharedName, sess.sharedName != ""
}

func (h *terminalHub) sharedRunning(slug string) bool {
	if v, ok := h.sharedRunningCache.get(slug); ok {
		return v
	}
	var running bool
	if name, ok := h.sharedSessionName(slug); ok {
		running = sharedTerminalHasSession(name)
	} else {
		running = sharedTerminalHasSession(sharedTerminalSessionName(slug))
	}
	h.sharedRunningCache.set(slug, running)
	return running
}

func (h *terminalHub) startSessionLocked(launch terminalLaunch, cols, rows int) (*terminalSession, error) {
	provider := launch.Provider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	if err := h.server.ensureProviderAvailable(provider); err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	sharedName := ""
	initialScrollback := []byte(nil)
	if sharedTerminalAvailable() {
		name, created, err := h.server.ensureSharedTerminalSession(launch, cols, rows)
		if err != nil {
			return nil, err
		}
		sharedName = name
		if !created {
			history, historyErr := sharedTerminalCaptureHistory(name)
			if historyErr != nil {
				fmt.Fprintf(os.Stderr, "warning: capture shared terminal history: %v\n", historyErr)
			} else {
				initialScrollback = history
			}
		}
		// `-f` is server-startup only — only the first tmux invocation
		// that actually starts the tmux server reads the config; later
		// invocations against the same server ignore it. Passing it on
		// attach is a defensive belt-and-braces in case attach races
		// ahead of new-session in some path. ensureTmuxConfig errors
		// are non-fatal: tmux without our config still works, just
		// without the mouse-scroll default.
		attachArgs := []string{"attach-session", "-t", name}
		if cfgPath, cfgErr := ensureTmuxConfig(h.server.cfg.FlowRoot); cfgErr == nil && cfgPath != "" {
			attachArgs = append([]string{"-f", cfgPath}, attachArgs...)
		}
		cmd = exec.Command("tmux", attachArgs...)
	} else {
		bin := provider
		if provider == agents.ProviderClaude {
			bin = "claude"
		}
		cmd = exec.Command(bin, launch.Args...)
	}
	cmd.Dir = launch.WorkDir
	env := terminalEnvWithHook(h.server.cfg.FlowRoot, h.server.cfg.CommandPath, h.server.cfg.HookURL)
	if launch.FreeAgent {
		env = append(env, "FLOW_FREE_AGENT=1")
	} else {
		env = append(env, "FLOW_TASK="+launch.Slug)
	}
	cmd.Env = append(env,
		"FLOW_SESSION_PROVIDER="+provider,
		"FLOW_PERMISSION_MODE="+normalizedTerminalPermissionMode(launch.PermissionMode),
	)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("start %s terminal: %w", provider, err)
	}
	sess := &terminalSession{
		hub:        h,
		slug:       launch.Slug,
		sessionID:  launch.SessionID,
		provider:   provider,
		workDir:    launch.WorkDir,
		sharedName: sharedName,
		cmd:        cmd,
		tty:        f,
		done:       make(chan struct{}),
		clients:    map[*terminalClient]struct{}{},
		scrollback: initialScrollback,
		cols:       cols,
		rows:       rows,
	}
	go sess.readPTY()
	go sess.wait()
	if launch.NeedsCapture {
		go sess.captureCodexSession(launch.StartedAt)
	}
	// A new session just started; force the next sharedRunning check to
	// re-query tmux so the UI flips to "running" within one tick.
	h.sharedRunningCache.invalidate(launch.Slug)
	return sess, nil
}

func terminalEnv(flowRoot, commandPath string) []string {
	env := os.Environ()
	env = setEnvValue(env, "TERM", "xterm-256color")
	env = setEnvValue(env, "COLORTERM", "truecolor")
	env = setEnvValue(env, "FORCE_COLOR", "1")
	env = setEnvValue(env, "TERM_PROGRAM", "flow-ui")
	env = setEnvValue(env, "CLAUDE_CODE_NO_FLICKER", "0")
	env = setEnvValue(env, "CLAUDE_CODE_DISABLE_MOUSE", "1")
	env = appendEnvDefault(env, "LANG", "en_US.UTF-8")
	env = appendEnvDefault(env, "LC_CTYPE", "en_US.UTF-8")
	// Mark this PTY as flow-spawned for the agent-event hook (see
	// internal/app/hook.go injectHookMetadata). Lets the server tell
	// apart flow-managed sessions from ambient agents in the same repo.
	env = setEnvValue(env, "FLOW_HOOK_OWNED", "1")
	if root := strings.TrimSpace(flowRoot); root != "" {
		env = setEnvValue(env, "FLOW_ROOT", root)
	} else if root := os.Getenv("FLOW_ROOT"); root != "" {
		env = setEnvValue(env, "FLOW_ROOT", root)
	}
	// Make `gh` work inside sandboxed agent sessions. Codex's workspace-write
	// sandbox can't read the macOS Keychain where `gh` stores its token, so
	// `gh` fails to authenticate even with network enabled. Resolve the token
	// here (in the server, outside any sandbox) and pass it via GH_TOKEN, which
	// gh prefers over keychain/config. No-op if a token is already in the env
	// or can't be resolved (e.g. gh not logged in).
	if envValueLocal(env, "GH_TOKEN") == "" && envValueLocal(env, "GITHUB_TOKEN") == "" {
		if tok := ghAuthToken(); tok != "" {
			env = setEnvValue(env, "GH_TOKEN", tok)
		}
	}
	return prependCommandDirToPath(env, commandPath)
}

// ghAuthToken resolves the gh CLI token from the server's environment (outside
// any agent sandbox). Overridable in tests. Returns "" when gh is unavailable
// or not logged in.
var ghAuthToken = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "-h", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func terminalEnvWithHook(flowRoot, commandPath, hookURL string) []string {
	env := terminalEnv(flowRoot, commandPath)
	if hookURL = strings.TrimSpace(hookURL); hookURL != "" {
		env = setEnvValue(env, "FLOW_HOOK_URL", hookURL)
	}
	return env
}

func terminalEnvMap(flowRoot, commandPath, hookURL, slug, provider, permissionMode string, freeAgent bool) map[string]string {
	env := terminalEnvWithHook(flowRoot, commandPath, hookURL)
	out := map[string]string{
		"TERM":                      "xterm-256color",
		"COLORTERM":                 "truecolor",
		"FORCE_COLOR":               "1",
		"TERM_PROGRAM":              "flow-ui",
		"CLAUDE_CODE_NO_FLICKER":    "0",
		"CLAUDE_CODE_DISABLE_MOUSE": "1",
		"LANG":                      "en_US.UTF-8",
		"LC_CTYPE":                  "en_US.UTF-8",
		"FLOW_HOOK_OWNED":           "1",
		"FLOW_SESSION_PROVIDER":     provider,
		"FLOW_PERMISSION_MODE":      normalizedTerminalPermissionMode(permissionMode),
	}
	if freeAgent {
		out["FLOW_FREE_AGENT"] = "1"
	} else {
		out["FLOW_TASK"] = slug
	}
	for _, key := range []string{"PATH", "FLOW_ROOT", "FLOW_HOOK_URL", "GH_TOKEN"} {
		if value := envValueLocal(env, key); value != "" {
			out[key] = value
		}
	}
	return out
}

func normalizedTerminalPermissionMode(mode string) string {
	normalized, err := flowdb.NormalizePermissionMode(mode)
	if err != nil {
		return flowdb.DefaultPermissionMode
	}
	return normalized
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func appendEnvDefault(env []string, key, value string) []string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

func prependCommandDirToPath(env []string, commandPath string) []string {
	commandPath = strings.TrimSpace(commandPath)
	if commandPath == "" {
		return env
	}
	dir := filepath.Dir(commandPath)
	if dir == "." || dir == "" {
		return env
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	current := envValueLocal(env, "PATH")
	if current == "" {
		return setEnvValue(env, "PATH", dir)
	}
	for _, part := range filepath.SplitList(current) {
		if part == dir {
			return env
		}
	}
	return setEnvValue(env, "PATH", dir+string(os.PathListSeparator)+current)
}

func envValueLocal(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

var (
	sharedTerminalLookPath = exec.LookPath
	sharedTerminalCommand  = func(args ...string) ([]byte, error) {
		return exec.Command("tmux", args...).CombinedOutput()
	}

	// sharedTerminalAvailable resolves once per process and is the result of
	// an exec.LookPath("tmux") — which walks every directory in $PATH doing
	// stat() syscalls. Before caching, this ran on every per-task UI refresh
	// (~15 tasks × every 2s). Tests that swap sharedTerminalLookPath must
	// call resetSharedTerminalAvailable() to invalidate.
	sharedTerminalAvailableOnce sync.Once
	sharedTerminalAvailableVal  bool
)

func sharedTerminalAvailable() bool {
	sharedTerminalAvailableOnce.Do(func() {
		_, err := sharedTerminalLookPath("tmux")
		sharedTerminalAvailableVal = err == nil
	})
	return sharedTerminalAvailableVal
}

// resetSharedTerminalAvailable forces the next sharedTerminalAvailable call to
// re-run LookPath. Tests that swap sharedTerminalLookPath rely on this.
func resetSharedTerminalAvailable() {
	sharedTerminalAvailableOnce = sync.Once{}
	sharedTerminalAvailableVal = false
}

func sharedTerminalSessionName(slug string) string {
	var b strings.Builder
	b.WriteString("flow-")
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 80 {
		return name[:80]
	}
	return name
}

func sharedTerminalHasSession(name string) bool {
	if strings.TrimSpace(name) == "" || !sharedTerminalAvailable() {
		return false
	}
	_, err := sharedTerminalCommand("has-session", "-t", name)
	return err == nil
}

func sharedTerminalSessionMatchesLaunch(name string, launch terminalLaunch) (bool, error) {
	if strings.TrimSpace(name) == "" || !sharedTerminalAvailable() {
		return false, nil
	}
	out, err := sharedTerminalCommand("list-panes", "-t", name, "-F", "#{pane_start_command}")
	if err != nil {
		return false, fmt.Errorf("inspect shared terminal session %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	command := string(out)
	task := shellCommandEnvValue(command, "FLOW_TASK")
	provider := shellCommandEnvValue(command, "FLOW_SESSION_PROVIDER")
	permissionMode := shellCommandEnvValue(command, "FLOW_PERMISSION_MODE")
	// Older/manual sessions may not carry Flow's launch metadata. Preserve
	// them unless we can positively identify a stale Flow-owned mismatch.
	if task == "" && provider == "" && permissionMode == "" {
		return true, nil
	}
	wantProvider := strings.TrimSpace(launch.Provider)
	if wantProvider == "" {
		wantProvider = agents.ProviderClaude
	}
	wantPermissionMode := normalizedTerminalPermissionMode(launch.PermissionMode)
	if task != "" && task != launch.Slug {
		return false, nil
	}
	if provider != "" && provider != wantProvider {
		return false, nil
	}
	if permissionMode != "" && normalizedTerminalPermissionMode(permissionMode) != wantPermissionMode {
		return false, nil
	}
	return true, nil
}

func shellCommandEnvValue(command, key string) string {
	prefix := key + "="
	for _, field := range strings.Fields(command) {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		value := strings.TrimPrefix(field, prefix)
		return strings.Trim(value, `"'`)
	}
	return ""
}

func sharedTerminalCaptureHistory(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("tmux session name not set")
	}
	out, err := sharedTerminalCommand("capture-pane", "-p", "-e", "-S", "-", "-E", "-1", "-t", name)
	if err != nil {
		return nil, fmt.Errorf("capture tmux history for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return normalizeCapturedPaneForTerminal(out), nil
}

func normalizeCapturedPaneForTerminal(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	// Drop BACKGROUND colors from the replayed scrollback. This is the root fix
	// for the colored-block bleed (green diff rows, grey input rows) the browser
	// terminal showed on attach.
	//
	// Why background, and why only the replay: tmux stores every history line at
	// the width the pane had when it was written and never reflows it on resize,
	// so capture-pane replays lines as wide as the widest client this session
	// ever had (often 150–175 cols). Replayed into a narrower browser grid those
	// lines autowrap, and xterm keeps the active background across the wrap —
	// painting the overflow rows too. A run of such rows (a diff block, a pasted
	// prompt) stacks into a solid colored brick that bleeds over neighbours. We
	// can't reflow multi-width history, and the width comes from real content
	// (not just trailing padding), so the only width-independent cure is to stop
	// the background from existing in the reconstructed history. Foreground and
	// attributes are kept, so the scrollback stays readable (diff +/- markers,
	// status colors, etc.). The LIVE stream is NOT normalized — it renders at the
	// matched pane width — so interactive output keeps its full background color.
	data = stripBackgroundSGR(data)
	// Then collapse each line's trailing whitespace so a long blank (now
	// background-less) run can't wrap into stray empty rows. Trailing whitespace
	// is never display-significant in a terminal (blank cells), so this is safe.
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		lines[i] = stripTrailingCellPadding(line)
	}
	data = bytes.Join(lines, []byte("\r\n"))
	if !bytes.HasSuffix(data, []byte("\r\n")) {
		data = append(data, '\r', '\n')
	}
	return data
}

// sgrSeqRE matches one SGR (Select Graphic Rendition) sequence: ESC [ params m.
var sgrSeqRE = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// stripBackgroundSGR removes background-color parameters from every SGR sequence
// in the captured replay while preserving foreground colors and text attributes.
// Background params are: 40–47 / 100–107 (named), 48;5;n and 48;2;r;g;b
// (extended), and 49 (default-background). 38;… (extended foreground) is kept
// together with its color spec. A sequence that carried only background is
// dropped entirely; bare resets (ESC[m / ESC[0m) are preserved. See
// normalizeCapturedPaneForTerminal for why the replayed scrollback must shed its
// background to avoid wrap-bleed.
func stripBackgroundSGR(data []byte) []byte {
	return sgrSeqRE.ReplaceAllFunc(data, func(seq []byte) []byte {
		params := string(sgrSeqRE.FindSubmatch(seq)[1])
		if params == "" || params == "0" {
			return seq // reset-all — keep verbatim
		}
		toks := strings.Split(params, ";")
		kept := make([]string, 0, len(toks))
		for i := 0; i < len(toks); i++ {
			switch t := toks[i]; {
			case isSimpleBgParam(t):
				// drop named bg / default-bg
			case t == "48":
				// extended background — skip "48" and its color spec
				if i+2 < len(toks) && toks[i+1] == "5" {
					i += 2
				} else if i+4 < len(toks) && toks[i+1] == "2" {
					i += 4
				}
			case t == "38":
				// extended foreground — keep "38" and its color spec
				if i+2 < len(toks) && toks[i+1] == "5" {
					kept = append(kept, toks[i:i+3]...)
					i += 2
				} else if i+4 < len(toks) && toks[i+1] == "2" {
					kept = append(kept, toks[i:i+5]...)
					i += 4
				} else {
					kept = append(kept, t)
				}
			default:
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return nil // sequence was background-only
		}
		return []byte("\x1b[" + strings.Join(kept, ";") + "m")
	})
}

func isSimpleBgParam(t string) bool {
	switch t {
	case "40", "41", "42", "43", "44", "45", "46", "47",
		"100", "101", "102", "103", "104", "105", "106", "107",
		"49":
		return true
	}
	return false
}

// trailingSGRRE matches a single SGR (color/attribute) sequence anchored at the
// end of a line, e.g. ESC[49m, ESC[0m, ESC[m, ESC[0;39;49m.
var trailingSGRRE = regexp.MustCompile(`\x1b\[[0-9;]*m$`)

// stripTrailingCellPadding removes a line's trailing run of spaces while
// preserving the SGR reset sequences tmux emits at end-of-line — so the parser
// state stays clean but the wide trailing padding that fuels the wrap-bleed (see
// normalizeCapturedPaneForTerminal) is gone. Spaces may be interleaved with the
// trailing resets, so we peel spaces and SGRs off the end alternately, then
// re-append the collected resets in their original order.
func stripTrailingCellPadding(line []byte) []byte {
	var suffix []byte // trailing SGR sequences to re-append, in original order
	for {
		trimmed := bytes.TrimRight(line, " ")
		if len(trimmed) != len(line) {
			line = trimmed
			continue
		}
		loc := trailingSGRRE.FindIndex(line)
		if loc == nil {
			break
		}
		seq := append([]byte(nil), line[loc[0]:loc[1]]...)
		suffix = append(seq, suffix...)
		line = line[:loc[0]]
	}
	return append(line, suffix...)
}

func sharedTerminalKillSession(name string) error {
	if strings.TrimSpace(name) == "" || !sharedTerminalAvailable() {
		return nil
	}
	_, err := sharedTerminalCommand("kill-session", "-t", name)
	return err
}

func (s *Server) ensureSharedTerminalSession(launch terminalLaunch, cols, rows int) (string, bool, error) {
	if !sharedTerminalAvailable() {
		return "", false, errors.New("read-write native/browser terminal sharing requires tmux on PATH")
	}
	name := sharedTerminalSessionName(launch.Slug)
	if sharedTerminalHasSession(name) {
		matches, err := sharedTerminalSessionMatchesLaunch(name, launch)
		if err != nil {
			return "", false, err
		}
		if !matches {
			_ = sharedTerminalKillSession(name)
		} else {
			if err := ensureSharedTerminalScrollOptions(name); err != nil {
				return "", false, err
			}
			return name, false, nil
		}
	}
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 500 {
		rows = 500
	}
	provider := launch.Provider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	command := agentShellCommand(provider, launch.Args)
	env := terminalEnvMap(s.cfg.FlowRoot, s.cfg.CommandPath, s.cfg.HookURL, launch.Slug, provider, launch.PermissionMode, launch.FreeAgent)
	// Prepend `-f <flowRoot>/tmux.conf` so the tmux server we're about
	// to start picks up flow's defaults (mouse scroll + larger
	// scrollback). The user's ~/.tmux.conf is sourced from inside our
	// config so personal preferences still win. ensureTmuxConfig writes
	// the file on first call per process; errors degrade gracefully —
	// the session still starts, just without the mouse-scroll default.
	args := []string{}
	if cfgPath, cfgErr := ensureTmuxConfig(s.cfg.FlowRoot); cfgErr == nil && cfgPath != "" {
		args = append(args, "-f", cfgPath)
	}
	args = append(args,
		// Mouse ON for the shared tmux session. Each browser tab attaches as its own
		// tmux client, so native wheel scroll and copy-mode stay owned by tmux.
		"set-option",
		"-g",
		"mouse",
		"on",
		";",
		// Size the pane to the latest (i.e. our browser) client rather than the
		// smallest of all clients, so the grid tracks the browser on resize.
		"set-option",
		"-g",
		"window-size",
		"latest",
		";",
		// Disable tmux's status bar. flow's own UI already shows the session
		// name/status/branch in its chrome, so the bar is redundant — and the
		// status row's periodic repaints otherwise leak into the browser
		// terminal's scrollback as a stranded "[flow-...]" bar. Off = no bar.
		"set-option",
		"-g",
		"status",
		"off",
		";",
		// Let tmux / inner apps emit OSC 52 to the browser terminal so native
		// tmux copy-mode can reach the system clipboard.
		"set-option",
		"-g",
		"set-clipboard",
		"on",
		";",
		"set-window-option",
		"-g",
		"history-limit",
		sharedTerminalHistoryLimit(),
		";",
		"new-session",
		"-d",
		"-s", name,
		"-c", launch.WorkDir,
		"-x", strconv.Itoa(cols),
		"-y", strconv.Itoa(rows),
		shellCommandLine(command, env),
	)
	out, err := sharedTerminalCommand(args...)
	if err != nil {
		if sharedTerminalHasSession(name) {
			if optErr := ensureSharedTerminalScrollOptions(name); optErr != nil {
				return "", false, optErr
			}
			return name, false, nil
		}
		return "", false, fmt.Errorf("start shared terminal session %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	if err := ensureSharedTerminalScrollOptions(name); err != nil {
		_ = sharedTerminalKillSession(name)
		return "", false, err
	}
	return name, true, nil
}

var terminalAltScreenRE = regexp.MustCompile(`\x1b\[\?([0-9;]*)([hl])`)
var terminalGeneratedInputRE = regexp.MustCompile(`\x1b\[(?:\?[0-9;]*|>[0-9;]*)c`)

func stripTerminalAltScreenControls(data []byte) []byte {
	return terminalAltScreenRE.ReplaceAllFunc(data, func(seq []byte) []byte {
		match := terminalAltScreenRE.FindSubmatch(seq)
		if len(match) < 2 {
			return seq
		}
		for _, mode := range strings.Split(string(match[1]), ";") {
			switch mode {
			case "47", "1047", "1048", "1049":
				return nil
			}
		}
		return seq
	})
}

func stripTerminalGeneratedInput(data string) string {
	return terminalGeneratedInputRE.ReplaceAllString(data, "")
}

func (s *Server) prepareTerminalLaunch(slug string) (terminalLaunch, error) {
	tx, err := s.cfg.DB.Begin()
	if err != nil {
		return terminalLaunch{}, err
	}
	defer tx.Rollback()

	task, err := flowdb.ScanTask(tx.QueryRow("SELECT "+flowdb.TaskCols+" FROM tasks WHERE slug = ?", slug))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return terminalLaunch{}, fmt.Errorf("task not found: %s", slug)
		}
		return terminalLaunch{}, err
	}
	if task.Slug == overviewTaskSlug {
		flowRoot := strings.TrimSpace(s.cfg.FlowRoot)
		if flowRoot == "" {
			return terminalLaunch{}, errors.New("flow root is not configured")
		}
		absRoot, err := filepath.Abs(flowRoot)
		if err != nil {
			return terminalLaunch{}, err
		}
		if err := os.MkdirAll(filepath.Join(absRoot, "tasks", overviewTaskSlug, "updates"), 0o755); err != nil {
			return terminalLaunch{}, err
		}
		now := flowdb.NowISO()
		if _, err := tx.Exec(
			`UPDATE tasks SET
				project_slug = NULL,
				status = 'backlog',
				kind = 'regular',
				playbook_slug = NULL,
				work_dir = ?,
				waiting_on = NULL,
				session_provider = 'claude',
				session_id = NULL,
				session_started = NULL,
				session_last_resumed = NULL,
				status_changed_at = ?,
				updated_at = ?
			 WHERE slug = ?`,
			absRoot, now, now, task.Slug,
		); err != nil {
			return terminalLaunch{}, err
		}
		task.ProjectSlug = sql.NullString{}
		task.Status = "backlog"
		task.Kind = "regular"
		task.PlaybookSlug = sql.NullString{}
		task.WorkDir = absRoot
		task.WaitingOn = sql.NullString{}
		task.SessionID = sql.NullString{}
		task.SessionStarted = sql.NullString{}
		task.SessionLastResumed = sql.NullString{}
	}
	if strings.TrimSpace(task.WorkDir) == "" {
		return terminalLaunch{}, fmt.Errorf("task %s has no work_dir", task.Slug)
	}
	// A done task only reaches here via revisit/resume: both bridge entry points
	// (openBrowserTerminalBridge, openTaskBridge) gate startability for non-done
	// and rely on this path to reload the prior session, flipping it back to
	// in-progress below. So skip the startability gate for done — revisit must
	// not be blocked by a now-unfinished dependency — while a fresh start of a
	// non-done task still gets the full check.
	if task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
			return terminalLaunch{}, err
		}
	}

	now := flowdb.NowISO()
	sessionID := strings.TrimSpace(task.SessionID.String)
	provider := task.SessionProvider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	created := false
	if sessionID == "" {
		created = true
		if provider == agents.ProviderCodex {
			if _, err := tx.Exec(
				`UPDATE tasks SET
					status = 'in-progress',
					status_changed_at = ?,
					session_provider = 'codex',
					session_id = NULL,
					session_started = ?,
					updated_at = ?
				 WHERE slug = ?`,
				now, now, now, task.Slug,
			); err != nil {
				return terminalLaunch{}, err
			}
		} else {
			sessionID = uuid.NewString()
			if _, err := tx.Exec(
				`UPDATE tasks SET
					status = 'in-progress',
					status_changed_at = ?,
					session_provider = 'claude',
					session_id = ?,
					session_started = ?,
					updated_at = ?
				 WHERE slug = ?`,
				now, sessionID, now, now, task.Slug,
			); err != nil {
				return terminalLaunch{}, err
			}
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE tasks SET
				status = 'in-progress',
				session_last_resumed = ?,
				updated_at = ?
			 WHERE slug = ?`,
			now, now, task.Slug,
		); err != nil {
			return terminalLaunch{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return terminalLaunch{}, err
	}

	originalWorkDir := task.WorkDir
	if task.Slug != overviewTaskSlug {
		wt, wtErr := worktree.Ensure(originalWorkDir, provider, task.Slug)
		if wtErr != nil {
			if created {
				s.rollbackPreparedTerminalLaunch(terminalLaunch{
					Slug:      task.Slug,
					SessionID: sessionID,
					Provider:  provider,
				})
			}
			return terminalLaunch{}, fmt.Errorf("worktree setup failed for %s: %w", task.Slug, wtErr)
		}
		if wt.IsRepo {
			task.WorkDir = wt.WorktreePath
			task.WorktreePath = sql.NullString{String: wt.WorktreePath, Valid: true}
			if _, err := s.cfg.DB.Exec(
				`UPDATE tasks SET worktree_path = ?, updated_at = ? WHERE slug = ?`,
				wt.WorktreePath, flowdb.NowISO(), task.Slug,
			); err != nil {
				if created {
					s.rollbackPreparedTerminalLaunch(terminalLaunch{
						Slug:      task.Slug,
						SessionID: sessionID,
						Provider:  provider,
					})
				}
				return terminalLaunch{}, fmt.Errorf("persist worktree_path: %w", err)
			}
		}
	}

	if err := workdirreg.Touch(s.cfg.DB, originalWorkDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: bump workdir last_used_at: %v\n", err)
	}
	if _, err := agenthooks.InstallLocalWithOptions(task.WorkDir, agenthooks.InstallOptions{
		CommandPath: s.cfg.CommandPath,
		HookURL:     s.cfg.HookURL,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: install local agent hooks: %v\n", err)
	}

	if created {
		prompt := buildBrowserTerminalBootstrapPrompt(s.cfg.DB, task)
		if task.Slug == overviewTaskSlug {
			prompt = overviewInitialPrompt(s.cfg.FlowRoot, task)
		}
		args := agentTerminalArgs(provider, true, sessionID, task.WorkDir, s.cfg.FlowRoot, prompt, task.PermissionMode, s.resolveTaskLaunchModel(task, provider, true))
		return terminalLaunch{
			Slug:           task.Slug,
			SessionID:      sessionID,
			Provider:       provider,
			PermissionMode: task.PermissionMode,
			WorkDir:        task.WorkDir,
			Args:           args,
			Created:        created,
			NeedsCapture:   provider == agents.ProviderCodex,
			StartedAt:      time.Now().Add(-2 * time.Second),
		}, nil
	}
	args := agentTerminalArgs(provider, false, sessionID, task.WorkDir, s.cfg.FlowRoot, "", task.PermissionMode, s.resolveTaskLaunchModel(task, provider, false))
	return terminalLaunch{
		Slug:           task.Slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: task.PermissionMode,
		WorkDir:        task.WorkDir,
		Args:           args,
		Created:        created,
	}, nil
}

func (s *Server) prepareOverviewFloatingLaunch(req actionRequest) (terminalLaunch, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return terminalLaunch{}, errors.New("prompt is required")
	}
	flowRoot := strings.TrimSpace(s.cfg.FlowRoot)
	if flowRoot == "" {
		return terminalLaunch{}, errors.New("flow root is not configured")
	}
	absRoot, err := filepath.Abs(flowRoot)
	if err != nil {
		return terminalLaunch{}, err
	}
	provider, err := flowdb.NormalizeSessionProvider(req.Provider)
	if err != nil {
		return terminalLaunch{}, err
	}
	permissionMode, err := flowdb.NormalizePermissionMode(req.PermissionMode)
	if err != nil {
		return terminalLaunch{}, err
	}
	sessionID := uuid.NewString()
	args := agentTerminalArgs(provider, true, sessionID, absRoot, absRoot, overviewBrief(prompt), permissionMode, "")
	return terminalLaunch{
		Slug:           "overview-" + uuid.NewString(),
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: permissionMode,
		WorkDir:        absRoot,
		Args:           args,
		FreeAgent:      true,
		Created:        true,
		NeedsCapture:   false,
		StartedAt:      time.Now().Add(-2 * time.Second),
	}, nil
}

// prepareSendReplyFloatingLaunch builds an ephemeral, watchable floating session
// that posts an operator-approved reply via the Slack MCP. A headless
// `claude -p` has no claude.ai connector MCPs, so it CANNOT post to Slack — only
// a real interactive session can. This launch is therefore a normal bypass
// Claude PTY (Slack MCP available), primed to post the approved draft and then
// run `flow attention sent <feed-id> --close-floating <slug>` to mark the card
// sent and close its own window. It carries no task row (FreeAgent), so nothing
// lands in the Tasks list. On a failure the agent leaves the window open so the
// operator can see what went wrong, and the card stays 'new' for a retry.
func (s *Server) prepareSendReplyFloatingLaunch(item flowdb.FeedItem, text, instructions string) (terminalLaunch, error) {
	if strings.TrimSpace(text) == "" {
		return terminalLaunch{}, errors.New("send-reply requires non-empty text")
	}
	flowRoot := strings.TrimSpace(s.cfg.FlowRoot)
	if flowRoot == "" {
		return terminalLaunch{}, errors.New("flow root is not configured")
	}
	absRoot, err := filepath.Abs(flowRoot)
	if err != nil {
		return terminalLaunch{}, err
	}
	// The Slack MCP is a Claude (claude.ai) connector, so the sending session
	// must be Claude regardless of any provider hint on the item.
	const provider = agents.ProviderClaude
	// The operator approved the exact text via the feed — there is nothing left
	// to gate, so bypass is correct (same rationale as SendReplyViaAgent).
	const permissionMode = "bypass"
	sessionID := uuid.NewString()
	slug := "send-" + uuid.NewString()
	doneCmd := fmt.Sprintf("flow attention sent %s --close-floating %s", item.ID, slug)
	prompt := steering.SlackSendSessionPrompt(item, text, instructions, doneCmd)
	// Build the Claude args directly (rather than agentTerminalArgs) so we can pin
	// the model: posting an approved one-liner needs no frontier model, and the
	// session would otherwise inherit the user's default (e.g. Opus 4.8). Prompt
	// stays last (positional). Always Claude here — the Slack MCP is Claude-only.
	args := []string{"--session-id", sessionID, "--model", steering.SendReplyModel()}
	args = append(args, claudePermissionArgs(permissionMode)...)
	args = append(args, prompt)
	return terminalLaunch{
		Slug:           slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: permissionMode,
		WorkDir:        absRoot,
		Args:           args,
		FreeAgent:      true,
		Created:        true,
		NeedsCapture:   false,
		StartedAt:      time.Now().Add(-2 * time.Second),
	}, nil
}

func (s *terminalSession) captureCodexSession(started time.Time) {
	if started.IsZero() {
		started = time.Now().Add(-2 * time.Second)
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case <-s.done:
			return
		case <-deadline:
			return
		case <-ticker.C:
			captured, err := agents.CaptureCodexSessionForTaskSince(s.hub.server.cfg.DB, s.slug, s.workDir, started)
			if err != nil || captured == "" {
				continue
			}
			s.mu.Lock()
			s.sessionID = captured
			clients := make([]*terminalClient, 0, len(s.clients))
			for client := range s.clients {
				clients = append(clients, client)
			}
			s.mu.Unlock()
			for _, client := range clients {
				client.queue(terminalWSMessage{Type: "status", Message: "connected to codex session " + captured})
			}
			return
		}
	}
}

func agentTerminalArgs(provider string, fresh bool, sessionID, workDir, flowRootPath, prompt, permissionMode, model string) []string {
	if provider == agents.ProviderCodex {
		args := []string{"--no-alt-screen", "-C", workDir}
		args = appendCodexWritableRoot(args, workDir, flowRootPath)
		args = append(args, modelTerminalArgs(model)...)
		args = append(args, codexPermissionArgs(permissionMode)...)
		if fresh {
			return append(args, prompt)
		}
		resume := []string{"resume", "--include-non-interactive", "--no-alt-screen", "-C", workDir}
		resume = appendCodexWritableRoot(resume, workDir, flowRootPath)
		resume = append(resume, modelTerminalArgs(model)...)
		resume = append(resume, codexPermissionArgs(permissionMode)...)
		return append(resume, sessionID)
	}
	if fresh {
		args := []string{"--session-id", sessionID}
		args = append(args, modelTerminalArgs(model)...)
		args = append(args, claudePermissionArgs(permissionMode)...)
		return append(args, prompt)
	}
	args := []string{"--resume", sessionID}
	args = append(args, modelTerminalArgs(model)...)
	return append(args, claudePermissionArgs(permissionMode)...)
}

// modelTerminalArgs returns the `--model <m>` flag passed to claude/codex when
// the task pinned (or flow resolved) an explicit model, or nil to let the
// provider use its own default. Both CLIs take `--model`. This is what makes a
// UI-launched session honor tasks.model — the #30 model feature only threaded
// --model through `flow do`, never the server's terminal bridge, so web-UI
// sessions silently launched on the provider default (e.g. claude → Opus)
// regardless of the pinned model.
func modelTerminalArgs(model string) []string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return []string{"--model", strings.TrimSpace(model)}
}

// resolveTaskLaunchModel mirrors app.resolveLaunchModel for the server's
// terminal-bridge launches. On bootstrap (fresh) it runs flow's tier resolution
// — an explicit per-task pin wins, otherwise the baseline tier (default medium)
// is downshifted one rung when the brief is descriptive enough. On resume it
// passes only an explicit pin, never re-running the heuristic, so a live session
// never silently switches models mid-life. Empty result = pass no --model.
func (s *Server) resolveTaskLaunchModel(task *flowdb.Task, provider string, fresh bool) string {
	if task == nil {
		return ""
	}
	explicit := ""
	if task.Model.Valid {
		explicit = task.Model.String
	}
	if !fresh {
		return flowdb.NormalizeModel(explicit)
	}
	briefText := ""
	if root := strings.TrimSpace(s.cfg.FlowRoot); root != "" {
		if b, err := os.ReadFile(filepath.Join(root, "tasks", task.Slug, "brief.md")); err == nil {
			briefText = string(b)
		}
	}
	return flowdb.ResolveSessionModel(provider, explicit, briefText).Model
}

func appendCodexWritableRoot(args []string, workDir, flowRootPath string) []string {
	flowRootPath = strings.TrimSpace(flowRootPath)
	if flowRootPath == "" {
		return args
	}
	cleanWorkDir := strings.TrimSpace(workDir)
	if cleanWorkDir != "" {
		if abs, err := filepath.Abs(cleanWorkDir); err == nil {
			cleanWorkDir = abs
		}
	}
	if abs, err := filepath.Abs(flowRootPath); err == nil {
		flowRootPath = abs
	}
	if cleanWorkDir == flowRootPath {
		return args
	}
	return append(args, "--add-dir", flowRootPath)
}

func claudePermissionArgs(mode string) []string {
	switch strings.TrimSpace(mode) {
	case "auto":
		return []string{"--permission-mode", "auto"}
	case "bypass":
		return []string{"--dangerously-skip-permissions"}
	default:
		// `default` is the moderate baseline: auto-accept file edits but
		// still prompt for execution. `auto` and `bypass` cover the more
		// permissive options.
		return []string{"--permission-mode", "acceptEdits"}
	}
}

func codexPermissionArgs(mode string) []string {
	// Codex's workspace-write sandbox blocks outbound network by default, which
	// breaks tools flow tasks routinely need — `gh` (PR create/edit), `git
	// push`, package installs — with "error connecting to api.github.com". Flip
	// network on for the sandboxed modes. The sandbox is all-or-nothing here
	// (Codex has no per-domain allowlist), so this enables full egress; `bypass`
	// already runs unsandboxed.
	const allowNetwork = "sandbox_workspace_write.network_access=true"
	switch strings.TrimSpace(mode) {
	case "auto":
		return []string{"--ask-for-approval", "never", "--sandbox", "workspace-write", "-c", allowNetwork}
	case "bypass":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default:
		return []string{"--ask-for-approval", "on-request", "--sandbox", "workspace-write", "-c", allowNetwork}
	}
}

func overviewInitialPrompt(root string, task *flowdb.Task) string {
	body, err := os.ReadFile(filepath.Join(root, "tasks", task.Slug, "brief.md"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(body))
	const marker = "Latest user request:"
	if idx := strings.LastIndex(text, marker); idx >= 0 {
		return strings.TrimSpace(text[idx+len(marker):])
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "#") {
		text = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return text
}

func (s *Server) rollbackPreparedTerminalLaunch(launch terminalLaunch) {
	if launch.Slug == "" {
		return
	}
	if launch.Provider == agents.ProviderCodex && launch.SessionID == "" {
		if _, err := s.cfg.DB.Exec(
			`UPDATE tasks SET
				session_id = NULL,
				session_started = NULL,
				status = 'backlog',
				status_changed_at = NULL,
				updated_at = ?
			 WHERE slug = ? AND session_provider = 'codex' AND session_id IS NULL`,
			flowdb.NowISO(), launch.Slug,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback browser codex terminal session: %v\n", err)
		}
		return
	}
	if launch.SessionID == "" {
		return
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET
			session_id = NULL,
			session_started = NULL,
			status = 'backlog',
			status_changed_at = NULL,
			updated_at = ?
		 WHERE slug = ? AND session_id = ?`,
		flowdb.NowISO(), launch.Slug, launch.SessionID,
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: rollback browser terminal session: %v\n", err)
	}
}

func buildBrowserTerminalBootstrapPrompt(db *sql.DB, task *flowdb.Task) string {
	if task.Kind != "playbook_run" {
		prompt := fmt.Sprintf(
			"You are the execution session for flow task %s. Do ALL of the following in order before touching code:\n"+
				"1. Load the flow operating manual. If a Skill tool is available, invoke the flow skill via the Skill tool. Otherwise read ~/.codex/skills/flow/SKILL.md or ~/.claude/skills/flow/SKILL.md, whichever exists. This governs workflows, bootstrap contract, KB discipline, and scope-creep detection.\n"+
				"2. Run: flow show task. Read the file at the brief: path AND every file listed under updates:. Files listed under other: are sidecar references; load on demand when relevant, not eagerly.\n"+
				"3. If a project is listed on the task, run: flow show project <that-project-slug>. Read its brief AND every file under updates:. Files under other: are on-demand references.\n"+
				"4. Read AGENTS.md and/or CLAUDE.md in your work_dir and any nested convention files under subdirectories you will modify. These override any assumption from the brief.\n"+
				"5. Only then begin work. If any brief section is blank or unclear, ASK; do not infer.",
			task.Slug,
		)
		// Brief the session on upstream dependency work that may be unmerged.
		if note := flowdb.DependencyBootstrapNote(db, task.Slug); note != "" {
			prompt += "\n\n" + note
		}
		return prompt
	}
	playbookSlug := ""
	if task.PlaybookSlug.Valid {
		playbookSlug = task.PlaybookSlug.String
	}
	isFirstRun := false
	if playbookSlug != "" {
		var runCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM tasks WHERE playbook_slug = ? AND kind = 'playbook_run' AND archived_at IS NULL AND deleted_at IS NULL`,
			playbookSlug,
		).Scan(&runCount); err == nil {
			isFirstRun = runCount <= 1
		}
	}
	prompt := fmt.Sprintf(
		"You are running playbook %s as run %s. Do ALL of the following in order before executing anything:\n"+
			"1. Load the flow operating manual. If a Skill tool is available, invoke the flow skill via the Skill tool. Otherwise read ~/.codex/skills/flow/SKILL.md or ~/.claude/skills/flow/SKILL.md, whichever exists.\n"+
			"2. Run: flow show playbook %s. This shows the playbook definition and recent runs as context only, not your instructions.\n"+
			"3. Run: flow show task. Read the file at the brief: path AND every file listed under updates:. The brief is your authoritative snapshot for this run.\n"+
			"4. If a project is listed on the task, run: flow show project <that-project-slug>. Read its brief and every file under updates:.\n"+
			"5. Read AGENTS.md and/or CLAUDE.md in your work_dir.\n"+
			"6. Only then begin executing your brief.",
		playbookSlug, task.Slug, playbookSlug,
	)
	if isFirstRun {
		prompt += "\n\nThis is the first run of this playbook. Be proactive about asking whether scripts, decision rules, and edge cases discovered during the run should be captured back into the live playbook for future runs."
	}
	return prompt
}

func (s *terminalSession) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// captureReplay returns the authoritative pane history used to (re)seed a
// browser's scrollback: a fresh capture-pane spanning tmux's full history when
// the session is tmux-backed, else the sanitized accumulated byte stream. This
// is the only complete source of history — the live attach stream repaints
// repaint-style agents (Codex) in place and never accumulates their scrollback.
func (s *terminalSession) captureReplay() []byte {
	s.mu.Lock()
	scrollback := append([]byte(nil), s.scrollback...)
	sharedName := s.sharedName
	s.mu.Unlock()
	if sharedName != "" {
		if captured, err := sharedTerminalCaptureHistory(sharedName); err == nil {
			return captured
		}
	}
	if len(scrollback) > 0 {
		return stripTerminalAltScreenControls(scrollback)
	}
	return nil
}

func queueTerminalDataChunks(client *terminalClient, typ string, data []byte) {
	chunkSize := adaptiveTerminalChunkBytes(client, len(data))
	for len(data) > 0 {
		n := min(len(data), chunkSize)
		client.queue(terminalWSMessage{Type: typ, Data: string(data[:n])})
		data = data[n:]
	}
}

func adaptiveTerminalChunkBytes(client *terminalClient, dataLen int) int {
	chunkSize := terminalReplayChunkBytes()
	if client == nil || client.send == nil || dataLen <= chunkSize {
		return chunkSize
	}
	availableSlots := cap(client.send) - len(client.send)
	if availableSlots <= 0 {
		return chunkSize
	}
	if chunks := (dataLen + chunkSize - 1) / chunkSize; chunks > availableSlots {
		chunkSize = (dataLen + availableSlots - 1) / availableSlots
	}
	return max(1, chunkSize)
}

func (s *terminalSession) addClient(client *terminalClient, replay bool, cols, rows int) {
	// Seed the history replay BEFORE this client joins the broadcast set.
	//
	// captureReplay execs `tmux capture-pane` (slow, tens of ms). If the client
	// were already in s.clients during that window, a concurrent readPTY →
	// broadcast could queue LIVE output frames AHEAD of this history dump. The
	// client's FIFO send channel would then carry [live…, history], so the
	// browser paints newer text first and the big history dump lands underneath
	// it — exactly the "scrollback is reversed / blocks out of order" bug.
	//
	// So: run the slow capture first (outside the lock), then queue status +
	// replay and join the broadcast set together under s.mu. queue() is
	// non-blocking, so holding the lock across it can't deadlock with broadcast.
	// Every live frame this client receives is now ordered strictly after the
	// replayed history.
	//
	// For tmux-backed sessions captureReplay returns a FRESH rendered
	// capture-pane (clean final state of every history line, full scrollback),
	// not flow's raw byte stream — the raw stream is tmux redraws + status-bar
	// paints that strand stacked "[flow-…]" bars and reflow garble.
	var replayData []byte
	if replay {
		replayData = s.captureReplay()
	}
	cols, rows = normalizeTerminalClientSize(cols, rows)

	s.mu.Lock()
	provider := s.provider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	message := "connected to " + provider
	if s.sessionID != "" {
		message += " session " + s.sessionID
	} else {
		message += " session pending capture"
	}
	client.queue(terminalWSMessage{Type: "status", Message: message})
	if len(replayData) > 0 {
		queueTerminalDataChunks(client, "output", replayData)
	}
	client.cols = cols
	client.rows = rows
	s.clients[client] = struct{}{}
	resizeCols, resizeRows, resizeOwner := s.resizeTargetLocked()
	s.resizeOwner = resizeOwner
	shouldResize := resizeCols > 0 && resizeRows > 0 && (resizeCols != s.cols || resizeRows != s.rows)
	if s.closed {
		client.queue(terminalWSMessage{Type: "status", Message: s.exitStatus})
	}
	s.mu.Unlock()
	if shouldResize {
		_ = s.resize(resizeCols, resizeRows)
	}
}

func (s *terminalSession) clientCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *terminalSession) removeClient(client *terminalClient) {
	var resizeCols, resizeRows int
	var shouldResize bool
	s.mu.Lock()
	delete(s.clients, client)
	resizeCols, resizeRows, s.resizeOwner = s.resizeTargetLocked()
	if len(s.clients) > 0 && resizeCols > 0 && resizeRows > 0 {
		shouldResize = resizeCols != s.cols || resizeRows != s.rows
	}
	s.mu.Unlock()
	if shouldResize {
		_ = s.resize(resizeCols, resizeRows)
	}
	client.close()
}

func (s *terminalSession) detachBrowserAttach() {
	if s == nil {
		return
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	if s.tty != nil {
		_ = s.tty.Close()
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.exitStatus = "terminal detached"
		if s.done != nil {
			close(s.done)
		}
	}
	s.mu.Unlock()
	if s.hub != nil && s.hub.sharedRunningCache != nil {
		s.hub.sharedRunningCache.invalidate(s.slug)
	}
}

func (s *terminalSession) readPTY() {
	buf := make([]byte, 8192)
	pending := []byte{}
	for {
		n, err := s.tty.Read(buf)
		if n > 0 {
			data := append(pending, buf[:n]...)
			ready, rest := completeUTF8Prefix(data)
			pending = rest
			if len(ready) > 0 {
				ready = stripTerminalAltScreenControls(ready)
				if len(ready) > 0 {
					s.appendScrollback(ready)
					s.broadcast(terminalWSMessage{Type: "output", Data: string(ready)})
				}
			}
		}
		if err != nil {
			if len(pending) > 0 {
				ready := bytes.ToValidUTF8(pending, []byte("\uFFFD"))
				ready = stripTerminalAltScreenControls(ready)
				if len(ready) > 0 {
					s.appendScrollback(ready)
					s.broadcast(terminalWSMessage{Type: "output", Data: string(ready)})
				}
			}
			return
		}
	}
}

func completeUTF8Prefix(data []byte) ([]byte, []byte) {
	if utf8.Valid(data) {
		return data, nil
	}
	for tailLen := 1; tailLen <= 3 && tailLen <= len(data); tailLen++ {
		head := data[:len(data)-tailLen]
		tail := data[len(data)-tailLen:]
		if utf8.Valid(head) && !utf8.FullRune(tail) {
			return head, append([]byte(nil), tail...)
		}
	}
	return bytes.ToValidUTF8(data, []byte("\uFFFD")), nil
}

func (s *terminalSession) wait() {
	err := s.cmd.Wait()
	provider := s.provider
	if provider == "" {
		provider = agents.ProviderClaude
	}
	status := provider + " terminal exited"
	if err != nil {
		status = provider + " terminal exited: " + err.Error()
	}
	_ = s.tty.Close()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.exitStatus = status
		close(s.done)
	}
	clients := make([]*terminalClient, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	for _, client := range clients {
		client.queue(terminalWSMessage{Type: "status", Message: status})
	}
	s.hub.mu.Lock()
	if s.hub.sessions[s.slug] == s {
		delete(s.hub.sessions, s.slug)
	}
	s.hub.mu.Unlock()
	if s.hub.server != nil && s.hub.server.inboxMonitors != nil {
		s.hub.server.inboxMonitors.stop(s.slug)
	}
	s.hub.sharedRunningCache.invalidate(s.slug)
}

func (s *terminalSession) terminate() {
	if s.sharedName != "" {
		_ = sharedTerminalKillSession(s.sharedName)
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	if s.tty != nil {
		_ = s.tty.Close()
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.exitStatus = "terminal stopped"
		if s.done != nil {
			close(s.done)
		}
	}
	s.mu.Unlock()
	if s.hub != nil && s.hub.sharedRunningCache != nil {
		s.hub.sharedRunningCache.invalidate(s.slug)
	}
}

func (s *terminalSession) appendScrollback(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scrollback = append(s.scrollback, data...)
	s.lastOutputAt = time.Now()
	// Trim in bulk once we overshoot the cap by the headroom, dropping back to
	// the cap — amortizes the copy to ~once per headroom bytes (see consts).
	capBytes := terminalScrollbackBytes()
	if len(s.scrollback) > capBytes+terminalScrollbackHeadroomBytes() {
		s.scrollback = trimScrollbackToLineBoundary(s.scrollback, capBytes)
	}
}

// trimScrollbackToLineBoundary drops buf back to the last capBytes, then advances
// the cut to just past the next newline so a replay never begins mid-line or
// mid-escape-sequence. A raw byte-offset slice can otherwise land inside a CSI
// sequence (e.g. "\x1b[3" | "2m"), which corrupts the client terminal's parser
// for the rest of the replay — the leading bytes are consumed as bogus
// parameters and everything after shifts/overlaps.
func trimScrollbackToLineBoundary(buf []byte, capBytes int) []byte {
	if len(buf) <= capBytes {
		return buf
	}
	cut := len(buf) - capBytes
	if nl := bytes.IndexByte(buf[cut:], '\n'); nl >= 0 {
		cut += nl + 1
	}
	return append([]byte(nil), buf[cut:]...)
}

func (s *terminalSession) broadcast(msg terminalWSMessage) {
	s.mu.Lock()
	clients := make([]*terminalClient, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	for _, client := range clients {
		client.queue(msg)
	}
}

func (s *terminalSession) write(data string) error {
	if data == "" {
		return nil
	}
	_, err := s.tty.Write([]byte(data))
	return err
}

func normalizeTerminalClientSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 500 {
		rows = 500
	}
	return cols, rows
}

func normalizeTerminalResize(cols, rows int) (int, int, bool) {
	if cols <= 0 || rows <= 0 {
		return 0, 0, false
	}
	if cols > 500 {
		cols = 500
	}
	if rows > 500 {
		rows = 500
	}
	return cols, rows, true
}

func betterResizeOwner(candidate, current *terminalClient) bool {
	if candidate == nil {
		return false
	}
	if current == nil {
		return true
	}
	candidateArea := candidate.cols * candidate.rows
	currentArea := current.cols * current.rows
	if candidateArea != currentArea {
		return candidateArea > currentArea
	}
	if candidate.cols != current.cols {
		return candidate.cols > current.cols
	}
	return candidate.rows > current.rows
}

func (s *terminalSession) resizeTargetLocked() (int, int, *terminalClient) {
	cols, rows := 0, 0
	var owner *terminalClient
	for client := range s.clients {
		if client.cols > cols {
			cols = client.cols
		}
		if client.rows > rows {
			rows = client.rows
		}
		if betterResizeOwner(client, owner) {
			owner = client
		}
	}
	return cols, rows, owner
}

func (s *terminalSession) resize(cols, rows int) error {
	cols, rows, ok := normalizeTerminalResize(cols, rows)
	if !ok {
		return nil
	}
	s.mu.Lock()
	if cols == s.cols && rows == s.rows {
		s.mu.Unlock()
		return nil
	}
	s.cols = cols
	s.rows = rows
	tty := s.tty
	s.mu.Unlock()
	if tty == nil {
		return nil
	}
	return pty.Setsize(tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (s *terminalSession) clientOwnsResize(client *terminalClient) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resizeOwner == client
}

func (s *terminalSession) resizeFrom(client *terminalClient, cols, rows int) error {
	cols, rows, ok := normalizeTerminalResize(cols, rows)
	if !ok {
		return nil
	}
	s.mu.Lock()
	if _, ok := s.clients[client]; !ok {
		s.mu.Unlock()
		return nil
	}
	client.cols = cols
	client.rows = rows
	resizeCols, resizeRows, resizeOwner := s.resizeTargetLocked()
	s.resizeOwner = resizeOwner
	shouldResize := resizeCols > 0 && resizeRows > 0 && (resizeCols != s.cols || resizeRows != s.rows)
	s.mu.Unlock()
	if !shouldResize {
		return nil
	}
	return s.resize(resizeCols, resizeRows)
}

func (c *terminalClient) readLoop(sess *terminalSession) {
	defer c.conn.Close()
	c.conn.SetReadLimit(64 * 1024)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	for {
		var msg terminalWSMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case "input":
			if input := stripTerminalGeneratedInput(msg.Data); input != "" {
				_ = sess.write(input)
			}
		case "resize":
			_ = sess.resizeFrom(c, msg.Cols, msg.Rows)
		}
	}
}

func (c *terminalClient) writeLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *terminalClient) queue(msg terminalWSMessage) {
	select {
	case c.send <- msg:
	case <-c.done:
	default:
		c.close()
	}
}

func (c *terminalClient) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}
