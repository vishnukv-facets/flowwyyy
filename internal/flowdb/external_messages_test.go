package flowdb

import (
	"encoding/json"
	"testing"
)

func TestRecordExternalMessageAndActionForFTS(t *testing.T) {
	db := openTempDB(t)
	event, _, err := InsertMonitorEventIfNew(db, MonitorEventInput{
		Source:   "slack",
		Kind:     "personal_mention",
		SourceID: "C123:1710000002.000001",
		Title:    "Slack mention of you from Vishnu kv in #test-kv",
		Body:     "@vishnu is this task done?",
		Severity: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, isNew, err := RecordExternalMessage(db, ExternalMessageInput{
		Source:          "slack",
		EventID:         event.ID,
		ConversationID:  "C123",
		ChannelID:       "C123",
		ThreadTS:        "1710000000.000001",
		MessageTS:       "1710000002.000001",
		Direction:       "inbound",
		SenderID:        "U234",
		SenderName:      "Vishnu kv",
		Text:            "@vishnu is this task done?",
		NormalizedText:  "vishnu is this task done",
		Intent:          "question",
		ConfidenceBasis: "question heuristic",
		RawJSON:         `{"event":{"channel":"C123","ts":"1710000002.000001"}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isNew {
		t.Fatal("first message insert isNew=false, want true")
	}
	if msg.ID == "" || msg.EventID.String != event.ID || msg.Direction != "inbound" || msg.Intent.String != "question" {
		t.Fatalf("message = %+v", msg)
	}

	_, isNew, err = RecordExternalMessage(db, ExternalMessageInput{
		Source:         "slack",
		EventID:        event.ID,
		ConversationID: "C123",
		MessageTS:      "1710000002.000001",
		Direction:      "inbound",
		Text:           "changed text should not rewrite first-seen content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("duplicate message insert isNew=true, want false")
	}

	payload, _ := json.Marshal(map[string]string{
		"text":       "From Flow: task is done.",
		"basis":      "flow.task.status",
		"task_slug":  "slack-task",
		"thread_ts":  "1710000000.000001",
		"message_ts": "1710000002.000001",
	})
	if err := RecordExternalAction(db, ExternalActionInput{
		Source:       "slack",
		EventID:      event.ID,
		MessageID:    msg.ID,
		ActionType:   "final_answer",
		Status:       "sent",
		PayloadJSON:  string(payload),
		AutoApproved: true,
	}); err != nil {
		t.Fatal(err)
	}
	actions, err := ListExternalActionsForEvent(db, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions len = %d, want 1", len(actions))
	}
	if actions[0].ActionType != "final_answer" || actions[0].Status != "sent" || !actions[0].AutoApproved {
		t.Fatalf("action = %+v", actions[0])
	}
	if actions[0].PayloadJSON.String == "" {
		t.Fatal("payload_json empty; FTS/provenance payload should be durable")
	}
}
