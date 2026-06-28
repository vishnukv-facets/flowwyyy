package server

import (
	"flow/internal/agents"
	"flow/internal/flowdb"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

// sessionBooted reports whether a freshly (re)started session looks ready to
// accept input: it has produced some boot output AND that output has been quiet
// for at least `stable`. A resuming `claude --resume` renders its prior
// conversation then settles at the prompt; quiescence is our proxy for "the TUI
// is now reading input". Pure so it's unit-testable without a live PTY.
func sessionBooted(sawOutput bool, lastOutput, now time.Time, stable time.Duration) bool {
	return sawOutput && !lastOutput.IsZero() && now.Sub(lastOutput) >= stable
}

// waitForSessionReady blocks until a just-started session has booted and gone
// quiet at its prompt, so a wake paste lands in a ready input box instead of
// racing the agent's boot. That race silently dropped steerer/Slack deliveries:
// resume started the PTY and wakeTask pasted immediately, before `claude --resume`
// was reading input, so the message vanished while the path still returned nil
// ("delivered"). Best-effort: returns on quiescence, on the session going away,
// or when `timeout` elapses (so a chatty session can't strand the wake forever).
func (h *terminalHub) waitForSessionReady(slug string, stable, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	sawOutput := false
	for time.Now().Before(deadline) {
		if !h.running(slug) {
			return // gone — nothing to wait for; caller's wake will no-op/err
		}
		last, ok := h.lastOutputAt(slug)
		if ok {
			sawOutput = true
		}
		if sessionBooted(sawOutput, last, time.Now(), stable) {
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func waitForSharedSessionReady(name string, stable, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	pollEvery := 120 * time.Millisecond
	if stable > 0 && stable/2 < pollEvery {
		pollEvery = stable / 2
		if pollEvery < 10*time.Millisecond {
			pollEvery = 10 * time.Millisecond
		}
	}
	var last string
	var lastChange time.Time
	sawOutput := false
	for time.Now().Before(deadline) {
		cur, ok := sharedTerminalCapturePane(name)
		if !ok {
			return
		}
		now := time.Now()
		if !sawOutput || cur != last {
			sawOutput = true
			last = cur
			lastChange = now
		}
		if sessionBooted(sawOutput, lastChange, now, stable) {
			return
		}
		time.Sleep(pollEvery)
	}
}

// wakeTask delivers a wake prompt into a live browser-PTY session — UNLESS the
// session is currently blocked on the operator's input (an open AskUserQuestion
// selector or a permission prompt), in which case injecting would auto-submit
// that prompt (the bug that once fired an unreviewed Slack reply). When blocked,
// the prompt is buffered (persisted) and re-delivered once the session leaves
// the human-input wait — see flushWakes. Returns nil on a successful inject OR a
// successful buffer.
func (h *terminalHub) wakeTask(slug, prompt string) error {
	if h.awaitingHumanInput(slug) {
		if err := h.wakes.push(slug, prompt); err != nil {
			return fmt.Errorf("buffer wake for %s: %w", slug, err)
		}
		fmt.Fprintf(os.Stderr, "[term-wake %s] session awaiting operator input; buffered wake (delivers when free)\n", slug)
		return nil
	}
	return h.injectWake(slug, prompt)
}

// injectWake performs the actual paste+Enter into the live PTY. The caller must
// have already decided the session is safe to inject into (wakeTask gates on
// awaitingHumanInput; flushWakes re-checks before each buffered delivery).
func (h *terminalHub) injectWake(slug, prompt string) error {
	// Paste the prompt WITHOUT a trailing newline, then submit with a separate
	// Enter. A \r in the same write as the bracketed-paste terminator gets
	// absorbed into the (usually multi-line) input buffer instead of submitting —
	// the prompt ends up sitting unsent in the box. The Enter is sent by
	// submitAfterPaste only once the session is actually ready for it (see
	// there); `baseline` is the pre-paste output marker so it can tell the paste
	// has rendered before acting.
	paste := "\x1b[200~" + prompt + "\x1b[201~"
	baseline, _ := h.lastOutputAt(slug)
	if err := h.sendInput(slug, paste); err != nil {
		fmt.Fprintf(os.Stderr, "[term-wake %s] paste send failed (%v); re-attaching\n", slug, err)
		if _, aerr := h.attach(slug, 120, 32); aerr != nil {
			return aerr
		}
		baseline, _ = h.lastOutputAt(slug)
		if err := h.sendInput(slug, paste); err != nil {
			return err
		}
	}
	go h.submitAfterPaste(slug, baseline)
	return nil
}

// submitAfterPaste presses Enter to submit a wake paste. It does NOT wait for the
// agent to be idle: Claude Code and Codex both QUEUE a message submitted mid-turn,
// so waiting for "quiet" only strands the wake for the length of a long turn — the
// streaming spinner keeps output fresh, so quiet never arrives and the prompt sits
// unsent (the bug this replaces). We only wait for the bracketed paste to land
// (output advances past the pre-paste marker, or a short grace elapses), THEN press
// Enter regardless of whether the agent is mid-turn: busy ⇒ the host queues it, idle
// ⇒ it runs now. The Enter must be a SEPARATE write from the paste (a \r in the same
// write as the paste terminator is absorbed into the buffer). A second Enter after a
// short grace is cheap insurance against a first Enter that landed before the editor
// left paste mode — a no-op on an already-submitted/empty input.
func (h *terminalHub) submitAfterPaste(slug string, baseline time.Time) {
	const (
		pollEvery   = 90 * time.Millisecond
		renderGrace = 1200 * time.Millisecond // press Enter by here even if no paste echo is seen
		secondEnter = 700 * time.Millisecond
	)
	deadline := time.Now().Add(renderGrace)
	for {
		if !h.running(slug) {
			fmt.Fprintf(os.Stderr, "[term-wake %s] session gone before submit\n", slug)
			return
		}
		if last, ok := h.lastOutputAt(slug); (ok && last.After(baseline)) || time.Now().After(deadline) {
			break // paste landed (or grace elapsed) — submit now, mid-turn or not
		}
		time.Sleep(pollEvery)
	}
	if err := h.sendInput(slug, "\r"); err != nil {
		fmt.Fprintf(os.Stderr, "[term-wake %s] submit enter send failed: %v\n", slug, err)
		return
	}
	time.Sleep(secondEnter)
	if h.running(slug) {
		_ = h.sendInput(slug, "\r") // insurance; no-op if the first Enter already submitted
	}
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
// the editor as pasted text, not submitted line-by-line), then a separate Enter
// sent by submitAfterSharedPaste only once the session is ready — never a blind
// fixed-delay Enter, which gets swallowed if the agent is still streaming a
// previous answer or the editor hasn't left paste mode.
func (h *terminalHub) wakeSharedTask(slug, prompt string) bool {
	if h.awaitingHumanInput(slug) {
		if err := h.wakes.push(slug, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "[term-wake %s] buffer shared wake failed: %v\n", slug, err)
			return false
		}
		fmt.Fprintf(os.Stderr, "[term-wake %s] shared session awaiting operator input; buffered wake\n", slug)
		return true // owned: buffered, will deliver on flush — caller must not fall through
	}
	return h.injectSharedWake(slug, prompt)
}

// injectSharedWake performs the actual tmux paste+Enter into a detached session.
// The caller must have already decided it is safe to inject (wakeSharedTask
// gates on awaitingHumanInput; flushWakes re-checks).
func (h *terminalHub) injectSharedWake(slug, prompt string) bool {
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
	go h.submitAfterSharedPaste(name)
	return true
}

// sharedTerminalCapturePane returns the current VISIBLE pane content (plain
// text, escape codes stripped) for a shared tmux session. Comparing successive
// captures detects when the agent has stopped repainting — a streaming answer
// constantly changes the pane; an idle agent at the prompt is stable. Plain
// text (no -e) so color-only repaints don't read as activity. ok=false on any
// tmux error (e.g. the session is gone).
func sharedTerminalCapturePane(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	out, err := sharedTerminalCommand("capture-pane", "-p", "-t", name)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// submitAfterSharedPaste is the tmux-path twin of submitAfterPaste: it presses
// Enter to submit a wake paste into a detached tmux session. Like its twin it does
// NOT wait for the agent to be idle (the host queues a mid-turn message); it only
// waits for the paste to land in the pane (pane content changes, or a short grace),
// then sends Enter, then a second Enter as insurance against a first one absorbed
// before paste mode exited. This is the post-`flow ui serve`-restart path where the
// agent is alive in tmux but absent from the in-memory hub.
func (h *terminalHub) submitAfterSharedPaste(name string) {
	const (
		pollEvery   = 150 * time.Millisecond // tmux exec is heavier than a mem read — poll gentler
		renderGrace = 1200 * time.Millisecond
		secondEnter = 700 * time.Millisecond
	)
	prev, ok := sharedTerminalCapturePane(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "[term-wake %s] shared session gone before submit\n", name)
		return
	}
	deadline := time.Now().Add(renderGrace)
	for {
		time.Sleep(pollEvery)
		cur, ok := sharedTerminalCapturePane(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "[term-wake %s] shared session gone before submit\n", name)
			return
		}
		if cur != prev || time.Now().After(deadline) {
			break // paste landed (or grace elapsed) — submit now, mid-turn or not
		}
	}
	if _, err := sharedTerminalCommand("send-keys", "-t", name, "Enter"); err != nil {
		fmt.Fprintf(os.Stderr, "[term-wake %s] shared submit enter failed: %v\n", name, err)
		return
	}
	time.Sleep(secondEnter)
	_, _ = sharedTerminalCommand("send-keys", "-t", name, "Enter") // insurance; no-op if already submitted
}

// wakeFlushGap spaces out buffered wakes during a flush so each paste+Enter
// settles before the next (submitAfterPaste's own grace + second-Enter is ~2s).
const wakeFlushGap = 2500 * time.Millisecond

func pendingWakeReady(pw flowdb.PendingWake, now time.Time) (bool, time.Time) {
	if strings.TrimSpace(pw.NotBefore) == "" {
		return true, time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(pw.NotBefore))
	if err != nil {
		return true, time.Time{}
	}
	return !t.After(now), t
}

func (h *terminalHub) scheduleWakeFlush(slug string, at time.Time) {
	if at.IsZero() {
		go h.flushWakes(slug)
		return
	}
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay+250*time.Millisecond, func() {
		h.flushWakes(slug)
	})
}

// awaitingHumanInput reports whether slug's agent session is currently blocked
// on the operator's input — a question it asked (elicitation) or a permission
// prompt — per the recorded agent runtime state. Fail-open: an unknown/missing
// state returns false so un-instrumented sessions wake exactly as before. The
// gate engages only when there is positive evidence of a pending operator
// prompt, which is precisely the case that must not be auto-submitted.
func (h *terminalHub) awaitingHumanInput(slug string) bool {
	if h.server == nil || h.server.cfg.DB == nil {
		return false
	}
	provider, sid := h.sessionIdentity(slug)
	if provider == "" || sid == "" {
		return false
	}
	st, err := flowdb.AgentRuntimeStateBySessionID(h.server.cfg.DB, provider, sid)
	if err != nil {
		return false
	}
	return st.AwaitingHumanInput()
}

// sessionIdentity resolves (provider, session_id) for a slug: from the live hub
// session when attached, else from the task row (covers post-restart tmux-only
// sessions absent from the in-memory hub, and the window before Codex captures
// its thread id).
func (h *terminalHub) sessionIdentity(slug string) (string, string) {
	h.mu.Lock()
	var provider, sid string
	if sess := h.sessions[slug]; sess != nil {
		provider = sess.provider
		sid = strings.TrimSpace(sess.sessionID)
	}
	h.mu.Unlock()
	if sid != "" {
		return provider, sid
	}
	if h.server != nil && h.server.cfg.DB != nil {
		if task, err := flowdb.GetTask(h.server.cfg.DB, slug); err == nil && task != nil && task.SessionID.Valid {
			return task.SessionProvider, strings.TrimSpace(task.SessionID.String)
		}
	}
	return "", ""
}

// flushWakes delivers any buffered wakes for slug once it is no longer blocked
// on the operator's input. Serialized per slug (beginFlush) so buffered pastes
// never interleave on the PTY. Before each delivery it re-checks
// awaitingHumanInput and stops (leaving the rest buffered) if the session
// re-entered a human-input wait — e.g. a delivered wake made the agent ask
// another question. peek is non-destructive; a row is acked only after a
// confirmed inject, and an undeliverable wake (no live session) is left queued
// for the next attach rather than dropped.
func (h *terminalHub) flushWakes(slug string) {
	if !h.wakes.has(slug) {
		return
	}
	if !h.wakes.beginFlush(slug) {
		return
	}
	go func() {
		defer h.wakes.endFlush(slug)
		for {
			if h.awaitingHumanInput(slug) {
				return
			}
			pw, ok := h.wakes.peek(slug)
			if !ok {
				return
			}
			if ready, at := pendingWakeReady(pw, time.Now()); !ready {
				h.scheduleWakeFlush(slug, at)
				return
			}
			if h.awaitingHumanInput(slug) {
				return // re-blocked between peek and inject; leave it queued
			}
			if !h.injectWakeRouted(slug, pw.Prompt) {
				return // no live session right now; keep buffered for next attach
			}
			h.wakes.ack(pw.ID)
			time.Sleep(wakeFlushGap)
		}
	}()
}

// injectWakeRouted delivers a buffered wake by the same routing a fresh wake
// uses: the live PTY when attached, else the detached tmux session. Returns
// false when neither is live (the wake stays buffered for the next attach).
func (h *terminalHub) injectWakeRouted(slug, prompt string) bool {
	if h.running(slug) {
		if err := h.injectWake(slug, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "[term-wake %s] flush inject failed: %v\n", slug, err)
			return false
		}
		return true
	}
	return h.injectSharedWake(slug, prompt)
}

// resumeBufferedWakes re-attempts delivery of every slug that still has buffered
// wakes after a restart. Best-effort: flushWakes withholds anything still
// blocked on the operator (the runtime state is persisted too) and routes the
// rest to whatever session is live (tmux after a `flow ui serve` restart).
func (h *terminalHub) resumeBufferedWakes() {
	if h.server == nil || h.server.cfg.DB == nil {
		return
	}
	slugs, err := flowdb.PendingWakeSlugs(h.server.cfg.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[term-wake] resume buffered wakes: %v\n", err)
		return
	}
	for _, slug := range slugs {
		h.flushWakes(slug)
	}
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
