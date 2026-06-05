package steering

import (
	"context"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestRetriageUpdatesCardInPlace(t *testing.T) {
	c, db := cascadeFixture(t)

	// Seed a surfaced card with the OLD verdict (no match).
	item := flowdb.FeedItem{
		ID: "rt1", Source: "slack", ThreadKey: "D03:1780660471.796299",
		ChannelType: "im", Channel: "D03", Author: "U_ishaan",
		Summary: "CoinSwitch UAT infra setup Monday", SuggestedAction: "reply",
		Reason: "no existing task matches", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// New triage run: deep triage now matches an existing task.
	stubClassifier(t, func(prompt string) (string, error) {
		return `{"suggested_action":"forward","confidence":0.8}`, nil
	})
	var deepPrompt string
	stubDeepTriage(t, func(prompt string) (string, error) {
		deepPrompt = prompt
		return `{"suggested_action":"forward","matched_task":"coinswitch-task","confidence":0.9,"summary":"CoinSwitch UAT","reason":"continues the coinswitch coordination task"}`, nil
	})

	if err := c.Retriage(context.Background(), item); err != nil {
		t.Fatalf("Retriage: %v", err)
	}
	// The deep-triage prompt must carry the task index (so it can read briefs).
	if !strings.Contains(deepPrompt, "Tasks:") {
		t.Errorf("deep prompt missing task index:\n%s", deepPrompt)
	}
	// Same card (coalesced by thread_key), now with the fresh verdict.
	got, err := flowdb.GetFeedItem(db, "rt1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.SuggestedAction != "forward" || got.MatchedTask != "coinswitch-task" {
		t.Errorf("card after retriage = action %q matched %q, want forward / coinswitch-task", got.SuggestedAction, got.MatchedTask)
	}
	if news, _ := flowdb.ListFeedItems(db, "new"); len(news) != 1 {
		t.Errorf("retriage must update in place, not duplicate: %d new cards", len(news))
	}
}
