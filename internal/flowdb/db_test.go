package flowdb

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flow.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertProject(t *testing.T, db *sql.DB, slug, name, wd, priority string) {
	t.Helper()
	now := NowISO()
	_, err := db.Exec(`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at) VALUES (?, ?, 'active', ?, ?, ?, ?)`,
		slug, name, priority, wd, now, now)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

func insertTask(t *testing.T, db *sql.DB, slug, name, status, priority, wd string, project any) {
	t.Helper()
	now := NowISO()
	_, err := db.Exec(`INSERT INTO tasks (slug, name, project_slug, status, priority, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		slug, name, project, status, priority, wd, now, now)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func TestOpenDBCreatesSchema(t *testing.T) {
	db := openTempDB(t)
	for _, tbl := range []string{"projects", "tasks", "brain_runs", "workdirs", "agent_runtime_states"} {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

func TestMigrationAddsAutoRunColumns(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "auto-slug", "Auto task", "backlog", "medium", t.TempDir(), nil)
	if _, err := db.Exec(
		`UPDATE tasks SET auto_run_status='running', auto_run_pid=4242 WHERE slug='auto-slug'`,
	); err != nil {
		t.Fatalf("set auto_run_status: %v", err)
	}
	task, err := GetTask(db, "auto-slug")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !task.AutoRunStatus.Valid || task.AutoRunStatus.String != "running" {
		t.Errorf("AutoRunStatus = %v, want running", task.AutoRunStatus)
	}
	if !task.AutoRunPID.Valid || task.AutoRunPID.Int64 != 4242 {
		t.Errorf("AutoRunPID = %v, want 4242", task.AutoRunPID)
	}
}

func TestMigrationAddsHarnessColumn(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "harness-slug", "Harness task", "backlog", "medium", t.TempDir(), nil)

	has, err := columnExists(db, "tasks", "harness")
	if err != nil {
		t.Fatalf("columnExists(tasks.harness): %v", err)
	}
	if !has {
		t.Fatal("tasks.harness column missing")
	}

	task, err := GetTask(db, "harness-slug")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Harness != "claude" {
		t.Fatalf("Harness = %q, want claude default", task.Harness)
	}
}

func TestMigrationPreservesHarnessAcrossSessionInvariantRebuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")

	pre, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-06-11T00:00:00Z"
	if _, err := pre.Exec(`
		CREATE TABLE projects (
			slug TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			priority TEXT NOT NULL DEFAULT 'medium',
			work_dir TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			archived_at TEXT,
			deleted_at TEXT
		);
		CREATE TABLE tasks (
			slug TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			project_slug TEXT,
			status TEXT NOT NULL DEFAULT 'backlog',
			kind TEXT NOT NULL DEFAULT 'regular',
			playbook_slug TEXT,
			parent_slug TEXT,
			forked_from_slug TEXT,
			fork_reason TEXT,
			priority TEXT NOT NULL DEFAULT 'medium',
			work_dir TEXT NOT NULL,
			waiting_on TEXT,
			due_date TEXT,
			assignee TEXT,
			permission_mode TEXT NOT NULL DEFAULT 'auto',
			model TEXT,
			status_changed_at TEXT,
			session_provider TEXT NOT NULL DEFAULT 'claude',
			session_id TEXT,
			session_started TEXT,
			session_last_resumed TEXT,
			session_path TEXT,
			worktree_path TEXT,
			inbox_seen_at TEXT,
			harness TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			archived_at TEXT,
			deleted_at TEXT,
			CHECK (status = 'backlog' OR session_id IS NOT NULL)
		);
		CREATE TABLE workdirs (
			path TEXT PRIMARY KEY,
			name TEXT,
			description TEXT,
			git_remote TEXT,
			last_used_at TEXT,
			created_at TEXT NOT NULL
		);
		CREATE TABLE schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE agent_runtime_states (
			provider TEXT NOT NULL,
			session_id TEXT NOT NULL,
			task_slug TEXT,
			status TEXT NOT NULL,
			event_kind TEXT NOT NULL,
			message TEXT,
			updated_at TEXT NOT NULL,
			last_seq INTEGER NOT NULL DEFAULT 0,
			raw_json TEXT,
			PRIMARY KEY (provider, session_id)
		);
		INSERT INTO tasks (
			slug, name, status, priority, work_dir, session_provider, session_id,
			session_started, harness, created_at, updated_at
		) VALUES (
			'codex-owned', 'Codex owned', 'in-progress', 'medium', '/tmp/owned',
			'codex', '11111111-1111-4111-8111-111111111111',
			'2026-06-11T00:00:00Z', 'codex', ?, ?
		);
	`, now, now); err != nil {
		pre.Close()
		t.Fatalf("seed pre-migration harness DB: %v", err)
	}
	pre.Close()

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	task, err := GetTask(db, "codex-owned")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Harness != "codex" {
		t.Fatalf("Harness = %q, want codex", task.Harness)
	}
	if task.SessionProvider != "codex" {
		t.Fatalf("SessionProvider = %q, want codex", task.SessionProvider)
	}
}

func TestOpenDBIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")
	db1, err := OpenDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.Close()
	db2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

func TestGitHubEventLogRecordsFirstEventOnly(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "gh-pr-flow-42", "Review flow PR", "backlog", "high", t.TempDir(), nil)

	recorded, err := RecordGitHubEvent(db, GitHubEventLogEntry{
		EventKey:  "review-comment:MDU6PRRC_kwDOAAABBB",
		EventKind: "pr_review_comment",
		TaskSlug:  "gh-pr-flow-42",
		RawJSON:   `{"id":123}`,
	})
	if err != nil {
		t.Fatalf("RecordGitHubEvent first: %v", err)
	}
	if !recorded {
		t.Fatal("first event should be recorded")
	}

	seen, err := HasGitHubEvent(db, "review-comment:MDU6PRRC_kwDOAAABBB")
	if err != nil {
		t.Fatalf("HasGitHubEvent: %v", err)
	}
	if !seen {
		t.Fatal("recorded event should be seen")
	}

	recorded, err = RecordGitHubEvent(db, GitHubEventLogEntry{
		EventKey:  "review-comment:MDU6PRRC_kwDOAAABBB",
		EventKind: "pr_review_comment",
		TaskSlug:  "gh-pr-flow-42",
		RawJSON:   `{"id":123}`,
	})
	if err != nil {
		t.Fatalf("RecordGitHubEvent duplicate: %v", err)
	}
	if recorded {
		t.Fatal("duplicate event should not be recorded")
	}
}

// TestOpenDBConcurrentDoesNotBusy pins that two parallel OpenDB calls
// on the same path don't race during schema setup. Without busy_timeout
// applied at open time, the loser hits SQLITE_BUSY on `pragma
// table_info(tasks)` during runMigrations on slow runners — observed
// as a flaky CI failure on the app-level concurrent-do test.
func TestOpenDBConcurrentDoesNotBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			db, err := OpenDB(path)
			if err != nil {
				errs[i] = err
				return
			}
			db.Close()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

func TestProjectCRUD(t *testing.T) {
	db := openTempDB(t)
	insertProject(t, db, "alpha", "Alpha Project", "/tmp/alpha", "high")
	got, err := GetProject(db, "alpha")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Slug != "alpha" || got.Name != "Alpha Project" || got.Priority != "high" || got.WorkDir != "/tmp/alpha" {
		t.Errorf("unexpected project: %+v", got)
	}
	if _, err := GetProject(db, "nope"); err != sql.ErrNoRows {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
}

func TestListProjectsFilters(t *testing.T) {
	db := openTempDB(t)
	insertProject(t, db, "alpha", "Alpha", "/tmp/alpha", "high")
	insertProject(t, db, "beta", "Beta", "/tmp/beta", "medium")
	insertProject(t, db, "deleted", "Deleted", "/tmp/deleted", "low")
	if _, err := db.Exec(`UPDATE projects SET status='done' WHERE slug='beta'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET archived_at=? WHERE slug='alpha'`, NowISO()); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET deleted_at=? WHERE slug='deleted'`, NowISO()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := ListProjects(db, ProjectFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "beta" {
		t.Errorf("default filter: got %v", got)
	}
	got, err = ListProjects(db, ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("include archived: got %d", len(got))
	}
	got, err = ListProjects(db, ProjectFilter{DeletedOnly: true})
	if err != nil {
		t.Fatalf("list deleted: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "deleted" {
		t.Errorf("deleted only: got %v", got)
	}
}

func TestTaskCRUD(t *testing.T) {
	db := openTempDB(t)
	insertProject(t, db, "proj", "Proj", "/tmp/proj", "medium")
	insertTask(t, db, "work", "Some Work", "backlog", "medium", "/tmp/proj", "proj")
	got, err := GetTask(db, "work")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Slug != "work" || !got.ProjectSlug.Valid || got.ProjectSlug.String != "proj" {
		t.Errorf("unexpected task: %+v", got)
	}
	// Backlog floating task (no project, no session_id — both NULL is
	// allowed for backlog under the session-id invariant).
	insertTask(t, db, "float", "Floating", "backlog", "high", "/tmp/float", nil)
	floating, err := GetTask(db, "float")
	if err != nil {
		t.Fatalf("GetTask floating: %v", err)
	}
	if floating.ProjectSlug.Valid {
		t.Errorf("expected null project_slug")
	}
}

func TestRenameTaskCascadesTaskSlugReferences(t *testing.T) {
	db := openTempDB(t)
	insertTask(t, db, "old-task", "Old Task", "backlog", "medium", t.TempDir(), nil)
	insertTask(t, db, "parent-task", "Parent Task", "backlog", "medium", t.TempDir(), nil)
	insertTask(t, db, "child-task", "Child Task", "backlog", "medium", t.TempDir(), nil)
	now := NowISO()
	if _, err := db.Exec(`UPDATE tasks SET parent_slug = 'old-task' WHERE slug = 'child-task'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('old-task', 'parent-task', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('child-task', 'old-task', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_tags (task_slug, tag, created_at) VALUES ('old-task', 'slack', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAgentRuntimeState(db, AgentRuntimeStateInput{
		Provider:  "claude",
		SessionID: "session-1",
		TaskSlug:  "old-task",
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}

	if err := RenameTask(db, "old-task", "new-task"); err != nil {
		t.Fatalf("RenameTask: %v", err)
	}
	if _, err := GetTask(db, "new-task"); err != nil {
		t.Fatalf("new task missing: %v", err)
	}
	if _, err := GetTask(db, "old-task"); err != sql.ErrNoRows {
		t.Fatalf("old task lookup err = %v, want sql.ErrNoRows", err)
	}
	assertCount := func(query string, want int) {
		t.Helper()
		var got int
		if err := db.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", query, got, want)
		}
	}
	assertCount(`SELECT COUNT(*) FROM tasks WHERE slug = 'child-task' AND parent_slug = 'new-task'`, 1)
	assertCount(`SELECT COUNT(*) FROM task_dependencies WHERE child_slug = 'new-task' AND parent_slug = 'parent-task'`, 1)
	assertCount(`SELECT COUNT(*) FROM task_dependencies WHERE child_slug = 'child-task' AND parent_slug = 'new-task'`, 1)
	assertCount(`SELECT COUNT(*) FROM task_tags WHERE task_slug = 'new-task' AND tag = 'slack'`, 1)
	state, err := AgentRuntimeStateBySessionID(db, "claude", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !state.TaskSlug.Valid || state.TaskSlug.String != "new-task" {
		t.Fatalf("agent runtime task_slug = %+v, want new-task", state.TaskSlug)
	}
}

func TestCodexInProgressCanHavePendingSessionID(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()
	wd := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, created_at, updated_at)
		 VALUES ('codex-pending', 'Codex Pending', 'in-progress', 'medium', ?, 'codex', ?, ?)`,
		wd, now, now,
	); err != nil {
		t.Fatalf("codex in-progress without session_id should be allowed: %v", err)
	}
	task, err := GetTask(db, "codex-pending")
	if err != nil {
		t.Fatal(err)
	}
	if task.SessionProvider != "codex" || task.SessionID.Valid {
		t.Fatalf("codex pending task = %+v", task)
	}

	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, created_at, updated_at)
		 VALUES ('claude-pending', 'Claude Pending', 'in-progress', 'medium', ?, 'claude', ?, ?)`,
		wd, now, now,
	); err == nil {
		t.Fatal("claude in-progress without session_id should violate the invariant")
	}
}

func TestWorkdirUpsert(t *testing.T) {
	db := openTempDB(t)
	if err := UpsertWorkdir(db, "/tmp/repo", "repo", "", "git@github.com:foo/bar.git"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	got, err := GetWorkdir(db, "/tmp/repo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Name.Valid || got.Name.String != "repo" {
		t.Errorf("name: got %+v", got.Name)
	}
}

func TestNowISO(t *testing.T) {
	s := NowISO()
	if len(s) < 19 {
		t.Errorf("NowISO too short: %q", s)
	}
}

func TestMigrationAddsDueDateAndStatusChangedAt(t *testing.T) {
	db := openTempDB(t)
	for _, col := range []string{"due_date", "status_changed_at"} {
		has, err := columnExists(db, "tasks", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !has {
			t.Errorf("column %s should exist after migration", col)
		}
	}
}

func TestMigrationAddsSteeringTraceAuditColumns(t *testing.T) {
	db := openTempDB(t)
	for _, col := range []string{"stage1_reason", "autonomy_action", "autonomy_decision", "autonomy_reason"} {
		has, err := columnExists(db, "steering_trace", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !has {
			t.Errorf("steering_trace.%s should exist after migration", col)
		}
	}
}

func TestOpenDBCreatesSteeringTraceHotPathIndexes(t *testing.T) {
	db := openTempDB(t)
	for _, name := range []string{
		"idx_steering_trace_created_id",
		"idx_steering_trace_disposition_created_id",
		"idx_steering_trace_source_created_id",
		"idx_steering_trace_disposition_source_created_id",
		"idx_steering_trace_funnel",
	} {
		var got string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Fatalf("index %s missing: %v", name, err)
		}
	}
}

func TestMigrationAddsAssignee(t *testing.T) {
	db := openTempDB(t)
	has, err := columnExists(db, "tasks", "assignee")
	if err != nil {
		t.Fatalf("columnExists(assignee): %v", err)
	}
	if !has {
		t.Error("tasks.assignee column should exist after migration")
	}
	// Default for new rows must be NULL ("self") — not an empty string.
	now := NowISO()
	wd := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at) VALUES (?, ?, 'backlog', 'medium', ?, ?, ?)`,
		"a1", "Assignee default", wd, now, now,
	); err != nil {
		t.Fatal(err)
	}
	var assignee sql.NullString
	if err := db.QueryRow(`SELECT assignee FROM tasks WHERE slug='a1'`).Scan(&assignee); err != nil {
		t.Fatal(err)
	}
	if assignee.Valid {
		t.Errorf("default assignee should be NULL; got %q", assignee.String)
	}
}

// TestOpenDBOnPreMigrationDB simulates an existing user upgrading from a
// pre-feat/playbooks flow.db: tasks table exists but lacks the kind and
// playbook_slug columns. OpenDB must apply migrations cleanly without
// CREATE INDEX failing on the missing columns.
//
// Regression test: see commit fixing "no such column: kind" — schemaDDL
// used to include `CREATE INDEX ... ON tasks(kind)` which fails before
// runMigrations gets a chance to ALTER TABLE.
func TestOpenDBOnPreMigrationDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")

	// Create a "pre-migration" DB by hand: just the original tasks schema,
	// no kind/playbook_slug columns, no playbooks table.
	pre, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.Exec(`
		CREATE TABLE projects (
			slug TEXT PRIMARY KEY, name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active', priority TEXT NOT NULL DEFAULT 'medium',
			work_dir TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			archived_at TEXT
		);
		CREATE TABLE tasks (
			slug TEXT PRIMARY KEY, name TEXT NOT NULL,
			project_slug TEXT, status TEXT NOT NULL DEFAULT 'backlog',
			priority TEXT NOT NULL DEFAULT 'medium', work_dir TEXT NOT NULL,
			waiting_on TEXT, session_id TEXT, session_started TEXT,
			session_last_resumed TEXT, created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL, archived_at TEXT
		);
		CREATE TABLE workdirs (
			path TEXT PRIMARY KEY, name TEXT, git_remote TEXT,
			last_used_at TEXT, created_at TEXT NOT NULL
		);
		INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
			VALUES ('legacy', 'Legacy task', 'in-progress', 'high', '/tmp', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
	`); err != nil {
		pre.Close()
		t.Fatalf("seed pre-migration DB: %v", err)
	}
	pre.Close()

	// Now reopen via OpenDB — must not error.
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on pre-migration DB: %v", err)
	}
	defer db.Close()

	// Verify the legacy row is still readable and has kind='regular' default.
	var kind string
	if err := db.QueryRow(`SELECT kind FROM tasks WHERE slug='legacy'`).Scan(&kind); err != nil {
		t.Fatalf("read legacy row after migration: %v", err)
	}
	if kind != "regular" {
		t.Errorf("legacy row kind: got %q, want regular", kind)
	}
	var permissionMode string
	if err := db.QueryRow(`SELECT permission_mode FROM tasks WHERE slug='legacy'`).Scan(&permissionMode); err != nil {
		t.Fatalf("read legacy row permission mode after migration: %v", err)
	}
	if permissionMode != "auto" {
		t.Errorf("legacy row permission mode: got %q, want auto", permissionMode)
	}

	// Verify the new playbooks table is queryable.
	if _, err := db.Exec(`SELECT slug FROM playbooks LIMIT 1`); err != nil {
		t.Errorf("playbooks table not created: %v", err)
	}

	for _, table := range []string{"projects", "tasks", "playbooks"} {
		has, err := columnExists(db, table, "deleted_at")
		if err != nil {
			t.Fatalf("columnExists(%s.deleted_at): %v", table, err)
		}
		if !has {
			t.Fatalf("%s.deleted_at missing after migration", table)
		}
	}

	// Verify the new indexes exist.
	for _, idx := range []string{"idx_tasks_kind", "idx_tasks_playbook_slug", "idx_playbooks_project", "idx_projects_deleted_at", "idx_playbooks_deleted_at", "idx_tasks_deleted_at"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name)
		if err != nil {
			t.Errorf("index %s not created: %v", idx, err)
		}
	}
}

func TestPlaybooksTableExists(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name='playbooks'`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("playbooks table missing")
	}
	rows.Close()

	now := NowISO()
	wd := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO playbooks (slug, name, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"p1", "Playbook 1", wd, now, now,
	); err != nil {
		t.Fatalf("insert playbook: %v", err)
	}
	var slug, name, gotWD string
	err = db.QueryRow(`SELECT slug, name, work_dir FROM playbooks WHERE slug='p1'`).Scan(&slug, &name, &gotWD)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Playbook 1" || gotWD != wd {
		t.Errorf("unexpected: slug=%q name=%q wd=%q", slug, name, gotWD)
	}
}

func TestMigrationAddsTasksKindAndPlaybookSlug(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	hasKind, err := columnExists(db, "tasks", "kind")
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind {
		t.Error("tasks.kind column missing")
	}
	hasPB, err := columnExists(db, "tasks", "playbook_slug")
	if err != nil {
		t.Fatal(err)
	}
	if !hasPB {
		t.Error("tasks.playbook_slug column missing")
	}

	// Default kind should be 'regular' for new rows.
	now := NowISO()
	wd := t.TempDir()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at) VALUES (?, ?, 'backlog', 'medium', ?, ?, ?)`,
		"t1", "Task 1", wd, now, now,
	); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := db.QueryRow(`SELECT kind FROM tasks WHERE slug='t1'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "regular" {
		t.Errorf("default kind: got %q, want regular", kind)
	}
}

func TestPlaybookCRUD(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wd := t.TempDir()
	if err := UpsertPlaybook(db, &Playbook{
		Slug:    "triage-cs",
		Name:    "Triage CS inbox",
		WorkDir: wd,
	}); err != nil {
		t.Fatalf("UpsertPlaybook: %v", err)
	}

	pb, err := GetPlaybook(db, "triage-cs")
	if err != nil {
		t.Fatalf("GetPlaybook: %v", err)
	}
	if pb.Name != "Triage CS inbox" || pb.WorkDir != wd {
		t.Errorf("got %+v", pb)
	}

	pbs, err := ListPlaybooks(db, PlaybookFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pbs) != 1 {
		t.Errorf("ListPlaybooks: got %d, want 1", len(pbs))
	}
}

func TestPlaybookScheduleLifecycle(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wd := t.TempDir()
	if err := UpsertPlaybook(db, &Playbook{Slug: "digest", Name: "Daily digest", WorkDir: wd}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	past := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	// Unscheduled => not due.
	if due, err := DuePlaybooks(db, now.Format(time.RFC3339)); err != nil || len(due) != 0 {
		t.Fatalf("unscheduled DuePlaybooks: due=%d err=%v", len(due), err)
	}

	// Arm with a past next-fire => due.
	if err := SetPlaybookSchedule(db, "digest", "@every 6h", "every 6 hours", past); err != nil {
		t.Fatal(err)
	}
	due, err := DuePlaybooks(db, now.Format(time.RFC3339))
	if err != nil || len(due) != 1 || due[0].Slug != "digest" {
		t.Fatalf("armed DuePlaybooks: due=%d err=%v", len(due), err)
	}
	pb, _ := GetPlaybook(db, "digest")
	if pb.ScheduleSpec.String != "@every 6h" || pb.ScheduleInput.String != "every 6 hours" {
		t.Errorf("schedule not stored: %+v", pb)
	}

	// Future next-fire => not due.
	if err := SetPlaybookSchedule(db, "digest", "@every 6h", "every 6 hours", future); err != nil {
		t.Fatal(err)
	}
	if due, _ := DuePlaybooks(db, now.Format(time.RFC3339)); len(due) != 0 {
		t.Fatalf("future fire should not be due, got %d", len(due))
	}

	// Pause => not due even when armed in the past.
	if err := SetPlaybookSchedule(db, "digest", "@every 6h", "every 6 hours", past); err != nil {
		t.Fatal(err)
	}
	if err := PausePlaybookSchedule(db, "digest"); err != nil {
		t.Fatal(err)
	}
	if due, _ := DuePlaybooks(db, now.Format(time.RFC3339)); len(due) != 0 {
		t.Fatalf("paused should not be due, got %d", len(due))
	}
	pb, _ = GetPlaybook(db, "digest")
	if !pb.SchedulePausedAt.Valid || pb.NextFireAt.Valid {
		t.Errorf("pause should set paused_at and clear next_fire: %+v", pb)
	}

	// Resume re-arms.
	if err := ResumePlaybookSchedule(db, "digest", past); err != nil {
		t.Fatal(err)
	}
	if due, _ := DuePlaybooks(db, now.Format(time.RFC3339)); len(due) != 1 {
		t.Fatalf("resumed should be due, got %d", len(due))
	}

	// Record a fire advances next_fire and stamps history.
	if err := RecordPlaybookFired(db, "digest", now.Format(time.RFC3339), future, "digest--run-1"); err != nil {
		t.Fatal(err)
	}
	pb, _ = GetPlaybook(db, "digest")
	if pb.LastFireRunSlug.String != "digest--run-1" || pb.NextFireAt.String != future {
		t.Errorf("record fired not stamped: %+v", pb)
	}

	// Clear removes the schedule.
	if err := ClearPlaybookSchedule(db, "digest"); err != nil {
		t.Fatal(err)
	}
	pb, _ = GetPlaybook(db, "digest")
	if pb.ScheduleSpec.Valid || pb.NextFireAt.Valid {
		t.Errorf("clear should null schedule: %+v", pb)
	}

	// Pause without a schedule errors (no matching row).
	if err := PausePlaybookSchedule(db, "digest"); err == nil {
		t.Error("pause without schedule should error")
	}
}

func TestTaskWithKindAndPlaybookSlug(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	wd := t.TempDir()
	now := NowISO()
	if err := UpsertPlaybook(db, &Playbook{Slug: "p1", Name: "P1", WorkDir: wd}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, playbook_slug, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'playbook_run', ?, 'medium', ?, ?, ?)`,
		"p1--2026-04-30-10-30", "p1 run", "p1", wd, now, now,
	); err != nil {
		t.Fatal(err)
	}

	task, err := GetTask(db, "p1--2026-04-30-10-30")
	if err != nil {
		t.Fatal(err)
	}
	if task.Kind != "playbook_run" {
		t.Errorf("Kind: got %q", task.Kind)
	}
	if !task.PlaybookSlug.Valid || task.PlaybookSlug.String != "p1" {
		t.Errorf("PlaybookSlug: got %+v", task.PlaybookSlug)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	db.Close()
}

// TestMigrationDeduplicatesSessionIDs simulates Anshul's bug: a DB
// where two non-archived tasks share the same session_id (could
// happen via the now-removed `flow update task --session-id` flag,
// or a manual edit). The naive partial UNIQUE INDEX would fail to
// create. The migration should dedupe (winner = most recent
// updated_at) and then succeed at creating the index.
func TestMigrationDeduplicatesSessionIDs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flow.db")

	// Bootstrap a DB and seed two tasks sharing one session_id.
	// Bypass OpenDB's migration by directly writing to the table
	// after first open, then reopening to trigger migration.
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the unique index that the migration just created so we
	// can simulate a pre-migration state with duplicates.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_tasks_session_id`); err != nil {
		t.Fatal(err)
	}
	const sharedSID = "deadbeef-1111-4222-8333-444455556666"
	now := NowISO()
	old := "2026-01-01T00:00:00Z"
	// Two tasks with same session_id; the one with newer updated_at
	// should win.
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_id, session_started, created_at, updated_at)
		 VALUES ('winner', 'W', 'in-progress', 'medium', '/tmp', ?, ?, ?, ?)`,
		sharedSID, old, old, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_id, session_started, created_at, updated_at)
		 VALUES ('loser', 'L', 'done', 'medium', '/tmp', ?, ?, ?, ?)`,
		sharedSID, old, old, old,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen — the migration's dedupe step should fire.
	db, err = OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen with duplicates failed: %v", err)
	}
	defer db.Close()

	// Winner keeps the session_id and stays in-progress.
	winner, err := GetTask(db, "winner")
	if err != nil {
		t.Fatal(err)
	}
	if !winner.SessionID.Valid || winner.SessionID.String != sharedSID {
		t.Errorf("winner session_id = %+v, want %s", winner.SessionID, sharedSID)
	}
	if winner.Status != "in-progress" {
		t.Errorf("winner status = %q, want in-progress", winner.Status)
	}

	// Loser gets demoted: NULL session_id, status='backlog'.
	loser, err := GetTask(db, "loser")
	if err != nil {
		t.Fatal(err)
	}
	if loser.SessionID.Valid {
		t.Errorf("loser session_id should be NULL, got %q", loser.SessionID.String)
	}
	if loser.Status != "backlog" {
		t.Errorf("loser status = %q, want backlog (demoted)", loser.Status)
	}

	// The unique index should now exist.
	var idxSQL sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_tasks_session_id'`,
	).Scan(&idxSQL); err != nil {
		t.Fatalf("unique index missing after migration: %v", err)
	}
	if !idxSQL.Valid {
		t.Error("unique index DDL is NULL")
	}

	// Idempotent: opening again is a no-op.
	db.Close()
	db2, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("second reopen failed: %v", err)
	}
	db2.Close()
}

func TestSchemaMetaMarker(t *testing.T) {
	db := openTempDB(t)
	has, err := schemaMetaHas(db, "demo-marker")
	if err != nil {
		t.Fatalf("schemaMetaHas: %v", err)
	}
	if has {
		t.Fatal("marker should be absent on a fresh DB")
	}
	if err := schemaMetaSet(db, "demo-marker"); err != nil {
		t.Fatalf("schemaMetaSet: %v", err)
	}
	has, err = schemaMetaHas(db, "demo-marker")
	if err != nil {
		t.Fatalf("schemaMetaHas after set: %v", err)
	}
	if !has {
		t.Fatal("marker should be present after set")
	}
	// Idempotent: second set must not error (INSERT OR IGNORE).
	if err := schemaMetaSet(db, "demo-marker"); err != nil {
		t.Fatalf("schemaMetaSet second: %v", err)
	}
}

func TestStartBlockerIgnoresHierarchyParent(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "sub", "Subtask", "backlog", "medium", wd, nil)
	// Hierarchy only: sub is a subtask of epic, NO dependency row.
	if _, err := db.Exec(`UPDATE tasks SET parent_slug = 'epic' WHERE slug = 'sub'`); err != nil {
		t.Fatalf("set parent_slug: %v", err)
	}
	sub, err := GetTask(db, "sub")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	blocker, err := TaskStartBlockerFor(db, sub)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor: %v", err)
	}
	if blocker != nil {
		t.Fatalf("hierarchy parent must NOT block; got %v", blocker)
	}
}

func TestStartBlockerHonorsDependency(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	insertTask(t, db, "deploy", "Deploy", "backlog", "medium", wd, nil)
	now := NowISO()
	if _, err := db.Exec(
		`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('deploy','setup',?)`, now,
	); err != nil {
		t.Fatalf("insert dep: %v", err)
	}
	deploy, err := GetTask(db, "deploy")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	blocker, err := TaskStartBlockerFor(db, deploy)
	if err != nil {
		t.Fatalf("TaskStartBlockerFor: %v", err)
	}
	if blocker == nil || blocker.Kind != "dependency" {
		t.Fatalf("dependency on non-done task must block; got %v", blocker)
	}
}

func TestTaskDoneAllowsNoSessionID(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('external-close', 'External Close', 'done', 'medium', '/tmp', ?, ?)`,
		now, now,
	); err != nil {
		t.Fatalf("insert done task without session_id: %v", err)
	}
	task, err := GetTask(db, "external-close")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "done" {
		t.Fatalf("status = %q, want done", task.Status)
	}
	if task.SessionID.Valid {
		t.Fatalf("session_id = %q, want NULL", task.SessionID.String)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetTaskHierarchyParent(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "epic", "Epic", "backlog", "medium", wd, nil)
	insertTask(t, db, "sub", "Sub", "backlog", "medium", wd, nil)
	if err := SetTaskHierarchyParent(db, "sub", "epic"); err != nil {
		t.Fatalf("SetTaskHierarchyParent: %v", err)
	}
	got, err := GetTask(db, "sub")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.ParentSlug.Valid || got.ParentSlug.String != "epic" {
		t.Fatalf("parent_slug = %v, want epic", got.ParentSlug)
	}
	if err := ClearTaskHierarchyParent(db, "sub"); err != nil {
		t.Fatalf("ClearTaskHierarchyParent: %v", err)
	}
	got, _ = GetTask(db, "sub")
	if got.ParentSlug.Valid {
		t.Fatalf("parent_slug should be NULL after clear, got %v", got.ParentSlug)
	}
}

func TestSetTaskHierarchyParentRejectsCycle(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "a", "A", "backlog", "medium", wd, nil)
	insertTask(t, db, "b", "B", "backlog", "medium", wd, nil)
	insertTask(t, db, "c", "C", "backlog", "medium", wd, nil)
	mustNoErr(t, SetTaskHierarchyParent(db, "b", "a")) // b ⊂ a
	mustNoErr(t, SetTaskHierarchyParent(db, "c", "b")) // c ⊂ b
	// a ⊂ c would close the cycle a→c→b→a.
	if err := SetTaskHierarchyParent(db, "a", "c"); err == nil {
		t.Fatal("expected hierarchy cycle to be rejected")
	}
	// self-parent rejected too
	if err := SetTaskHierarchyParent(db, "a", "a"); err == nil {
		t.Fatal("expected self-parent to be rejected")
	}
}

func TestAddTaskDependencyDoesNotMirrorParentSlug(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "setup", "Setup", "backlog", "medium", wd, nil)
	insertTask(t, db, "deploy", "Deploy", "backlog", "medium", wd, nil)
	if err := AddTaskDependency(db, "deploy", "setup"); err != nil {
		t.Fatalf("AddTaskDependency: %v", err)
	}
	got, _ := GetTask(db, "deploy")
	if got.ParentSlug.Valid {
		t.Fatalf("dependency must NOT set parent_slug (hierarchy); got %v", got.ParentSlug)
	}
	parents, err := ListParentSlugs(db, "deploy")
	if err != nil {
		t.Fatalf("ListParentSlugs: %v", err)
	}
	if len(parents) != 1 || parents[0] != "setup" {
		t.Fatalf("dependency parents = %v, want [setup]", parents)
	}
}

func TestAddTaskDependencyRejectsCycle(t *testing.T) {
	db := openTempDB(t)
	wd := t.TempDir()
	insertTask(t, db, "a", "A", "backlog", "medium", wd, nil)
	insertTask(t, db, "b", "B", "backlog", "medium", wd, nil)
	insertTask(t, db, "c", "C", "backlog", "medium", wd, nil)
	mustNoErr(t, AddTaskDependency(db, "b", "a")) // b depends on a
	mustNoErr(t, AddTaskDependency(db, "c", "b")) // c depends on b
	// a depends on c would close a→c→b→a.
	if err := AddTaskDependency(db, "a", "c"); err == nil {
		t.Fatal("expected dependency cycle to be rejected")
	}
	if err := AddTaskDependency(db, "a", "a"); err == nil {
		t.Fatal("expected self-dependency to be rejected")
	}
}

func TestMigrateSplitHierarchyDependency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	wd := t.TempDir()
	insertTask(t, db, "parent", "Parent", "done", "medium", wd, nil)
	insertTask(t, db, "child", "Child", "backlog", "medium", wd, nil)
	// Simulate a legacy mirror: a dependency row PLUS the parent_slug mirror
	// pointing at the same edge (what the old AddTaskParent produced).
	now := NowISO()
	if _, err := db.Exec(
		`INSERT INTO task_dependencies (child_slug, parent_slug, created_at) VALUES ('child','parent',?)`, now,
	); err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET parent_slug = 'parent' WHERE slug = 'child'`); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	// Simulate a pre-split (legacy) DB: the first OpenDB unconditionally stamps
	// the split marker, so clear it here so the next open actually runs the
	// split migration against the seeded legacy mirror.
	if _, err := db.Exec(`DELETE FROM schema_meta WHERE key = 'hierarchy_dependency_split'`); err != nil {
		t.Fatalf("clear split marker: %v", err)
	}
	db.Close()

	// Reopen → migration runs.
	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	// The dependency edge is preserved (still blocking).
	parents, _ := ListParentSlugs(db, "child")
	if len(parents) != 1 || parents[0] != "parent" {
		t.Fatalf("dependency must survive migration; got %v", parents)
	}
	// The mirror is nulled (hierarchy starts clean).
	got, _ := GetTask(db, "child")
	if got.ParentSlug.Valid {
		t.Fatalf("legacy parent_slug mirror should be nulled; got %v", got.ParentSlug)
	}
	// Idempotent + non-destructive to NEW hierarchy: set a real hierarchy
	// parent that coincides with the dep, reopen, and confirm it survives
	// (the marker must gate the migration on subsequent opens).
	mustNoErr(t, SetTaskHierarchyParent(db, "child", "parent"))
	db.Close()
	db2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer db2.Close()
	got, _ = GetTask(db2, "child")
	if !got.ParentSlug.Valid || got.ParentSlug.String != "parent" {
		t.Fatalf("new hierarchy edge must survive re-open (marker should gate the migration); got %v", got.ParentSlug)
	}
}

func TestGetSetMeta(t *testing.T) {
	db := openTempDB(t)
	// Unset key reads as empty with no error.
	if v, err := GetMeta(db, "gh_discovery_watermark"); err != nil || v != "" {
		t.Fatalf("unset GetMeta = %q, %v; want empty, nil", v, err)
	}
	if err := SetMeta(db, "gh_discovery_watermark", "2026-06-04T00:00:00Z"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v, _ := GetMeta(db, "gh_discovery_watermark"); v != "2026-06-04T00:00:00Z" {
		t.Fatalf("GetMeta after set = %q", v)
	}
	// Second SetMeta upserts (no PK conflict error).
	if err := SetMeta(db, "gh_discovery_watermark", "2026-07-01T00:00:00Z"); err != nil {
		t.Fatalf("SetMeta upsert: %v", err)
	}
	if v, _ := GetMeta(db, "gh_discovery_watermark"); v != "2026-07-01T00:00:00Z" {
		t.Fatalf("GetMeta after upsert = %q", v)
	}
}

// The withheld-content marker must surface via waiting_on without clobbering an
// operator's own note, and must clear only when it still matches the marker.
func TestWaitingOnIfClearAndClearIfNote(t *testing.T) {
	db := openTempDB(t)
	const note = "withheld connector content — open this task in a supervised (non-bypass) session to review"

	insertTask(t, db, "wh1", "wh1", "backlog", "medium", "/tmp", nil)
	if set, err := SetTaskWaitingOnIfClear(db, "wh1", note); err != nil || !set {
		t.Fatalf("set on empty: set=%v err=%v", set, err)
	}
	if set, _ := SetTaskWaitingOnIfClear(db, "wh1", note); set {
		t.Fatal("second set must be a no-op (already non-empty)")
	}

	// An operator's own note must never be clobbered, and clearing our marker
	// must not touch it.
	insertTask(t, db, "wh2", "wh2", "backlog", "medium", "/tmp", nil)
	if _, err := db.Exec(`UPDATE tasks SET waiting_on=? WHERE slug=?`, "waiting on Manan", "wh2"); err != nil {
		t.Fatal(err)
	}
	if set, _ := SetTaskWaitingOnIfClear(db, "wh2", note); set {
		t.Fatal("must not clobber an operator note")
	}
	if cleared, _ := ClearTaskWaitingOnIfNote(db, "wh2", note); cleared {
		t.Fatal("must not clear an operator note that isn't our marker")
	}

	// Our marker clears once delivered (attended).
	if cleared, err := ClearTaskWaitingOnIfNote(db, "wh1", note); err != nil || !cleared {
		t.Fatalf("clear matching marker: cleared=%v err=%v", cleared, err)
	}
}
