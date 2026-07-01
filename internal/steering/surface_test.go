package steering

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func surfaceTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSurfaceCardRejectsForeignThreadKey(t *testing.T) {
	db := surfaceTestDB(t)

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "100.1",
		ThreadKey:   "C1:999.9",
		Action:      "digest_only",
		Summary:     "hello",
		Confidence:  0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" {
		t.Fatalf("want a surfaced card, got surfaced=%v id=%q", surfaced, id)
	}
	got, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.ThreadKey != "C1:100.1" {
		t.Errorf("foreign key should fall back to raw C1:100.1, got %q", got.ThreadKey)
	}
}

func TestSurfaceCardMergesIntoOpenCard(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "a1",
		Source:          "slack",
		ThreadKey:       "C1:50.0",
		SuggestedAction: "digest_only",
		Summary:         "repo access for dynamodb",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U1",
		TS:              "50.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "60.0",
		ThreadKey:   "C1:50.0",
		Action:      "digest_only",
		Summary:     "list the repo names",
		Confidence:  0.5,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced {
		t.Fatalf("want surfaced")
	}
	if id != "a1" {
		t.Errorf("want merge into existing card a1, got new id %q", id)
	}
}

func TestSurfaceCardUsesDurableWorkstreamAlias(t *testing.T) {
	db := surfaceTestDB(t)
	existing := flowdb.FeedItem{
		ID:              "cert1",
		Source:          "slack",
		ThreadKey:       "D1:100.0",
		SuggestedAction: "make_task",
		Summary:         "Goniyo cert-manager IRSA migration needs follow-up",
		Reason:          "cert-manager IRSA DNS-01 smoke path",
		Channel:         "D1",
		ChannelType:     "im",
		Author:          "U_OMENDRA",
		TS:              "100.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}
	if _, err := flowdb.UpsertFeedItem(db, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := flowdb.EnsureAttentionWorkstreamForFeed(db, existing, "D1:110.0", flowdb.NowISO()); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "D1",
		ChannelType: "im",
		TS:          "110.0",
		Author:      "U_OMENDRA",
		Action:      "make_task",
		Summary:     "Please check this one too",
		Reason:      "This follow-up was already linked by the operator merge path.",
		Confidence:  0.8,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id != "cert1" {
		t.Fatalf("want alias merge into cert1, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("workstream alias should prevent a duplicate card, got %+v", items)
	}
}

func TestSurfaceCardDoesNotAutoClubPublicChannelWithoutThreadKey(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "cert1",
		Source:          "slack",
		ThreadKey:       "C1:100.0",
		SuggestedAction: "make_task",
		Summary:         "Goniyo cert-manager IRSA migration needs follow-up",
		Reason:          "cert-manager IRSA DNS-01 smoke path",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U_OMENDRA",
		TS:              "100.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "110.0",
		Author:      "U_OMENDRA",
		Action:      "make_task",
		Summary:     "cert-manager IRSA migration smoke timed out",
		Reason:      "same words are not enough in a public channel without an explicit thread key",
		Confidence:  0.8,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" || id == "cert1" {
		t.Fatalf("want separate public-channel card, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("public-channel cards should remain separate, got %+v", items)
	}
}

func TestSurfaceCardDoesNotAutoClubUnrelatedDMWorkstream(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "cert1",
		Source:          "slack",
		ThreadKey:       "D1:100.0",
		SuggestedAction: "make_task",
		Summary:         "Goniyo cert-manager IRSA migration needs follow-up",
		Reason:          "cert-manager IRSA DNS-01 smoke path",
		Channel:         "D1",
		ChannelType:     "im",
		Author:          "U_OMENDRA",
		TS:              "100.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "D1",
		ChannelType: "im",
		TS:          "110.0",
		Author:      "U_OMENDRA",
		Action:      "make_task",
		Summary:     "Update the billing export dashboard owner",
		Reason:      "Omendra needs a separate task for dashboard access and invoice visibility.",
		Confidence:  0.8,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" || id == "cert1" {
		t.Fatalf("want separate unrelated DM card, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("unrelated DM cards should remain separate, got %+v", items)
	}
}

func TestSurfaceCardAskTaskAgentRequestsHandoff(t *testing.T) {
	db := surfaceTestDB(t)
	_, tells := stubActionIO(t)

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:       "slack",
		Channel:      "C1",
		ChannelType:  "channel",
		TS:           "65.0",
		Action:       "forward",
		MatchedTask:  "rollout-task",
		Summary:      "Omendra needs release guardrail input",
		Reason:       "same Goniyo migration handoff",
		ContextJSON:  `{"parent":{"text":"destroy not allowed before validation"}}`,
		Confidence:   0.8,
		AskTaskAgent: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id == "" {
		t.Fatalf("want surfaced card, got surfaced=%v id=%q", surfaced, id)
	}
	if len(*tells) != 1 || (*tells)[0].slug != "rollout-task" {
		t.Fatalf("task tells = %+v, want one handoff to rollout-task", *tells)
	}
	if !strings.Contains((*tells)[0].msg, "flow attention handoff accept") {
		t.Fatalf("handoff tell missing accept command:\n%s", (*tells)[0].msg)
	}
	h, ok, err := flowdb.LatestAttentionHandoffForFeed(db, id)
	if err != nil || !ok {
		t.Fatalf("LatestAttentionHandoffForFeed ok=%v err=%v", ok, err)
	}
	if h.Receiver != "rollout-task" || h.Status != "pending" {
		t.Fatalf("handoff = %+v, want pending rollout-task", h)
	}
}

func TestSurfaceCardContextOnlyDoesNotSurface(t *testing.T) {
	db := surfaceTestDB(t)

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "70.0",
		Action:      "digest_only",
		Summary:     "operator's own note",
		ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if surfaced || id != "" {
		t.Errorf("context_only must not surface, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("context_only should not create feed items, got %+v", items)
	}
}

func TestSurfaceCardContextOnlyRefreshesExistingCardOnly(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "a1",
		Source:          "slack",
		ThreadKey:       "C1:50.0",
		SuggestedAction: "reply",
		Summary:         "please respond",
		Draft:           "checking",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U1",
		TS:              "50.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "60.0",
		ThreadKey:   "C1:50.0",
		Action:      "reply",
		Summary:     "operator replied but follow-up still needs tracking",
		Draft:       "Thanks, will follow up with the repo names.",
		Reason:      "operator response changed the draft",
		Confidence:  0.8,
		ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if !surfaced || id != "a1" {
		t.Fatalf("context_only should refresh existing card a1, got surfaced=%v id=%q", surfaced, id)
	}
	got, err := flowdb.GetFeedItem(db, "a1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Summary != "operator replied but follow-up still needs tracking" || got.Draft == "checking" {
		t.Fatalf("card was not refreshed: %+v", got)
	}

	id, surfaced, err = SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "70.0",
		ThreadKey:   "C1:missing",
		Action:      "reply",
		Summary:     "should not create a new card",
		ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard missing card: %v", err)
	}
	if surfaced || id != "" {
		t.Fatalf("missing context_only card must no-op, got surfaced=%v id=%q", surfaced, id)
	}
	items, err := flowdb.ListFeedItems(db, "new")
	if err != nil {
		t.Fatalf("ListFeedItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "a1" {
		t.Fatalf("context_only refresh must not create extra cards, got %+v", items)
	}
}

func TestSurfaceCardContextOnlyDropResolvesExistingCard(t *testing.T) {
	db := surfaceTestDB(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID:              "a1",
		Source:          "slack",
		ThreadKey:       "C1:50.0",
		SuggestedAction: "reply",
		Summary:         "please respond",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U1",
		TS:              "50.0",
		Status:          "new",
		CreatedAt:       flowdb.NowISO(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, surfaced, err := SurfaceCard(context.Background(), db, SurfaceCardParams{
		Source:      "slack",
		Channel:     "C1",
		ChannelType: "channel",
		TS:          "60.0",
		ThreadKey:   "C1:50.0",
		Action:      "drop",
		Summary:     "operator already answered",
		ContextOnly: true,
	})
	if err != nil {
		t.Fatalf("SurfaceCard: %v", err)
	}
	if surfaced || id != "a1" {
		t.Fatalf("drop should resolve existing card a1 without surfacing, got surfaced=%v id=%q", surfaced, id)
	}
	got, err := flowdb.GetFeedItem(db, "a1")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if got.Status != "acted" || got.ActedAt == "" {
		t.Fatalf("card was not resolved: %+v", got)
	}
}
