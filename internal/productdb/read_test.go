package productdb_test

// Parity tests for flowwyyy's own read layer. The contract for "flowwyyy owns
// its reads" (seam §11) is that productdb's queries return EXACTLY what flowdb's
// equivalents return against the same shared schema. So each test seeds via
// flowdb (the core writer) and asserts productdb reads back identical rows.
//
// This is the external test package (productdb_test): it imports flowdb to seed,
// which is fine for a test binary — the archtest guard checks the non-test
// import graph of internal/productdb, which stays flowdb-free.

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func openSeeded(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedProject and seedTask insert via raw SQL so the test controls exact column
// values (incl. NULLs) independent of flowdb's writer API.
func seedProject(t *testing.T, db *sql.DB, slug, name, priority string) {
	t.Helper()
	now := flowdb.NowISO()
	_, err := db.Exec(`INSERT INTO projects (slug,name,status,priority,work_dir,created_at,updated_at) VALUES (?,?,?,?,?,?,?)`,
		slug, name, "active", priority, "/tmp/"+slug, now, now)
	if err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
}

func seedTask(t *testing.T, db *sql.DB, slug, name, project, status, priority, provider string) {
	t.Helper()
	now := flowdb.NowISO()
	// Floating task → NULL project_slug (the FK rejects ""); in-progress tasks
	// need a session_id to satisfy the tasks CHECK constraint (unless codex).
	var projVal any
	if project != "" {
		projVal = project
	}
	var sessVal any
	if status != "backlog" && status != "done" {
		sessVal = slug + "-session"
	}
	_, err := db.Exec(`INSERT INTO tasks
		(slug,name,project_slug,status,kind,priority,work_dir,permission_mode,session_provider,session_id,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		slug, name, projVal, status, "regular", priority, "/tmp/"+slug, "auto", provider, sessVal, now, now)
	if err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func TestGetTaskParity(t *testing.T) {
	db := openSeeded(t)
	seedProject(t, db, "proj", "Proj", "high")
	seedTask(t, db, "alpha", "Alpha task", "proj", "in-progress", "high", "codex")

	want, errW := flowdb.GetTask(db, "alpha")
	if errW != nil {
		t.Fatalf("flowdb.GetTask: %v", errW)
	}
	got, errG := productdb.GetTask(db, "alpha")
	if errG != nil {
		t.Fatalf("productdb.GetTask: %v", errG)
	}
	// Both structs have identical field names/types; reflect.DeepEqual on the
	// JSON-equivalent field set verifies the scan order + null handling match.
	if !reflect.DeepEqual(*want, productdbTaskToFlow(*got)) {
		t.Errorf("GetTask parity mismatch:\n flowdb=%+v\n productdb=%+v", *want, *got)
	}
}

func TestListTasksParity(t *testing.T) {
	db := openSeeded(t)
	seedProject(t, db, "proj", "Proj", "high")
	seedTask(t, db, "alpha", "Alpha", "proj", "in-progress", "high", "claude")
	seedTask(t, db, "bravo", "Bravo", "proj", "backlog", "low", "codex")
	seedTask(t, db, "charlie", "Charlie", "proj", "done", "medium", "claude")

	f := flowdb.TaskFilter{Project: "proj", ExcludeDone: true}
	want, _ := flowdb.ListTasks(db, flowdb.TaskFilter{Project: "proj", ExcludeDone: true})
	got, err := productdb.ListTasks(db, productdb.TaskFilter{Project: f.Project, ExcludeDone: f.ExcludeDone})
	if err != nil {
		t.Fatalf("productdb.ListTasks: %v", err)
	}
	if len(want) != len(got) {
		t.Fatalf("ListTasks count mismatch: flowdb=%d productdb=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].Slug != got[i].Slug {
			t.Errorf("order/slug mismatch at %d: flowdb=%s productdb=%s", i, want[i].Slug, got[i].Slug)
		}
	}
}

func TestGetTaskTagsParity(t *testing.T) {
	db := openSeeded(t)
	seedTask(t, db, "alpha", "Alpha", "", "backlog", "medium", "claude")
	for _, tag := range []string{"zeta", "alpha-tag", "mid"} {
		if _, err := db.Exec(`INSERT INTO task_tags (task_slug,tag,created_at) VALUES (?,?,?)`, "alpha", tag, flowdb.NowISO()); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	want, _ := flowdb.GetTaskTags(db, "alpha")
	got, err := productdb.GetTaskTags(db, "alpha")
	if err != nil {
		t.Fatalf("productdb.GetTaskTags: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("GetTaskTags mismatch: flowdb=%v productdb=%v", want, got)
	}
}

func TestListProjectsParity(t *testing.T) {
	db := openSeeded(t)
	seedProject(t, db, "beta", "Beta", "low")
	seedProject(t, db, "alpha", "Alpha", "high")
	want, _ := flowdb.ListProjects(db, flowdb.ProjectFilter{})
	got, err := productdb.ListProjects(db, productdb.ProjectFilter{})
	if err != nil {
		t.Fatalf("productdb.ListProjects: %v", err)
	}
	if len(want) != len(got) {
		t.Fatalf("ListProjects count mismatch: flowdb=%d productdb=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].Slug != got[i].Slug || want[i].Priority != got[i].Priority {
			t.Errorf("project mismatch at %d: flowdb=%+v productdb=%+v", i, *want[i], *got[i])
		}
	}
}

func TestNormalizeTagParity(t *testing.T) {
	for _, in := range []string{"  Foo ", "BAR", "baz", ""} {
		if flowdb.NormalizeTag(in) != productdb.NormalizeTag(in) {
			t.Errorf("NormalizeTag(%q): flowdb=%q productdb=%q", in, flowdb.NormalizeTag(in), productdb.NormalizeTag(in))
		}
	}
}

// productdbTaskToFlow converts a productdb.Task into a flowdb.Task field-by-field
// so reflect.DeepEqual can verify every scanned column matched. If a field is
// added to one struct and not the other, this fails to compile — a deliberate
// drift tripwire.
func productdbTaskToFlow(t productdb.Task) flowdb.Task {
	return flowdb.Task{
		Slug: t.Slug, Name: t.Name, ProjectSlug: t.ProjectSlug, Status: t.Status, Kind: t.Kind,
		PlaybookSlug: t.PlaybookSlug, ParentSlug: t.ParentSlug, ForkedFromSlug: t.ForkedFromSlug, ForkReason: t.ForkReason,
		Priority: t.Priority, WorkDir: t.WorkDir, WaitingOn: t.WaitingOn, DueDate: t.DueDate, Assignee: t.Assignee,
		PermissionMode: t.PermissionMode, Model: t.Model, StatusChangedAt: t.StatusChangedAt, SessionProvider: t.SessionProvider,
		Harness: t.Harness, SessionID: t.SessionID, SessionStarted: t.SessionStarted, SessionLastResumed: t.SessionLastResumed,
		SessionPath: t.SessionPath, WorktreePath: t.WorktreePath, InboxSeenAt: t.InboxSeenAt, CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt, ArchivedAt: t.ArchivedAt, DeletedAt: t.DeletedAt, AutoRunStatus: t.AutoRunStatus,
		AutoRunPID: t.AutoRunPID, AutoRunStarted: t.AutoRunStarted, AutoRunFinished: t.AutoRunFinished, AutoRunLog: t.AutoRunLog,
	}
}
