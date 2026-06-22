package monitor

import (
	"os"
	"testing"
)

func TestAppendFooter(t *testing.T) {
	cases := []struct{ text, footer, want string }{
		{"hello there", "_Sent using @flow_", "hello there\n\n_Sent using @flow_"},
		{"hello\n", "_Sent using @flow_", "hello\n\n_Sent using @flow_"}, // trailing newline collapsed
		{"hello", "", "hello"},                                          // no footer configured → unchanged
		{"", "_Sent using @flow_", "_Sent using @flow_"},                // empty body → just the footer
	}
	for _, tc := range cases {
		if got := appendFooter(tc.text, tc.footer); got != tc.want {
			t.Fatalf("appendFooter(%q, %q) = %q, want %q", tc.text, tc.footer, got, tc.want)
		}
	}
}

func TestSlackSendFooterConfig(t *testing.T) {
	// Unset footer → built-in "Sent using @<app>", where the app handle is the
	// REAL installed app name (here via FLOW_SLACK_APP_NAME), never a guess.
	os.Unsetenv("FLOW_SLACK_SEND_FOOTER")
	t.Setenv("FLOW_SLACK_APP_NAME", "Acme Ops")
	if got := SlackSendFooter(); got != "_Sent using @Acme Ops_" {
		t.Fatalf("unset footer + app name: got %q, want the real app name", got)
	}
	// Explicit empty disables it (escape hatch).
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "")
	if SlackSendFooter() != "" {
		t.Fatalf("empty env should disable the footer, got %q", SlackSendFooter())
	}
	// Custom text wins outright.
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "made with flow")
	if SlackSendFooter() != "made with flow" {
		t.Fatalf("custom env: got %q", SlackSendFooter())
	}
}

// The app handle must come from the real installed app, not a hardcoded name:
// FLOW_SLACK_APP_NAME wins, else the bot's auth.test handle, else the fallback.
func TestSlackAppHandle(t *testing.T) {
	t.Setenv("FLOW_SLACK_APP_NAME", "@Acme")
	if got := SlackAppHandle(); got != "Acme" {
		t.Fatalf("app-name env should win (leading @ stripped); got %q", got)
	}

	os.Unsetenv("FLOW_SLACK_APP_NAME")
	orig := resolveSlackBotHandleFn
	defer func() { resolveSlackBotHandleFn = orig }()
	resolveSlackBotHandleFn = func() string { return "acme_bot" }
	if got := SlackAppHandle(); got != "acme_bot" {
		t.Fatalf("should fall back to the bot's auth.test handle; got %q", got)
	}
	resolveSlackBotHandleFn = func() string { return "" }
	if got := SlackAppHandle(); got != fallbackAppHandle {
		t.Fatalf("with nothing resolvable, should fall back to %q; got %q", fallbackAppHandle, got)
	}
}

// The footer is suppressed on the operator↔bot command DM (a flow system
// message), but applied to ordinary channels/DMs.
func TestFooterSkippedOnCommandDM(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "_Sent using @flow_")
	withCommandChannel(t, "D_cmd")
	var got string
	orig := sendAsBotFn
	sendAsBotFn = func(channel, threadTS, text, identity string) error { got = text; return nil }
	defer func() { sendAsBotFn = orig }()

	if err := SendAsThread("D_cmd", "", "ack", "bot"); err != nil {
		t.Fatalf("SendAsThread: %v", err)
	}
	if got != "ack" {
		t.Fatalf("command DM should NOT get a footer; got %q", got)
	}
}

// The footer is applied at the send chokepoint, so every path (manual, agent,
// approved external send) gets exactly one — the composer never writes it.
func TestSendAsThreadAppendsFooter(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "_Sent using @flow_")
	var got string
	orig := sendAsBotFn
	sendAsBotFn = func(channel, threadTS, text, identity string) error { got = text; return nil }
	defer func() { sendAsBotFn = orig }()

	if err := SendAsThread("C1", "1700.1", "hello there", "user"); err != nil {
		t.Fatalf("SendAsThread: %v", err)
	}
	if got != "hello there\n\n_Sent using @flow_" {
		t.Fatalf("sent text = %q, want footer appended", got)
	}
}
