package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestCmdAskOperatorPostsBoundTaskQuestionToServer(t *testing.T) {
	root := setupFlowRoot(t)
	db := openFlowDB(t)
	insertTask(t, db, "ask-me", "Ask me", "in-progress", "medium", filepath.Join(root, "repo"), nil)
	t.Setenv("FLOW_TASK", "ask-me")

	var got struct {
		TaskSlug string `json:"task_slug"`
		Question string `json:"question"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/operator/ask" {
			t.Fatalf("path = %q, want /api/operator/ask", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D_bot","thread_ts":"1700000000.000100"}`))
	}))
	defer srv.Close()
	t.Setenv("FLOW_UI_URL", srv.URL)

	rc := Run([]string{"ask", "operator", "Which option should I use?"})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if got.TaskSlug != "ask-me" || got.Question != "Which option should I use?" {
		t.Fatalf("request = %+v", got)
	}
}
