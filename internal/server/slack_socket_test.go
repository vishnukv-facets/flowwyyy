package server

import (
	"context"
	"strings"
	"testing"

	"flow/internal/flowdb"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func TestSlackSocketConnectedClearsLegacyPollingError(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})
	if _, err := flowdb.RecordMonitorSyncEnd(db, "slack", "error", "slack conversations.history D123: rate limited"); err != nil {
		t.Fatal(err)
	}

	newSlackSocketListener(s).handleEvent(context.Background(), nil, socketmode.Event{
		Type: socketmode.EventTypeConnected,
	})

	state, err := flowdb.GetMonitorSyncState(db, "slack")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastStatus != "ok" {
		t.Fatalf("last_status = %q, want ok", state.LastStatus)
	}
	if state.LastError.Valid {
		t.Fatalf("last_error = %q, want NULL", state.LastError.String)
	}
	if state.IsSyncing {
		t.Fatalf("is_syncing = true, want false")
	}
}

func TestSlackEventTypePrefersInnerType(t *testing.T) {
	got := slackEventType(slackevents.EventsAPIEvent{
		Type: "event_callback",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{},
		},
	})
	if got != "message" {
		t.Fatalf("slackEventType = %q, want message", got)
	}
}

func TestHandleSlackSocketEventsAPIStoresDM(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})

	kept, newCount, err := s.handleSlackSocketEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:        "message",
				Channel:     "D123",
				ChannelType: slackevents.ChannelTypeIM,
				User:        "U234",
				Text:        "can you review this?",
				TimeStamp:   "1710000000.000001",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 1 {
		t.Fatalf("kept=%d new=%d, want 1/1", kept, newCount)
	}
	events, err := flowdb.ListMonitorEvents(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "dm" || events[0].SourceID != "D123:1710000000.000001" {
		t.Fatalf("events = %+v", events)
	}
	notifications, err := flowdb.ListMonitorNotifications(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Level != "approval" {
		t.Fatalf("notifications = %+v", notifications)
	}
}

func TestHandleSlackSocketEventsAPIEnrichesSlackDisplayContext(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U999")

	oldUser := slackResolveUserName
	oldChannel := slackResolveChannelName
	slackResolveUserName = func(_ context.Context, userID string) string {
		switch userID {
		case "U234":
			return "Manan Bhandari"
		case "U999":
			return "Vishnu kv"
		default:
			return userID
		}
	}
	slackResolveChannelName = func(_ context.Context, channelID string) string {
		if channelID == "C123" {
			return "#test-kv"
		}
		return channelID
	}
	t.Cleanup(func() {
		slackResolveUserName = oldUser
		slackResolveChannelName = oldChannel
	})

	kept, newCount, err := s.handleSlackSocketEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:        "message",
				Channel:     "C123",
				ChannelType: slackevents.ChannelTypeChannel,
				User:        "U234",
				Text:        "<@U999> can you check this?",
				TimeStamp:   "1710000002.000001",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 1 {
		t.Fatalf("kept=%d new=%d, want 1/1", kept, newCount)
	}
	events, err := flowdb.ListMonitorEvents(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "personal_mention" {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Title != "Slack mention of you from Manan Bhandari in #test-kv" {
		t.Fatalf("title = %q", events[0].Title)
	}
	if body := events[0].Body.String; !strings.Contains(body, "@Vishnu kv") || strings.Contains(body, "<@U999>") {
		t.Fatalf("body = %q, want resolved user mention", body)
	}
}
