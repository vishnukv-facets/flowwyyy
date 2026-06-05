package steering

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestSendReplyViaAgent(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	var gotPrompt string
	old := sendReplyRunner
	sendReplyRunner = func(_ context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "posted", nil
	}
	t.Cleanup(func() { sendReplyRunner = old })

	item := flowdb.FeedItem{
		ID: "sr1", Source: "slack", ThreadKey: "C_eng:1700000000.000100",
		SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := SendReplyViaAgent(context.Background(), db, item, "thanks, on it", "keep it to one sentence"); err != nil {
		t.Fatalf("SendReplyViaAgent: %v", err)
	}
	if !strings.Contains(gotPrompt, "thanks, on it") || !strings.Contains(gotPrompt, "C_eng:1700000000.000100") {
		t.Errorf("prompt missing draft/thread:\n%s", gotPrompt)
	}
	if !strings.Contains(gotPrompt, "keep it to one sentence") {
		t.Errorf("prompt should carry operator instructions:\n%s", gotPrompt)
	}
	// No task spawned; feed row acted with empty linked_task.
	got, _ := flowdb.GetFeedItem(db, "sr1")
	if got.Status != "acted" {
		t.Errorf("status = %q, want acted", got.Status)
	}
	if got.LinkedTask != "" {
		t.Errorf("linked_task = %q, want empty (no task spawned)", got.LinkedTask)
	}
}

func TestSendReplyViaAgentErrorLeavesCard(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	old := sendReplyRunner
	sendReplyRunner = func(_ context.Context, _ string) (string, error) {
		return "ERROR: no Slack MCP available", nil
	}
	t.Cleanup(func() { sendReplyRunner = old })

	item := flowdb.FeedItem{ID: "sr2", Source: "slack", ThreadKey: "C:2.1", SuggestedAction: "reply", Status: "new", CreatedAt: "2026-06-05T10:00:00Z"}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SendReplyViaAgent(context.Background(), db, item, "hi", ""); err == nil {
		t.Fatal("expected an error when the agent reports it could not post")
	}
	// Card must remain 'new' so the operator can retry.
	if got, _ := flowdb.GetFeedItem(db, "sr2"); got.Status != "new" {
		t.Errorf("status = %q, want new (unresolved on failure)", got.Status)
	}
}
