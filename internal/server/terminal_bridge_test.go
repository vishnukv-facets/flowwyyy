package server

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCompleteUTF8PrefixCarriesSplitRune(t *testing.T) {
	input := []byte("hello ")
	input = append(input, []byte("★")[:2]...)

	ready, pending := completeUTF8Prefix(input)
	if string(ready) != "hello " {
		t.Fatalf("ready = %q", string(ready))
	}
	if len(pending) != 2 {
		t.Fatalf("pending len = %d", len(pending))
	}

	ready, pending = completeUTF8Prefix(append(pending, []byte("★")[2:]...))
	if string(ready) != "★" || len(pending) != 0 {
		t.Fatalf("ready=%q pending=%q", string(ready), string(pending))
	}
}

func TestCompleteUTF8PrefixReplacesInvalidBytes(t *testing.T) {
	ready, pending := completeUTF8Prefix([]byte{'o', 'k', ' ', 0xff})
	if string(ready) != "ok \uFFFD" {
		t.Fatalf("ready = %q", string(ready))
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %q", string(pending))
	}
}

// TokensUsed is the last turn's full total (context occupancy, incl. cache).
// TokensSession matches Claude Code /stats: fresh input + output + cache
// CREATION, EXCLUDING cache re-reads.
func TestAccumulateTranscriptUsageSumsClaudeSession(t *testing.T) {
	var stats transcriptUsageStats
	// Turn 1 writes 5000 tokens to cache (cache_creation) — those ARE counted
	// (genuinely new tokens; /stats counts them). The 1000+1100 cache READS are
	// not (re-reads of already-cached context).
	accumulateTranscriptUsage(&stats, []byte(`{"type":"assistant","message":{"model":"claude","usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":5000,"output_tokens":20}}}`))
	accumulateTranscriptUsage(&stats, []byte(`{"type":"assistant","message":{"model":"claude","usage":{"input_tokens":5,"cache_read_input_tokens":1100,"output_tokens":30}}}`))
	if stats.TokensUsed != 1135 { // context = last turn total: 5+1100+30
		t.Fatalf("TokensUsed = %d, want 1135 (context = last turn)", stats.TokensUsed)
	}
	// tokens = fresh input + output + cache_creation, cache reads excluded:
	// turn1 (10+20+5000) + turn2 (5+30) = 5065 (NOT 65 with creation dropped,
	// NOT 7165 with the 2100 cache reads added).
	if stats.TokensSession != 5065 {
		t.Fatalf("TokensSession = %d, want 5065 (cache_creation incl, cache reads excl)", stats.TokensSession)
	}
}

// Codex bundles cached tokens into input_tokens (exposed as cached_input_tokens);
// session usage subtracts that, context tracks last_token_usage.
func TestAccumulateTranscriptUsageCodexTotal(t *testing.T) {
	var stats transcriptUsageStats
	accumulateTranscriptUsage(&stats, []byte(`{"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"output_tokens":50},"total_token_usage":{"input_tokens":9000,"cached_input_tokens":8000,"output_tokens":1000},"model_context_window":272000}}}`))
	if stats.TokensUsed != 150 { // last_token_usage: 100+50
		t.Fatalf("TokensUsed = %d, want 150 (context)", stats.TokensUsed)
	}
	// processedTokens of total_token_usage: (9000-8000) + 1000 = 2000 (Codex has
	// no cache_creation; cached reads excluded).
	if stats.TokensSession != 2000 {
		t.Fatalf("TokensSession = %d, want 2000 (session, cache reads excluded)", stats.TokensSession)
	}
	if stats.TokensMax != 272000 {
		t.Fatalf("TokensMax = %d, want 272000", stats.TokensMax)
	}
}

// When scrollback overflows the cap, the trim must advance to a line boundary so
// a reconnect's replay never begins mid-line or mid-escape-sequence (a byte-
// offset slice could otherwise land inside a CSI like "\x1b[3"|"2m" and corrupt
// the client terminal's parser for the rest of the replay).
func TestTrimScrollbackToLineBoundary(t *testing.T) {
	// 32-byte lines, each starting with an SGR sequence so a naive byte-offset
	// cut would have a high chance of landing inside one.
	body := append([]byte("\x1b[32m"), bytes.Repeat([]byte("x"), 26)...)
	line := append(body, '\n') // 32 bytes
	if len(line) != 32 {
		t.Fatalf("test line len = %d, want 32", len(line))
	}
	var buf []byte
	for i := 0; i < 100; i++ {
		buf = append(buf, line...) // 3200 bytes total
	}

	// Cap that lands mid-line (1000 is not a multiple of 32) \u2014 the trim must
	// advance past the next newline rather than slicing inside a line/sequence.
	got := trimScrollbackToLineBoundary(buf, 1000)
	if len(got) > 1000 {
		t.Fatalf("trimmed len %d exceeds cap 1000", len(got))
	}
	if len(got)%len(line) != 0 {
		t.Fatalf("trimmed len %d not aligned to %d-byte line boundary", len(got), len(line))
	}
	if !bytes.HasPrefix(got, body) {
		t.Fatalf("trimmed buffer does not start at a clean line boundary: %q\u2026", got[:min(8, len(got))])
	}
	// Under the cap \u2192 returned unchanged.
	if got := trimScrollbackToLineBoundary(line, 1000); len(got) != len(line) {
		t.Fatalf("under-cap buffer was trimmed: %d != %d", len(got), len(line))
	}
}

func TestTerminalScrollbackDefaultsAreBoundedAndConfigurable(t *testing.T) {
	if got := terminalScrollbackBytes(); got != 128*1024*1024 {
		t.Fatalf("terminalScrollbackBytes default = %d, want 128MiB", got)
	}
	if got := terminalScrollbackHeadroomBytes(); got != 1024*1024 {
		t.Fatalf("terminalScrollbackHeadroomBytes default = %d, want 1MiB", got)
	}

	t.Setenv("FLOW_TERMINAL_SCROLLBACK_BYTES", "2097152")
	t.Setenv("FLOW_TERMINAL_SCROLLBACK_HEADROOM_BYTES", "65536")
	if got := terminalScrollbackBytes(); got != 2097152 {
		t.Fatalf("terminalScrollbackBytes env = %d, want 2097152", got)
	}
	if got := terminalScrollbackHeadroomBytes(); got != 65536 {
		t.Fatalf("terminalScrollbackHeadroomBytes env = %d, want 65536", got)
	}
}

func TestTerminalClientQueueClosesOnBackpressure(t *testing.T) {
	client := &terminalClient{
		send: make(chan terminalWSMessage, 1),
		done: make(chan struct{}),
	}
	client.queue(terminalWSMessage{Type: "output", Data: "first"})
	client.queue(terminalWSMessage{Type: "output", Data: "overflow"})

	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("overflowed terminal client was not closed")
	}
}

func TestTerminalAddClientChunksLargeReplay(t *testing.T) {
	replay := bytes.Repeat([]byte("x"), terminalReplayChunkBytes()+17)
	sess := &terminalSession{
		provider:   "codex",
		sessionID:  "55555555-5555-4555-8555-555555555555",
		clients:    map[*terminalClient]struct{}{},
		scrollback: replay,
	}
	client := &terminalClient{send: make(chan terminalWSMessage, 8), done: make(chan struct{})}

	sess.addClient(client, true, 120, 32)

	status := <-client.send
	if status.Type != "status" {
		t.Fatalf("first message = %+v, want status", status)
	}
	first := <-client.send
	second := <-client.send
	if first.Type != "output" || len(first.Data) != terminalReplayChunkBytes() {
		t.Fatalf("first replay chunk = type %q len %d", first.Type, len(first.Data))
	}
	if second.Type != "output" || len(second.Data) != 17 {
		t.Fatalf("second replay chunk = type %q len %d", second.Type, len(second.Data))
	}
}

func TestTerminalProtocolDoesNotExposeHistoryReseedMessages(t *testing.T) {
	data, err := os.ReadFile("terminal_bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{`"history-start"`, `"history-chunk"`, `"history-end"`, "sendHistory"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("terminal bridge still exposes history reseed protocol marker %s", forbidden)
		}
	}
}

func TestBrowserTerminalDefaultScrollbackIsMaxPractical(t *testing.T) {
	data, err := os.ReadFile("ui/src/components/Terminal.tsx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "const DEFAULT_TERMINAL_SCROLLBACK_LINES = 1_000_000") {
		t.Fatal("browser terminal default scrollback should be 1,000,000 lines")
	}
}

func TestBrowserTerminalUsesNativeTmuxMouseScrollAndCopy(t *testing.T) {
	data, err := os.ReadFile("ui/src/components/Terminal.tsx")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{
		"attachCustomWheelEventHandler",
		"copy scroll-guard",
		"armCopyScrollGuard",
		"term.onSelectionChange",
		"term.getSelection()",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("browser terminal should let tmux own mouse scroll/copy; found %q", forbidden)
		}
	}
	for _, required := range []string{
		"registerOscHandler(52",
		"uploadTerminalAttachments",
		"host.addEventListener('paste', onHostPaste, true)",
		"host.addEventListener('drop', onHostDrop)",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("browser terminal lost required copy/attachment support %q", required)
		}
	}
}

func TestBrowserTerminalUsesSharedTmuxSessionWithoutBrowserAttachWorkaround(t *testing.T) {
	data, err := os.ReadFile("terminal_bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{
		"ensureBrowserAttachOptions",
		"ensureSharedTerminalBrowserAttachOptions",
		`"mouse", "off"`,
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("browser terminal should use the shared tmux session without attach workaround %q", forbidden)
		}
	}
}

func TestTerminalDataChunksExpandToFitClientQueue(t *testing.T) {
	t.Setenv("FLOW_TERMINAL_REPLAY_CHUNK_BYTES", "16384")
	replay := bytes.Repeat([]byte("x"), 50*1024)
	client := &terminalClient{send: make(chan terminalWSMessage, 4), done: make(chan struct{})}

	client.queue(terminalWSMessage{Type: "status"})
	queueTerminalDataChunks(client, "output", replay)

	select {
	case <-client.done:
		t.Fatal("adaptive replay chunking should not overflow available queue slots")
	default:
	}
	if got := len(client.send); got != 4 {
		t.Fatalf("queued messages = %d, want 4", got)
	}
}

func TestTerminalResizeOwnerUsesLargestConnectedGrid(t *testing.T) {
	sess := &terminalSession{clients: map[*terminalClient]struct{}{}}
	first := &terminalClient{send: make(chan terminalWSMessage, 4), done: make(chan struct{})}
	second := &terminalClient{send: make(chan terminalWSMessage, 4), done: make(chan struct{})}

	sess.addClient(first, false, 190, 36)
	if !sess.clientOwnsResize(first) {
		t.Fatal("large first client should own resize after initial attach")
	}
	sess.addClient(second, false, 100, 25)
	if !sess.clientOwnsResize(first) {
		t.Fatal("smaller later client must not shrink the shared terminal")
	}
	if sess.clientOwnsResize(second) {
		t.Fatal("smaller later client should not own resize")
	}

	if err := sess.resizeFrom(second, 220, 44); err != nil {
		t.Fatal(err)
	}
	if !sess.clientOwnsResize(second) {
		t.Fatal("larger resized client should become resize owner")
	}
}

func TestTerminalHubBrowserAttachUsesSeparatePTYForConcurrentBrowserClients(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	if _, err := db.Exec(`UPDATE tasks SET status = 'in-progress', session_provider = 'codex', session_id = '55555555-5555-4555-8555-555555555555' WHERE slug = 'build-ui'`); err != nil {
		t.Fatal(err)
	}

	oldSharedLookPath := sharedTerminalLookPath
	oldSharedCommand := sharedTerminalCommand
	sharedTerminalLookPath = func(name string) (string, error) {
		if name == "tmux" {
			return "/usr/bin/tmux", nil
		}
		return "", fmt.Errorf("not found")
	}
	resetSharedTerminalAvailable()
	defer func() {
		sharedTerminalLookPath = oldSharedLookPath
		sharedTerminalCommand = oldSharedCommand
		resetSharedTerminalAvailable()
	}()

	var commands [][]string
	sessionExists := false
	sharedTerminalCommand = func(args ...string) ([]byte, error) {
		commands = append(commands, append([]string(nil), args...))
		if len(args) == 0 {
			return nil, nil
		}
		switch args[0] {
		case "has-session":
			if sessionExists {
				return nil, nil
			}
			return nil, fmt.Errorf("missing session")
		case "list-panes":
			return []byte("FLOW_PERMISSION_MODE='auto' FLOW_SESSION_PROVIDER='codex' FLOW_TASK='build-ui' codex exec prompt\n"), nil
		case "capture-pane":
			return []byte("history line\n"), nil
		case "kill-session":
			sessionExists = false
			return nil, nil
		default:
			if containsString(args, "new-session") {
				sessionExists = true
			}
			return nil, nil
		}
	}

	binDir := t.TempDir()
	for _, bin := range []string{"codex", "tmux"} {
		path := binDir + "/" + bin
		if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !sharedTerminalAvailable() {
		t.Fatal("test setup did not make tmux available")
	}

	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/flow"})

	first, firstTransient, err := srv.terminals.attachBrowser("build-ui", 120, 32)
	if err != nil {
		t.Fatalf("first attachBrowser: %v", err)
	}
	if firstTransient {
		t.Fatal("first browser attach should be the tracked terminal session")
	}
	firstClient := &terminalClient{send: make(chan terminalWSMessage, 8), done: make(chan struct{})}
	first.addClient(firstClient, false, 120, 32)
	defer first.terminate()

	second, secondTransient, err := srv.terminals.attachBrowser("build-ui", 80, 24)
	if err != nil {
		t.Fatalf("second attachBrowser: %v", err)
	}
	if !secondTransient {
		t.Fatal("second concurrent browser attach should use its own transient PTY")
	}
	if second == first {
		t.Fatal("second browser attach reused the first browser PTY")
	}
	defer second.detachBrowserAttach()
	if got := srv.terminals.sessions["build-ui"]; got != first {
		t.Fatal("transient browser attach replaced the tracked terminal session")
	}

	got := commandLog(commands)
	if strings.Count(got, "new-session") != 1 {
		t.Fatalf("expected one shared tmux session, got commands:\n%s", got)
	}
}

func TestNormalizeCapturedPaneStripsBackgroundAndPadding(t *testing.T) {
	// A real capture-pane -e diff-add row: green background (ESC[48;5;22m) over
	// the content, padded across the full pane width, reset at the end. Replayed
	// into a narrower grid this wraps and the green background bleeds onto the
	// overflow rows. The normalizer must drop the background (so nothing can
	// bleed regardless of width) while keeping the foreground + text.
	pad := bytes.Repeat([]byte(" "), 150)
	line := append([]byte("\x1b[38;5;77m\x1b[48;5;22m 434 +\x1b[38;5;231m.slack-wizard {"), pad...)
	line = append(line, []byte("\x1b[39m\x1b[49m")...)

	got := normalizeCapturedPaneForTerminal(append(append([]byte(nil), line...), '\n'))

	if bytes.Contains(got, []byte("\x1b[48;5;22m")) {
		t.Fatalf("green background SGR survived — it must be stripped: %q", got)
	}
	if bytes.Contains(got, pad) {
		t.Fatalf("trailing space padding survived normalization: %q", got)
	}
	// Foreground colors and the line content must be preserved.
	if !bytes.Contains(got, []byte(".slack-wizard {")) {
		t.Fatalf("line content was lost: %q", got)
	}
	if !bytes.Contains(got, []byte("\x1b[38;5;77m")) || !bytes.Contains(got, []byte("\x1b[38;5;231m")) {
		t.Fatalf("foreground colors were dropped: %q", got)
	}
	if !bytes.HasSuffix(got, []byte("\r\n")) {
		t.Fatalf("output not CRLF-terminated: %q", got)
	}
}

func TestStripBackgroundSGR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Extended fg + extended bg in one sequence: keep fg, drop bg.
		{"fg+extbg", "\x1b[38;5;77m\x1b[48;5;22mX", "\x1b[38;5;77mX"},
		// Combined params: fg(38;5;231) + bg(48;5;237) → keep only fg.
		{"combined", "\x1b[38;5;231;48;5;237m❯", "\x1b[38;5;231m❯"},
		// Named background (42) dropped; named foreground (32) kept.
		{"named", "\x1b[32;42mok", "\x1b[32mok"},
		// Background-only sequence is removed entirely.
		{"bg-only", "a\x1b[41mb", "ab"},
		// Default-background (49) dropped; default-foreground (39) kept.
		{"defaults", "\x1b[39;49mz", "\x1b[39mz"},
		// Truecolor background dropped, truecolor foreground kept.
		{"truecolor", "\x1b[38;2;1;2;3m\x1b[48;2;9;9;9mq", "\x1b[38;2;1;2;3mq"},
		// Bare reset and full reset are preserved verbatim.
		{"resets", "\x1b[0m\x1b[mw", "\x1b[0m\x1b[mw"},
		// Attribute (bold=1) preserved alongside dropped bg.
		{"bold+bg", "\x1b[1;44mB", "\x1b[1mB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripBackgroundSGR([]byte(tc.in))); got != tc.want {
				t.Fatalf("stripBackgroundSGR(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripTrailingCellPaddingPreservesInteriorAndBorders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// No trailing padding, no SGR: unchanged.
		{"plain", "hello world", "hello world"},
		// Box-drawing table rows end in a border glyph, not a space: untouched.
		{"table-border", "\u2502 cell value             \u2502", "\u2502 cell value             \u2502"},
		// Interior spaces (alignment) are never trimmed \u2014 only the trailing run.
		{"interior-spaces", "a    b      ", "a    b"},
		// Spaces interleaved with the trailing resets: peel both, keep the resets.
		{"interleaved", "x  \x1b[39m \x1b[49m", "x\x1b[39m\x1b[49m"},
		// A bare ESC[m reset (empty params) still counts as a trailing SGR.
		{"bare-reset", "y   \x1b[m", "y\x1b[m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(stripTrailingCellPadding([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("stripTrailingCellPadding(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
