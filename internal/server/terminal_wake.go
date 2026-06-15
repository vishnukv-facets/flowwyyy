package server

import (
	"flow/internal/agents"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

func terminalPasteInput(prompt string) string {
	return "\x1b[200~" + prompt + "\x1b[201~\r"
}

func (h *terminalHub) wakeTask(slug, prompt string) error {
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

// sessionQuietFor reports whether a session whose last PTY output was at `last`
// is ready to accept a submit keystroke as of `now`: it must have produced some
// output (non-zero `last`, i.e. the paste rendered or a prior turn ran) AND have
// since been quiet for at least `window`. A streaming agent keeps `last` fresh,
// so this stays false until it returns to the prompt — which is exactly when an
// Enter actually submits rather than being swallowed mid-turn.
func sessionQuietFor(last, now time.Time, window time.Duration) bool {
	if last.IsZero() {
		return false
	}
	return now.Sub(last) >= window
}

// submitAfterPaste presses Enter to submit a wake paste, but only once the
// session is READY — i.e. the paste has rendered and the agent is idle at the
// prompt, not mid-turn. A blind fixed-delay Enter (the old behavior) was
// swallowed when the wake landed while the agent was still streaming a previous
// answer, or before the editor left paste mode under load — stranding the text
// unsent in the input box. This: (1) waits for the paste to register and the
// session to go quiet, (2) sends Enter, (3) confirms the submit took (a real
// submit makes the agent react, so fresh output appears) and retries if it
// didn't. Bounded by maxWait so the goroutine can't outlive a stuck session,
// but long enough to outlast normal agent turns.
func (h *terminalHub) submitAfterPaste(slug string, baseline time.Time) {
	const (
		readyQuiet  = 450 * time.Millisecond // no output for this long ⇒ paste settled & agent idle
		pollEvery   = 90 * time.Millisecond
		renderGrace = 1500 * time.Millisecond // proceed even if no paste echo was observed
		confirmFor  = 1200 * time.Millisecond // fresh output within this after Enter ⇒ it submitted
		maxWait     = 5 * time.Minute         // outlasts normal turns; prevents a leaked goroutine
		maxEnters   = 3
	)
	deadline := time.Now().Add(maxWait)
	start := time.Now()
	for enters := 0; enters < maxEnters && time.Now().Before(deadline); enters++ {
		// Phase 1 — wait until the session is ready: the paste has rendered
		// (output advanced past baseline, or the render grace elapsed) AND output
		// has since been quiet for readyQuiet (the agent isn't mid-turn).
		for {
			if time.Now().After(deadline) {
				return
			}
			if !h.running(slug) {
				return // session gone — nothing to submit into
			}
			last, ok := h.lastOutputAt(slug)
			rendered := (ok && last.After(baseline)) || time.Since(start) >= renderGrace
			if rendered && sessionQuietFor(last, time.Now(), readyQuiet) {
				break
			}
			time.Sleep(pollEvery)
		}
		// Phase 2 — submit.
		before, _ := h.lastOutputAt(slug)
		if err := h.sendInput(slug, "\r"); err != nil {
			return
		}
		// Phase 3 — confirm. A successful submit makes the agent react, so fresh
		// output appears. If none does, the Enter was absorbed: loop and retry
		// (the session is already quiet, so the next attempt fires promptly). An
		// extra Enter on an already-submitted/empty prompt is a harmless no-op.
		confirmDeadline := time.Now().Add(confirmFor)
		for time.Now().Before(confirmDeadline) {
			if last, ok := h.lastOutputAt(slug); ok && last.After(before) {
				return // submitted
			}
			time.Sleep(pollEvery)
		}
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
// Enter to submit a wake paste into a detached tmux session, but only once the
// pane has gone STABLE (the agent is idle at the prompt, not mid-turn). It then
// confirms the submit took — a real submit clears the input box, so the pane
// changes — and retries if it didn't. Bounded by maxWait. This is the
// post-`flow ui serve`-restart path where the agent is alive in tmux but absent
// from the in-memory hub, so there is no lastOutputAt to gate on; pane snapshots
// stand in for it.
func (h *terminalHub) submitAfterSharedPaste(name string) {
	const (
		readyQuiet = 450 * time.Millisecond // pane unchanged this long ⇒ agent idle/ready
		pollEvery  = 150 * time.Millisecond // tmux exec is heavier than a mem read — poll gentler
		confirmFor = 1500 * time.Millisecond
		maxWait    = 5 * time.Minute
		maxEnters  = 3
	)
	deadline := time.Now().Add(maxWait)
	for enters := 0; enters < maxEnters && time.Now().Before(deadline); enters++ {
		// Phase 1 — wait until the pane is stable for readyQuiet.
		prev, ok := sharedTerminalCapturePane(name)
		if !ok {
			return // session gone
		}
		stableSince := time.Now()
		for {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(pollEvery)
			cur, ok := sharedTerminalCapturePane(name)
			if !ok {
				return
			}
			if cur != prev {
				prev = cur
				stableSince = time.Now()
				continue
			}
			if time.Since(stableSince) >= readyQuiet {
				break
			}
		}
		// Phase 2 — submit.
		beforeEnter := prev
		if _, err := sharedTerminalCommand("send-keys", "-t", name, "Enter"); err != nil {
			return
		}
		// Phase 3 — confirm: a real submit clears the input box, so the pane
		// changes. If it doesn't, the Enter was absorbed; loop to retry. An extra
		// Enter on an already-submitted/empty prompt is a harmless no-op.
		confirmDeadline := time.Now().Add(confirmFor)
		for time.Now().Before(confirmDeadline) {
			time.Sleep(pollEvery)
			cur, ok := sharedTerminalCapturePane(name)
			if !ok {
				return
			}
			if cur != beforeEnter {
				return // submitted
			}
		}
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
