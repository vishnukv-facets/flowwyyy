package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flow/internal/flowdb"
)

// fakeChatSink records OpenOrContinueChat calls so tests can assert the
// dispatcher routed an authorized command-channel DM into the chat sink.
type fakeChatSink struct {
	calls  []struct{ Channel, Text string }
	err    error
	onCall func()
}

func (f *fakeChatSink) OpenOrContinueChat(_ context.Context, channel, text string) error {
	if f.onCall != nil {
		f.onCall()
	}
	f.calls = append(f.calls, struct{ Channel, Text string }{channel, text})
	return f.err
}

type slackAPIRequest struct {
	Path string
	Body string
}

func withSlackAPIStub(t *testing.T) (*[]slackAPIRequest, func()) {
	t.Helper()
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_WRITE_TOKEN", "xoxb-test")
	var (
		mu       sync.Mutex
		requests []slackAPIRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, slackAPIRequest{Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Setenv("FLOW_SLACK_API_BASE_URL", srv.URL)
	return &requests, func() {
		srv.Close()
	}
}

// withCommandChannel stubs conversationIsBotIMFn so IsCommandChannel reports
// true for the given IM channel (the bot is a member), and restores it (and the
// cache) on cleanup.
func withCommandChannel(t *testing.T, channel string) {
	t.Helper()
	orig := conversationIsBotIMFn
	conversationIsBotIMFn = func(ch string) bool { return ch == channel }
	resetCommandChannelCache()
	t.Cleanup(func() {
		conversationIsBotIMFn = orig
		resetCommandChannelCache()
	})
}

func TestDispatcher_CommandChannelDM_RoutesToChatSink(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")
	slackRequests, cleanupSlack := withSlackAPIStub(t)
	defer cleanupSlack()

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	reactionsAtSink := -1
	sink.onCall = func() { reactionsAtSink = len(*slackRequests) }
	d.ChatSink = sink
	// Wire a steerer too, to prove the command short-circuit fires BEFORE it.
	steerer := &fakeSteerer{}
	d.Steerer = steerer
	d.SteererOwnsRouting = func() bool { return true }

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", TS: "1710000000.000100", ThreadTS: "1710000000.000100", UserID: "U_me", Text: "what's on my plate?"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reactionsAtSink != 1 {
		t.Fatalf("eyes reaction should be sent before the chat sink starts; reactions at sink = %d", reactionsAtSink)
	}
	if len(*slackRequests) != 1 {
		t.Fatalf("expected one Slack reaction request, got %d: %+v", len(*slackRequests), *slackRequests)
	}
	req := (*slackRequests)[0]
	if req.Path != "/reactions.add" {
		t.Fatalf("Slack path = %q, want /reactions.add", req.Path)
	}
	for _, want := range []string{`"channel":"D_bot"`, `"timestamp":"1710000000.000100"`, `"name":"eyes"`} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("reaction body missing %s: %s", want, req.Body)
		}
	}
	if len(sink.calls) != 1 {
		t.Fatalf("expected 1 chat-sink call, got %d", len(sink.calls))
	}
	if sink.calls[0].Channel != "D_bot" || sink.calls[0].Text != "what's on my plate?" {
		t.Errorf("chat sink got channel=%q text=%q", sink.calls[0].Channel, sink.calls[0].Text)
	}
	if len(steerer.events) != 0 {
		t.Errorf("command-channel DM must NOT reach the steerer, got %d events", len(steerer.events))
	}
}

func TestDispatcher_CommandChannelThreadReplyRoutesToTrackedTask(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	seedSlackTask(t, db, "asking-task", "D_bot:1700000000.000100")
	if _, err := flowdb.SetTaskWaitingOnIfClear(db, "asking-task", "operator question: choose deployment window"); err != nil {
		t.Fatalf("set waiting_on: %v", err)
	}
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", TS: "1700000000.000200", ThreadTS: "1700000000.000100", UserID: "U_me", Text: "Use option B"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("tracked command-DM thread reply must not open chat sink, got %+v", sink.calls)
	}
	entries, err := ReadInboxEntries("asking-task")
	if err != nil {
		t.Fatalf("ReadInboxEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	if entries[0].Event.Text != "Use option B" || entries[0].Event.Channel != "D_bot" {
		t.Fatalf("inbox event = %+v", entries[0].Event)
	}
	task, err := flowdb.GetTask(db, "asking-task")
	if err != nil {
		t.Fatal(err)
	}
	if task.WaitingOn.Valid {
		t.Fatalf("waiting_on should be cleared after operator answer, got %q", task.WaitingOn.String)
	}
}

func TestDispatcher_CommandChannelAskRootEchoDoesNotClearWaiting(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	seedSlackTask(t, db, "asking-task", "D_bot:1700000000.000100")
	if _, err := flowdb.SetTaskWaitingOnIfClear(db, "asking-task", "operator question: choose deployment window"); err != nil {
		t.Fatalf("set waiting_on: %v", err)
	}
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", TS: "1700000000.000100", ThreadTS: "1700000000.000100", UserID: "U_me", Text: "Task `asking-task` needs your input"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("ask root echo must not open chat sink, got %+v", sink.calls)
	}
	entries, err := ReadInboxEntries("asking-task")
	if err != nil {
		t.Fatalf("ReadInboxEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ask root echo must not be appended as an answer, got %+v", entries)
	}
	task, err := flowdb.GetTask(db, "asking-task")
	if err != nil {
		t.Fatal(err)
	}
	if !task.WaitingOn.Valid {
		t.Fatalf("waiting_on should remain set after ask root echo")
	}
}

func TestDispatcher_CommandChannelDisabledTrackedThreadReplyStillRoutes(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")
	sends := withSendAsBotStub(t)
	clearHintedDMChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	seedSlackTask(t, db, "asking-task", "D_bot:1700000000.000100")
	d := NewDispatcher(db, nil)
	d.ChatSink = &fakeChatSink{}

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", TS: "1700000000.000200", ThreadTS: "1700000000.000100", UserID: "U_me", Text: "Use option B"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(*sends) != 0 {
		t.Fatalf("tracked reply should not get command-channel disabled hint, got %+v", *sends)
	}
	entries, err := ReadInboxEntries("asking-task")
	if err != nil {
		t.Fatalf("ReadInboxEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Event.Text != "Use option B" {
		t.Fatalf("inbox entries = %+v", entries)
	}
}

func TestDispatcher_CommandChannelEyesAckOnlyForOperatorBotDM(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")
	slackRequests, cleanupSlack := withSlackAPIStub(t)
	defer cleanupSlack()
	_ = withSendAsBotStub(t) // unauthorized bot DMs may send a static reject; do not hit Slack.
	clearRejectedDMChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	d.ChatSink = &fakeChatSink{}
	d.Steerer = &fakeSteerer{}

	cases := []InboundEvent{
		{Kind: "message", ChannelType: "im", Channel: "D_bot", TS: "1.1", ThreadTS: "1.1", UserID: "U_other", Text: "let me in"},
		{Kind: "message", ChannelType: "im", Channel: "D_colleague", TS: "2.1", ThreadTS: "2.1", UserID: "U_me", Text: "colleague DM"},
		{Kind: "message", ChannelType: "channel", Channel: "C_general", TS: "3.1", ThreadTS: "3.1", UserID: "U_me", Text: "operator in channel"},
	}
	for _, ev := range cases {
		if err := d.Dispatch(context.Background(), ev); err != nil {
			t.Fatalf("Dispatch(%+v): %v", ev, err)
		}
	}
	if len(*slackRequests) != 0 {
		t.Fatalf("non-command/operator cases must not get eyes reactions, got %+v", *slackRequests)
	}
}

// withSendAsBotStub swaps the package-level sendAsBotFn for a recorder and
// enables Slack writes for the test's duration, restoring both on cleanup.
// Returns a pointer to the recorded (channel,text) sends.
func withSendAsBotStub(t *testing.T) *[]struct{ Channel, Text string } {
	t.Helper()
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	var sends []struct{ Channel, Text string }
	orig := sendAsBotFn
	sendAsBotFn = func(channel, threadTS, text, identity string) error {
		sends = append(sends, struct{ Channel, Text string }{channel, text})
		return nil
	}
	t.Cleanup(func() { sendAsBotFn = orig })
	return &sends
}

// clearRejectedDMChannel removes a channel from the per-channel reject-dedup map
// so a test starts from a clean slate (the map is a process-global sync.Map).
func clearRejectedDMChannel(t *testing.T, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		rejectedDMChannels.Delete(ch)
	}
	t.Cleanup(func() {
		for _, ch := range channels {
			rejectedDMChannels.Delete(ch)
		}
	})
}

// clearHintedDMChannel removes a channel from the per-channel off-state-hint
// dedup map so a test starts from a clean slate (process-global sync.Map).
func clearHintedDMChannel(t *testing.T, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		hintedDMChannels.Delete(ch)
	}
	t.Cleanup(func() {
		for _, ch := range channels {
			hintedDMChannels.Delete(ch)
		}
	})
}

func TestDispatcher_CommandChannelDM_UnauthorizedRejectedOnce(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_stranger")
	sends := withSendAsBotStub(t)
	clearRejectedDMChannel(t, "D_stranger")

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	steerer := &fakeSteerer{}
	d.Steerer = steerer

	// A stranger DMs the bot (bot is a member, but author is not the operator).
	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_stranger", UserID: "U_other", Text: "let me in"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// The stranger sends again — the reject must NOT fire a second time.
	msg2 := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_stranger", UserID: "U_other", Text: "really, let me in"}
	if err := d.Dispatch(context.Background(), msg2); err != nil {
		t.Fatalf("Dispatch (second): %v", err)
	}

	if len(sink.calls) != 0 {
		t.Errorf("unauthorized DM must NOT reach the chat sink, got %d calls", len(sink.calls))
	}
	if len(steerer.events) != 0 {
		t.Errorf("unauthorized bot DM must NOT reach the steerer, got %d events", len(steerer.events))
	}
	if len(*sends) != 1 {
		t.Fatalf("expected exactly ONE reject reply (per-channel dedup), got %d: %+v", len(*sends), *sends)
	}
	if (*sends)[0].Channel != "D_stranger" || (*sends)[0].Text != unauthorizedDMReply {
		t.Errorf("reject reply wrong: channel=%q text=%q", (*sends)[0].Channel, (*sends)[0].Text)
	}
}

// TestDispatcher_SelfAuthoredBotMessage_Dropped is the regression test for the
// live bug where flow processed its OWN bot output. flow posts an ack ("On it
// — working on this now") into the command DM as its bot; Slack echoes that
// message back to the listener as an inbound event authored by the bot's user
// id. Before the fix this (1) hit the command-channel default branch — the bot
// isn't the operator — and fired the "I'm a personal flow assistant…" reject AT
// THE OPERATOR, and (2) when it didn't reject, reached the steerer and surfaced
// a bogus "Participant acknowledged a request" attention card the AFK operator
// couldn't action. The fix drops self-authored messages at the Dispatch
// chokepoint: no reject, no chat-sink call, no steerer event.
func TestDispatcher_SelfAuthoredBotMessage_Dropped(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")
	sends := withSendAsBotStub(t)
	clearRejectedDMChannel(t, "D_bot")

	// flow's own bot resolves to U_bot.
	origSelf := selfBotUserIDFn
	selfBotUserIDFn = func() string { return "U_bot" }
	resetCommandChannelCache()
	t.Cleanup(func() { selfBotUserIDFn = origSelf; resetCommandChannelCache() })

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	steerer := &fakeSteerer{}
	d.Steerer = steerer
	d.SteererOwnsRouting = func() bool { return true }

	// The bot's own ack echoes back into the command DM, authored by the bot.
	echo := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_bot", Text: "⏳ On it — working on this now. I'll reply here when it's ready."}
	if err := d.Dispatch(context.Background(), echo); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(*sends) != 0 {
		t.Errorf("flow's own bot message must NOT trigger a reject, got %+v", *sends)
	}
	if len(sink.calls) != 0 {
		t.Errorf("flow's own bot message must NOT reach the chat sink, got %d calls", len(sink.calls))
	}
	if len(steerer.events) != 0 {
		t.Errorf("flow's own bot message must NOT reach the steerer (no bogus attention card), got %d events", len(steerer.events))
	}
}

// TestDispatcher_CommandChannelDM_OperatorWithColleagueFallsThrough verifies that
// an IM where the flow bot is NOT a member (the operator DMing a colleague) is
// NOT treated as the command channel: no reject is sent and no chat session is
// opened. It falls through to normal routing.
func TestDispatcher_CommandChannelDM_OperatorWithColleagueFallsThrough(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot") // bot is a member of D_bot only
	sends := withSendAsBotStub(t)
	clearRejectedDMChannel(t, "D_colleague")

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	steerer := &fakeSteerer{}
	d.Steerer = steerer

	// Operator's DM with a colleague: bot is NOT a member of D_colleague.
	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_colleague", UserID: "U_me", Text: "hey colleague"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Errorf("colleague DM must NOT reach the chat sink, got %d calls", len(sink.calls))
	}
	if len(*sends) != 0 {
		t.Errorf("colleague DM must NOT trigger a reject reply, got %+v", *sends)
	}
	// It falls through to normal routing — here the untracked DM reaches the
	// steerer (proving no panic and no reject in the fall-through path).
	if len(steerer.events) != 1 {
		t.Errorf("colleague DM should fall through to normal routing (steerer), got %d events", len(steerer.events))
	}
}

// TestDispatcher_CommandChannelDisabled_FallsThrough verifies that with the
// command channel OFF, a NON-operator bot DM neither opens a chat session nor
// gets hinted (the off-state hint is operator-only) — it falls through to normal
// routing (the steerer). The off + operator hint path is covered separately by
// TestDispatcher_CommandChannelDisabled_OperatorHintedOnce.
func TestDispatcher_CommandChannelDisabled_FallsThrough(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "0") // feature off
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	sends := withSendAsBotStub(t)
	clearHintedDMChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	// With the feature off and a steerer wired, a non-operator bot DM falls
	// through to the steerer (normal routing) rather than the chat sink.
	steerer := &fakeSteerer{}
	d.Steerer = steerer

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_other", Text: "still a normal dm"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Errorf("with the command channel disabled, the chat sink must not be called, got %d", len(sink.calls))
	}
	if len(*sends) != 0 {
		t.Errorf("off + non-operator DM must NOT be hinted (hint is operator-only), got %+v", *sends)
	}
	if len(steerer.events) != 1 {
		t.Errorf("disabled command channel must fall through to normal routing (steerer), got %d events", len(steerer.events))
	}
}

// TestDispatcher_CommandChannelDisabled_OperatorHintedOnce verifies the off-state
// nudge: when the command channel is OFF and the OPERATOR DMs the flow bot, the
// dispatcher replies once with the static enable hint — it does NOT open a chat
// session and does NOT re-hint on a second DM in the same channel.
func TestDispatcher_CommandChannelDisabled_OperatorHintedOnce(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "") // feature off (default)
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	withCommandChannel(t, "D_bot")
	sends := withSendAsBotStub(t)
	clearHintedDMChannel(t, "D_bot")

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	steerer := &fakeSteerer{}
	d.Steerer = steerer

	// Operator DMs the bot while the feature is off → single enable nudge.
	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_me", Text: "hey flow"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// A second DM on the same channel must NOT fire a second hint.
	msg2 := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_me", Text: "you there?"}
	if err := d.Dispatch(context.Background(), msg2); err != nil {
		t.Fatalf("Dispatch (second): %v", err)
	}

	if len(sink.calls) != 0 {
		t.Errorf("off-state operator DM must NOT open a chat session, got %d calls", len(sink.calls))
	}
	if len(steerer.events) != 0 {
		t.Errorf("off-state bot DM must NOT reach the steerer, got %d events", len(steerer.events))
	}
	if len(*sends) != 1 {
		t.Fatalf("expected exactly ONE off-state hint (per-channel dedup), got %d: %+v", len(*sends), *sends)
	}
	if (*sends)[0].Channel != "D_bot" || (*sends)[0].Text != commandChannelDisabledHint {
		t.Errorf("hint reply wrong: channel=%q text=%q", (*sends)[0].Channel, (*sends)[0].Text)
	}
}

// TestDispatcher_CommandChannelDisabled_StrangerNoHintNoAPICall verifies that
// when the command channel is OFF and a NON-operator DMs the bot, the dispatcher
// neither hints nor opens a session, and — crucially — never makes the
// conversations.info membership call (the (enabled||operator) guard short-circuits
// before IsCommandChannel). The event falls through to normal routing.
func TestDispatcher_CommandChannelDisabled_StrangerNoHintNoAPICall(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "") // feature off (default)
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	sends := withSendAsBotStub(t)
	clearHintedDMChannel(t, "D_stranger")

	// Wrap the membership resolver with a call counter so we can assert it is
	// never invoked for an off + non-operator DM.
	orig := conversationIsBotIMFn
	membershipCalls := 0
	conversationIsBotIMFn = func(ch string) bool {
		membershipCalls++
		return true
	}
	resetCommandChannelCache()
	t.Cleanup(func() {
		conversationIsBotIMFn = orig
		resetCommandChannelCache()
	})

	db := dispatcherTestDB(t)
	d := NewDispatcher(db, nil)
	sink := &fakeChatSink{}
	d.ChatSink = sink
	steerer := &fakeSteerer{}
	d.Steerer = steerer

	msg := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_stranger", UserID: "U_other", Text: "anyone home?"}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if membershipCalls != 0 {
		t.Errorf("off + non-operator DM must NOT trigger a conversations.info membership call, got %d", membershipCalls)
	}
	if len(*sends) != 0 {
		t.Errorf("off + non-operator DM must NOT send a hint or reject, got %+v", *sends)
	}
	if len(sink.calls) != 0 {
		t.Errorf("off + non-operator DM must NOT open a chat session, got %d calls", len(sink.calls))
	}
	// Falls through to normal routing — the untracked DM reaches the steerer.
	if len(steerer.events) != 1 {
		t.Errorf("off + non-operator DM should fall through to the steerer, got %d events", len(steerer.events))
	}
}
