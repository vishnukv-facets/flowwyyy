package monitor

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"

	"github.com/slack-go/slack/slackevents"
)

func openMonitorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPollGitHubStoresReviewRequestNotification(t *testing.T) {
	db := openMonitorTestDB(t)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "gh" && strings.Contains(joined, "review-requested:@me"):
			return []byte(`[{"number":48,"title":"Add monitor daemon","url":"https://github.com/acme/flow/pull/48","repository":{"nameWithOwner":"acme/flow"}}]`), nil
		case name == "gh":
			return []byte(`[]`), nil
		default:
			t.Fatalf("unexpected command: %s %s", name, joined)
		}
		return nil, nil
	}
	summaries, err := (Poller{DB: db, Runner: runner}).Poll(context.Background(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Events != 1 || summaries[0].New != 1 {
		t.Fatalf("summary = %+v", summaries)
	}
	events, err := flowdb.ListMonitorEvents(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Source != "github" || events[0].Kind != "review_requested" {
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

func TestPollGitHubMergedLinkedPRMarksTaskDone(t *testing.T) {
	db := openMonitorTestDB(t)
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_id, session_started, created_at, updated_at)
		 VALUES ('monitor-pr', 'Monitor PR', 'in-progress', 'medium', ?, '11111111-1111-4111-8111-111111111111', ?, ?, ?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := flowdb.UpsertTaskPRLink(db, "monitor-pr", "acme/flow", 48, "https://github.com/acme/flow/pull/48"); err != nil {
		t.Fatal(err)
	}
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "gh" && strings.Contains(joined, "pr view"):
			return []byte(`{"state":"MERGED","mergedAt":"2026-05-15T08:00:00Z","url":"https://github.com/acme/flow/pull/48","number":48,"title":"Add monitor daemon"}`), nil
		case name == "gh":
			return []byte(`[]`), nil
		default:
			t.Fatalf("unexpected command: %s %s", name, joined)
		}
		return nil, nil
	}
	if _, err := (Poller{DB: db, Runner: runner}).Poll(context.Background(), "github"); err != nil {
		t.Fatal(err)
	}
	task, err := flowdb.GetTask(db, "monitor-pr")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Fatalf("status = %s, want done", task.Status)
	}
	links, err := flowdb.ListTaskPRLinks(db, "monitor-pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].State != "merged" {
		t.Fatalf("links = %+v", links)
	}
}

func TestGitHubNotificationsUseWebURLs(t *testing.T) {
	events, err := githubNotifications([]byte(`[{
		"id":"n1",
		"repository":{"full_name":"acme/flow"},
		"subject":{"title":"Review me","type":"PullRequest","url":"https://api.github.com/repos/acme/flow/pulls/48"}
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].URL != "https://github.com/acme/flow/pull/48" {
		t.Fatalf("url = %q", events[0].URL)
	}
}

func TestPollAllDoesNotPollSlack(t *testing.T) {
	db := openMonitorTestDB(t)
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "slack" {
			t.Fatalf("Poll(all) must not invoke Slack polling: %s %v", name, args)
		}
		if name == "gh" {
			return []byte(`[]`), nil
		}
		t.Fatalf("unexpected command: %s %v", name, args)
		return nil, nil
	}
	summaries, err := (Poller{DB: db, Runner: runner}).Poll(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Source != "github" {
		t.Fatalf("summaries = %+v, want only github", summaries)
	}
}

func TestPollSlackReportsSocketModeOnly(t *testing.T) {
	db := openMonitorTestDB(t)
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatalf("Poll(slack) must not invoke command polling: %s %v", name, args)
		return nil, nil
	}
	summaries, err := (Poller{DB: db, Runner: runner}).Poll(context.Background(), "slack")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Source != "slack" || summaries[0].Events != 0 || len(summaries[0].Errors) != 0 {
		t.Fatalf("summary = %+v", summaries)
	}
	if got := strings.Join(summaries[0].Diagnostics, " "); !strings.Contains(got, "Socket Mode") {
		t.Fatalf("diagnostics = %q, want Socket Mode guidance", got)
	}
}

func TestSlackEventsAPIInputsConvertDMAndMention(t *testing.T) {
	dm := SlackEventsAPIEventInputs(slackevents.EventsAPIEvent{
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
	if len(dm) != 1 || dm[0].Kind != "dm" || dm[0].SourceID != "D123:1710000000.000001" {
		t.Fatalf("dm inputs = %+v", dm)
	}

	mention := SlackEventsAPIEventInputs(slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: &slackevents.AppMentionEvent{
				Type:      string(slackevents.AppMention),
				Channel:   "C123",
				User:      "U234",
				Text:      "<@U999> heads up",
				TimeStamp: "1710000001.000001",
			},
		},
	})
	if len(mention) != 1 || mention[0].Kind != "mention" || mention[0].SourceID != "C123:1710000001.000001" {
		t.Fatalf("mention inputs = %+v", mention)
	}
}

func TestSlackEventsAPIInputsKeepPersonalMentionWithoutChannelAllowlist(t *testing.T) {
	t.Setenv("FLOW_SLACK_CHANNELS", "")
	t.Setenv("FLOW_SLACK_MENTION_USER_ID", "U999")

	inputs := SlackEventsAPIEventInputs(slackevents.EventsAPIEvent{
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
	if len(inputs) != 1 || inputs[0].Kind != "personal_mention" || inputs[0].SourceID != "C123:1710000002.000001" {
		t.Fatalf("personal mention inputs = %+v", inputs)
	}

	ignored := SlackEventsAPIEventInputs(slackevents.EventsAPIEvent{
		TeamID: "T123",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{
				Type:        "message",
				Channel:     "C123",
				ChannelType: slackevents.ChannelTypeChannel,
				User:        "U234",
				Text:        "plain channel chatter",
				TimeStamp:   "1710000003.000001",
			},
		},
	})
	if len(ignored) != 0 {
		t.Fatalf("non-mention channel message inputs = %+v, want ignored", ignored)
	}
}

func TestStoreSlackEventsUsesInsertNewPolicyAndRules(t *testing.T) {
	db := openMonitorTestDB(t)
	input := flowdb.MonitorEventInput{
		Source:   "slack",
		Kind:     "dm",
		SourceID: "D123:1710000000.000001",
		Title:    "Slack dm in D123",
		Body:     "can you review this?",
		Severity: "medium",
	}
	poller := Poller{DB: db}
	kept, newCount, err := poller.StoreSlackEvents([]flowdb.MonitorEventInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 1 {
		t.Fatalf("first store kept=%d new=%d", kept, newCount)
	}
	kept, newCount, err = poller.StoreSlackEvents([]flowdb.MonitorEventInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || newCount != 0 {
		t.Fatalf("second store kept=%d new=%d, want dedup without new rule firing", kept, newCount)
	}
	notifications, err := flowdb.ListMonitorNotifications(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Level != "approval" {
		t.Fatalf("notifications = %+v", notifications)
	}
}
