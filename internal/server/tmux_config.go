package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultSharedTerminalHistoryLimit = 200000

func sharedTerminalHistoryLimit() string {
	return fmt.Sprintf("%d", positiveIntEnv("FLOW_TMUX_HISTORY_LIMIT", defaultSharedTerminalHistoryLimit, 1000, 2147483647))
}

// tmuxConfigBody is the tiny tmux config flow ships so that browser /
// shared-terminal sessions have sensible defaults — most importantly
// mouse scroll, which 99% of new tmux users get tripped up by. We avoid
// stamping any opinion beyond "scroll should work" and let the user's
// personal ~/.tmux.conf (sourced at the bottom) override anything.
//
// Why this lives here rather than in CLI app:
//   - The terminal bridge is the only path that spawns tmux sessions.
//     `flow init` doesn't need to write it preemptively; lazy-write on
//     first ensureSharedTerminalSession call is sufficient and means
//     existing installs pick it up automatically on next session.
//   - Tests stub out sharedTerminalCommand and don't exercise the
//     filesystem write path; keeping this self-contained avoids
//     touching app init code.
func tmuxConfigBody() string {
	return `# flow tmux defaults (auto-managed; safe to delete — flow will recreate
# it on the next session start).
#
# These exist so that scrolling JUST WORKS the first time you open a
# flow-spawned session without anyone having to hand-write a tmux.conf.
# Edit your personal ~/.tmux.conf for permanent customization — it is
# sourced at the bottom of this file and wins on conflicts.

# Mouse OFF. The browser terminal (xterm.js) is the real UI: it owns scrolling
# (its own scrollback, seeded from tmux history on attach) and text selection
# (native selection → clipboard). With mouse ON, tmux grabs the wheel — and
# because flow runs the agent with mouse tracking disabled, a single scroll
# drops the pane into copy-mode: the view freezes, a "[pos/total]" indicator
# appears, and the render duplicates/garbles. tmux is invisible plumbing here,
# so it must never touch the mouse.
set -g mouse off

# Size each window to the latest (browser) client, not the smallest of all
# clients, so the grid tracks the browser terminal on resize.
set -g window-size latest

# Let an inner app's own OSC 52 reach the outer terminal. Harmless with mouse
# off; the browser terminal's native selection handles drag-to-copy.
set -g set-clipboard on

# Hide tmux's status bar. flow's web UI already shows the session name,
# status, and branch in its own chrome, so the bar is redundant — and the
# browser terminal renders tmux's status-row repaints imperfectly, leaving
# the green status bar stranded in the scrollback.
set -g status off

# History buffer per pane. tmux does not expose a true "unlimited"
# history switch; this uses the largest value accepted by tmux so
# flow-spawned terminals can scroll back to the beginning of practical
# interactive sessions.
set -g history-limit ` + sharedTerminalHistoryLimit() + `

# Source the user's personal config last so their settings win on
# conflicts. -q makes this a no-op if the file doesn't exist.
if-shell '[ -f ~/.tmux.conf ]' 'source-file -q ~/.tmux.conf'
`
}

var (
	tmuxConfigWriteOnce sync.Mutex
	// tmuxConfigWritten guards against re-writing the file on every
	// session spawn within a single server process. Process restart
	// happily re-checks and re-writes if needed; it's cheap.
	tmuxConfigWritten = false
)

// ensureTmuxConfig writes flow's tmux defaults to <flowRoot>/tmux.conf if
// it's missing. Returns the absolute path so callers can pass it to
// tmux via `-f`. Idempotent: writes once per server process, then short
// -circuits. Errors are returned but callers may safely ignore them —
// tmux without `-f` still works, just without our defaults.
func ensureTmuxConfig(flowRoot string) (string, error) {
	if flowRoot == "" {
		return "", fmt.Errorf("flow root not set")
	}
	path := filepath.Join(flowRoot, "tmux.conf")
	tmuxConfigWriteOnce.Lock()
	defer tmuxConfigWriteOnce.Unlock()
	if tmuxConfigWritten {
		return path, nil
	}
	// If the file already exists (any size, any content) we leave it
	// alone. Users who customize the flow defaults shouldn't have those
	// stomped on every restart. To revert: delete the file and let flow
	// recreate it.
	if _, err := os.Stat(path); err == nil {
		tmuxConfigWritten = true
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat tmux config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir for tmux config: %w", err)
	}
	if err := os.WriteFile(path, []byte(tmuxConfigBody()), 0o644); err != nil {
		return "", fmt.Errorf("write tmux config: %w", err)
	}
	tmuxConfigWritten = true
	return path, nil
}

func ensureSharedTerminalScrollOptions(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tmux session name not set")
	}
	// Mouse OFF (session-scoped, so it wins over any global). The browser
	// terminal owns scrolling and selection; tmux owning the wheel drops the
	// pane into copy-mode on a single scroll and garbles the render. This also
	// flips existing sessions on reattach, not just freshly-created ones.
	if out, err := sharedTerminalCommand("set-option", "-t", name, "mouse", "off"); err != nil {
		return fmt.Errorf("disable tmux mouse for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	// Track the latest (browser) client's size so the pane matches the xterm grid.
	if out, err := sharedTerminalCommand("set-option", "-t", name, "window-size", "latest"); err != nil {
		return fmt.Errorf("set tmux window-size for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	// Hide the status bar on already-running sessions too (new sessions get it
	// via the creation args). flow's UI shows session info in its own chrome,
	// and the bar otherwise leaks into the browser terminal's scrollback.
	if out, err := sharedTerminalCommand("set-option", "-t", name, "status", "off"); err != nil {
		return fmt.Errorf("disable tmux status bar for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	if out, err := sharedTerminalCommand("set-option", "-t", name, "set-clipboard", "on"); err != nil {
		return fmt.Errorf("enable tmux OSC 52 clipboard for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	if out, err := sharedTerminalCommand("set-window-option", "-t", name+":", "history-limit", sharedTerminalHistoryLimit()); err != nil {
		return fmt.Errorf("set tmux history limit for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	// Clear any copy-mode a previous (mouse-on) attach left the pane stuck in, so
	// the live view isn't frozen. Best-effort: it errors with "not in a mode"
	// when the pane is already live, which is fine to ignore.
	_, _ = sharedTerminalCommand("send-keys", "-t", name, "-X", "cancel")
	return nil
}

func ensureSharedTerminalDefaultScrollOptions() error {
	// Mouse off (see ensureSharedTerminalScrollOptions): the browser terminal
	// owns scrolling/selection; tmux must not grab the wheel into copy-mode.
	if out, err := sharedTerminalCommand("set-option", "-g", "mouse", "off"); err != nil {
		return fmt.Errorf("disable tmux mouse globally: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := sharedTerminalCommand("set-option", "-g", "window-size", "latest"); err != nil {
		return fmt.Errorf("set tmux window-size globally: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := sharedTerminalCommand("set-option", "-g", "set-clipboard", "on"); err != nil {
		return fmt.Errorf("enable tmux OSC 52 clipboard globally: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := sharedTerminalCommand("set-window-option", "-g", "history-limit", sharedTerminalHistoryLimit()); err != nil {
		return fmt.Errorf("set tmux global history limit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
