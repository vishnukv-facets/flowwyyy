package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

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
const tmuxConfigBody = `# flow tmux defaults (auto-managed; safe to delete — flow will recreate
# it on the next session start).
#
# These exist so that scrolling JUST WORKS the first time you open a
# flow-spawned session without anyone having to hand-write a tmux.conf.
# Edit your personal ~/.tmux.conf for permanent customization — it is
# sourced at the bottom of this file and wins on conflicts.

# Mouse wheel scrolls tmux history. Enters copy-mode automatically;
# press 'q' or Escape to exit. Drag-select copies to OS clipboard on
# any modern terminal (OSC 52). This is the #1 ergonomic miss in
# default tmux.
set -g mouse on

# History buffer per pane. Default 2000 fills up in seconds with
# verbose tools (claude transcripts, kubectl logs, git log). 100k is
# cheap memory-wise (~few MB) and you'll rarely hit it.
set -g history-limit 100000

# Source the user's personal config last so their settings win on
# conflicts. -q makes this a no-op if the file doesn't exist.
if-shell '[ -f ~/.tmux.conf ]' 'source-file -q ~/.tmux.conf'
`

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
	if err := os.WriteFile(path, []byte(tmuxConfigBody), 0o644); err != nil {
		return "", fmt.Errorf("write tmux config: %w", err)
	}
	tmuxConfigWritten = true
	return path, nil
}
