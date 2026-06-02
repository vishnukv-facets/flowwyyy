package monitor

import (
	"context"
	"testing"
)

type fakeReplies struct {
	msgs      []SlackMessage
	gotOldest string
	calls     int
}

func (f *fakeReplies) Replies(_ context.Context, _, _, oldest string, _ int) ([]SlackMessage, error) {
	f.calls++
	f.gotOldest = oldest
	return f.msgs, nil
}

// seedInbox writes a Slack message entry so the backfill has a baseline ts.
func seedInbox(t *testing.T, slug, channel, threadTS, ts, text string) {
	t.Helper()
	if err := AppendInboxEvent(slug, InboundEvent{
		Kind: "message", Channel: channel, ChannelType: "channel",
		TS: ts, ThreadTS: threadTS, Text: text,
	}); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}
}

func TestSlackBackfillReconcile_AppendsOnlyNewerDeduped(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	const slug, channel, root = "slack-c1-50-000000", "C1", "50.000000"
	seedInbox(t, slug, channel, root, "100.000000", "first reply")

	fake := &fakeReplies{msgs: []SlackMessage{
		{TS: root, User: "U1", Text: "thread root"},               // skipped: == threadTS
		{TS: "100.000000", User: "U1", Text: "first reply"},       // skipped: already seen
		{TS: "120.000000", User: "U2", Text: "newer reply A"},     // appended
		{TS: "150.000000", User: "U3", Text: "newer reply B"},     // appended
		{TS: "160.000000", User: "U4", Text: "edit", SubType: "message_changed"}, // skipped: subtype
		{TS: "170.000000", User: "", Text: ""},                    // skipped: empty
		{TS: "180.000000", User: "U5", Text: "broadcast", SubType: "thread_broadcast"}, // appended
	}}

	bf := &SlackBackfill{client: fake, limit: 200}
	n, err := bf.reconcile(context.Background(), slug, channel, root)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 3 {
		t.Fatalf("appended = %d, want 3 (120, 150, 180)", n)
	}
	if fake.gotOldest != "100.000000" {
		t.Fatalf("oldest passed = %q, want the inbox max ts 100.000000", fake.gotOldest)
	}

	// inbox.jsonl now holds the original + 3 recovered, no dupes.
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	got := map[string]int{}
	for _, e := range entries {
		got[e.Event.TS]++
	}
	for _, ts := range []string{"100.000000", "120.000000", "150.000000", "180.000000"} {
		if got[ts] != 1 {
			t.Errorf("ts %s appears %d times, want exactly 1", ts, got[ts])
		}
	}
	for _, ts := range []string{"50.000000", "160.000000", "170.000000"} {
		if got[ts] != 0 {
			t.Errorf("ts %s should not be in inbox, found %d", ts, got[ts])
		}
	}

	// Idempotent: a second pass over the same replies appends nothing.
	n2, err := bf.reconcile(context.Background(), slug, channel, root)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second pass appended = %d, want 0 (dedup)", n2)
	}
}

func TestSlackBackfillReconcile_NoBaselineSkips(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	const slug, channel, root = "slack-c2-10-000000", "C2", "10.000000"
	// No inbox.jsonl at all → no baseline → backfill must not flood history,
	// and must not even call Slack.
	fake := &fakeReplies{msgs: []SlackMessage{{TS: "20.000000", User: "U1", Text: "reply"}}}
	bf := &SlackBackfill{client: fake, limit: 200}
	n, err := bf.reconcile(context.Background(), slug, channel, root)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Fatalf("appended = %d, want 0 with no baseline", n)
	}
	if fake.calls != 0 {
		t.Fatalf("Slack called %d times, want 0 with no baseline", fake.calls)
	}
}
