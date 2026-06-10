package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

func TestHandleOverviewIncludesBriefing(t *testing.T) {
	s, db := attentionTestServer(t)
	now := "2026-06-07T08:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('flow-manager', 'Flow Manager', 'active', 'medium', ?, ?, ?)`,
		filepath.Join(s.cfg.FlowRoot, "repo"), now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (
			slug, name, project_slug, status, priority, work_dir,
			session_provider, session_id, status_changed_at, created_at, updated_at
		 ) VALUES ('deploy-followup', 'Follow up on deploy', 'flow-manager', 'in-progress', 'high', ?, 'codex',
		 '00000000-0000-4000-8000-000000000001', ?, ?, ?)`,
		filepath.Join(s.cfg.FlowRoot, "repo"), now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "overview-feed", Source: "slack", ThreadKey: "C1:overview",
		Summary: "Deploy needs an answer", SuggestedAction: "forward",
		MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Urgency: "urgent", Confidence: 0.9, Status: "new", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleOverview(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := body["briefing"].(map[string]any)
	if !ok {
		t.Fatalf("overview missing briefing: %s", rec.Body.String())
	}
	items, ok := raw["needs_you"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("briefing.needs_you empty or missing: %+v", raw)
	}
	first, _ := items[0].(map[string]any)
	if first["kind"] != "attention" || first["ref"] != "overview-feed" {
		t.Fatalf("first needs_you item = %+v, want attention overview-feed", first)
	}
}
