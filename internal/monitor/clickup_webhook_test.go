package monitor

import "testing"

func TestNormalizeClickUpWebhookTaskComment(t *testing.T) {
	body := []byte(`{
		"event":"taskCommentPosted",
		"task_id":"cu-123",
		"webhook_id":"wh-1",
		"history_items":[{
			"id":"hist-1",
			"date":"1642736194135",
			"field":"comment",
			"user":{"id":183,"username":"Jane"},
			"after":{"comment_text":"Please update the rollout checklist"}
		}]
	}`)

	events, err := NormalizeClickUpWebhook(body)
	if err != nil {
		t.Fatalf("NormalizeClickUpWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != ClickUpEventTaskCommentPosted {
		t.Fatalf("Kind = %q", ev.Kind)
	}
	if ev.TaskID != "cu-123" || ev.WebhookID != "wh-1" || ev.HistoryID != "hist-1" {
		t.Fatalf("ids = %+v", ev)
	}
	if ev.EventKeyValue() != "wh-1:hist-1" {
		t.Fatalf("EventKeyValue = %q", ev.EventKeyValue())
	}
	if ev.LinkTag() != "clickup-task::cu-123" {
		t.Fatalf("LinkTag = %q", ev.LinkTag())
	}
	if ev.Author != "Jane" {
		t.Fatalf("Author = %q", ev.Author)
	}
	if ev.Body != "Please update the rollout checklist" {
		t.Fatalf("Body = %q", ev.Body)
	}
	in := clickUpEventToInboxEvent(ev)
	if in.ChannelType != "clickup" || in.ThreadTS != ev.LinkTag() || in.EventKey != "wh-1:hist-1" {
		t.Fatalf("inbox event = %+v", in)
	}
}

func TestNormalizeClickUpWebhookAssigneeUpdateUsesFallbackText(t *testing.T) {
	body := []byte(`{
		"event":"taskAssigneeUpdated",
		"task_id":"cu-456",
		"webhook_id":"wh-2",
		"history_items":[{
			"id":"hist-2",
			"date":"1642736194135",
			"field":"assignee_add",
			"user":{"id":183,"username":"Jane"},
			"after":{"id":184,"username":"Sam"}
		}]
	}`)

	events, err := NormalizeClickUpWebhook(body)
	if err != nil {
		t.Fatalf("NormalizeClickUpWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != ClickUpEventTaskAssigneeUpdated {
		t.Fatalf("Kind = %q", ev.Kind)
	}
	if ev.Body != "assignee_add changed to Sam" {
		t.Fatalf("Body = %q", ev.Body)
	}
}

func TestNormalizeClickUpWebhookDeletedWithoutHistoryHasStableKey(t *testing.T) {
	body := []byte(`{"event":"taskDeleted","task_id":"cu-789","webhook_id":"wh-3"}`)

	events, err := NormalizeClickUpWebhook(body)
	if err != nil {
		t.Fatalf("NormalizeClickUpWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.EventKeyValue() != "wh-3:taskDeleted:cu-789" {
		t.Fatalf("EventKeyValue = %q", ev.EventKeyValue())
	}
}

func TestNormalizeClickUpWebhookMalformedJSON(t *testing.T) {
	if _, err := NormalizeClickUpWebhook([]byte(`{bad json`)); err == nil {
		t.Fatal("malformed payload should error")
	}
}

func TestNormalizeClickUpWebhookIgnoresUnsupportedEvent(t *testing.T) {
	events, err := NormalizeClickUpWebhook([]byte(`{"event":"spaceUpdated","space_id":"s1","webhook_id":"wh-4"}`))
	if err != nil {
		t.Fatalf("NormalizeClickUpWebhook: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want ignored", events)
	}
}
