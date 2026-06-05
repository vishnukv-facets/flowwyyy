package monitor

import "testing"

// A real-shaped Socket Mode payload for a message that forwards a thread reply
// into a DM (the "Made you the owner" case). The shared message lives in
// attachments[] with channel_id + ts + a permalink whose thread_ts is the
// original thread parent.
const forwardedDMPayload = `{
  "team_id": "T1",
  "api_app_id": "A1",
  "event": {
    "type": "message",
    "channel": "D_dm",
    "channel_type": "im",
    "user": "U_samarthya",
    "ts": "1700000999.000100",
    "text": "Made you the owner",
    "attachments": [
      {
        "is_share": true,
        "channel_id": "C_eng",
        "ts": "1700000500.000300",
        "from_url": "https://acme.slack.com/archives/C_eng/p1700000500000300?thread_ts=1700000000.000100&cid=C_eng",
        "author_id": "U_me",
        "text": "Quick blocker on the triage…",
        "footer": "Thread in #engineering-team"
      }
    ]
  }
}`

func TestParseSharedRef_ForwardedThreadReply(t *testing.T) {
	ref, ok := parseSharedRef([]byte(forwardedDMPayload))
	if !ok {
		t.Fatal("expected a shared ref from a forwarded message")
	}
	if ref.Channel != "C_eng" {
		t.Errorf("Channel = %q, want C_eng", ref.Channel)
	}
	// thread_ts (parent), pulled from the permalink — NOT the shared reply's ts.
	if ref.ThreadTS != "1700000000.000100" {
		t.Errorf("ThreadTS = %q, want the permalink thread_ts parent", ref.ThreadTS)
	}
	if ref.TS != "1700000500.000300" {
		t.Errorf("TS = %q, want the shared message ts", ref.TS)
	}
}

func TestParseSharedRef_UnfurlWithoutThreadTSFallsBackToTS(t *testing.T) {
	// A pasted-permalink unfurl of a top-level message: is_msg_unfurl, no
	// thread_ts anywhere. ThreadTS must fall back to the message's own ts.
	payload := `{"event":{"type":"message","channel":"D1","ts":"1.2",
	  "attachments":[{"is_msg_unfurl":true,"channel_id":"C_x","ts":"1700000700.000900",
	  "from_url":"https://acme.slack.com/archives/C_x/p1700000700000900"}]}}`
	ref, ok := parseSharedRef([]byte(payload))
	if !ok {
		t.Fatal("expected a shared ref from an unfurl")
	}
	if ref.Channel != "C_x" || ref.TS != "1700000700.000900" {
		t.Errorf("ref = %+v, want channel C_x ts 1700000700.000900", ref)
	}
	if ref.ThreadTS != ref.TS {
		t.Errorf("ThreadTS = %q, want fallback to TS %q", ref.ThreadTS, ref.TS)
	}
}

func TestParseSharedRef_PlainMessageHasNoRef(t *testing.T) {
	payload := `{"event":{"type":"message","channel":"C1","ts":"1.2","text":"hi"}}`
	if _, ok := parseSharedRef([]byte(payload)); ok {
		t.Error("a plain message must not yield a shared ref")
	}
}

func TestParseSharedRef_AttachmentMissingIDsSkipped(t *testing.T) {
	// A decorative attachment (link preview with no channel_id/ts) is not a
	// message reference and must be ignored.
	payload := `{"event":{"type":"message","channel":"C1","ts":"1.2",
	  "attachments":[{"title":"Some link","text":"preview","from_url":"https://example.com"}]}}`
	if _, ok := parseSharedRef([]byte(payload)); ok {
		t.Error("non-message attachment must not yield a shared ref")
	}
}

func TestParseSharedRef_EmptyAndGarbage(t *testing.T) {
	if _, ok := parseSharedRef(nil); ok {
		t.Error("nil payload must not yield a ref")
	}
	if _, ok := parseSharedRef([]byte("not json")); ok {
		t.Error("garbage payload must not yield a ref")
	}
}

func TestSharedRef_ThreadKeys(t *testing.T) {
	ref := SharedRef{Channel: "C_eng", ThreadTS: "1700000000.000100", TS: "1700000500.000300"}
	keys := ref.ThreadKeys()
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2 (parent then message)", keys)
	}
	if keys[0] != "C_eng:1700000000.000100" {
		t.Errorf("first key = %q, want parent key", keys[0])
	}
	if keys[1] != "C_eng:1700000500.000300" {
		t.Errorf("second key = %q, want message key", keys[1])
	}

	// When parent == message ts, only one key (deduped).
	same := SharedRef{Channel: "C", ThreadTS: "9.9", TS: "9.9"}
	if got := same.ThreadKeys(); len(got) != 1 {
		t.Errorf("deduped keys = %v, want 1", got)
	}
}
