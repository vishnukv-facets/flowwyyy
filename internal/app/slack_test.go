package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func TestCmdSlackNoSubcommand(t *testing.T) {
	rc := cmdSlack([]string{})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestCmdSlackUnknownSubcommand(t *testing.T) {
	rc := cmdSlack([]string{"blast"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

// stubPostSlackSend swaps postSlackSendFn for the test and restores it.
func stubPostSlackSend(t *testing.T, fn func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error)) {
	t.Helper()
	orig := postSlackSendFn
	t.Cleanup(func() { postSlackSendFn = orig })
	postSlackSendFn = fn
}

// stubSlackSend swaps slackSendFn (in-process fallback) for the test.
func stubSlackSend(t *testing.T, fn func(channel, threadTS, text, identity string) error) {
	t.Helper()
	orig := slackSendFn
	t.Cleanup(func() { slackSendFn = orig })
	slackSendFn = fn
}

func stubSlackScheduleSend(t *testing.T, fn func(channel, threadTS, text, identity string, postAt int64) (string, error)) {
	t.Helper()
	orig := slackScheduleSendFn
	t.Cleanup(func() { slackScheduleSendFn = orig })
	slackScheduleSendFn = fn
}

func stubSlackSendNow(t *testing.T, now time.Time) {
	t.Helper()
	orig := slackSendNow
	t.Cleanup(func() { slackSendNow = orig })
	slackSendNow = func() time.Time { return now }
}

// Server POST succeeds -> rc 0, no fallback to in-process slackSendFn.
func TestCmdSlackSendViaServerSuccess(t *testing.T) {
	var gotChannel, gotText string
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotChannel, gotText = channel, text
		return 200, `{"ok":true}`, nil
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		t.Fatal("slackSendFn must not be called when server POST succeeds")
		return nil
	})

	rc := cmdSlack([]string{"send", "--channel", "D1", "--text", "hi"})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if gotChannel != "D1" || gotText != "hi" {
		t.Errorf("posted channel=%q text=%q, want D1/hi", gotChannel, gotText)
	}
}

func TestCmdSlackSendRecordsWorkEvent(t *testing.T) {
	root := setupFlowRoot(t)
	t.Setenv("CODEX_SESSION_ID", "codex-slack-session")
	if rc := cmdAdd([]string{"task", "Reply in Slack", "--slug", "slack-reply-task", "--work-dir", t.TempDir(), "--agent", "codex"}); rc != 0 {
		t.Fatalf("cmdAdd rc=%d", rc)
	}
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET session_id = ?, session_provider = 'codex' WHERE slug = ?`, "codex-slack-session", "slack-reply-task"); err != nil {
		t.Fatalf("bind task session: %v", err)
	}
	db.Close()

	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 200, `{"ok":true}`, nil
	})
	rc := cmdSlack([]string{"send", "--channel", "C123", "--thread-ts", "1780000000.000100", "--text", "On it", "--as", "user"})
	if rc != 0 {
		t.Fatalf("cmdSlack rc=%d", rc)
	}

	db, err = flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer db.Close()
	rows, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "slack_send", TaskSlug: "slack-reply-task"})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("slack_send rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Source != "slack" || got.Provider != "codex" || got.SessionID != "codex-slack-session" || got.ExternalID != "C123:1780000000.000100" {
		t.Fatalf("slack_send provenance = %+v", got)
	}
	if !strings.Contains(got.MetadataJSON, `"identity":"user"`) || !strings.Contains(got.MetadataJSON, `"text_len":5`) {
		t.Fatalf("slack_send metadata = %s", got.MetadataJSON)
	}
}

// --thread-ts forwards the thread target to the server POST.
func TestCmdSlackSendForwardsThreadTS(t *testing.T) {
	var gotThreadTS string
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotThreadTS = threadTS
		return 200, `{"ok":true}`, nil
	})

	rc := cmdSlack([]string{"send", "--channel", "C1", "--thread-ts", "1234.000100", "--text", "hi"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotThreadTS != "1234.000100" {
		t.Errorf("forwarded thread_ts = %q, want 1234.000100", gotThreadTS)
	}
}

// Server reached but Slack rejected (e.g. account_inactive) -> rc 1, no fallback.
func TestCmdSlackSendViaServerSlackError(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 502, `{"error":"account_inactive"}`, nil
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		t.Fatal("slackSendFn must not be called when server returns a Slack error")
		return nil
	})

	rc := cmdSlack([]string{"send", "--channel", "D1", "--text", "hi"})
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
}

// Server unreachable -> fall back to in-process slackSendFn.
func TestCmdSlackSendFallsBackWhenServerUnreachable(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 0, "", fmt.Errorf("connection refused")
	})
	var fellBack bool
	var gotChannel, gotText string
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		fellBack = true
		gotChannel, gotText = channel, text
		return nil
	})

	rc := cmdSlack([]string{"send", "--channel", "D1", "--text", "hi"})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if !fellBack {
		t.Error("expected fallback to slackSendFn when server unreachable")
	}
	if gotChannel != "D1" || gotText != "hi" {
		t.Errorf("fallback channel=%q text=%q, want D1/hi", gotChannel, gotText)
	}
}

func TestCmdSlackSendFallsBackWithThreadTS(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 0, "", fmt.Errorf("connection refused")
	})
	var gotThreadTS string
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		gotThreadTS = threadTS
		return nil
	})

	rc := cmdSlack([]string{"send", "--channel", "C1", "--thread-ts", "1234.000100", "--text", "hi"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotThreadTS != "1234.000100" {
		t.Errorf("fallback thread_ts = %q, want 1234.000100", gotThreadTS)
	}
}

// Server unreachable AND in-process fallback fails -> rc 1.
func TestCmdSlackSendFallbackError(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 0, "", fmt.Errorf("connection refused")
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		return fmt.Errorf("network failure")
	})

	rc := cmdSlack([]string{"send", "--channel", "D1", "--text", "hello"})
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
}

// --as bot forwards the identity override to the server POST.
func TestCmdSlackSendForwardsAsBot(t *testing.T) {
	var gotIdentity string
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotIdentity = identity
		return 200, `{"ok":true}`, nil
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error { return nil })

	rc := cmdSlack([]string{"send", "--channel", "C1", "--text", "hi", "--as", "bot"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotIdentity != "bot" {
		t.Errorf("forwarded identity = %q, want bot", gotIdentity)
	}
}

func TestCmdSlackSendRejectsBadAs(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		t.Fatal("must not POST when --as is invalid")
		return 0, "", nil
	})
	rc := cmdSlack([]string{"send", "--channel", "C1", "--text", "hi", "--as", "nonsense"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2 for invalid --as", rc)
	}
}

// --file forwards the path to the server POST (text rides along as comment).
func TestCmdSlackSendForwardsFile(t *testing.T) {
	var gotFile, gotText string
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotFile, gotText = file, text
		return 200, `{"ok":true}`, nil
	})
	rc := cmdSlack([]string{"send", "--channel", "C1", "--file", "/tmp/x.pdf", "--text", "see attached", "--as", "bot"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotFile != "/tmp/x.pdf" || gotText != "see attached" {
		t.Errorf("forwarded file=%q text=%q, want /tmp/x.pdf/see attached", gotFile, gotText)
	}
}

func TestCmdSlackSendReadsTextFile(t *testing.T) {
	path := t.TempDir() + "/reply.txt"
	if err := os.WriteFile(path, []byte("hello from a file\nwith punctuation: \"ok\""), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotText string
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotText = text
		return 200, `{"ok":true}`, nil
	})

	rc := cmdSlack([]string{"send", "--channel", "C1", "--thread-ts", "1234.000100", "--text-file", path, "--as", "user"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotText != "hello from a file\nwith punctuation: \"ok\"" {
		t.Errorf("text = %q", gotText)
	}
}

// --file with no --text is allowed (attachment without a comment).
func TestCmdSlackSendFileNoTextAllowed(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 200, `{"ok":true}`, nil
	})
	rc := cmdSlack([]string{"send", "--channel", "C1", "--file", "/tmp/x.png"})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (file without text is valid)", rc)
	}
}

// Server unreachable with --file -> fall back to slackFileSendFn, not slackSendFn.
func TestCmdSlackSendFileFallsBack(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 0, "", fmt.Errorf("connection refused")
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		t.Fatal("text fallback must not be used for a file send")
		return nil
	})
	var gotFile string
	orig := slackFileSendFn
	t.Cleanup(func() { slackFileSendFn = orig })
	slackFileSendFn = func(channel, threadTS, comment, filePath, identity string) error {
		gotFile = filePath
		return nil
	}
	rc := cmdSlack([]string{"send", "--channel", "C1", "--file", "/tmp/x.pdf"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if gotFile != "/tmp/x.pdf" {
		t.Errorf("file fallback path = %q, want /tmp/x.pdf", gotFile)
	}
}

func TestParseSlackPostAtFormatsAndWindow(t *testing.T) {
	loc := time.FixedZone("IST", 5*60*60+30*60)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, loc)
	stubSlackSendNow(t, now)

	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"relative", "+30m", now.Add(30 * time.Minute).Unix()},
		{"absolute local", "2026-06-18 12:45", time.Date(2026, 6, 18, 12, 45, 0, 0, loc).Unix()},
		{"epoch", fmt.Sprint(now.Add(2 * time.Hour).Unix()), now.Add(2 * time.Hour).Unix()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSlackPostAt(c.in)
			if err != nil {
				t.Fatalf("parseSlackPostAt(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("post_at = %d, want %d", got, c.want)
			}
		})
	}

	for _, in := range []string{"+1m", fmt.Sprint(now.Add(121 * 24 * time.Hour).Unix())} {
		if _, err := parseSlackPostAt(in); err == nil {
			t.Errorf("parseSlackPostAt(%q) expected window error", in)
		}
	}
}

func TestCmdSlackSendAtForwardsPostAtAndPrintsConfirmation(t *testing.T) {
	loc := time.FixedZone("IST", 5*60*60+30*60)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, loc)
	stubSlackSendNow(t, now)
	var gotPostAt int64
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		gotPostAt = postAt
		return 200, `{"ok":true,"scheduled_message_id":"Q123"}`, nil
	})

	out := captureStdout(t, func() {
		rc := cmdSlack([]string{"send", "--channel", "C1", "--text", "hi", "--at", "+30m"})
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if gotPostAt != now.Add(30*time.Minute).Unix() {
		t.Errorf("post_at = %d, want %d", gotPostAt, now.Add(30*time.Minute).Unix())
	}
	if !strings.Contains(out, "Q123") || !strings.Contains(out, "2026-06-18T12:30:00+05:30") {
		t.Errorf("confirmation = %q, want id and resolved time", out)
	}
}

func TestCmdSlackSendAtRejectsFile(t *testing.T) {
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		t.Fatal("must not POST scheduled file uploads")
		return 0, "", nil
	})
	rc := cmdSlack([]string{"send", "--channel", "C1", "--file", "/tmp/x.pdf", "--at", "+30m"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestCmdSlackSendAtFallsBackToSchedule(t *testing.T) {
	loc := time.FixedZone("IST", 5*60*60+30*60)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, loc)
	stubSlackSendNow(t, now)
	stubPostSlackSend(t, func(channel, threadTS, text, identity, file string, postAt int64) (int, string, error) {
		return 0, "", fmt.Errorf("connection refused")
	})
	stubSlackSend(t, func(channel, threadTS, text, identity string) error {
		t.Fatal("immediate fallback must not be used for scheduled sends")
		return nil
	})
	var gotPostAt int64
	stubSlackScheduleSend(t, func(channel, threadTS, text, identity string, postAt int64) (string, error) {
		gotPostAt = postAt
		return "Q456", nil
	})

	out := captureStdout(t, func() {
		rc := cmdSlack([]string{"send", "--channel", "C1", "--text", "hi", "--at", "+45m"})
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if gotPostAt != now.Add(45*time.Minute).Unix() {
		t.Errorf("fallback post_at = %d, want %d", gotPostAt, now.Add(45*time.Minute).Unix())
	}
	if !strings.Contains(out, "Q456") || !strings.Contains(out, "2026-06-18T12:45:00+05:30") {
		t.Errorf("confirmation = %q, want id and resolved time", out)
	}
}

func TestCmdSlackSendMissingChannel(t *testing.T) {
	rc := cmdSlack([]string{"send", "--text", "hello"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestCmdSlackSendMissingText(t *testing.T) {
	rc := cmdSlack([]string{"send", "--channel", "D1"})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestServerSlackError(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"error":"account_inactive"}`, "account_inactive"},
		{`{"error":"slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)"}`, "slack writes disabled (set FLOW_SLACK_WRITES_ENABLED=1)"},
		{"plain text error", "plain text error"},
		{"", "slack send failed (server)"},
	}
	for _, c := range cases {
		if got := serverSlackError(c.in); got != c.want {
			t.Errorf("serverSlackError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
