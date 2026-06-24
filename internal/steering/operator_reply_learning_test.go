package steering

import (
	"context"
	"testing"

	"flow/internal/monitor"
	"flow/internal/productdb"
)

// seedTriagedThread gives a thread prior decision state AND a surfaced card, so
// the learning gate passes and a calibration signal can reference the card.
func seedTriagedThread(t *testing.T, c *Cascade, threadKey, channel, suggested string, conf float64) {
	t.Helper()
	if _, _, err := productdb.UpsertFeedItemSurfaced(c.DB, productdb.FeedItem{
		ID: "card-" + threadKey, Source: "slack", ThreadKey: threadKey, Channel: channel,
		ChannelType: "channel", SuggestedAction: suggested, Confidence: conf, MatchedTask: "task-x",
		Status: "new", CreatedAt: "2026-06-12T06:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed card: %v", err)
	}
	if err := productdb.RecordThreadDecision(c.DB, productdb.ThreadDecision{
		ThreadKey: threadKey, Source: "slack", Action: suggested, Confidence: conf,
		Reason: "prior card", At: "2026-06-12T06:00:00Z",
	}); err != nil {
		t.Fatalf("seed prior decision: %v", err)
	}
}

func operatorReplyEvent(channel, threadTS, ts, text string) monitor.InboundEvent {
	return monitor.InboundEvent{
		Channel: channel, ChannelType: "channel", ThreadTS: threadTS, TS: ts,
		UserID: "U_ME", Text: text,
	}
}

// On a thread flow already triaged, an operator's hand-written reply is learned:
// recorded into thread memory, the card resolved, an operator action logged, a
// calibration feedback row emitted, and (live + substantive) KB capture attempted.
func TestOperatorReplyLearnsOnStatefulThread(t *testing.T) {
	c := newSteeringTestCascade(t)
	c.KBDir = "/tmp/flow/kb"
	prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })
	seedTriagedThread(t, c, "C1:1.1", "C1", "reply", 0.6)

	ev := operatorReplyEvent("C1", "1.1", "9.9", "We've decided to standardize on us-east-1 for all prod envs.")
	c.learnFromOperatorReply(context.Background(), ev, "live")

	s, ok, err := productdb.GetThreadState(c.DB, "C1:1.1")
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if len(s.OperatorReplies) != 1 || s.OperatorReplies[0].TS != "9.9" {
		t.Errorf("OperatorReplies = %+v, want one reply at ts 9.9", s.OperatorReplies)
	}
	if len(s.OperatorActions) != 1 || s.OperatorActions[0].Action != "operator_reply" ||
		s.OperatorActions[0].Outcome != "handled" || s.OperatorActions[0].LinkedTask != "task-x" {
		t.Errorf("OperatorActions = %+v, want one operator_reply/handled linked to task-x", s.OperatorActions)
	}
	card, err := productdb.GetFeedItem(c.DB, "card-C1:1.1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if card.Status != "acted" {
		t.Errorf("card status = %q, want acted (operator handled it)", card.Status)
	}
	fb, err := productdb.ListAttentionFeedback(c.DB, productdb.AttentionFeedbackFilter{})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].SuggestedAction != "reply" || fb[0].FinalAction != "operator_reply" ||
		fb[0].Outcome != productdb.OutcomeOperatorHandled {
		t.Fatalf("feedback = %+v, want one calibration row (reply→operator_reply/operator_handled)", fb)
	}
	if *prompt == "" {
		t.Error("KB capture runner was not invoked on a live substantive reply")
	}
}

// A self-authored message on a thread flow never triaged is still a plain drop —
// no thread-state row, no feedback, no KB call. This is the no-firehose guarantee.
func TestOperatorReplyDroppedOnNewThread(t *testing.T) {
	c := newSteeringTestCascade(t)
	c.KBDir = "/tmp/flow/kb"
	prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })

	ev := operatorReplyEvent("C9", "1.1", "1.1", "Some long message that would otherwise be substantive enough.")
	c.learnFromOperatorReply(context.Background(), ev, "live")

	if _, ok, err := productdb.GetThreadState(c.DB, "C9:1.1"); err != nil || ok {
		t.Errorf("thread state created for an untriaged thread (ok=%v err=%v) — firehose regression", ok, err)
	}
	fb, _ := productdb.ListAttentionFeedback(c.DB, productdb.AttentionFeedbackFilter{})
	if len(fb) != 0 {
		t.Errorf("feedback rows = %d, want 0 for an untriaged thread", len(fb))
	}
	if *prompt != "" {
		t.Error("KB capture invoked for an untriaged thread")
	}
}

// Backfill replay learns into memory but does NOT fire the expensive KB agent
// (avoids burst LLM spend on catch-up).
func TestOperatorReplyNoKBCaptureOnBackfill(t *testing.T) {
	c := newSteeringTestCascade(t)
	c.KBDir = "/tmp/flow/kb"
	prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })
	seedTriagedThread(t, c, "C1:2.2", "C1", "reply", 0.6)

	ev := operatorReplyEvent("C1", "2.2", "9.9", "Standardizing prod on us-east-1 going forward, please note.")
	c.learnFromOperatorReply(context.Background(), ev, "backfill")

	if *prompt != "" {
		t.Error("KB capture invoked on backfill origin")
	}
	s, _, _ := productdb.GetThreadState(c.DB, "C1:2.2")
	if len(s.OperatorReplies) != 1 {
		t.Errorf("OperatorReplies = %+v, want the reply still recorded on backfill", s.OperatorReplies)
	}
}

// Replaying the same reply (same ts) must not double-record it.
func TestOperatorReplyDedupSameTS(t *testing.T) {
	c := newSteeringTestCascade(t)
	stubCaptureKBRunner(t, func(string) (string, error) { return "NOTHING-DURABLE", nil })
	seedTriagedThread(t, c, "C1:3.3", "C1", "reply", 0.6)

	ev := operatorReplyEvent("C1", "3.3", "9.9", "I'll handle this one directly, thanks team.")
	c.learnFromOperatorReply(context.Background(), ev, "backfill")
	c.learnFromOperatorReply(context.Background(), ev, "backfill")

	s, _, _ := productdb.GetThreadState(c.DB, "C1:3.3")
	if len(s.OperatorReplies) != 1 {
		t.Errorf("OperatorReplies = %d, want 1 after a duplicate replay", len(s.OperatorReplies))
	}
	if len(s.OperatorActions) != 1 {
		t.Errorf("OperatorActions = %d, want 1 after a duplicate replay", len(s.OperatorActions))
	}
	fb, _ := productdb.ListAttentionFeedback(c.DB, productdb.AttentionFeedbackFilter{})
	if len(fb) != 1 {
		t.Errorf("feedback rows = %d, want 1 after a duplicate replay", len(fb))
	}
}

// A reply in one thread must not resolve an unrelated open card in the same
// busy channel.
func TestOperatorReplyNoChannelRecoveryInRegularChannel(t *testing.T) {
	c := newSteeringTestCascade(t)
	seedTriagedThread(t, c, "C1:1.1", "C1", "reply", 0.6) // channelType "channel"

	ev := operatorReplyEvent("C1", "9.9", "9.9", "Unrelated message in the same channel, a different thread.")
	c.learnFromOperatorReply(context.Background(), ev, "live")

	card, _ := productdb.GetFeedItem(c.DB, "card-C1:1.1")
	if card.Status != "new" {
		t.Errorf("card status = %q, want new — a regular-channel reply must not recover/resolve a different thread's card", card.Status)
	}
}

// A reply the AGENT itself sent (send_reply on the user token) echoes back through
// the socket as self-authored. It must NOT be re-learned as an operator hand-reply:
// no new operator_reply action, no operator_handled calibration row, no KB capture.
// The open card is still stood down.
func TestAgentSentReplyEchoNotRelearned(t *testing.T) {
	c := newSteeringTestCascade(t)
	c.KBDir = "/tmp/flow/kb"
	prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })
	seedTriagedThread(t, c, "C1:4.4", "C1", "reply", 0.6)

	// The agent posted this reply moments ago (recorded as a send_reply feedback row
	// with the draft text), within the echo window of the cascade's fixed clock.
	sent := "Thanks for the ping — the migration is scheduled for Friday."
	agentRow := productdb.AttentionFeedback{
		ID: "sent-1", FeedItemID: "card-C1:4.4", Source: "slack", Channel: "C1", ThreadType: "channel",
		ThreadKey: "C1:4.4", SuggestedAction: "reply", FinalAction: "send_reply", Outcome: "approved",
		Confidence: 0.6, DraftAfter: sent, CreatedAt: "2026-06-12T06:30:00Z",
	}
	if err := productdb.RecordAttentionFeedback(c.DB, agentRow); err != nil {
		t.Fatalf("seed agent send_reply row: %v", err)
	}

	ev := operatorReplyEvent("C1", "4.4", "9.9", sent) // same text echoes back
	c.learnFromOperatorReply(context.Background(), ev, "live")

	s, _, _ := productdb.GetThreadState(c.DB, "C1:4.4")
	if len(s.OperatorReplies) != 0 {
		t.Errorf("OperatorReplies = %+v, want 0 — agent echo must not be recorded as an operator reply", s.OperatorReplies)
	}
	for _, a := range s.OperatorActions {
		if a.Action == "operator_reply" {
			t.Errorf("recorded an operator_reply action for an agent-sent echo: %+v", s.OperatorActions)
		}
	}
	fb, _ := productdb.ListAttentionFeedback(c.DB, productdb.AttentionFeedbackFilter{})
	for _, row := range fb {
		if row.Outcome == productdb.OutcomeOperatorHandled {
			t.Errorf("emitted an operator_handled calibration row for an agent echo: %+v", row)
		}
	}
	if *prompt != "" {
		t.Error("KB capture invoked for an agent-sent echo")
	}
	// The card is still resolved (the send path or this stand-down handles it).
	card, _ := productdb.GetFeedItem(c.DB, "card-C1:4.4")
	if card.Status != "acted" {
		t.Errorf("card status = %q, want acted", card.Status)
	}
}
