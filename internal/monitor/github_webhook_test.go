package monitor

import (
	"context"
	"testing"
)

// TestNormalizeGitHubWebhook is the cross-transport contract: a webhook
// delivery must normalize into the SAME GitHubEvent kind + EventKey the poller
// would have produced for the same underlying GitHub object. Identical keys are
// what make github_event_log dedupe work across both transports — a PR comment
// seen by a poll AND a webhook must collapse to one inbox append.
func TestNormalizeGitHubWebhook(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		wantKind  GitHubEventKind
		wantKey   string
		wantOwner string
		wantRepo  string
		wantNum   int
		wantCmtID string // only for comment/review kinds
		wantAuth  string
	}{
		{
			name:      "pull_request review_requested",
			eventType: "pull_request",
			payload: `{"action":"review_requested","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"title":"Add feature","html_url":"https://github.com/o/r/pull/5",
				"user":{"login":"alice"},"base":{"ref":"main"},"head":{"ref":"feat","sha":"abc123"}}}`,
			wantKind: GitHubEventPRReviewRequested, wantKey: "gh-pr:o/r#5:pr_review_requested",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantAuth: "alice",
		},
		{
			name:      "pull_request assigned",
			eventType: "pull_request",
			payload: `{"action":"assigned","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"title":"T","html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}}}`,
			wantKind: GitHubEventPRAssigned, wantKey: "gh-pr:o/r#5:pr_assigned",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantAuth: "alice",
		},
		{
			name:      "pull_request synchronize -> head updated keyed by sha",
			eventType: "pull_request",
			payload: `{"action":"synchronize","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"},
				"head":{"ref":"feat","sha":"deadbeef"}}}`,
			wantKind: GitHubEventPRHeadUpdated, wantKey: "pr-head:o/r#5:deadbeef",
			wantOwner: "o", wantRepo: "r", wantNum: 5,
		},
		{
			name:      "pull_request closed merged -> merged",
			eventType: "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"merged":true,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}}}`,
			wantKind: GitHubEventPRMerged, wantKey: "pr-merged:o/r#5",
			wantOwner: "o", wantRepo: "r", wantNum: 5,
		},
		{
			name:      "pull_request closed unmerged -> closed",
			eventType: "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"merged":false,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}}}`,
			wantKind: GitHubEventPRClosed, wantKey: "pr-closed:o/r#5",
			wantOwner: "o", wantRepo: "r", wantNum: 5,
		},
		{
			name:      "issues assigned",
			eventType: "issues",
			payload: `{"action":"assigned","repository":{"full_name":"o/r"},
				"issue":{"number":7,"title":"Bug","html_url":"https://github.com/o/r/issues/7","user":{"login":"bob"}}}`,
			wantKind: GitHubEventIssueAssigned, wantKey: "gh-issue:o/r#7:issue_assigned",
			wantOwner: "o", wantRepo: "r", wantNum: 7, wantAuth: "bob",
		},
		{
			name:      "issue_comment on PR -> pr comment (issue carries pull_request)",
			eventType: "issue_comment",
			payload: `{"action":"created","repository":{"full_name":"o/r"},
				"issue":{"number":5,"pull_request":{"url":"https://api.github.com/repos/o/r/pulls/5"}},
				"comment":{"id":111,"node_id":"IC_node","body":"looks good","html_url":"https://github.com/o/r/pull/5#issuecomment-111","user":{"login":"carol"}}}`,
			wantKind: GitHubEventPRComment, wantKey: "issue-comment:IC_node",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantCmtID: "IC_node", wantAuth: "carol",
		},
		{
			name:      "issue_comment on issue -> issue comment",
			eventType: "issue_comment",
			payload: `{"action":"created","repository":{"full_name":"o/r"},
				"issue":{"number":7},
				"comment":{"id":112,"node_id":"IC_node2","body":"thanks","html_url":"https://github.com/o/r/issues/7#issuecomment-112","user":{"login":"carol"}}}`,
			wantKind: GitHubEventIssueComment, wantKey: "issue-comment:IC_node2",
			wantOwner: "o", wantRepo: "r", wantNum: 7, wantCmtID: "IC_node2", wantAuth: "carol",
		},
		{
			name:      "pull_request_review changes_requested",
			eventType: "pull_request_review",
			payload: `{"action":"submitted","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}},
				"review":{"id":222,"node_id":"REV_node","state":"changes_requested","body":"fix this","html_url":"https://github.com/o/r/pull/5#pullrequestreview-222","user":{"login":"dave"}}}`,
			wantKind: GitHubEventPRReviewChangesRequested, wantKey: "review:REV_node",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantCmtID: "REV_node", wantAuth: "dave",
		},
		{
			name:      "pull_request_review approved",
			eventType: "pull_request_review",
			payload: `{"action":"submitted","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}},
				"review":{"id":223,"node_id":"REV_node2","state":"approved","html_url":"https://github.com/o/r/pull/5#pullrequestreview-223","user":{"login":"dave"}}}`,
			wantKind: GitHubEventPRReviewApproved, wantKey: "review:REV_node2",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantCmtID: "REV_node2", wantAuth: "dave",
		},
		{
			name:      "pull_request_review commented",
			eventType: "pull_request_review",
			payload: `{"action":"submitted","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}},
				"review":{"id":224,"node_id":"REV_node3","state":"commented","body":"nit","html_url":"https://github.com/o/r/pull/5#pullrequestreview-224","user":{"login":"dave"}}}`,
			wantKind: GitHubEventPRReviewComment, wantKey: "review:REV_node3",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantCmtID: "REV_node3", wantAuth: "dave",
		},
		{
			name:      "pull_request_review_comment created",
			eventType: "pull_request_review_comment",
			payload: `{"action":"created","repository":{"full_name":"o/r"},
				"pull_request":{"number":5,"html_url":"https://github.com/o/r/pull/5","user":{"login":"alice"}},
				"comment":{"id":333,"node_id":"PRRC_node","body":"inline nit","html_url":"https://github.com/o/r/pull/5#discussion_r333","user":{"login":"eve"}}}`,
			wantKind: GitHubEventPRReviewComment, wantKey: "review-comment:PRRC_node",
			wantOwner: "o", wantRepo: "r", wantNum: 5, wantCmtID: "PRRC_node", wantAuth: "eve",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := NormalizeGitHubWebhook(c.eventType, "delivery-1", []byte(c.payload))
			if err != nil {
				t.Fatalf("NormalizeGitHubWebhook error: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			ev := events[0]
			if ev.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", ev.Kind, c.wantKind)
			}
			if ev.EventKeyValue() != c.wantKey {
				t.Errorf("EventKeyValue() = %q, want %q (cross-transport dedupe key)", ev.EventKeyValue(), c.wantKey)
			}
			if ev.Owner != c.wantOwner || ev.Repo != c.wantRepo || ev.Number != c.wantNum {
				t.Errorf("repo/num = %s/%s#%d, want %s/%s#%d", ev.Owner, ev.Repo, ev.Number, c.wantOwner, c.wantRepo, c.wantNum)
			}
			if c.wantCmtID != "" && ev.CommentID != c.wantCmtID {
				t.Errorf("CommentID = %q, want %q", ev.CommentID, c.wantCmtID)
			}
			if c.wantAuth != "" && ev.Author != c.wantAuth {
				t.Errorf("Author = %q, want %q", ev.Author, c.wantAuth)
			}
			if ev.RawJSON == "" {
				t.Errorf("RawJSON not preserved")
			}
		})
	}
}

// TestNormalizeGitHubWebhookIgnored covers event/action combinations Flow does
// not act on. They must normalize to zero events WITHOUT erroring, so the
// receiver can record them as "ignored" and still return a 2xx to GitHub.
func TestNormalizeGitHubWebhookIgnored(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
	}{
		{"ping", "ping", `{"zen":"Keep it simple","hook_id":1}`},
		{"pull_request opened", "pull_request", `{"action":"opened","repository":{"full_name":"o/r"},"pull_request":{"number":5,"user":{"login":"a"}}}`},
		{"pull_request labeled", "pull_request", `{"action":"labeled","repository":{"full_name":"o/r"},"pull_request":{"number":5,"user":{"login":"a"}}}`},
		{"issues opened", "issues", `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":7,"user":{"login":"a"}}}`},
		{"issue_comment edited", "issue_comment", `{"action":"edited","repository":{"full_name":"o/r"},"issue":{"number":7},"comment":{"id":1,"node_id":"n"}}`},
		{"review dismissed", "pull_request_review", `{"action":"dismissed","repository":{"full_name":"o/r"},"pull_request":{"number":5},"review":{"id":1,"node_id":"n","state":"approved"}}`},
		{"unknown event", "deployment_status", `{"action":"created","repository":{"full_name":"o/r"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := NormalizeGitHubWebhook(c.eventType, "d", []byte(c.payload))
			if err != nil {
				t.Fatalf("unexpected error for ignored event: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("got %d events, want 0 (ignored)", len(events))
			}
		})
	}
}

// TestNormalizeGitHubWebhookMalformed: a body that isn't valid JSON is a hard
// error so the receiver can record the delivery as errored.
func TestNormalizeGitHubWebhookMalformed(t *testing.T) {
	if _, err := NormalizeGitHubWebhook("pull_request", "d", []byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestWebhookNormalizeDispatchAppendsInboxOnce is the end-to-end webhook seam:
// a signed-delivery payload, normalized and pushed through the REAL dispatcher,
// appends the tracked task's inbox exactly once — and replaying the same
// delivery does NOT double-append, because the webhook event key matches what
// github_event_log already deduped. This is the cross-transport invariant: get
// the key wrong and the redelivery assertion fails.
func TestWebhookNormalizeDispatchAppendsInboxOnce(t *testing.T) {
	t.Setenv("FLOW_GH_AUTOOPEN", "0")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()
	seedGitHubTask(t, "tracked-pr", db, "gh-pr:Facets-cloud/flow-manager#42")
	d := NewGitHubDispatcher(db, nil)

	body := []byte(`{"action":"created","repository":{"full_name":"Facets-cloud/flow-manager"},
		"pull_request":{"number":42,"html_url":"https://github.com/Facets-cloud/flow-manager/pull/42","user":{"login":"author"}},
		"comment":{"id":333,"node_id":"PRRC_node","body":"please tighten the docs","html_url":"https://github.com/Facets-cloud/flow-manager/pull/42#discussion_r333","user":{"login":"reviewer"}}}`)

	dispatch := func() {
		events, err := NormalizeGitHubWebhook("pull_request_review_comment", "delivery-1", body)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if err := d.Dispatch(context.Background(), events[0]); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}

	dispatch()
	entries, _ := ReadInboxEntries("tracked-pr")
	if len(entries) != 1 {
		t.Fatalf("after first delivery: %d inbox entries, want 1", len(entries))
	}

	// Redelivery of the same underlying comment must not append again.
	dispatch()
	entries, _ = ReadInboxEntries("tracked-pr")
	if len(entries) != 1 {
		t.Fatalf("after redelivery: %d inbox entries, want 1 (cross-transport dedupe)", len(entries))
	}
}
