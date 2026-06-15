package steering

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// A single surfaced verdict seeds the thread's running understanding under the
// card's thread_key.
func TestWriteFeedRecordsThreadDecision(t *testing.T) {
	c := newClubTestCascade(t)
	v := Verdict{Source: "slack", ThreadKey: "C9:1.1", SuggestedAction: ActionReply, Confidence: 0.8, Reason: "customer q", Summary: "asks about pricing"}
	ev := monitor.InboundEvent{Channel: "C9", ChannelType: "channel", UserID: "U1", TS: "1.1", ThreadTS: "1.1"}
	if _, _, err := c.writeFeed(context.Background(), v, ev, ThreadContext{}); err != nil {
		t.Fatalf("writeFeed: %v", err)
	}
	s, ok, err := flowdb.GetThreadState(c.DB, "C9:1.1")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if s.EventCount != 1 || s.CurrentAction != "reply" || s.Summary != "asks about pricing" {
		t.Errorf("state = %+v, want 1 event / reply / pricing summary", s)
	}
}

// The keystone clubbed-rewrite case: two standalone DM messages within the
// conversation gap club into ONE card; their thread state must accumulate under
// the SAME anchor key (proving state survives the clubbed thread_key rewrite),
// not fragment into a row per message.
func TestThreadStateSurvivesClubbedRewrite(t *testing.T) {
	c := newClubTestCascade(t)
	c.MatchConversation = func(_ context.Context, _ ClubMessage, _ []ClubCandidate) (string, string, error) {
		t.Fatal("matcher must not be called for a 1:1 DM")
		return "", "", nil
	}

	first := Verdict{Source: "slack", ThreadKey: "D7:1000.0", SuggestedAction: ActionReply, Summary: "why does PR #2101 fail?"}
	ev1 := monitor.InboundEvent{Channel: "D7", ChannelType: "im", UserID: "U1", TS: "1000.0"}
	if _, surf, err := c.writeFeed(context.Background(), first, ev1, ThreadContext{}); err != nil || !surf {
		t.Fatalf("first writeFeed: surfaced=%v err=%v", surf, err)
	}

	// 100s later, within the 30-min gap → clubbed onto the first card's key.
	second := Verdict{Source: "slack", ThreadKey: "D7:1100.0", SuggestedAction: ActionForward, Summary: "lgtm"}
	ev2 := monitor.InboundEvent{Channel: "D7", ChannelType: "im", UserID: "U1", TS: "1100.0"}
	if _, surf, err := c.writeFeed(context.Background(), second, ev2, ThreadContext{}); err != nil || !surf {
		t.Fatalf("second writeFeed: surfaced=%v err=%v", surf, err)
	}

	if n := openCardCount(t, c); n != 1 {
		t.Fatalf("open cards = %d, want 1 (DM burst clubbed)", n)
	}
	// State under the anchor key accumulated both events; the standalone key for
	// the second message must NOT have its own row.
	anchor, ok, err := flowdb.GetThreadState(c.DB, "D7:1000.0")
	if err != nil || !ok {
		t.Fatalf("GetThreadState(anchor) ok=%v err=%v", ok, err)
	}
	if anchor.EventCount != 2 {
		t.Errorf("anchor EventCount = %d, want 2 (state survived club rewrite)", anchor.EventCount)
	}
	if _, ok, _ := flowdb.GetThreadState(c.DB, "D7:1100.0"); ok {
		t.Errorf("a separate state row exists for the clubbed-away key D7:1100.0; want none")
	}
}

// End-to-end through Observe: two events on one thread accumulate state, and the
// read-back-before-triage path runs without disturbing the verdict.
func TestCascadeObserveAccumulatesThreadState(t *testing.T) {
	c, db := cascadeFixture(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return stage1JSONForPrompt(prompt, true, "relevant"), nil
		}
		return `{"suggested_action":"reply","confidence":0.7,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"customer q","draft":"On it."}`, nil
	})

	// Parent message, then a reply on the same thread (distinct event ts so the
	// verdict cache doesn't suppress it; same thread_key C1:1.1).
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "need help")); err != nil {
		t.Fatalf("observe 1: %v", err)
	}
	reply := monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C1", TS: "2.2", ThreadTS: "1.1", UserID: "U_OTHER", Text: "still stuck"}
	if err := c.Observe(context.Background(), reply); err != nil {
		t.Fatalf("observe 2: %v", err)
	}

	s, ok, err := flowdb.GetThreadState(db, "C1:1.1")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if s.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2 (two events on one thread)", s.EventCount)
	}
	if s.CurrentAction != "reply" {
		t.Errorf("CurrentAction = %q, want reply", s.CurrentAction)
	}
}

// Replaying multiple events on one thread feeds each subsequent deep-triage the
// PRIOR decision (incremental context), and an incremental model that honors it
// produces a STABLE decision instead of flip-flopping. Also asserts the cascade
// degrades gracefully when layer-3 retrieval errors.
func TestIncrementalReplayStableDecision(t *testing.T) {
	c, db := cascadeFixture(t)
	t.Cleanup(func() { retrievalSearch = flowdb.SearchDocsMatch })
	// Layer 3 errors on every call — triage must still complete (degrades to nil).
	retrievalSearch = func(*sql.DB, string, []flowdb.SearchScope, int) ([]flowdb.SearchResult, error) {
		return nil, errors.New("index cold")
	}
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return stage1JSONForPrompt(prompt, true, "relevant"), nil
		}
		return `{"suggested_action":"forward","confidence":0.6,"summary":"q"}`, nil
	})

	var deepPrompts []string
	stubDeepTriage(t, func(prompt string) (string, error) {
		deepPrompts = append(deepPrompts, prompt)
		// An incremental model: if told a prior decision, hold it; else seed "forward".
		action := "forward"
		if strings.Contains(prompt, "Prior running understanding (JSON)") {
			action = priorActionFromPrompt(prompt)
		}
		return `{"suggested_action":"` + action + `","confidence":0.9,"summary":"customer q"}`, nil
	})

	// Three events on one thread (C1:1.1), distinct event ts so the verdict cache
	// doesn't suppress them.
	events := []monitor.InboundEvent{
		{Kind: "message", ChannelType: "channel", Channel: "C1", TS: "1.1", ThreadTS: "1.1", UserID: "U_OTHER", Text: "can you look at the oauth rollout?"},
		{Kind: "message", ChannelType: "channel", Channel: "C1", TS: "2.2", ThreadTS: "1.1", UserID: "U_OTHER", Text: "any update?"},
		{Kind: "message", ChannelType: "channel", Channel: "C1", TS: "3.3", ThreadTS: "1.1", UserID: "U_OTHER", Text: "bump"},
	}
	for i, ev := range events {
		if err := c.Observe(context.Background(), ev); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}

	if len(deepPrompts) != 3 {
		t.Fatalf("deep triage ran %d times, want 3", len(deepPrompts))
	}
	// Events 2 and 3 must have carried the prior decision into the prompt.
	for i := 1; i < 3; i++ {
		if !strings.Contains(deepPrompts[i], "Prior running understanding (JSON)") ||
			!strings.Contains(deepPrompts[i], "INCREMENTAL UPDATE") {
			t.Fatalf("deep prompt %d missing incremental prior context:\n%s", i, deepPrompts[i])
		}
	}
	// The decision stayed stable across the replay (no flip-flop).
	s, ok, err := flowdb.GetThreadState(db, "C1:1.1")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if s.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", s.EventCount)
	}
	if s.CurrentAction != "forward" {
		t.Errorf("CurrentAction = %q, want stable forward across replay", s.CurrentAction)
	}
}

// priorActionFromPrompt extracts the prior suggested_action the incremental
// prompt carried, so the test's stub model can "hold" it.
func priorActionFromPrompt(prompt string) string {
	marker := "Prior running understanding (JSON)"
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		return "forward"
	}
	tail := prompt[idx:]
	key := `"action":"`
	a := strings.Index(tail, key)
	if a < 0 {
		return "forward"
	}
	a += len(key)
	end := strings.IndexByte(tail[a:], '"')
	if end < 0 {
		return "forward"
	}
	return tail[a : a+end]
}

// An intentional operator resolution (dismiss) records onto the thread's
// running understanding via the recordActionFeedback chokepoint.
func TestOperatorActionRecordedOnDismiss(t *testing.T) {
	c := newClubTestCascade(t)
	item := flowdb.FeedItem{
		ID: "f1", Source: "slack", ThreadKey: "C1:5.5", SuggestedAction: "reply",
		Status: "new", CreatedAt: "2026-06-12T06:40:00Z",
	}
	if _, _, err := flowdb.UpsertFeedItemSurfaced(c.DB, item); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	if err := DismissFeed(c.DB, "f1"); err != nil {
		t.Fatalf("DismissFeed: %v", err)
	}
	s, ok, err := flowdb.GetThreadState(c.DB, "C1:5.5")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if len(s.OperatorActions) != 1 || s.OperatorActions[0].Action != "dismiss" || s.OperatorActions[0].Outcome != "dismissed" {
		t.Errorf("OperatorActions = %+v, want one dismiss/dismissed", s.OperatorActions)
	}
}

// An operator's own reply on a thread flow ALREADY triaged fills the
// operator-reply slot (Stage 0 still drops the event; the learning path persists
// the slot). The gate requires prior decision state — see the operator-reply
// learning tests for the unwatched/new-thread drop case.
func TestOperatorReplyRecorded(t *testing.T) {
	c := newClubTestCascade(t)
	if err := flowdb.RecordThreadDecision(c.DB, flowdb.ThreadDecision{
		ThreadKey: "C1:1.1", Source: "slack", Action: "reply", Confidence: 0.7,
		Reason: "prior card", At: "2026-06-12T06:40:00Z",
	}); err != nil {
		t.Fatalf("seed prior decision: %v", err)
	}
	ev := monitor.InboundEvent{Channel: "C1", ChannelType: "channel", ThreadTS: "1.1", TS: "1.1", UserID: "U_ME", Text: "I'll take it from here"}
	c.learnFromOperatorReply(context.Background(), ev, "backfill")
	s, ok, err := flowdb.GetThreadState(c.DB, "C1:1.1")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if len(s.OperatorReplies) != 1 || s.OperatorReplies[0].Author != "U_ME" || s.OperatorReplies[0].Text != "I'll take it from here" {
		t.Errorf("OperatorReplies = %+v, want one reply from U_ME", s.OperatorReplies)
	}
}
