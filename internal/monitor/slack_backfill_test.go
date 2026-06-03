package monitor

import (
	"context"
	"testing"
	"time"

	"flow/internal/flowdb"
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

// fakeHistory stubs the user-token DM replies client (conversations.replies),
// answering by thread root ts since DM conversations are threaded.
type fakeHistory struct {
	repliesByRoot  map[string][]SlackMessage // replies keyed by thread root ts
	gotOldest      string
	gotChannel     string
	replyCalls     int
	gotThreadRoots map[string]bool
}

func (f *fakeHistory) Replies(_ context.Context, channel, threadTS, oldest string, _ int) ([]SlackMessage, error) {
	f.replyCalls++
	f.gotOldest = oldest
	f.gotChannel = channel
	if f.gotThreadRoots == nil {
		f.gotThreadRoots = map[string]bool{}
	}
	f.gotThreadRoots[threadTS] = true
	return f.repliesByRoot[threadTS], nil
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

func TestSlackBackfill_DMThreadUsesUserClient(t *testing.T) {
	// A DM thread (slack-thread:<dm-channel>:<root>) reconciles via the USER
	// token client — the bot can't read the operator's DMs — and recovers a
	// reply missed during a gap. The bot client must NOT be touched for a DM.
	t.Setenv("FLOW_ROOT", t.TempDir())
	const slug, dm, root = "slack-dm-threaded", "D_ALICE", "1780480392.819809"
	if err := AppendInboxEvent(slug, InboundEvent{
		Kind: "message", Channel: dm, ChannelType: "im",
		TS: "1780489629.079919", ThreadTS: root, UserID: "U_me", Text: "stepping out",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	user := &fakeHistory{repliesByRoot: map[string][]SlackMessage{
		root: {
			{TS: "1780489629.079919", ThreadTS: root, User: "U_me", Text: "stepping out"},        // seen
			{TS: "1780491705.662279", ThreadTS: root, User: "U_ishaan", Text: "why new file?"},   // missed
		},
	}}
	bot := &fakeReplies{} // must NOT be used for a DM channel
	bf := &SlackBackfill{client: bot, limit: 200}
	bf.SetDMRepliesClient(user)

	n, err := bf.reconcile(context.Background(), slug, dm, root)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("appended = %d, want 1 (the missed reply)", n)
	}
	if bot.calls != 0 {
		t.Fatalf("DM channel must use the user client, not the bot client; bot.calls=%d", bot.calls)
	}
	if user.replyCalls == 0 || !user.gotThreadRoots[root] {
		t.Fatalf("expected user client Replies for root %s; replyCalls=%d roots=%v", root, user.replyCalls, user.gotThreadRoots)
	}
	if user.gotOldest != "1780489629.079919" {
		t.Fatalf("per-channel cursor = %q, want the DM's max ts", user.gotOldest)
	}
}

func TestSlackBackfill_ChannelThreadUsesBotClient(t *testing.T) {
	// Regression guard: a normal channel thread keeps using the bot-token
	// client and never touches the user client.
	t.Setenv("FLOW_ROOT", t.TempDir())
	const slug, ch, root = "slack-chan", "C_THREAD", "50.000000"
	seedInbox(t, slug, ch, root, "100.000000", "first")
	bot := &fakeReplies{msgs: []SlackMessage{{TS: "120.000000", User: "U2", Text: "newer"}}}
	user := &fakeHistory{}
	bf := &SlackBackfill{client: bot, limit: 200}
	bf.SetDMRepliesClient(user)

	n, err := bf.reconcile(context.Background(), slug, ch, root)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("appended = %d, want 1", n)
	}
	if bot.calls == 0 {
		t.Fatalf("channel thread must use the bot client")
	}
	if user.replyCalls != 0 {
		t.Fatalf("channel thread must not touch the user client; replyCalls=%d", user.replyCalls)
	}
}

func TestSlackBackfill_RunOnceReconcilesAllThreadTags(t *testing.T) {
	// A task carries its origin channel thread AND a DM thread (both as
	// slack-thread tags). runOnce must reconcile BOTH — origin via the bot
	// client, DM via the user client — not just the first tag.
	db := dispatcherTestDB(t)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES ('t','t','backlog','high',?, 'default','claude',?,?,?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"slack-reply", "slack-thread:C_ORIGIN:50.000000", "slack-thread:D_ALICE:1780480392.819809"} {
		if err := flowdb.AddTaskTag(db, "t", tag); err != nil {
			t.Fatal(err)
		}
	}
	seedInbox(t, "t", "C_ORIGIN", "50.000000", "100.000000", "origin baseline")
	if err := AppendInboxEvent("t", InboundEvent{
		Kind: "message", Channel: "D_ALICE", ChannelType: "im",
		TS: "1780489629.079919", ThreadTS: "1780480392.819809", UserID: "U_me", Text: "dm baseline",
	}); err != nil {
		t.Fatal(err)
	}

	bot := &fakeReplies{msgs: []SlackMessage{{TS: "120.000000", User: "U2", Text: "origin newer"}}}
	user := &fakeHistory{repliesByRoot: map[string][]SlackMessage{
		"1780480392.819809": {{TS: "1780491705.662279", ThreadTS: "1780480392.819809", User: "U_ishaan", Text: "dm newer"}},
	}}
	bf := NewSlackBackfill(db, bot, time.Hour)
	bf.SetDMRepliesClient(user)
	bf.runOnce(context.Background())

	entries, _ := ReadInboxEntries("t")
	var gotOrigin, gotDM bool
	for _, e := range entries {
		if e.Event.TS == "120.000000" {
			gotOrigin = true
		}
		if e.Event.TS == "1780491705.662279" {
			gotDM = true
		}
	}
	if !gotOrigin {
		t.Errorf("origin channel-thread reply not recovered")
	}
	if !gotDM {
		t.Errorf("DM-thread reply not recovered (multi-tag reconcile failed)")
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
