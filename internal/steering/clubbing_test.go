package steering

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// newClubTestCascade builds a Cascade backed by a real temp SQLite DB with
// deterministic ids and a fixed clock, for exercising writeFeed clubbing.
func newClubTestCascade(t *testing.T) *Cascade {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	c := NewCascade(db, WatchConfig{})
	var n int
	c.newID = func() string { n++; return fmt.Sprintf("id%d", n) }
	c.now = func() time.Time { return time.Date(2026, 6, 12, 6, 40, 0, 0, time.UTC) }
	return c
}

func openCardCount(t *testing.T, c *Cascade) int {
	t.Helper()
	items, err := flowdb.ListFeedItems(c.DB, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	return len(items)
}

// Consecutive 1:1 DM messages within the conversation gap collapse into ONE
// card deterministically — by time + participant, NOT the LLM matcher (the
// matcher must never be consulted for a DM, since context-free per-message
// summaries can't be linked semantically).
func TestWriteFeedClubsDMByTimeGap(t *testing.T) {
	c := newClubTestCascade(t)
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		t.Fatal("matcher must not be called for a 1:1 DM")
		return "", "", nil
	}

	first := Verdict{Source: "slack", ThreadKey: "D1:1000.0", SuggestedAction: ActionReply, Summary: "why does PR #2101 fail?"}
	ev1 := monitor.InboundEvent{Channel: "D1", ChannelType: "im", UserID: "U1", TS: "1000.0"}
	id1, surf1, err := c.writeFeed(context.Background(), first, ev1, ThreadContext{})
	if err != nil || !surf1 {
		t.Fatalf("first writeFeed: id=%q surfaced=%v err=%v", id1, surf1, err)
	}

	// 100s later — well within the 30-min gap — a terse fragment.
	second := Verdict{Source: "slack", ThreadKey: "D1:1100.0", SuggestedAction: ActionReply, Summary: "lgtm"}
	ev2 := monitor.InboundEvent{Channel: "D1", ChannelType: "im", UserID: "U1", TS: "1100.0"}
	id2, surf2, err := c.writeFeed(context.Background(), second, ev2, ThreadContext{})
	if err != nil || !surf2 {
		t.Fatalf("second writeFeed: id=%q surfaced=%v err=%v", id2, surf2, err)
	}

	if id2 != id1 {
		t.Errorf("clubbed card id = %q, want coalesced onto %q", id2, id1)
	}
	if n := openCardCount(t, c); n != 1 {
		t.Fatalf("open cards = %d, want 1 (DM burst clubbed)", n)
	}
	// The framing summary is preserved, not degraded to the "lgtm" fragment.
	got, err := flowdb.GetFeedItem(c.DB, id1)
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Summary != "why does PR #2101 fail?" {
		t.Errorf("clubbed summary = %q, want the preserved framing summary", got.Summary)
	}
}

// A DM message after a long pause (beyond the conversation gap) starts a NEW
// card — a fresh conversation, not a continuation of the earlier burst.
func TestWriteFeedDMNewConversationAfterGap(t *testing.T) {
	c := newClubTestCascade(t)
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		t.Fatal("matcher must not be called for a 1:1 DM")
		return "", "", nil
	}
	// 2000s apart > 1800s gap → two separate conversations.
	for _, ts := range []string{"1000.0", "3000.0"} {
		v := Verdict{Source: "slack", ThreadKey: "D1:" + ts, SuggestedAction: ActionReply, Summary: "msg " + ts}
		ev := monitor.InboundEvent{Channel: "D1", ChannelType: "im", UserID: "U1", TS: ts}
		if _, _, err := c.writeFeed(context.Background(), v, ev, ThreadContext{}); err != nil {
			t.Fatalf("writeFeed %s: %v", ts, err)
		}
	}
	if n := openCardCount(t, c); n != 2 {
		t.Fatalf("open cards = %d, want 2 (gap exceeded → new conversation)", n)
	}
}

// In a multi-person channel the LLM matcher decides clubbing. A match collapses
// two top-level posts into one card using the matcher's combined summary.
func TestWriteFeedClubsChannelViaMatcher(t *testing.T) {
	c := newClubTestCascade(t)
	var calls int
	c.MatchConversation = func(_ context.Context, _ ClubMessage, cands []ClubCandidate) (string, string, error) {
		calls++
		return cands[0].ThreadKey, "combined channel topic", nil
	}

	first := Verdict{Source: "slack", ThreadKey: "C1:1000.0", SuggestedAction: ActionReply, Summary: "rollout question"}
	ev1 := monitor.InboundEvent{Channel: "C1", ChannelType: "channel", UserID: "U1", TS: "1000.0"}
	id1, _, err := c.writeFeed(context.Background(), first, ev1, ThreadContext{})
	if err != nil {
		t.Fatalf("first writeFeed: %v", err)
	}
	second := Verdict{Source: "slack", ThreadKey: "C1:1100.0", SuggestedAction: ActionReply, Summary: "follow-up"}
	ev2 := monitor.InboundEvent{Channel: "C1", ChannelType: "channel", UserID: "U2", TS: "1100.0"}
	id2, _, err := c.writeFeed(context.Background(), second, ev2, ThreadContext{})
	if err != nil {
		t.Fatalf("second writeFeed: %v", err)
	}
	if id2 != id1 {
		t.Errorf("clubbed id = %q, want %q", id2, id1)
	}
	if n := openCardCount(t, c); n != 1 {
		t.Fatalf("open cards = %d, want 1 (channel clubbed via matcher)", n)
	}
	if calls != 1 {
		t.Errorf("matcher calls = %d, want 1", calls)
	}
	if got, _ := flowdb.GetFeedItem(c.DB, id1); got.Summary != "combined channel topic" {
		t.Errorf("summary = %q, want combined", got.Summary)
	}
}

// A genuine threaded reply (thread_key != channel:ts) already coalesces via its
// thread_key — clubbing must NOT run for it (no matcher call, no cross-thread
// merge risk).
func TestWriteFeedSkipsClubbingForThreadedReply(t *testing.T) {
	c := newClubTestCascade(t)
	var calls int
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		calls++
		return "", "", nil
	}
	// thread_key anchored to parent 100.1, but this reply's own ts is 250.1.
	v := Verdict{Source: "slack", ThreadKey: "C1:100.1", SuggestedAction: ActionReply, Summary: "reply"}
	ev := monitor.InboundEvent{Channel: "C1", ChannelType: "channel", UserID: "U1", TS: "250.1"}
	if _, _, err := c.writeFeed(context.Background(), v, ev, ThreadContext{}); err != nil {
		t.Fatalf("writeFeed: %v", err)
	}
	if calls != 0 {
		t.Errorf("matcher calls = %d, want 0 (threaded reply must skip clubbing)", calls)
	}
}

// A matcher error (channel path) must fail open: surface a fresh card rather
// than dropping it.
func TestWriteFeedClubbingFailsOpen(t *testing.T) {
	c := newClubTestCascade(t)
	// Seed one open card so there IS a candidate, forcing the matcher to run.
	seed := Verdict{Source: "slack", ThreadKey: "C1:100.1", SuggestedAction: ActionReply, Summary: "seed"}
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		return "", "", nil
	}
	if _, _, err := c.writeFeed(context.Background(), seed, monitor.InboundEvent{Channel: "C1", ChannelType: "channel", UserID: "U1", TS: "100.1"}, ThreadContext{}); err != nil {
		t.Fatalf("seed writeFeed: %v", err)
	}
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		return "", "", fmt.Errorf("classifier boom")
	}
	v := Verdict{Source: "slack", ThreadKey: "C1:200.1", SuggestedAction: ActionReply, Summary: "second"}
	ev := monitor.InboundEvent{Channel: "C1", ChannelType: "channel", UserID: "U1", TS: "200.1"}
	id, surf, err := c.writeFeed(context.Background(), v, ev, ThreadContext{})
	if err != nil || !surf || id == "" {
		t.Fatalf("fail-open writeFeed: id=%q surfaced=%v err=%v", id, surf, err)
	}
	if n := openCardCount(t, c); n != 2 {
		t.Fatalf("open cards = %d, want 2 (matcher error → fresh card, not a drop)", n)
	}
}

// DedupeOpenFeedConversations collapses already-surfaced DM cards from one
// burst (within the conversation gap) into a single anchor, dismissing the
// rest, while a message after a long pause stays its own card. This mirrors the
// real data: a PR thread fragmented across several minutes + an unrelated
// follow-up ~an hour later. The matcher is NEVER consulted for DMs.
func TestDedupeOpenFeedConversationsDMByGap(t *testing.T) {
	c := newClubTestCascade(t)
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		t.Fatal("matcher must not be called for a 1:1 DM")
		return "", "", nil
	}

	seed := func(id, channel, channelType, ts, summary, createdAt string) {
		t.Helper()
		if _, err := flowdb.UpsertFeedItem(c.DB, flowdb.FeedItem{
			ID: id, Source: "slack", ThreadKey: channel + ":" + ts, SuggestedAction: "reply",
			Summary: summary, Channel: channel, ChannelType: channelType, Author: "U1", TS: ts,
			Status: "new", CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// PR burst: ts 1000/1100/1200 (≤100s apart). Unrelated: ts 5000 (3800s after
	// c3 > 1800s gap → separate). Lone card in another channel: left alone.
	seed("c1", "D1", "im", "1000.0", "why does PR #2101 fail?", "2026-06-12T10:00:00Z")
	seed("c2", "D1", "im", "1100.0", "i think you were setting this var", "2026-06-12T10:01:40Z")
	seed("c3", "D1", "im", "1200.0", "lgtm", "2026-06-12T10:03:20Z")
	seed("distinct", "D1", "im", "5000.0", "pending on coinswitch team now", "2026-06-12T11:03:20Z")
	seed("other", "D2", "im", "1000.0", "different DM", "2026-06-12T10:06:00Z")

	res, err := c.DedupeOpenFeedConversations(context.Background())
	if err != nil {
		t.Fatalf("DedupeOpenFeedConversations: %v", err)
	}
	if res.Merged != 2 {
		t.Errorf("Merged = %d, want 2 (c2,c3 collapsed into c1)", res.Merged)
	}

	open, err := flowdb.ListFeedItems(c.DB, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	openByID := map[string]flowdb.FeedItem{}
	for _, it := range open {
		openByID[it.ID] = it
	}
	for _, id := range []string{"c1", "distinct", "other"} {
		if _, ok := openByID[id]; !ok {
			t.Errorf("card %q should still be open, open=%v", id, feedIDsStr(open))
		}
	}
	for _, id := range []string{"c2", "c3"} {
		if _, ok := openByID[id]; ok {
			t.Errorf("card %q should have been dismissed", id)
		}
	}
	// Anchor keeps the framing summary, not the "lgtm" fragment.
	if a := openByID["c1"]; a.Summary != "why does PR #2101 fail?" {
		t.Errorf("anchor c1 summary = %q, want the preserved framing summary", a.Summary)
	}
}

// In a multi-person channel the dedupe pass uses the LLM matcher to cluster.
func TestDedupeOpenFeedConversationsChannelViaMatcher(t *testing.T) {
	c := newClubTestCascade(t)
	// Cluster by the first word of the summary.
	c.MatchConversation = func(_ context.Context, msg ClubMessage, cands []ClubCandidate) (string, string, error) {
		first := func(s string) string { return strings.Fields(s + " ")[0] }
		for _, cd := range cands {
			if first(cd.Summary) == first(msg.Text) && first(msg.Text) != "" {
				return cd.ThreadKey, "kong combined thread", nil
			}
		}
		return "", "", nil
	}
	seed := func(id, ts, summary string) {
		t.Helper()
		if _, err := flowdb.UpsertFeedItem(c.DB, flowdb.FeedItem{
			ID: id, Source: "slack", ThreadKey: "C1:" + ts, SuggestedAction: "reply",
			Summary: summary, Channel: "C1", ChannelType: "channel", Author: "U1", TS: ts,
			Status: "new", CreatedAt: "2026-06-12T10:0" + ts[0:1] + ":00Z",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("k1", "1.0", "kong rollout")
	seed("k2", "2.0", "kong split timing")
	seed("k3", "3.0", "unrelated deploy note")

	res, err := c.DedupeOpenFeedConversations(context.Background())
	if err != nil {
		t.Fatalf("DedupeOpenFeedConversations: %v", err)
	}
	if res.Merged != 1 {
		t.Errorf("Merged = %d, want 1 (k2 into k1; k3 distinct)", res.Merged)
	}
	open, _ := flowdb.ListFeedItems(c.DB, "new")
	if len(open) != 2 {
		t.Fatalf("open = %v, want 2 (k1 anchor + k3)", feedIDsStr(open))
	}
}

func feedIDsStr(items []flowdb.FeedItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestFeedTSWithinGap(t *testing.T) {
	gap := 30 * time.Minute
	tests := []struct {
		a, b string
		want bool
	}{
		{"1100.0", "1000.0", true},      // 100s apart
		{"1000.0", "1100.0", true},      // order-independent
		{"3000.0", "1000.0", false},     // 2000s > 1800s
		{"2800.0", "1000.0", true},      // exactly 1800s → within
		{"1000.0", "", false},           // unparseable → not within (fail safe)
		{"notanumber", "1000.0", false}, // unparseable → not within
	}
	for _, tt := range tests {
		if got := feedTSWithinGap(tt.a, tt.b, gap); got != tt.want {
			t.Errorf("feedTSWithinGap(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseConversationMatch(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		n           int
		wantIdx     int
		wantSummary string
		wantErr     bool
	}{
		{name: "match with summary", raw: `{"match":1,"summary":"combined topic"}`, n: 3, wantIdx: 1, wantSummary: "combined topic"},
		{name: "explicit new", raw: `{"match":-1,"summary":""}`, n: 3, wantIdx: -1},
		{name: "out of range is new", raw: `{"match":5,"summary":"x"}`, n: 3, wantIdx: -1},
		{name: "negative other than -1 is new", raw: `{"match":-7}`, n: 3, wantIdx: -1},
		{name: "tolerates code fence and prose", raw: "sure!\n```json\n{\"match\":0,\"summary\":\"hi\"}\n```", n: 2, wantIdx: 0, wantSummary: "hi"},
		{name: "missing match field is new", raw: `{"summary":"x"}`, n: 2, wantIdx: -1},
		{name: "no json errors", raw: `I could not decide`, n: 2, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, summary, err := parseConversationMatch(tt.raw, tt.n)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseConversationMatch(%q) err = nil, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConversationMatch(%q): %v", tt.raw, err)
			}
			if idx != tt.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tt.wantIdx)
			}
			if summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tt.wantSummary)
			}
		})
	}
}
