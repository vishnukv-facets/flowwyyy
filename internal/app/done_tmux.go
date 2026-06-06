package app

import (
	"fmt"
	"os/exec"
	"strings"
)

var taskTmuxCommandRunner = func(args ...string) ([]byte, error) {
	return exec.Command("tmux", args...).CombinedOutput()
}

var taskTmuxSessionCloser = closeTaskTmuxSession

// closeTaskTmuxSession schedules the kill through tmux itself so a
// `flow done` running inside the target session can return before the pane dies.
func closeTaskTmuxSession(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if _, err := taskTmuxCommandRunner("has-session", "-t", name); err != nil {
		return nil
	}
	script := "sleep 0.3; tmux kill-session -t " + shellQuoteTmuxArg(name) + " >/dev/null 2>&1 || true"
	if out, err := taskTmuxCommandRunner("run-shell", "-b", script); err != nil {
		return fmt.Errorf("schedule tmux close for %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func taskTmuxSessionName(slug string) string {
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

func shellQuoteTmuxArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
