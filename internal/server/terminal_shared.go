package server

import (
	"bytes"
	"errors"
	"flow/internal/agents"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

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
