package server

import (
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

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

// terminalUpgrader is shared by every /ws/* endpoint. CheckOrigin is the strict
// same-origin gate from audit P0-1: it rejects an empty Origin and requires an
// exact host match (no substring). The token half of the handshake auth is
// enforced per-handler via authorizeWSHandshake. See session_token.go.
var terminalUpgrader = websocket.Upgrader{
	CheckOrigin: checkLocalWSOrigin,
}

// wsUpgrader returns the WebSocket upgrader for a request: the strict local
// origin gate by default, or the remote-aware gate when remoteAuth marked the
// request remote (X-Flow-Remote). Both share every other upgrader setting.
// Returns a pointer because websocket.Upgrader.Upgrade has a pointer receiver.
func (s *Server) wsUpgrader(r *http.Request) *websocket.Upgrader {
	if r.Header.Get(remoteFlagHeader) == "1" {
		return &websocket.Upgrader{CheckOrigin: s.checkRemoteWSOrigin}
	}
	return &terminalUpgrader
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
	// wakes buffers wake prompts that arrived while a session was blocked on
	// the operator's input, so they re-deliver once it's free instead of
	// auto-submitting the open prompt. See terminal_wake.go.
	wakes *wakeQueue
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

	mu                sync.Mutex
	clients           map[*terminalClient]struct{}
	scrollback        []byte
	closed            bool
	exitStatus        string
	lastOutputAt      time.Time
	resizeOwner       *terminalClient
	cols              int
	rows              int
	browserDraftRunes int
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
		wakes:              newWakeQueue(s.cfg.DB),
	}
}

func (s *Server) handleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeWSHandshake(w, r) {
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if err := validateSlug(slug); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cols := intQueryDefault(r, "cols", 120)
	rows := intQueryDefault(r, "rows", 32)
	conn, err := s.wsUpgrader(r).Upgrade(w, r, nil)
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
	if !s.authorizeWSHandshake(w, r) {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if err := validateSlug(id); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cols := intQueryDefault(r, "cols", 120)
	rows := intQueryDefault(r, "rows", 32)
	conn, err := s.wsUpgrader(r).Upgrade(w, r, nil)
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
