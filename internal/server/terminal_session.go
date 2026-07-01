package server

import (
	"bytes"
	"encoding/json"
	"flow/internal/agents"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

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
				ready := bytes.ToValidUTF8(pending, []byte("�"))
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
	return bytes.ToValidUTF8(data, []byte("�")), nil
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

func (s *terminalSession) noteBrowserInput(data string) bool {
	if s == nil || data == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next, cleared := terminalDraftRunesAfter(s.browserDraftRunes, data)
	s.browserDraftRunes = next
	return cleared
}

func (s *terminalSession) hasBrowserDraft() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.browserDraftRunes > 0
}

func terminalDraftRunesAfter(current int, data string) (int, bool) {
	before := current
	for len(data) > 0 {
		switch data[0] {
		case '\r', '\x03': // Enter submits; Ctrl-C clears.
			current = 0
			data = data[1:]
		case '\b', '\x7f':
			if current > 0 {
				current--
			}
			data = data[1:]
		case '\x1b':
			data = data[skipTerminalEscape(data):]
		default:
			r, size := utf8.DecodeRuneInString(data)
			if r == utf8.RuneError && size == 1 {
				data = data[1:]
				continue
			}
			if r >= ' ' && r != '\x7f' {
				current++
			}
			data = data[size:]
		}
	}
	return current, before > 0 && current == 0
}

func skipTerminalEscape(data string) int {
	if len(data) < 2 {
		return len(data)
	}
	if data[1] == '[' {
		for i := 2; i < len(data); i++ {
			if data[i] >= 0x40 && data[i] <= 0x7e {
				return i + 1
			}
		}
		return len(data)
	}
	return 2
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
				clearedDraft := sess.noteBrowserInput(input)
				_ = sess.write(input)
				if clearedDraft && sess.hub != nil {
					sess.hub.scheduleWakeFlush(sess.slug, time.Now().Add(250*time.Millisecond))
				}
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
