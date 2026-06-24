package server

import (
	"database/sql"
	"encoding/json"
	"flow/internal/productdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskRunsAPIReturnsLedgerRowsAndDetail(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	now := productdb.NowISO()
	run := &productdb.BrainRun{
		RunID:          "run-123",
		FamilySlug:     "build-ui",
		TaskSlug:       "build-ui",
		Role:           "worker",
		Provider:       "claude",
		PermissionMode: "auto",
		Status:         "completed",
		InputSummary:   sql.NullString{String: "worker autonomous run; task build-ui", Valid: true},
		OutputJSON:     sql.NullString{String: `{"status":"completed","task_status":"done"}`, Valid: true},
		EvidenceJSON:   sql.NullString{String: `{"log_path":"/tmp/auto-runs/run-123.log","pid":4242}`, Valid: true},
		StartedAt:      sql.NullString{String: now, Valid: true},
		FinishedAt:     sql.NullString{String: now, Valid: true},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := productdb.UpsertBrainRun(db, run); err != nil {
		t.Fatalf("UpsertBrainRun: %v", err)
	}

	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/build-ui/runs", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list BrainRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.FamilySlug != "build-ui" || list.TaskSlug != "build-ui" {
		t.Fatalf("unexpected response context: %+v", list)
	}
	if len(list.Runs) != 1 {
		t.Fatalf("run count = %d, want 1", len(list.Runs))
	}
	got := list.Runs[0]
	if got.RunID != "run-123" || got.TaskName != "Build dashboard UI" {
		t.Fatalf("unexpected run list item: %+v", got)
	}
	if got.Legacy {
		t.Fatal("persisted run should not be marked legacy")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/build-ui/runs/run-123", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail BrainRunView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.RunID != "run-123" || detail.TaskName != "Build dashboard UI" || detail.TaskStatus != "backlog" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
	if detail.Legacy {
		t.Fatal("detail should not be legacy")
	}
	if !strings.Contains(string(detail.OutputJSON), `"completed"`) {
		t.Fatalf("output_json missing expected state: %s", string(detail.OutputJSON))
	}
	if !strings.Contains(string(detail.EvidenceJSON), `"/tmp/auto-runs/run-123.log"`) {
		t.Fatalf("evidence_json missing log path: %s", string(detail.EvidenceJSON))
	}
}

func TestTaskRunsAPILegacyFallbackUsesAutoRunFields(t *testing.T) {
	root, db := testRootDB(t)
	insertProjectTask(t, db, root)
	if _, err := db.Exec(
		`UPDATE tasks SET auto_run_status='running', auto_run_pid=4242, auto_run_started=?, auto_run_log=? WHERE slug='build-ui'`,
		"2026-05-12T10:00:00+05:30",
		"/tmp/auto-runs/legacy.log",
	); err != nil {
		t.Fatalf("seed legacy auto run fields: %v", err)
	}

	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test"}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/build-ui/runs", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var list BrainRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.FamilySlug != "build-ui" || len(list.Runs) != 1 {
		t.Fatalf("unexpected response: %+v", list)
	}
	got := list.Runs[0]
	if !got.Legacy {
		t.Fatalf("expected legacy compatibility row, got %+v", got)
	}
	if got.Status != "running" || got.PID == nil || *got.PID != 4242 {
		t.Fatalf("unexpected legacy run payload: %+v", got)
	}
	if got.TaskName != "Build dashboard UI" {
		t.Fatalf("legacy task name missing: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/build-ui/runs/"+strings.ReplaceAll("legacy:auto-run:build-ui", ":", "%3A"), nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail BrainRunView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode legacy detail: %v", err)
	}
	if !detail.Legacy || detail.Status != "running" || detail.TaskSlug != "build-ui" {
		t.Fatalf("unexpected legacy detail: %+v", detail)
	}
}
