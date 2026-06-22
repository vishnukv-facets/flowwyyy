package monitor

import "testing"

func TestReactAsThread(t *testing.T) {
	orig := reactAsBotFn
	defer func() { reactAsBotFn = orig }()

	var got struct{ channel, ts, emoji, identity string }
	reactAsBotFn = func(channel, ts, emoji, identity string) error {
		got.channel, got.ts, got.emoji, got.identity = channel, ts, emoji, identity
		return nil
	}

	// Disabled writes → refuse, no call.
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "")
	if err := ReactAsThread("C1", "1700.1", "+1", "user"); err == nil {
		t.Fatal("expected error when slack writes are disabled")
	}

	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	// Missing channel / ts / emoji → validation errors.
	if err := ReactAsThread("", "1700.1", "+1", "user"); err == nil {
		t.Error("expected error for empty channel")
	}
	if err := ReactAsThread("C1", "", "+1", "user"); err == nil {
		t.Error("expected error for empty ts")
	}
	if err := ReactAsThread("C1", "1700.1", "::", "user"); err == nil {
		t.Error("expected error for empty emoji")
	}

	// Valid: surrounding colons stripped, args forwarded verbatim.
	if err := ReactAsThread("C1", "1700.1", ":+1:", "user"); err != nil {
		t.Fatalf("ReactAsThread: %v", err)
	}
	if got.channel != "C1" || got.ts != "1700.1" || got.emoji != "+1" || got.identity != "user" {
		t.Fatalf("forwarded args = %+v, want channel=C1 ts=1700.1 emoji=+1 identity=user", got)
	}
}
