package server

import (
	"sync"
	"testing"
	"time"

	"flow/internal/flowdb"
)

// TestEventHubPublishFanout pins the basic contract: publish reaches
// every subscriber whose filter matches, and never reaches ones whose
// filter doesn't.
func TestEventHubPublishFanout(t *testing.T) {
	hub := newEventHub()
	all := hub.subscribe(eventFilter{})
	byTask := hub.subscribe(eventFilter{TaskSlug: "build-ui"})
	other := hub.subscribe(eventFilter{TaskSlug: "rabbit-hole"})
	defer hub.unsubscribe(all)
	defer hub.unsubscribe(byTask)
	defer hub.unsubscribe(other)

	hub.publish(eventEnvelope{Type: "agent_hook", TaskSlug: "build-ui", SessionID: "sid-1"})

	want := func(sub *eventSubscriber, expect bool, name string) {
		t.Helper()
		select {
		case <-sub.send:
			if !expect {
				t.Fatalf("%s received an event but should not have", name)
			}
		case <-time.After(100 * time.Millisecond):
			if expect {
				t.Fatalf("%s did not receive event within deadline", name)
			}
		}
	}
	want(all, true, "all")
	want(byTask, true, "byTask")
	want(other, false, "other")
}

// TestEventHubDropsOnBackpressure pins that a slow subscriber (full
// buffer) gets events silently dropped instead of stalling the
// publisher. Without this, one stuck UI client would stall every
// agent-event ingest on the host.
func TestEventHubDropsOnBackpressure(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe(eventFilter{})
	defer hub.unsubscribe(sub)

	// Fill the channel beyond capacity from inside a single goroutine —
	// the hub should drop excess rather than block.
	for range cap(sub.send) + 50 {
		hub.publish(eventEnvelope{Type: "agent_hook", SessionID: "sid-fill"})
	}
	// If we got here without deadlock, backpressure-drop works. Drain
	// what we can to free the channel for the defer'd unsubscribe.
	drained := 0
	for {
		select {
		case <-sub.send:
			drained++
		default:
			if drained == 0 {
				t.Fatal("subscriber received zero events despite fill loop")
			}
			return
		}
	}
}

// TestEventHubTypeFilter ensures the `types` query filter scopes a
// subscription to specific envelope types only.
func TestEventHubTypeFilter(t *testing.T) {
	hub := newEventHub()
	livenessOnly := hub.subscribe(eventFilter{Types: []string{"liveness"}})
	defer hub.unsubscribe(livenessOnly)

	var wg sync.WaitGroup
	got := make(chan eventEnvelope, 4)
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case env, ok := <-livenessOnly.send:
				if !ok {
					return
				}
				got <- env
			case <-timer.C:
				return
			}
		}
	}()

	hub.publish(eventEnvelope{Type: "agent_hook", SessionID: "ignored"})
	hub.publish(eventEnvelope{Type: "liveness", SessionID: "kept"})
	hub.publish(eventEnvelope{Type: "agent_hook", SessionID: "also-ignored"})
	wg.Wait()
	close(got)

	delivered := 0
	for env := range got {
		delivered++
		if env.Type != "liveness" {
			t.Fatalf("subscriber received non-liveness event: %#v", env)
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (only matching event)", delivered)
	}
}

func TestPublishInboxItemUsesNotificationLevelForNeedsReview(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})
	sub := s.events.subscribe(eventFilter{Types: []string{"inbox_item"}})
	defer s.events.unsubscribe(sub)

	event, _, err := flowdb.InsertMonitorEventIfNew(db, flowdb.MonitorEventInput{
		Source:   "slack",
		Kind:     "personal_mention",
		SourceID: "C123:1710000000.000001",
		Title:    "Slack mention of you from Vishnu kv in #test-kv",
		Body:     "@Vishnu kv hi",
		Severity: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flowdb.CreateNotificationForEvent(db, *event, "approval"); err != nil {
		t.Fatal(err)
	}

	s.publishInboxItem(*event, "", "")

	select {
	case env := <-sub.send:
		if env.InboxItem == nil {
			t.Fatalf("inbox_item payload = nil")
		}
		if env.InboxItem.Level != "approval" {
			t.Fatalf("level = %q, want approval", env.InboxItem.Level)
		}
		if !env.InboxItem.NeedsReview {
			t.Fatal("needs_review = false, want true for approval notification")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive inbox_item event")
	}
}
