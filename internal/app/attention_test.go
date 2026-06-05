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
}

func TestCmdAttentionSent(t *testing.T) {
	db := attentionTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{ID: "s1", Source: "slack", ThreadKey: "C1:1.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}); err != nil {
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
			CreatedAt:   "2026-06-05T10:00:00Z",
			Origin:      "slack",
			Disposition: "dropped",
			StageReached: "stage0",
			FinalConfidence: 0.0,
			Channel:     "C123",
			DropReason:  "self-authored",
			TextPreview: "some text",
		},
		{
			CreatedAt:   "2026-06-05T09:00:00Z",
			Origin:      "github",
			Disposition: "surfaced",
			StageReached: "stage3",
			FinalConfidence: 0.9,
			Channel:     "C456",
			DropReason:  "",
			TextPreview: "PR review requested",
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
