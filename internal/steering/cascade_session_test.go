package steering

import (
	"context"
	"errors"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// fakeSessionSink records deliveries and can be told to fail (to exercise fail-open).
type fakeSessionSink struct {
	calls   []fakeDelivery
	failNow bool
}

type fakeDelivery struct {
	key string
	p   SteererDelivery
}

func (f *fakeSessionSink) DeliverToChannelSession(key string, p SteererDelivery) error {
	f.calls = append(f.calls, fakeDelivery{key, p})
	if f.failNow {
		return errors.New("sink boom")
	}
	return nil
}

// newSessionTestCascade builds a Cascade wired to a fake sink, with the watched
// channel "C1" in scope and zero classifier budget so the COLD path (when reached)
// drops cheaply without shelling out to claude.
func newSessionTestCascade(t *testing.T, sink SteererSessionSink) *Cascade {
	t.Helper()
	db := surfaceTestDB(t) // existing steering test helper (surface_test.go)
	c := NewCascade(db, WatchConfig{
		WatchedChannels: map[string]bool{"C1": true},
		Identity:        OperatorIdentity{UserIDs: []string{"UOP"}},
	})
	c.SessionSink = sink
	c.classifierBudget = newBudgetGuard(0)
	c.budget = newBudgetGuard(0)
	c.trace = func(flowdb.SteeringTrace) {} // swallow traces
	return c
}

func recordSessionTraces(t *testing.T, c *Cascade) {
	t.Helper()
	c.trace = func(tr flowdb.SteeringTrace) {
		if err := flowdb.InsertSteeringTrace(c.DB, tr); err != nil {
			t.Fatalf("InsertSteeringTrace: %v", err)
		}
	}
}

func sessSlackMsg(channel, ts, user, text string) monitor.InboundEvent {
	return monitor.InboundEvent{Kind: "message", Channel: channel, ChannelType: "channel", TS: ts, ThreadTS: ts, UserID: user, Text: text}
}

func TestObserveDeliversSurvivorToSink(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	if err := c.Observe(context.Background(), sessSlackMsg("C1", "100.1", "U2", "hello")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(sink.calls))
	}
	if sink.calls[0].key != "C1" || sink.calls[0].p.ContextOnly {
		t.Fatalf("bad delivery: %+v", sink.calls[0])
	}
	// No card surfaced by the cascade — the session surfaces later via the tool.
	items, _ := flowdb.ListFeedItems(c.DB, "new")
	if len(items) != 0 {
		t.Fatalf("cascade must not write a card on the session path, got %d", len(items))
	}
}

func TestObserveTwoSameChannelReuseKey(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	_ = c.Observe(context.Background(), sessSlackMsg("C1", "100.1", "U2", "msg one"))
	_ = c.Observe(context.Background(), sessSlackMsg("C1", "100.2", "U3", "list the repo names for this"))
	if len(sink.calls) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(sink.calls))
	}
	if sink.calls[0].key != "C1" || sink.calls[1].key != "C1" {
		t.Fatalf("both must key the same channel C1, got %q and %q", sink.calls[0].key, sink.calls[1].key)
	}
}

func TestObserveFlagOffDoesNotDeliver(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "off")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink) // classifierBudget 0 ⇒ cold path drops at stage1, no claude
	if err := c.Observe(context.Background(), sessSlackMsg("C1", "100.1", "U2", "hello")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("flag off must not deliver to sink, got %d", len(sink.calls))
	}
}

func TestObserveOperatorSelfFeedsContextOnly(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	// Authored by the operator (UOP) ⇒ Stage0 drops "self-authored" ⇒ context_only feed.
	if err := c.Observe(context.Background(), sessSlackMsg("C1", "100.1", "UOP", "ignore the last message")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(sink.calls) != 1 || !sink.calls[0].p.ContextOnly {
		t.Fatalf("operator-self must feed context_only once, got %+v", sink.calls)
	}
	items, _ := flowdb.ListFeedItems(c.DB, "new")
	if len(items) != 0 {
		t.Fatalf("context_only must not surface a card, got %d", len(items))
	}
}

func TestObserveOperatorSelfDuplicateSessionDeliverySkipped(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	recordSessionTraces(t, c)
	ev := sessSlackMsg("C1", "100.1", "UOP", "is this sorted?")
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe first: %v", err)
	}
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe duplicate: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("duplicate self-authored message must wake session once, got %d", len(sink.calls))
	}
	traces, err := flowdb.ListSteeringTrace(c.DB, flowdb.TraceFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListSteeringTrace: %v", err)
	}
	if !hasDuplicateSessionTrace(traces) {
		t.Fatalf("traces = %+v, want dropped/cache duplicate session delivery", traces)
	}
}

func TestObserveOperatorSelfUnwatchedChannelDoesNotOpenSession(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	if err := c.Observe(context.Background(), sessSlackMsg("C_RANDOM", "100.1", "UOP", "just chatting")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("unwatched self-authored channel must not open a session, got %+v", sink.calls)
	}
}

func TestObserveSelfEchoFeedsSelfEcho(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	if err := c.ObserveSelfAuthored(context.Background(), sessSlackMsg("C1", "100.1", "BOT", "On it — checking now.")); err != nil {
		t.Fatalf("ObserveSelfAuthored: %v", err)
	}
	if len(sink.calls) != 1 || !sink.calls[0].p.ContextOnly || !sink.calls[0].p.SelfEcho {
		t.Fatalf("bot echo must feed context_only+self_echo, got %+v", sink.calls)
	}
}

func TestObserveSelfEchoDuplicateSessionDeliverySkipped(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	recordSessionTraces(t, c)
	ev := sessSlackMsg("C1", "100.1", "BOT", "On it, checking now.")
	if err := c.ObserveSelfAuthored(context.Background(), ev); err != nil {
		t.Fatalf("ObserveSelfAuthored first: %v", err)
	}
	if err := c.ObserveSelfAuthored(context.Background(), ev); err != nil {
		t.Fatalf("ObserveSelfAuthored duplicate: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("duplicate self echo must wake session once, got %d", len(sink.calls))
	}
}

// TestObserveBatchDeliversSurvivorToSink is the regression guard for the backfill
// bug: ObserveBatch (the steerer catch-up path) must hand survivors to the channel
// session like the live Observe() does — not run stage1/2/3 and surface digest_only
// FYI cards. Before the fix sink.calls was 0 here (backfill never touched sessions).
func TestObserveBatchDeliversSurvivorToSink(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	evs := []monitor.InboundEvent{sessSlackMsg("C1", "100.1", "U2", "hello from backfill")}
	if err := c.ObserveBatch(context.Background(), evs); err != nil {
		t.Fatalf("ObserveBatch: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want 1 backfill delivery to session, got %d", len(sink.calls))
	}
	if sink.calls[0].key != "C1" || sink.calls[0].p.ContextOnly {
		t.Fatalf("bad delivery: %+v", sink.calls[0])
	}
	items, _ := flowdb.ListFeedItems(c.DB, "new")
	if len(items) != 0 {
		t.Fatalf("backfill must not surface a digest_only card on the session path, got %d", len(items))
	}
}

func TestObserveBatchOperatorSelfFeedsContextOnly(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	evs := []monitor.InboundEvent{sessSlackMsg("C1", "100.1", "UOP", "ignore the last message")}
	if err := c.ObserveBatch(context.Background(), evs); err != nil {
		t.Fatalf("ObserveBatch: %v", err)
	}
	if len(sink.calls) != 1 || !sink.calls[0].p.ContextOnly {
		t.Fatalf("operator-self backfill must feed context_only once, got %+v", sink.calls)
	}
}

func TestObserveBatchSkipsDeliveredOperatorSelf(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink)
	recordSessionTraces(t, c)
	ev := sessSlackMsg("C1", "100.1", "UOP", "is this sorted?")
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe live: %v", err)
	}
	if err := c.ObserveBatch(context.Background(), []monitor.InboundEvent{ev}); err != nil {
		t.Fatalf("ObserveBatch duplicate: %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("live+backfill duplicate must wake session once, got %d", len(sink.calls))
	}
}

func TestObserveBatchFlagOffDoesNotDeliver(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "off")
	sink := &fakeSessionSink{}
	c := newSessionTestCascade(t, sink) // budget 0 ⇒ cold path drops at stage1, no claude
	evs := []monitor.InboundEvent{sessSlackMsg("C1", "100.1", "U2", "hello")}
	if err := c.ObserveBatch(context.Background(), evs); err != nil {
		t.Fatalf("ObserveBatch: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("flag off must not deliver to sink, got %d", len(sink.calls))
	}
}

func TestObserveSurvivorDuplicateSkippedAcrossCascadeInstances(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	db := surfaceTestDB(t)
	cfg := WatchConfig{WatchedChannels: map[string]bool{"C1": true}, Identity: OperatorIdentity{UserIDs: []string{"UOP"}}}
	ev := sessSlackMsg("C1", "100.1", "U2", "hello")

	firstSink := &fakeSessionSink{}
	first := NewCascade(db, cfg)
	first.SessionSink = firstSink
	if err := first.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe first: %v", err)
	}
	if len(firstSink.calls) != 1 {
		t.Fatalf("first cascade must deliver once, got %d", len(firstSink.calls))
	}

	secondSink := &fakeSessionSink{}
	second := NewCascade(db, cfg)
	second.SessionSink = secondSink
	if err := second.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe duplicate: %v", err)
	}
	if len(secondSink.calls) != 0 {
		t.Fatalf("persisted duplicate must not wake restarted session, got %d", len(secondSink.calls))
	}
	traces, err := flowdb.ListSteeringTrace(db, flowdb.TraceFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListSteeringTrace: %v", err)
	}
	if !hasDuplicateSessionTrace(traces) {
		t.Fatalf("traces = %+v, want dropped/cache duplicate session delivery", traces)
	}
}

func hasDuplicateSessionTrace(traces []flowdb.SteeringTrace) bool {
	for _, tr := range traces {
		if tr.Disposition == "dropped" && tr.StageReached == "cache" && tr.DropReason == "duplicate session delivery" {
			return true
		}
	}
	return false
}

func TestObserveFailOpenFallsThrough(t *testing.T) {
	t.Setenv("FLOW_STEERING_SESSIONS", "1")
	sink := &fakeSessionSink{failNow: true}
	c := newSessionTestCascade(t, sink) // classifierBudget 0 ⇒ cold path drops cheaply after fallthrough
	if err := c.Observe(context.Background(), sessSlackMsg("C1", "100.1", "U2", "hello")); err != nil {
		t.Fatalf("Observe (fail-open): %v", err)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink attempted once then fell through, got %d calls", len(sink.calls))
	}
	// Fell through to the cold path; with budget 0 it drops without surfacing.
	items, _ := flowdb.ListFeedItems(c.DB, "new")
	if len(items) != 0 {
		t.Fatalf("cold fallback with budget 0 must not surface, got %d", len(items))
	}
}
