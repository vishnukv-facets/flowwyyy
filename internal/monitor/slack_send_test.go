package monitor

import (
	"errors"
	"testing"
)

func TestSendAsBotWritesDisabled(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "0")
	// FLOW_SLACK_WRITES_ENABLED unset (default false) — should error without
	// ever calling sendAsBotFn.
	called := false
	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error {
		called = true
		return nil
	}

	err := SendAsBot("D123", "hello")
	if err == nil {
		t.Fatal("expected error when writes disabled, got nil")
	}
	if called {
		t.Fatal("sendAsBotFn must not be called when writes disabled")
	}
}

func TestSendAsBotEmptyChannelError(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error { return nil }

	if err := SendAsBot("", "hello"); err == nil {
		t.Fatal("expected error for empty channel")
	}
	if err := SendAsBot("   ", "hello"); err == nil {
		t.Fatal("expected error for whitespace-only channel")
	}
}

func TestSendAsBotEmptyTextError(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error { return nil }

	if err := SendAsBot("D123", ""); err == nil {
		t.Fatal("expected error for empty text")
	}
	if err := SendAsBot("D123", "   "); err == nil {
		t.Fatal("expected error for whitespace-only text")
	}
}

func TestSendAsBotForwardsToFn(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	var gotChannel, gotText string
	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error {
		gotChannel = channel
		gotText = text
		return nil
	}

	if err := SendAsBot("D123", "hello world"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotChannel != "D123" {
		t.Errorf("channel = %q, want D123", gotChannel)
	}
	if gotText != "hello world" {
		t.Errorf("text = %q, want hello world", gotText)
	}
}

func TestSendAsThreadForwardsThreadTS(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	var gotThreadTS string
	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error {
		gotThreadTS = threadTS
		return nil
	}

	if err := SendAsThread("C1", "1234.000100", "hello", "bot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotThreadTS != "1234.000100" {
		t.Errorf("thread_ts = %q, want 1234.000100", gotThreadTS)
	}
}

func TestScheduleAsThreadWritesDisabled(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "0")
	called := false
	orig := scheduleAsBotFn
	defer func() { scheduleAsBotFn = orig }()
	scheduleAsBotFn = func(channel, threadTS, text, identity string, postAt int64) (string, error) {
		called = true
		return "", nil
	}

	if _, err := ScheduleAsThread("C1", "", "hello", "bot", 1781776200); err == nil {
		t.Fatal("expected error when writes disabled")
	}
	if called {
		t.Fatal("scheduleAsBotFn must not be called when writes disabled")
	}
}

func TestScheduleAsThreadForwardsPostAtAndThreadTS(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	var gotChannel, gotThreadTS, gotText, gotIdentity string
	var gotPostAt int64
	orig := scheduleAsBotFn
	defer func() { scheduleAsBotFn = orig }()
	scheduleAsBotFn = func(channel, threadTS, text, identity string, postAt int64) (string, error) {
		gotChannel, gotThreadTS, gotText, gotIdentity, gotPostAt = channel, threadTS, text, identity, postAt
		return "Q123", nil
	}

	id, err := ScheduleAsThread("C1", "1234.000100", "hello", "user", 1781776200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "Q123" {
		t.Errorf("scheduled id = %q, want Q123", id)
	}
	if gotChannel != "C1" || gotThreadTS != "1234.000100" || gotText != "hello" || gotIdentity != "user" || gotPostAt != 1781776200 {
		t.Errorf("forwarded (%q,%q,%q,%q,%d)", gotChannel, gotThreadTS, gotText, gotIdentity, gotPostAt)
	}
}

func TestSlackSendIdentity(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "bot"},
		{"bot", "bot"},
		{"user", "user"},
		{"USER", "user"},
		{"nonsense", "bot"},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("FLOW_SLACK_SEND_AS", c.env)
			if got := SlackSendIdentity(); got != c.want {
				t.Errorf("SlackSendIdentity() with %q = %q, want %q", c.env, got, c.want)
			}
		})
	}
}

// TestResolveSendIdentity covers the bot/user identity selection, including the
// invariant that the operator↔bot command IM is ALWAYS posted as the bot (never
// as the operator — that would loop and break self-echo detection).
func TestResolveSendIdentity(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-bot")
	t.Setenv("SLACK_USER_TOKEN", "xoxp-user")
	t.Setenv("FLOW_SLACK_USER_TOKEN", "")
	t.Setenv("FLOW_SLACK_WRITE_TOKEN", "")
	// Bot is a member of the command IM "Dcmd" only.
	orig := conversationIsBotIMFn
	conversationIsBotIMFn = func(ch string) bool { return ch == "Dcmd" }
	t.Cleanup(func() { conversationIsBotIMFn = orig; resetCommandChannelCache() })

	t.Run("default identity is bot", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "")
		resetCommandChannelCache()
		if tok, asUser := resolveSendIdentity("C123", ""); tok != "xoxb-bot" || asUser {
			t.Errorf("default = (%q,%v), want (xoxb-bot,false)", tok, asUser)
		}
	})
	t.Run("user identity posts as user in a channel", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "user")
		resetCommandChannelCache()
		if tok, asUser := resolveSendIdentity("C123", ""); tok != "xoxp-user" || !asUser {
			t.Errorf("user/channel = (%q,%v), want (xoxp-user,true)", tok, asUser)
		}
	})
	t.Run("command IM is always the bot even when user is set", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "user")
		resetCommandChannelCache()
		if tok, asUser := resolveSendIdentity("Dcmd", ""); tok != "xoxb-bot" || asUser {
			t.Errorf("user/command-IM = (%q,%v), want (xoxb-bot,false) — never impersonate operator in the command DM", tok, asUser)
		}
	})
	t.Run("user identity falls back to bot with no user token", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "user")
		t.Setenv("FLOW_SLACK_USER_TOKEN", "")
		t.Setenv("SLACK_USER_TOKEN", "")
		t.Setenv("FLOW_SLACK_TOKEN", "")
		t.Setenv("SLACK_TOKEN", "")
		resetCommandChannelCache()
		if tok, asUser := resolveSendIdentity("C123", ""); tok != "xoxb-bot" || asUser {
			t.Errorf("user/no-user-token = (%q,%v), want (xoxb-bot,false) fallback", tok, asUser)
		}
	})
	t.Run("override bot wins over global user", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "user") // global says user...
		resetCommandChannelCache()
		// ...but an explicit "bot" override forces the bot token (the path
		// `flow slack send --as bot` uses so automation gets chat:write).
		if tok, asUser := resolveSendIdentity("C123", "bot"); tok != "xoxb-bot" || asUser {
			t.Errorf("override-bot = (%q,%v), want (xoxb-bot,false)", tok, asUser)
		}
	})
	t.Run("override user wins over global bot", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "bot") // global says bot...
		resetCommandChannelCache()
		// ...but an explicit "user" override posts as the operator.
		if tok, asUser := resolveSendIdentity("C123", "user"); tok != "xoxp-user" || !asUser {
			t.Errorf("override-user = (%q,%v), want (xoxp-user,true)", tok, asUser)
		}
	})
	t.Run("override cannot impersonate operator in command IM", func(t *testing.T) {
		t.Setenv("FLOW_SLACK_SEND_AS", "bot")
		resetCommandChannelCache()
		// Even an explicit user override is overruled for the command IM.
		if tok, asUser := resolveSendIdentity("Dcmd", "user"); tok != "xoxb-bot" || asUser {
			t.Errorf("override-user/command-IM = (%q,%v), want (xoxb-bot,false)", tok, asUser)
		}
	})
}

func TestSendFileAsWritesDisabled(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "0")
	called := false
	orig := uploadFileFn
	defer func() { uploadFileFn = orig }()
	uploadFileFn = func(channel, threadTS, comment, filePath, identity string) error { called = true; return nil }

	if err := SendFileAs("C1", "caption", "/tmp/x.pdf", "bot"); err == nil {
		t.Fatal("expected error when writes disabled")
	}
	if called {
		t.Fatal("uploadFileFn must not be called when writes disabled")
	}
}

func TestSendFileAsForwardsToFn(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	var gotChannel, gotComment, gotPath, gotIdentity string
	orig := uploadFileFn
	defer func() { uploadFileFn = orig }()
	uploadFileFn = func(channel, threadTS, comment, filePath, identity string) error {
		gotChannel, gotComment, gotPath, gotIdentity = channel, comment, filePath, identity
		return nil
	}
	if err := SendFileAs("C1", "caption", "/tmp/x.pdf", "bot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotChannel != "C1" || gotComment != "caption" || gotPath != "/tmp/x.pdf" || gotIdentity != "bot" {
		t.Errorf("forwarded (%q,%q,%q,%q)", gotChannel, gotComment, gotPath, gotIdentity)
	}
}

func TestSendFileAsRequiresChannelAndFile(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	orig := uploadFileFn
	defer func() { uploadFileFn = orig }()
	uploadFileFn = func(channel, threadTS, comment, filePath, identity string) error { return nil }
	if err := SendFileAs("", "c", "/tmp/x", "bot"); err == nil {
		t.Error("expected error for empty channel")
	}
	if err := SendFileAs("C1", "c", "", "bot"); err == nil {
		t.Error("expected error for empty file")
	}
}

func TestSendFileAsThreadForwardsThreadTS(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	var gotThreadTS string
	orig := uploadFileFn
	defer func() { uploadFileFn = orig }()
	uploadFileFn = func(channel, threadTS, comment, filePath, identity string) error {
		gotThreadTS = threadTS
		return nil
	}

	if err := SendFileAsThread("C1", "1234.000100", "caption", "/tmp/x.pdf", "bot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotThreadTS != "1234.000100" {
		t.Errorf("thread_ts = %q, want 1234.000100", gotThreadTS)
	}
}

func TestSendAsBotPropagatesFnError(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")

	boom := errors.New("network error")
	orig := sendAsBotFn
	defer func() { sendAsBotFn = orig }()
	sendAsBotFn = func(channel, threadTS, text, identity string) error { return boom }

	err := SendAsBot("D123", "hello")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}
