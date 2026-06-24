package steering

import (
	"context"
	"flow/internal/productdb"
	"strings"
	"testing"
)

// stubCaptureKBRunner swaps the headless agent for a canned response and records
// the prompt it was handed, restoring the real runner after the test.
func stubCaptureKBRunner(t *testing.T, fn func(prompt string) (string, error)) *string {
	t.Helper()
	var seen string
	prev := captureKBRunner
	captureKBRunner = func(_ context.Context, prompt string) (string, error) {
		seen = prompt
		return fn(prompt)
	}
	t.Cleanup(func() { captureKBRunner = prev })
	return &seen
}

func captureKBTestItem() productdb.FeedItem {
	return productdb.FeedItem{
		ID:              "feed-kb-1",
		Source:          "slack",
		ThreadKey:       "D08FCPGLC8P:1781000000.1",
		Channel:         "D08FCPGLC8P",
		ChannelType:     "im",
		Author:          "U08DNTD6U4R",
		SuggestedAction: "capture_kb",
		Summary:         "Niyo plans to unpin 8 envs from older IaC versions to latest, starting niyo-common-platform/uat.",
		Status:          "new",
		CreatedAt:       "2026-06-11T10:00:00Z",
	}
}

func TestCaptureKBViaAgentMarksActedOnConfirm(t *testing.T) {
	db := backfillTestDB(t)
	if _, err := productdb.UpsertFeedItem(db, captureKBTestItem()); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/org.md", nil })

	if err := CaptureKBViaAgent(context.Background(), db, captureKBTestItem(), "/tmp/flow/kb"); err != nil {
		t.Fatalf("CaptureKBViaAgent: %v", err)
	}

	// The prompt must hand the agent the KB directory and the item's content.
	if !strings.Contains(*prompt, "/tmp/flow/kb") {
		t.Errorf("prompt missing kb dir: %s", *prompt)
	}
	if !strings.Contains(*prompt, "unpin 8 envs") {
		t.Errorf("prompt missing item summary: %s", *prompt)
	}
	// Plans/intentions must be captured with provisional "as of <date>" framing so
	// a later close-out sweep can recognize and settle them (Phase 2 upgrade path).
	if !strings.Contains(*prompt, "provisional") || !strings.Contains(*prompt, "as of") {
		t.Errorf("prompt missing provisional plan framing: %s", *prompt)
	}

	got, err := productdb.GetFeedItem(db, "feed-kb-1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" {
		t.Errorf("status = %q, want acted after a confirmed capture", got.Status)
	}
	fb, err := productdb.ListAttentionFeedback(db, productdb.AttentionFeedbackFilter{})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "capture_kb" || fb[0].Outcome != "captured" {
		t.Fatalf("feedback = %+v, want one capture_kb/captured row", fb)
	}
}

func TestCaptureKBViaAgentLeavesCardOnFailure(t *testing.T) {
	db := backfillTestDB(t)
	if _, err := productdb.UpsertFeedItem(db, captureKBTestItem()); err != nil {
		t.Fatalf("seed feed item: %v", err)
	}
	// Agent could not write (no file access, etc.) → strict failure.
	stubCaptureKBRunner(t, func(string) (string, error) { return "FAILED: no write access", nil })

	if err := CaptureKBViaAgent(context.Background(), db, captureKBTestItem(), "/tmp/flow/kb"); err == nil {
		t.Fatal("expected an error when the agent did not confirm capture")
	}
	got, err := productdb.GetFeedItem(db, "feed-kb-1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "new" {
		t.Errorf("status = %q, want it left 'new' so the operator sees it wasn't captured", got.Status)
	}
}

func TestCaptureOperatorReplyKB(t *testing.T) {
	t.Run("nothing-durable is a clean no-op", func(t *testing.T) {
		prompt := stubCaptureKBRunner(t, func(string) (string, error) { return "NOTHING-DURABLE", nil })
		if err := captureOperatorReplyKB(context.Background(), "C1:1", "slack", "ok thanks will do", "/tmp/flow/kb"); err != nil {
			t.Errorf("NOTHING-DURABLE should not error, got %v", err)
		}
		// The prompt offers the durable-iff-present contract and the operator's words.
		if !strings.Contains(*prompt, "NOTHING-DURABLE") || !strings.Contains(*prompt, "ok thanks will do") {
			t.Errorf("prompt missing contract/reply text: %s", *prompt)
		}
	})
	t.Run("captured succeeds", func(t *testing.T) {
		stubCaptureKBRunner(t, func(string) (string, error) { return "CAPTURED kb/processes.md", nil })
		if err := captureOperatorReplyKB(context.Background(), "C1:1", "slack", "we always deploy on fridays", "/tmp/flow/kb"); err != nil {
			t.Errorf("CAPTURED should succeed, got %v", err)
		}
	})
	t.Run("failure surfaces an error", func(t *testing.T) {
		stubCaptureKBRunner(t, func(string) (string, error) { return "FAILED: no kb dir", nil })
		if err := captureOperatorReplyKB(context.Background(), "C1:1", "slack", "durable enough fact here", "/tmp/flow/kb"); err == nil {
			t.Error("FAILED output should surface an error")
		}
	})
	t.Run("empty kb dir errors", func(t *testing.T) {
		if err := captureOperatorReplyKB(context.Background(), "C1:1", "slack", "x", ""); err == nil {
			t.Error("empty kb dir should error")
		}
	})
}

func TestSubstantive(t *testing.T) {
	cases := map[string]bool{
		"We standardized prod on us-east-1": true,
		"ok":                                false,
		"thanks":                            false,
		"👍":                                 false,
		"on it":                             false, // 2 words
		"will take a look now":              true,
	}
	for in, want := range cases {
		if got := substantive(in); got != want {
			t.Errorf("substantive(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCaptureConfirmed(t *testing.T) {
	cases := map[string]bool{
		"CAPTURED kb/org.md":            true,
		"captured into processes.md":    true,
		"FAILED: no kb dir":             false,
		"ERROR":                         false,
		"I cannot write to those files": false,
		"":                              false,
		"appended the fact":             false, // no convention token → not confirmed
	}
	for out, want := range cases {
		if got := captureConfirmed(out); got != want {
			t.Errorf("captureConfirmed(%q) = %v, want %v", out, got, want)
		}
	}
}
