package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flow/internal/flowdb"
)

func TestOperatorAskPostsToCommandDMAndMarksTaskWaiting(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "1")
	t.Setenv("FLOW_SLACK_WRITE_TOKEN", "xoxp-test")
	t.Setenv("SLACK_WRITE_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("FLOW_SLACK_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("FLOW_SLACK_USER_TOKEN", "")
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_operator")
	t.Setenv("FLOW_SLACK_SELF_BOT_USER_IDS", "U_flowbot")

	var (
		mu       sync.Mutex
		requests []struct{ Path, Body, Auth string }
	)
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, struct{ Path, Body, Auth string }{r.URL.Path, string(body), r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_operator_bot"}}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_operator_bot","ts":"1700000000.000100","message":{"ts":"1700000000.000100"}}`))
		default:
			t.Fatalf("unexpected Slack path %s", r.URL.Path)
		}
	}))
	defer slack.Close()
	t.Setenv("FLOW_SLACK_API_BASE_URL", slack.URL)

	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test"}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/operator/ask", strings.NewReader(`{"task_slug":"build-ui","question":"Which deployment window?"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK       bool   `json:"ok"`
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Channel != "D_operator_bot" || resp.ThreadTS != "1700000000.000100" {
		t.Fatalf("response = %+v", resp)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("Slack requests = %+v, want conversations.open + chat.postMessage", requests)
	}
	if requests[0].Path != "/conversations.open" ||
		requests[0].Auth != "Bearer xoxp-test" ||
		!strings.Contains(requests[0].Body, "U_flowbot") {
		t.Fatalf("open request = %+v", requests[0])
	}
	if requests[1].Path != "/chat.postMessage" ||
		requests[1].Auth != "Bearer xoxp-test" ||
		!strings.Contains(requests[1].Body, "D_operator_bot") ||
		!strings.Contains(requests[1].Body, "Which deployment window?") {
		t.Fatalf("post request = %+v", requests[1])
	}

	tags, err := flowdb.GetTaskTags(db, "build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(tags, "slack-thread:d_operator_bot:1700000000.000100") {
		t.Fatalf("tags = %v, want slack-thread tag", tags)
	}
	task, err := flowdb.GetTask(db, "build-ui")
	if err != nil {
		t.Fatal(err)
	}
	if !task.WaitingOn.Valid || !strings.Contains(task.WaitingOn.String, "Which deployment window?") {
		t.Fatalf("waiting_on = %+v", task.WaitingOn)
	}
}
