package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"flow/internal/flowdb"

	"github.com/slack-go/slack/slackevents"
)

type capturedSlackAutoReplyCall struct {
	actionType string
	channel    string
	ts         string
	threadTS   string
	emoji      string
	text       string
}

func stubSlackAutoReplyRunner(t *testing.T, retErr error) *[]capturedSlackAutoReplyCall {
	t.Helper()
	old := slackAutoReplyRunner
	calls := &[]capturedSlackAutoReplyCall{}
	slackAutoReplyRunner = func(_ context.Context, call slackAutoReplyCall) error {
		*calls = append(*calls, capturedSlackAutoReplyCall{
			actionType: call.ActionType,
			channel:    call.Channel,
			ts:         call.TS,
			threadTS:   call.ThreadTS,
			emoji:      call.Emoji,
			text:       call.Text,
		})
		return retErr
	}
	t.Cleanup(func() { slackAutoReplyRunner = old })
	return calls
}

func seedSlackThreadTask(t *testing.T, db *sql.DB, root, slug, channel, ts string, status string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, status, priority, work_dir, session_provider, session_id,
			session_started, status_changed_at, created_at, updated_at
		) VALUES (?, 'Patch production issue', ?, 'high', ?, 'claude', ?, ?, ?, ?, ?)`,
		slug, status, root, "session-"+slug, now, now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	id := flowdb.MonitorEventID("slack", channel+":"+ts)
	rawJSON := `{"event":{"channel":"` + channel + `","ts":"` + ts + `"}}`
	if _, err := db.Exec(
		`INSERT INTO monitor_events (id, source, kind, source_id, title, body, url, severity, status, first_seen_at, last_seen_at, last_seq, raw_json)
		 VALUES (?, 'slack', 'mention', ?, 'seed', 'seed', NULL, 'medium', 'new', ?, ?, 0, ?)`,
		id, channel+":"+ts, now, now, rawJSON,
	); err != nil {
		t.Fatalf("seed monitor event: %v", err)
	}
	if err := flowdb.RecordMonitorEventAction(db, id, "draft", slug, "rule mode auto_task"); err != nil {
		t.Fatalf("seed action: %v", err)
	}
}

func seedTask(t *testing.T, db *sql.DB, root, slug, name, status string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, status, priority, work_dir, session_provider, session_id,
			session_started, status_changed_at, created_at, updated_at
		) VALUES (?, ?, ?, 'medium', ?, 'claude', ?, ?, ?, ?, ?)`,
		slug, name, status, root, "session-"+slug, now, now, now, now,
	); err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func TestSlackAutoReplyAnswersKnownTaskStatusFromFlowThread(t *testing.T) {
	root, db := testRootDB(t)
	calls := stubSlackAutoReplyRunner(t, nil)
	t.Setenv("FLOW_SLACK_AUTO_REPLY_ENABLED", "1")
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U999")
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	seedSlackThreadTask(t, db, root, "slack-task", "C123", "1710000000.000001", "done")
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})

	kept, newCount, err := s.handleSlackSocketEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:            "message",
				Channel:         "C123",
				ChannelType:     slackevents.ChannelTypeChannel,
				User:            "U234",
				Text:            "<@U999> have u finsihed the task?",
				TimeStamp:       "1710000002.000001",
				ThreadTimeStamp: "1710000000.000001",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 1 {
		t.Fatalf("kept=%d new=%d, want 1/1", kept, newCount)
	}
	if len(*calls) != 2 {
		t.Fatalf("auto reply calls = %d, want reaction + final answer: %+v", len(*calls), *calls)
	}
	if (*calls)[0].actionType != "reaction_add" || (*calls)[0].emoji == "" {
		t.Fatalf("first call = %+v, want reaction", (*calls)[0])
	}
	answer := (*calls)[1]
	if answer.actionType != "final_answer" || answer.threadTS != "1710000000.000001" {
		t.Fatalf("answer call = %+v", answer)
	}
	if !strings.Contains(answer.text, "Patch production issue") || !strings.Contains(answer.text, "done") || !strings.Contains(answer.text, "http://flow.example/tasks/slack-task") {
		t.Fatalf("answer text = %q", answer.text)
	}
	eventID := flowdb.MonitorEventID("slack", "C123:1710000002.000001")
	actions, err := flowdb.ListExternalActionsForEvent(db, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].ActionType != "reaction_add" || actions[1].ActionType != "final_answer" {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestSlackAutoReplyAnswersCurrentWorkFromFlow(t *testing.T) {
	root, db := testRootDB(t)
	calls := stubSlackAutoReplyRunner(t, nil)
	t.Setenv("FLOW_SLACK_AUTO_REPLY_ENABLED", "1")
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U999")
	t.Setenv("FLOW_BASE_URL", "http://flow.example")
	seedTask(t, db, root, "slack-current-work", "Implement Slack auto replies", "in-progress")
	seedTask(t, db, root, "already-shipped", "Already shipped", "done")
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})

	kept, newCount, err := s.handleSlackSocketEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:        "message",
				Channel:     "C123",
				ChannelType: slackevents.ChannelTypeChannel,
				User:        "U234",
				Text:        "<@U999> what are all the task you are working on?",
				TimeStamp:   "1710000200.000001",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 1 {
		t.Fatalf("kept=%d new=%d, want 1/1", kept, newCount)
	}
	if len(*calls) != 2 {
		t.Fatalf("auto reply calls = %d, want reaction + final answer: %+v", len(*calls), *calls)
	}
	answer := (*calls)[1]
	if answer.actionType != "final_answer" || answer.threadTS != "1710000200.000001" {
		t.Fatalf("answer call = %+v", answer)
	}
	if !strings.Contains(answer.text, "Implement Slack auto replies") || !strings.Contains(answer.text, "http://flow.example/tasks/slack-current-work") {
		t.Fatalf("answer text = %q", answer.text)
	}
	if strings.Contains(answer.text, "Already shipped") {
		t.Fatalf("answer included done task: %q", answer.text)
	}
}

func TestSlackAutoReplyAsksClarifyingQuestionWhenNoFlowFact(t *testing.T) {
	root, db := testRootDB(t)
	calls := stubSlackAutoReplyRunner(t, nil)
	t.Setenv("FLOW_SLACK_AUTO_REPLY_ENABLED", "1")
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U999")
	s := New(Config{DB: db, FlowRoot: root, Version: "test"})

	_, _, err := s.handleSlackSocketEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:            "message",
				Channel:         "C999",
				ChannelType:     slackevents.ChannelTypeChannel,
				User:            "U234",
				Text:            "<@U999> is this done?",
				TimeStamp:       "1710000100.000001",
				ThreadTimeStamp: "1710000000.000001",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("auto reply calls = %d, want reaction + clarification: %+v", len(*calls), *calls)
	}
	if (*calls)[1].actionType != "clarifying_question" || !strings.Contains((*calls)[1].text, "Which Flow task") {
		t.Fatalf("clarifying call = %+v", (*calls)[1])
	}
	eventID := flowdb.MonitorEventID("slack", "C999:1710000100.000001")
	messages, err := flowdb.ListExternalMessagesForEvent(db, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Intent.String != "question" || messages[0].Text != "@U999 is this done?" {
		t.Fatalf("messages = %+v", messages)
	}
	actions, err := flowdb.ListExternalActionsForEvent(db, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[1].ActionType != "clarifying_question" {
		t.Fatalf("actions = %+v", actions)
	}
}
