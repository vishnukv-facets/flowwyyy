package server

import (
	"bytes"
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
// TokensSession is cumulative "work done" — fresh input + output, EXCLUDING both
// cache re-reads AND cache-creation churn.
func TestAccumulateTranscriptUsageSumsClaudeSession(t *testing.T) {
	var stats transcriptUsageStats
	// Turn 1 also writes 5000 tokens to cache (cache_creation) — that must NOT
	// count toward session work (it's the 5-min-TTL re-caching that inflated real
	// sessions ~10x).
	accumulateTranscriptUsage(&stats, []byte(`{"type":"assistant","message":{"model":"claude","usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":5000,"output_tokens":20}}}`))
	accumulateTranscriptUsage(&stats, []byte(`{"type":"assistant","message":{"model":"claude","usage":{"input_tokens":5,"cache_read_input_tokens":1100,"output_tokens":30}}}`))
	if stats.TokensUsed != 1135 { // context = last turn total: 5+1100+30
		t.Fatalf("TokensUsed = %d, want 1135 (context = last turn)", stats.TokensUsed)
	}
	// work = fresh input + output, excluding cache_read AND cache_creation:
	// (10+20) + (5+30) = 65 (NOT 2165 with cache_read, NOT 5065 with cache_creation).
	if stats.TokensSession != 65 {
		t.Fatalf("TokensSession = %d, want 65 (work only: cache reads + cache_creation excluded)", stats.TokensSession)
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
	// freshTotal of total_token_usage: (9000-8000) + 1000 = 2000 (cache excluded).
	if stats.TokensSession != 2000 {
		t.Fatalf("TokensSession = %d, want 2000 (session, cache excluded)", stats.TokensSession)
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

func TestTerminalSessionSendHistoryChunksLargeReplay(t *testing.T) {
	replay := bytes.Repeat([]byte("h"), terminalReplayChunkBytes()+9)
	sess := &terminalSession{
		provider:   "codex",
		clients:    map[*terminalClient]struct{}{},
		scrollback: replay,
	}
	client := &terminalClient{send: make(chan terminalWSMessage, 8), done: make(chan struct{})}

	sess.sendHistory(client)

	start := <-client.send
	first := <-client.send
	second := <-client.send
	end := <-client.send
	if start.Type != "history-start" {
		t.Fatalf("start message = %+v, want history-start", start)
	}
	if first.Type != "history-chunk" || len(first.Data) != terminalReplayChunkBytes() {
		t.Fatalf("first history chunk = type %q len %d", first.Type, len(first.Data))
	}
	if second.Type != "history-chunk" || len(second.Data) != 9 {
		t.Fatalf("second history chunk = type %q len %d", second.Type, len(second.Data))
	}
	if end.Type != "history-end" {
		t.Fatalf("end message = %+v, want history-end", end)
	}
}

func TestTerminalDataChunksExpandToFitClientQueue(t *testing.T) {
	t.Setenv("FLOW_TERMINAL_REPLAY_CHUNK_BYTES", "16384")
	replay := bytes.Repeat([]byte("x"), 50*1024)
	client := &terminalClient{send: make(chan terminalWSMessage, 4), done: make(chan struct{})}

	client.queue(terminalWSMessage{Type: "status"})
	queueTerminalDataChunks(client, "output", replay, 0)

	select {
	case <-client.done:
		t.Fatal("adaptive replay chunking should not overflow available queue slots")
	default:
	}
	if got := len(client.send); got != 4 {
		t.Fatalf("queued messages = %d, want 4", got)
	}
}

func TestTerminalDataChunksReserveQueueSlotsForHistoryEnd(t *testing.T) {
	t.Setenv("FLOW_TERMINAL_REPLAY_CHUNK_BYTES", "16384")
	replay := bytes.Repeat([]byte("h"), 50*1024)
	client := &terminalClient{send: make(chan terminalWSMessage, 4), done: make(chan struct{})}

	client.queue(terminalWSMessage{Type: "history-start"})
	queueTerminalDataChunks(client, "history-chunk", replay, 1)
	client.queue(terminalWSMessage{Type: "history-end"})

	select {
	case <-client.done:
		t.Fatal("adaptive history chunking should preserve room for history-end")
	default:
	}
	for i := 0; i < 4; i++ {
		if msg := <-client.send; msg.Type == "history-end" {
			return
		}
	}
	t.Fatal("history-end was not queued")
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
