package app

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
)

// attentionTestDB points FLOW_ROOT/HOME at a temp dir and returns an open DB at
// the same path the command will use (flowDBPath()).
func attentionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRenderAttentionFeed(t *testing.T) {
	items := []flowdb.FeedItem{
		{ID: "abc123", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "make_task", Confidence: 0.88, Urgency: "urgent", MatchedTask: "", Summary: "Customer wants rollout date"},
	}
	out := renderAttentionFeed(items)
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "make_task") || !strings.Contains(out, "Customer wants rollout date") {
		t.Errorf("rendered feed missing fields:\n%s", out)
	}

	empty := renderAttentionFeed(nil)
	if !strings.Contains(strings.ToLower(empty), "no ") && strings.TrimSpace(empty) == "" {
		t.Errorf("empty feed should render a friendly message, got %q", empty)
	}
}

func TestCmdAttentionActDismiss(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "d1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rc := cmdAttentionAct([]string{"d1", "dismiss"}); rc != 0 {
		t.Fatalf("act dismiss rc = %d, want 0", rc)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("item should be dismissed, got %d dismissed rows", len(items))
	}
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "d1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "dismiss" || fb[0].Outcome != "dismissed" {
		t.Errorf("dismiss feedback mismatch: %+v", fb)
	}
}

func TestCmdAttentionSent(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "s1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Draft: "ok to ship", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// No --close-floating + no FLOW_HOOK_URL → the close is a no-op, but the card
	// must still flip to acted.
	if rc := cmdAttentionSent([]string{"s1"}); rc != 0 {
		t.Fatalf("sent rc = %d, want 0", rc)
	}
	acted, _ := flowdb.ListFeedItems(db, "acted")
	if len(acted) != 1 || acted[0].ID != "s1" {
		t.Fatalf("item should be acted, got %d acted rows", len(acted))
	}
	if news, _ := flowdb.ListFeedItems(db, "new"); len(news) != 0 {
		t.Errorf("no items should remain 'new', got %d", len(news))
	}
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "s1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "send_reply" || fb[0].Outcome != "sent" || fb[0].DraftEditDelta != "unchanged" {
		t.Errorf("sent feedback mismatch: %+v", fb)
	}
}

func TestCmdAttentionSentErrors(t *testing.T) {
	attentionTestDB(t)
	if rc := cmdAttentionSent(nil); rc != 2 {
		t.Errorf("missing id should rc=2, got %d", rc)
	}
	if rc := cmdAttentionSent([]string{"missing-id"}); rc != 1 {
		t.Errorf("missing feed item should rc=1, got %d", rc)
	}
}

func TestCmdAttentionSurface(t *testing.T) {
	db := attentionTestDB(t)
	if rc := cmdAttention([]string{
		"surface",
		"--source", "slack",
		"--channel", "C1",
		"--channel-type", "channel",
		"--ts", "100.1",
		"--action", "digest_only",
		"--summary", "hi",
		"--confidence", "0.5",
	}); rc != 0 {
		t.Fatalf("surface rc = %d, want 0", rc)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 1 || items[0].ThreadKey != "C1:100.1" {
		t.Fatalf("want 1 card under C1:100.1, got %+v", items)
	}
}

func TestCmdAttentionHandoffAcceptAndMalformed(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "ha-cli", Source: "slack", ThreadKey: "C1:cli", SuggestedAction: "forward",
		MatchedTask: "owner-task", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	h, err := flowdb.CreateAttentionHandoff(db, flowdb.AttentionHandoff{
		FeedItemID: "ha-cli", Sender: "attention-router", Receiver: "owner-task",
		Context: "context", RequestedVerdict: "accept_or_decline",
		RequestedAt: "2026-06-05T10:01:00Z", ExpiresAt: "2026-06-05T11:01:00Z",
	})
	if err != nil {
		t.Fatalf("CreateAttentionHandoff: %v", err)
	}
	if rc := cmdAttentionHandoff([]string{"respond", h.ID, "maybe", "--reason", "unclear"}); rc != 2 {
		t.Fatalf("malformed verdict rc = %d, want 2", rc)
	}
	if got, _ := flowdb.GetAttentionHandoff(db, h.ID); got.Status != "pending" {
		t.Fatalf("malformed response should leave pending, got %+v", got)
	}

	out := captureStdout(t, func() {
		if rc := cmdAttentionHandoff([]string{"accept", h.ID, "--reason", "belongs to this task"}); rc != 0 {
			t.Fatalf("accept rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "accepted "+h.ID) {
		t.Fatalf("accept output = %q", out)
	}
	item, _ := flowdb.GetFeedItem(db, "ha-cli")
	if item.Status != "acted" || item.LinkedTask != "owner-task" {
		t.Fatalf("accepted handoff should act/link feed item, got %+v", item)
	}
}

func TestCmdAttentionActErrors(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "e1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rc := cmdAttentionAct([]string{"e1"}); rc != 2 {
		t.Errorf("missing action arg should rc=2, got %d", rc)
	}
	if rc := cmdAttentionAct([]string{"e1", "frobnicate"}); rc != 2 {
		t.Errorf("unknown action should rc=2, got %d", rc)
	}
	if rc := cmdAttentionAct([]string{"missing-id", "dismiss"}); rc != 1 {
		t.Errorf("missing feed item should rc=1, got %d", rc)
	}
}

func TestCmdAttentionListRuns(t *testing.T) {
	attentionTestDB(t)
	if rc := cmdAttentionList(nil); rc != 0 {
		t.Errorf("list on empty feed should rc=0, got %d", rc)
	}
}

func TestCmdAttentionFeedbackReportsByChannel(t *testing.T) {
	db := attentionTestDB(t)
	rows := []flowdb.AttentionFeedback{
		{ID: "a", FeedItemID: "fa", Source: "slack", Channel: "C1", Author: "U1", ThreadType: "channel", ThreadKey: "C1:1", SuggestedAction: "reply", FinalAction: "send_reply", Outcome: "approved", Confidence: 0.91, ConfidenceBand: "0.90-1.00", CreatedAt: "2026-06-05T10:00:00Z"},
		{ID: "b", FeedItemID: "fb", Source: "slack", Channel: "C1", Author: "U2", ThreadType: "channel", ThreadKey: "C1:2", SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed", Confidence: 0.72, ConfidenceBand: "0.70-0.79", CreatedAt: "2026-06-05T11:00:00Z"},
	}
	for _, row := range rows {
		if err := flowdb.RecordAttentionFeedback(db, row); err != nil {
			t.Fatalf("RecordAttentionFeedback: %v", err)
		}
	}

	out := captureStdout(t, func() {
		if rc := cmdAttentionFeedback([]string{"--group", "channel"}); rc != 0 {
			t.Fatalf("feedback rc = %d, want 0", rc)
		}
	})
	for _, want := range []string{"GROUP", "C1", "50%", "approved", "dismissed"} {
		if !strings.Contains(out, want) {
			t.Errorf("feedback output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTrace(t *testing.T) {
	funnel := flowdb.SteeringFunnel{
		Observed:      5,
		DroppedStage0: 2,
		DroppedStage1: 1,
		Surfaced:      1,
		Errors:        1,
	}
	items := []flowdb.SteeringTrace{
		{
			CreatedAt:       "2026-06-05T10:00:00Z",
			Origin:          "slack",
			Disposition:     "dropped",
			StageReached:    "stage0",
			FinalConfidence: 0.0,
			Channel:         "C123",
			DropReason:      "self-authored",
			TextPreview:     "some text",
		},
		{
			CreatedAt:       "2026-06-05T09:00:00Z",
			Origin:          "github",
			Disposition:     "surfaced",
			StageReached:    "stage3",
			FinalConfidence: 0.9,
			Channel:         "C456",
			DropReason:      "",
			TextPreview:     "PR review requested",
		},
	}

	out := renderTrace(funnel, items)
	for _, want := range []string{"observed 5", "surfaced 1", "errors 1", "WHEN", "self-authored"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTrace output missing %q:\n%s", want, out)
		}
	}

	// Empty items renders friendly message.
	emptyOut := renderTrace(funnel, nil)
	if !strings.Contains(emptyOut, "No trace rows in window.") {
		t.Errorf("empty items should render 'No trace rows in window.', got:\n%s", emptyOut)
	}
}

func TestSinceToRFC3339(t *testing.T) {
	// Valid duration parses and returns a past timestamp.
	ts, err := sinceToRFC3339("1h")
	if err != nil {
		t.Fatalf("sinceToRFC3339(1h) unexpected error: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("sinceToRFC3339(1h) returned invalid RFC3339 %q: %v", ts, err)
	}
	if !parsed.Before(time.Now()) {
		t.Errorf("sinceToRFC3339(1h) should return a past time, got %v", parsed)
	}

	// Invalid duration returns error.
	_, err = sinceToRFC3339("garbage")
	if err == nil {
		t.Error("sinceToRFC3339(garbage) should return error, got nil")
	}

	// Empty string defaults to 24h (no error).
	ts2, err := sinceToRFC3339("")
	if err != nil {
		t.Fatalf("sinceToRFC3339('') unexpected error: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, ts2); err != nil {
		t.Errorf("sinceToRFC3339('') returned invalid RFC3339 %q: %v", ts2, err)
	}
}
