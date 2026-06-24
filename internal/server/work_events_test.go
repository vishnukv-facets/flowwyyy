package server

import (
	"encoding/json"
	"flow/internal/productdb"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleWorkEventsFiltersByBucketAndSource(t *testing.T) {
	s, db := attentionTestServer(t)
	if _, err := productdb.UpsertFeedItem(db, productdb.FeedItem{
		ID: "we-feed", Source: "github", ThreadKey: "gh:1", Summary: "PR needs review",
		SuggestedAction: "forward", MatchedTask: "", Urgency: "normal", Confidence: 0.9,
		Reason: "task-linked PR changed", URL: "https://github.com/o/r/pull/1",
		Status: "new", CreatedAt: "2026-06-07T08:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/work-events?bucket=needs_action&source=github", nil)
	rec := httptest.NewRecorder()
	s.handleWorkEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
			Bucket string `json:"bucket"`
		} `json:"items"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Source != "github" || body.Items[0].Bucket != "needs_action" {
		t.Fatalf("body = %+v", body)
	}
}
