package flowdb

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestTaskFamilyRootWalksHierarchy(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "family-root", "Family Root", "backlog", "medium", t.TempDir(), nil)
	now := NowISO()
	wd := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, parent_slug, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, 'backlog', 'medium', ?, ?, ?)`,
		"family-child", "Family Child", "family-root", wd, now, now,
	); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	root, err := TaskFamilyRoot(db, "family-child")
	if err != nil {
		t.Fatalf("TaskFamilyRoot: %v", err)
	}
	if root != "family-root" {
		t.Fatalf("family root = %q, want family-root", root)
	}
}

func TestBrainRunUpsertLifecycle(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "run-task", "Run Task", "backlog", "medium", t.TempDir(), nil)

	now := NowISO()
	run := &BrainRun{
		RunID:          "run-1",
		FamilySlug:     "run-task",
		TaskSlug:       "run-task",
		Role:           "worker",
		Provider:       "claude",
		PermissionMode: "auto",
		Status:         "queued",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := UpsertBrainRun(db, run); err != nil {
		t.Fatalf("UpsertBrainRun create: %v", err)
	}

	run.Status = "running"
	run.PID = sql.NullInt64{Int64: 4242, Valid: true}
	run.SessionID = sql.NullString{String: "sess-1", Valid: true}
	run.LogPath = sql.NullString{String: "/tmp/run-1.log", Valid: true}
	run.StartedAt = sql.NullString{String: NowISO(), Valid: true}
	run.UpdatedAt = NowISO()
	if err := UpsertBrainRun(db, run); err != nil {
		t.Fatalf("UpsertBrainRun running: %v", err)
	}

	run.Status = "completed"
	run.FinishedAt = sql.NullString{String: NowISO(), Valid: true}
	run.OutputJSON = sql.NullString{String: `{"status":"completed"}`, Valid: true}
	run.EvidenceJSON = sql.NullString{String: `{"pid":4242}`, Valid: true}
	run.UpdatedAt = NowISO()
	if err := UpsertBrainRun(db, run); err != nil {
		t.Fatalf("UpsertBrainRun completed: %v", err)
	}

	got, err := GetBrainRun(db, "run-1")
	if err != nil {
		t.Fatalf("GetBrainRun: %v", err)
	}
	if got.Status != "completed" || !got.PID.Valid || got.PID.Int64 != 4242 {
		t.Fatalf("unexpected run state: %+v", got)
	}
	if !got.OutputJSON.Valid || !strings.Contains(got.OutputJSON.String, "completed") {
		t.Fatalf("output json missing: %+v", got)
	}
	if got.CreatedAt != now {
		t.Fatalf("created_at changed: got %q, want %q", got.CreatedAt, now)
	}
}

func TestBrainRunListOrdersRetriesAndSynthesizesLegacyAutoRun(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "retry-task", "Retry Task", "backlog", "medium", t.TempDir(), nil)

	first := &BrainRun{
		RunID:          "run-1",
		FamilySlug:     "retry-task",
		TaskSlug:       "retry-task",
		Role:           "worker",
		Provider:       "claude",
		PermissionMode: "auto",
		Status:         "dead",
		CreatedAt:      "2026-05-12T10:00:00Z",
		UpdatedAt:      "2026-05-12T10:00:00Z",
		StartedAt:      sql.NullString{String: "2026-05-12T10:00:00Z", Valid: true},
		FinishedAt:     sql.NullString{String: "2026-05-12T10:01:00Z", Valid: true},
	}
	second := &BrainRun{
		RunID:          "run-2",
		FamilySlug:     "retry-task",
		TaskSlug:       "retry-task",
		Role:           "worker",
		Provider:       "claude",
		PermissionMode: "auto",
		Status:         "completed",
		CreatedAt:      "2026-05-12T11:00:00Z",
		UpdatedAt:      "2026-05-12T11:00:00Z",
		StartedAt:      sql.NullString{String: "2026-05-12T11:00:00Z", Valid: true},
		FinishedAt:     sql.NullString{String: "2026-05-12T11:05:00Z", Valid: true},
	}
	if err := UpsertBrainRun(db, first); err != nil {
		t.Fatalf("UpsertBrainRun first: %v", err)
	}
	if err := UpsertBrainRun(db, second); err != nil {
		t.Fatalf("UpsertBrainRun second: %v", err)
	}

	runs, err := ListBrainRunsForFamily(db, "retry-task", 10)
	if err != nil {
		t.Fatalf("ListBrainRunsForFamily: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("run count = %d, want 2", len(runs))
	}
	if runs[0].RunID != "run-2" || runs[1].RunID != "run-1" {
		t.Fatalf("unexpected order: %+v", runs)
	}

	legacyNow := "2026-05-12T12:00:00Z"
	if _, err := db.Exec(
		`UPDATE tasks SET auto_run_status='dead', auto_run_pid=9999, auto_run_started=?, auto_run_finished=?, auto_run_log=? WHERE slug='retry-task'`,
		legacyNow, "2026-05-12T12:01:00Z", "/tmp/legacy.log",
	); err != nil {
		t.Fatalf("seed legacy auto_run_* fields: %v", err)
	}
	legacy, err := GetBrainRun(db, "legacy:auto-run:retry-task")
	if err != nil {
		t.Fatalf("GetBrainRun legacy: %v", err)
	}
	if !legacy.Legacy || legacy.Status != "dead" || !legacy.PID.Valid || legacy.PID.Int64 != 9999 {
		t.Fatalf("unexpected legacy run: %+v", legacy)
	}
	if !legacy.LogPath.Valid || legacy.LogPath.String != "/tmp/legacy.log" {
		t.Fatalf("legacy log path missing: %+v", legacy)
	}
}

func TestGetBrainRunLegacyMissingRowsReturnNoRows(t *testing.T) {
	db := openTempDB(t)

	_, err := GetBrainRun(db, brainRunLegacyID("missing-task"))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBrainRun legacy missing row error = %v, want sql.ErrNoRows", err)
	}
}

func TestListBrainRunsSince(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "since-task", "Since Task", "backlog", "medium", t.TempDir(), nil)

	// Three runs across two days plus one older than the cutoff.
	mk := func(id, started, finished, status string) *BrainRun {
		r := &BrainRun{
			RunID: id, FamilySlug: "since-task", TaskSlug: "since-task",
			Role: "worker", Provider: "claude", PermissionMode: "auto", Status: status,
			CreatedAt: started, UpdatedAt: started,
			StartedAt: sql.NullString{String: started, Valid: true},
		}
		if finished != "" {
			r.FinishedAt = sql.NullString{String: finished, Valid: true}
		}
		return r
	}
	for _, r := range []*BrainRun{
		mk("old", "2026-05-01T09:00:00Z", "2026-05-01T09:10:00Z", "completed"),
		mk("a", "2026-05-12T10:00:00Z", "2026-05-12T10:05:00Z", "completed"),
		mk("b", "2026-05-13T11:00:00Z", "", "running"), // in-flight
		mk("c", "2026-05-13T12:00:00Z", "2026-05-13T12:30:00Z", "dead"),
	} {
		if err := UpsertBrainRun(db, r); err != nil {
			t.Fatalf("seed %s: %v", r.RunID, err)
		}
	}

	runs, err := ListBrainRunsSince(db, "2026-05-10T00:00:00Z")
	if err != nil {
		t.Fatalf("ListBrainRunsSince: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("run count = %d, want 3 (old is before the cutoff)", len(runs))
	}
	// Most-recent first by start time.
	if runs[0].RunID != "c" || runs[2].RunID != "a" {
		t.Errorf("order = %s..%s, want c..a", runs[0].RunID, runs[2].RunID)
	}
	// In-flight run preserved (no finished_at), not synthesized away.
	var inflight int
	for _, r := range runs {
		if !r.FinishedAt.Valid {
			inflight++
		}
	}
	if inflight != 1 {
		t.Errorf("in-flight runs = %d, want 1", inflight)
	}

	// Empty cutoff returns every row.
	all, err := ListBrainRunsSince(db, "")
	if err != nil {
		t.Fatalf("ListBrainRunsSince(all): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all runs = %d, want 4", len(all))
	}
}
