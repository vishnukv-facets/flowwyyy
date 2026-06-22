package monitor

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestAppendFooter(t *testing.T) {
	cases := []struct{ text, footer, want string }{
		{"hello there", "_Sent using @flow_", "hello there\n\n_Sent using @flow_"},
		{"hello\n", "_Sent using @flow_", "hello\n\n_Sent using @flow_"}, // trailing newline collapsed
		{"hello", "", "hello"},                           // no footer configured → unchanged
		{"", "_Sent using @flow_", "_Sent using @flow_"}, // empty body → just the footer
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

// When the bot's user id is resolvable, the default footer must use a real Slack
// mention (<@U…>) so the "@flow" LINKS to the flow app, not render as dead text.
// Falls back to a plain "@handle" only when the user id can't be resolved.
func TestSlackSendFooterLinksAppMention(t *testing.T) {
	os.Unsetenv("FLOW_SLACK_SEND_FOOTER")
	os.Unsetenv("FLOW_SLACK_APP_NAME")

	origID := resolveSlackBotUserIDFn
	defer func() { resolveSlackBotUserIDFn = origID }()

	// Bot user id resolvable → mention.
	resolveSlackBotUserIDFn = func() string { return "U07FLOWBOT" }
	if got, want := SlackSendFooter(), "_Sent using_ <@U07FLOWBOT>"; got != want {
		t.Fatalf("with a resolvable bot user id, footer = %q, want a linked mention %q", got, want)
	}

	// No bot user id → plain "@handle" fallback (unchanged legacy behavior).
	resolveSlackBotUserIDFn = func() string { return "" }
	origHandle := resolveSlackBotHandleFn
	defer func() { resolveSlackBotHandleFn = origHandle }()
	resolveSlackBotHandleFn = func() string { return "flow" }
	if got, want := SlackSendFooter(), "_Sent using @flow_"; got != want {
		t.Fatalf("with no bot user id, footer = %q, want plain fallback %q", got, want)
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
// message), but present on ordinary channels/DMs. footerForChannel is the
// chokepoint resolver; "" means "no footer".
func TestFooterForChannel(t *testing.T) {
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "_Sent using @flow_")
	withCommandChannel(t, "D_cmd")
	if f := footerForChannel("D_cmd"); f != "" {
		t.Errorf("command DM should get no footer; got %q", f)
	}
	if f := footerForChannel("C1"); f != "_Sent using @flow_" {
		t.Errorf("channel footer = %q, want the configured footer", f)
	}
}

// SendAsThread passes the RAW body to the sender — the footer is no longer baked
// into the editable message text; it rides as a non-editable context block.
func TestSendAsThreadPassesRawBody(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SEND_FOOTER", "_Sent using @flow_")
	var got string
	orig := sendAsBotFn
	sendAsBotFn = func(channel, threadTS, text, identity string) error { got = text; return nil }
	defer func() { sendAsBotFn = orig }()

	if err := SendAsThread("C1", "1700.1", "hello there", "user"); err != nil {
		t.Fatalf("SendAsThread: %v", err)
	}
	if got != "hello there" {
		t.Fatalf("sent text = %q, want the raw body with no footer baked in", got)
	}
}

// slackPostOptions renders the footer as a non-editable context block below the
// message (short body), posts plain text when there's no footer, and falls back
// to appending the footer for an over-limit body. (slack-go stashes blocks in an
// unexported config field, not url.Values, so we assert on the `text` value —
// which proves where the footer went — plus the option count, which is one
// higher when a blocks option is present.)
func TestSlackPostOptionsFooterBlock(t *testing.T) {
	api := "https://slack.com/api/"
	textOf := func(opts []slack.MsgOption) (string, url.Values) {
		_, vals, err := slack.UnsafeApplyMsgOptions("tok", "C1", api, opts...)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		return vals.Get("text"), vals
	}

	// Footer + short body → context block: the footer is NOT in the body text,
	// and an extra (blocks) option is present.
	blockOpts := slackPostOptions("hi there", "_Sent using @flow_", true, "1700.1")
	txt, vals := textOf(blockOpts)
	if txt != "hi there" {
		t.Errorf("block-mode text = %q, want the raw body (footer in the block, not the text)", txt)
	}
	if vals.Get("thread_ts") != "1700.1" {
		t.Errorf("thread_ts = %q", vals.Get("thread_ts"))
	}

	// No footer → plain text, no blocks option (one fewer option than block-mode).
	plainOpts := slackPostOptions("hi there", "", true, "1700.1")
	if txt, _ := textOf(plainOpts); txt != "hi there" {
		t.Errorf("no-footer text = %q, want the raw body", txt)
	}
	if len(blockOpts) != len(plainOpts)+1 {
		t.Errorf("block-mode should add exactly one (blocks) option: block=%d plain=%d", len(blockOpts), len(plainOpts))
	}

	// Over-limit body → fallback appends the footer to text, no extra block option.
	long := strings.Repeat("x", sectionTextLimit+1)
	overOpts := slackPostOptions(long, "_Sent using @flow_", true, "1700.1")
	if txt, _ := textOf(overOpts); !strings.HasSuffix(txt, "_Sent using @flow_") {
		t.Errorf("over-limit fallback should append the footer to text")
	}
	if len(overOpts) != len(plainOpts) {
		t.Errorf("over-limit fallback should not add a blocks option: over=%d plain=%d", len(overOpts), len(plainOpts))
	}
}
