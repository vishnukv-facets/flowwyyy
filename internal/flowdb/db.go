// Package db implements the SQLite data layer for flow.
package flowdb

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPermissionMode is used when callers do not explicitly choose one.
const DefaultPermissionMode = "auto"

// SchemaVersion is the compatibility floor exposed by `flow version --json`.
const SchemaVersion = 1

// (idx_tasks_session_id is a partial UNIQUE index that requires a
// dedupe pass against existing data — a flat CREATE UNIQUE INDEX
// would fail on any DB that has two tasks sharing a session_id.
// migrateTasksSessionIDUnique handles both the dedupe and the index
// creation as one idempotent step.)

// ---------- models ----------

// Project mirrors the projects table.
type Project struct {
	Slug       string
	Name       string
	Status     string
	Priority   string
	WorkDir    string
	CreatedAt  string
	UpdatedAt  string
	ArchivedAt sql.NullString
	DeletedAt  sql.NullString
}

// Task mirrors the tasks table. ProjectSlug is nullable for floating tasks.
type Task struct {
	Slug               string
	Name               string
	ProjectSlug        sql.NullString
	Status             string
	Kind               string         // 'regular' | 'playbook_run'
	PlaybookSlug       sql.NullString // set when Kind='playbook_run'
	ParentSlug         sql.NullString // set by `flow spawn --parent`
	ForkedFromSlug     sql.NullString
	ForkReason         sql.NullString
	Priority           string
	WorkDir            string
	WaitingOn          sql.NullString
	DueDate            sql.NullString
	Assignee           sql.NullString
	PermissionMode     string
	Model              sql.NullString // explicit per-task model; empty = resolve at launch
	StatusChangedAt    sql.NullString
	SessionProvider    string
	Harness            string // runtime harness pin; empty DB values read as SessionProvider/claude
	SessionID          sql.NullString
	SessionStarted     sql.NullString
	SessionLastResumed sql.NullString
	SessionPath        sql.NullString
	WorktreePath       sql.NullString
	InboxSeenAt        sql.NullString // bumped when SessionStart consumes inbox.md
	CreatedAt          string
	UpdatedAt          string
	ArchivedAt         sql.NullString
	DeletedAt          sql.NullString
	// Autonomous-run bookkeeping (set by `flow do --auto`). AutoRunStatus
	// is one of 'running' | 'completed' | 'dead'. AutoRunPID is the
	// detached supervisor pid while running.
	AutoRunStatus   sql.NullString
	AutoRunPID      sql.NullInt64
	AutoRunStarted  sql.NullString
	AutoRunFinished sql.NullString
	AutoRunLog      sql.NullString
}

// PendingParent is one parent task that is preventing the child from
// starting (status != 'done' or row is missing/deleted).
type PendingParent struct {
	Slug    string
	Name    string
	Status  string
	Deleted bool
	Missing bool // true when the parent_slug refers to a row that no longer exists.
}

// TaskStartBlocker describes why a task session must not be started yet.
// Kind=="waiting" surfaces the freeform waiting_on note.
// Kind=="dependency" surfaces every non-done parent in Parents.
type TaskStartBlocker struct {
	Kind      string
	TaskSlug  string
	WaitingOn string
	Parents   []PendingParent
}

func (b *TaskStartBlocker) Error() string {
	if b == nil {
		return ""
	}
	switch b.Kind {
	case "waiting":
		return fmt.Sprintf("task %q is blocked: waiting on %s", b.TaskSlug, b.WaitingOn)
	case "dependency":
		if len(b.Parents) == 0 {
			return fmt.Sprintf("task %q is blocked: dependency unresolved", b.TaskSlug)
		}
		if len(b.Parents) == 1 {
			p := b.Parents[0]
			status := p.Status
			if status == "" {
				status = "unknown"
			}
			extra := ""
			if p.Deleted {
				extra = " and is deleted"
			}
			if p.Missing {
				extra += " (missing)"
			}
			if p.Name != "" {
				return fmt.Sprintf("task %q depends on %q (%s, %s%s); complete or clear the dependency before starting",
					b.TaskSlug, p.Slug, p.Name, status, extra)
			}
			return fmt.Sprintf("task %q depends on %q (%s%s); complete or clear the dependency before starting",
				b.TaskSlug, p.Slug, status, extra)
		}
		parts := make([]string, 0, len(b.Parents))
		for _, p := range b.Parents {
			status := p.Status
			if status == "" {
				status = "unknown"
			}
			extra := ""
			if p.Deleted {
				extra = ", deleted"
			}
			if p.Missing {
				extra += ", missing"
			}
			parts = append(parts, fmt.Sprintf("%q (%s%s)", p.Slug, status, extra))
		}
		return fmt.Sprintf("task %q is blocked by %d dependencies: %s; complete or clear them before starting",
			b.TaskSlug, len(b.Parents), strings.Join(parts, ", "))
	default:
		return fmt.Sprintf("task %q is blocked", b.TaskSlug)
	}
}

// TaskStartBlockerFor returns the reason a task should not start, if any.
// waiting_on is an explicit blocker. task_dependencies rows are formal
// dependencies: the child cannot start until every non-deleted parent is
// done. Dependencies are read exclusively from the task_dependencies table;
// the tasks.parent_slug hierarchy column is non-blocking and is ignored here.
//
// Special case: when waiting_on text was written at intake describing a
// dependency (e.g. "depends on notif-autospawn - …") and the parent later
// transitions to done, the waiting_on note no longer reflects reality. We
// treat it as resolved when ALL parents (≥1) are done/not-deleted AND the
// note mentions at least one of the parents' slugs. Unrelated waiting_on
// notes continue to block.
func TaskStartBlockerFor(db *sql.DB, task *Task) (*TaskStartBlocker, error) {
	if task == nil {
		return nil, errors.New("task is nil")
	}
	waiting := strings.TrimSpace(task.WaitingOn.String)
	hasWaiting := task.WaitingOn.Valid && waiting != ""

	parents, err := loadParentsForBlocker(db, task.Slug)
	if err != nil {
		return nil, err
	}

	pendingParents := make([]PendingParent, 0, len(parents))
	allDone := len(parents) > 0
	for _, p := range parents {
		done := !p.Missing && !p.Deleted && p.Status == "done"
		if !done {
			pendingParents = append(pendingParents, p)
			allDone = false
		}
	}

	if hasWaiting && allDone {
		// Stale "depends on <parent>" note left over from intake — drop the
		// waiting_on block if the note mentions any (now-done) parent slug.
		lower := strings.ToLower(waiting)
		for _, p := range parents {
			if p.Slug != "" && strings.Contains(lower, strings.ToLower(p.Slug)) {
				hasWaiting = false
				break
			}
		}
	}

	if hasWaiting {
		return &TaskStartBlocker{
			Kind:      "waiting",
			TaskSlug:  task.Slug,
			WaitingOn: waiting,
		}, nil
	}
	if len(pendingParents) > 0 {
		return &TaskStartBlocker{
			Kind:     "dependency",
			TaskSlug: task.Slug,
			Parents:  pendingParents,
		}, nil
	}
	return nil, nil
}

// loadParentsForBlocker returns one PendingParent per row in task_dependencies
// for the given child, joined with tasks for status/name/deleted info.
// Rows in task_dependencies that reference a now-missing tasks row appear
// with Missing=true (the FK cascade should have cleaned this up, but we
// surface it defensively).
func loadParentsForBlocker(db *sql.DB, childSlug string) ([]PendingParent, error) {
	rows, err := db.Query(`
		SELECT d.parent_slug,
		       COALESCE(t.name, ''),
		       COALESCE(t.status, ''),
		       t.deleted_at,
		       CASE WHEN t.slug IS NULL THEN 1 ELSE 0 END AS missing
		FROM task_dependencies d
		LEFT JOIN tasks t ON t.slug = d.parent_slug
		WHERE d.child_slug = ?
		ORDER BY d.created_at ASC, d.parent_slug ASC
	`, childSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingParent
	for rows.Next() {
		var p PendingParent
		var del sql.NullString
		var missing int
		if err := rows.Scan(&p.Slug, &p.Name, &p.Status, &del, &missing); err != nil {
			return nil, err
		}
		p.Deleted = del.Valid
		p.Missing = missing == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListParentSlugs returns the parent slugs for a child task, ordered by
// when the dependency was added (oldest first).
func ListParentSlugs(db *sql.DB, childSlug string) ([]string, error) {
	rows, err := db.Query(`
		SELECT parent_slug FROM task_dependencies
		WHERE child_slug = ?
		ORDER BY created_at ASC, parent_slug ASC
	`, childSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// wouldCycleDependency reports whether adding the edge "child depends on parent"
// would create a cycle, i.e. `parent` can already reach `child` by following
// depends-on edges. Bounded as a runaway guard.
func wouldCycleDependency(db *sql.DB, child, parent string) (bool, error) {
	if child == parent {
		return true, nil
	}
	visited := make(map[string]bool)
	stack := []string{parent}
	for len(stack) > 0 && len(visited) < 100000 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == child {
			return true, nil
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		rows, err := db.Query(`SELECT parent_slug FROM task_dependencies WHERE child_slug = ?`, cur)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return false, err
			}
			stack = append(stack, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		rows.Close()
	}
	return false, nil
}

// AddTaskDependency declares childSlug as blocked by parentSlug. The child
// cannot start until the parent is done (enforced by TaskStartBlockerFor).
// Idempotent (INSERT OR IGNORE). Does NOT touch tasks.parent_slug (that column
// is hierarchy, a separate concept). Rejects self-loops and cycles.
func AddTaskDependency(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	if childSlug == parentSlug {
		return errors.New("a task cannot depend on itself")
	}
	cyc, err := wouldCycleDependency(db, childSlug, parentSlug)
	if err != nil {
		return err
	}
	if cyc {
		return fmt.Errorf("adding dependency %q → %q would create a cycle", childSlug, parentSlug)
	}
	_, err = db.Exec(
		`INSERT OR IGNORE INTO task_dependencies (child_slug, parent_slug, created_at) VALUES (?, ?, ?)`,
		childSlug, parentSlug, NowISO(),
	)
	return err
}

// RemoveTaskDependency drops the (child, parent) blocking edge if present.
func RemoveTaskDependency(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	_, err := db.Exec(
		`DELETE FROM task_dependencies WHERE child_slug = ? AND parent_slug = ?`,
		childSlug, parentSlug,
	)
	return err
}

// ClearTaskDependencies removes every blocking dependency for the child.
func ClearTaskDependencies(db *sql.DB, childSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	if childSlug == "" {
		return errors.New("child slug is required")
	}
	_, err := db.Exec(`DELETE FROM task_dependencies WHERE child_slug = ?`, childSlug)
	return err
}

// wouldCycleHierarchy reports whether making `child` a subtask of `parent`
// would create a cycle in the parent_slug chain (child already an ancestor of
// parent, or child == parent). Depth-bounded as a runaway guard.
func wouldCycleHierarchy(db *sql.DB, child, parent string) (bool, error) {
	if child == parent {
		return true, nil
	}
	cur := parent
	for i := 0; i < 1000 && cur != ""; i++ {
		if cur == child {
			return true, nil
		}
		var next sql.NullString
		err := db.QueryRow(`SELECT parent_slug FROM tasks WHERE slug = ?`, cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !next.Valid {
			return false, nil
		}
		cur = next.String
	}
	return false, nil
}

// SetTaskHierarchyParent sets childSlug's organizational parent (subtask-of).
// Hierarchy is non-blocking — it never gates task start. Validates existence,
// self-loop, and cycle-freedom.
func SetTaskHierarchyParent(db *sql.DB, childSlug, parentSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	parentSlug = strings.TrimSpace(parentSlug)
	if childSlug == "" || parentSlug == "" {
		return errors.New("child and parent slugs are required")
	}
	if childSlug == parentSlug {
		return errors.New("a task cannot be a subtask of itself")
	}
	var exists string
	if err := db.QueryRow(`SELECT slug FROM tasks WHERE slug = ?`, parentSlug).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("parent task %q not found", parentSlug)
		}
		return err
	}
	cyc, err := wouldCycleHierarchy(db, childSlug, parentSlug)
	if err != nil {
		return err
	}
	if cyc {
		return fmt.Errorf("making %q a subtask of %q would create a hierarchy cycle", childSlug, parentSlug)
	}
	_, err = db.Exec(`UPDATE tasks SET parent_slug = ?, updated_at = ? WHERE slug = ?`,
		parentSlug, NowISO(), childSlug)
	return err
}

// ClearTaskHierarchyParent removes childSlug's organizational parent.
func ClearTaskHierarchyParent(db *sql.DB, childSlug string) error {
	childSlug = strings.TrimSpace(childSlug)
	if childSlug == "" {
		return errors.New("child slug is required")
	}
	_, err := db.Exec(`UPDATE tasks SET parent_slug = NULL, updated_at = ? WHERE slug = ?`,
		NowISO(), childSlug)
	return err
}

// EnsureTaskStartable fails when task dependencies or blockers say the task
// should not be started.
func EnsureTaskStartable(db *sql.DB, task *Task) error {
	blocker, err := TaskStartBlockerFor(db, task)
	if err != nil {
		return err
	}
	if blocker != nil {
		return blocker
	}
	return nil
}

// Workdir mirrors the workdirs convenience registry.
type Workdir struct {
	Path        string
	Name        sql.NullString
	Description sql.NullString
	GitRemote   sql.NullString
	LastUsedAt  sql.NullString
	CreatedAt   string
}

// AgentRuntimeState is the provider hook-backed runtime state for a session.
// It is intentionally separate from tasks.status, which is durable workflow
// state such as backlog/in-progress/done.
type AgentRuntimeState struct {
	Provider  string
	SessionID string
	TaskSlug  sql.NullString
	Status    string
	EventKind string
	Message   sql.NullString
	UpdatedAt string
	LastSeq   int64
	RawJSON   sql.NullString
}

// MonitorEventAction records the single action flow took for a monitor event.
// One row per event is the dedup guard for auto-spawn/draft/ping routing.
type MonitorEventAction struct {
	EventID   string
	Action    string
	TaskSlug  sql.NullString
	Note      sql.NullString
	CreatedAt string
}

// TaskFilter holds optional filters for ListTasks.
type TaskFilter struct {
	Status          string
	Project         string
	Priority        string
	Kind            string // "regular" (default), "playbook_run", or "" for all
	PlaybookSlug    string // optional; filter to runs of one playbook
	Tag             string // optional; only tasks carrying this tag (already normalized)
	Since           string // RFC3339 or "" for no lower bound
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
	ExcludeDone     bool // hide status=done; ignored if Status is set explicitly
}

// ProjectFilter is the equivalent for ListProjects.
type ProjectFilter struct {
	Status          string
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
}

// ---------- lifecycle ----------

// NowISO returns the current time formatted as RFC3339.
func NowISO() string {
	return time.Now().Format(time.RFC3339)
}

// ClearTaskWaitingOn clears a task's waiting_on note (Phase 2 loop-closing: a
// reply arrived on a task you were blocked on, so the wait is resolved). Returns
// true only when a non-empty note was actually cleared, so callers can log/notify
// just on a real transition.
func ClearTaskWaitingOn(db *sql.DB, slug string) (bool, error) {
	res, err := db.Exec(
		`UPDATE tasks SET waiting_on=NULL, updated_at=? WHERE slug=? AND waiting_on IS NOT NULL AND TRIM(waiting_on) != ''`,
		NowISO(), slug,
	)
	if err != nil {
		return false, fmt.Errorf("flowdb: clear waiting_on %q: %w", slug, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetTaskWaitingOnIfClear sets a task's waiting_on note only when it is currently
// empty, so an automated marker (e.g. "withheld connector content") never
// clobbers an operator's own waiting note. Idempotent: once set, the note is
// non-empty so a repeat is a no-op. Returns true only when it actually set it.
func SetTaskWaitingOnIfClear(db *sql.DB, slug, note string) (bool, error) {
	res, err := db.Exec(
		`UPDATE tasks SET waiting_on=?, updated_at=? WHERE slug=? AND (waiting_on IS NULL OR TRIM(waiting_on) = '')`,
		note, NowISO(), slug,
	)
	if err != nil {
		return false, fmt.Errorf("flowdb: set waiting_on %q: %w", slug, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearTaskWaitingOnIfNote clears waiting_on only when it exactly matches note —
// the auto-resolve twin of SetTaskWaitingOnIfClear, so clearing an automated
// marker never wipes an operator's own note. Returns true on a real clear.
func ClearTaskWaitingOnIfNote(db *sql.DB, slug, note string) (bool, error) {
	res, err := db.Exec(
		`UPDATE tasks SET waiting_on=NULL, updated_at=? WHERE slug=? AND waiting_on=?`,
		NowISO(), slug, note,
	)
	if err != nil {
		return false, fmt.Errorf("flowdb: clear waiting_on %q: %w", slug, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NullIfEmpty returns a *string pointing to s, or nil if s is empty.
func NullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NormalizePermissionMode canonicalizes the task-level agent permission mode.
// The DB stores a small flow-facing enum; launch code translates it to the
// current provider-specific CLI flags.
func NormalizePermissionMode(mode string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "":
		return DefaultPermissionMode, nil
	case "default":
		return "default", nil
	case "auto":
		return "auto", nil
	case "bypass", "bypasspermissions", "dangerously-skip-permissions", "dangerously_skip_permissions":
		return "bypass", nil
	default:
		return "", fmt.Errorf("permission mode must be default|auto|bypass, got %q", mode)
	}
}

// NormalizePriority canonicalizes a task or project priority value.
func NormalizePriority(priority string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(priority)) {
	case "high", "h":
		return "high", nil
	case "", "medium", "med", "m":
		return "medium", nil
	case "low", "l":
		return "low", nil
	default:
		return "", fmt.Errorf("priority must be high|medium|low, got %q", priority)
	}
}

// NormalizeSessionProvider canonicalizes the agent/provider used for a task
// session. Claude can accept pre-allocated session ids; Codex sessions are
// captured after launch from Codex's own session store.
func NormalizeSessionProvider(provider string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(provider)) {
	case "", "claude", "claude-code", "claudecode":
		return "claude", nil
	case "codex", "codex-cli":
		return "codex", nil
	default:
		return "", fmt.Errorf("session provider must be claude|codex, got %q", provider)
	}
}

// NormalizeHarnessName canonicalizes the runtime harness stored on a task.
// It currently follows the same supported agent families as session_provider,
// but lives separately so imported upstream harness features have a stable
// runtime pin without replacing Flow Manager's public provider contract.
func NormalizeHarnessName(harness string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(harness)) {
	case "", "claude", "claude-code", "claudecode":
		return "claude", nil
	case "codex", "codex-cli":
		return "codex", nil
	default:
		return "", fmt.Errorf("harness must be claude|codex, got %q", harness)
	}
}

func harnessForScan(sessionProvider string, harness sql.NullString) string {
	if harness.Valid && strings.TrimSpace(harness.String) != "" {
		if normalized, err := NormalizeHarnessName(harness.String); err == nil {
			return normalized
		}
		return strings.TrimSpace(strings.ToLower(harness.String))
	}
	normalized, err := NormalizeSessionProvider(sessionProvider)
	if err != nil {
		return "claude"
	}
	return normalized
}

// OpenDB opens (or creates) the SQLite database at path, ensures the
// schema is present, and runs idempotent migrations.
//
// busy_timeout = 30s is applied via both the DSN (_pragma busy_timeout
// so every pooled connection inherits it) and an explicit PRAGMA exec
// on the primary connection. This covers a worst-case migration-rebuild
// path running concurrently with another OpenDB on the same file (e.g.
// two `flow do` invocations starting from a fresh DB). Without it,
// SQLite's default "fail immediately on a locked DB" surfaces as flaky
// `migrate: pragma table_info(...): database is locked` on slow runners.
func OpenDB(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("_pragma", "busy_timeout(30000)")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 30000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}
	if _, err := db.Exec(coreSchemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Apply any product (non-core) migration sets registered via
	// RegisterMigrations. Core tables (incl. tasks) already exist, so a set
	// may reference them. A binary that never imports a product package
	// registers nothing here and gets a core-only DB.
	for _, s := range registeredSets {
		if err := s.Apply(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply migration set %s: %w", s.Domain, err)
		}
	}
	return db, nil
}

// ---------- project queries ----------

const ProjectCols = "slug, name, status, priority, work_dir, created_at, updated_at, archived_at, deleted_at"

func ScanProject(row interface{ Scan(dest ...any) error }) (*Project, error) {
	var p Project
	err := row.Scan(&p.Slug, &p.Name, &p.Status, &p.Priority, &p.WorkDir, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt, &p.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func GetProject(db *sql.DB, slug string) (*Project, error) {
	row := db.QueryRow("SELECT "+ProjectCols+" FROM projects WHERE slug = ?", slug)
	return ScanProject(row)
}

func ListProjects(db *sql.DB, filter ProjectFilter) ([]*Project, error) {
	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if !filter.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if filter.DeletedOnly {
		where = append(where, "deleted_at IS NOT NULL")
	} else if !filter.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	q := "SELECT " + ProjectCols + " FROM projects"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY slug"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := ScanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- task queries ----------

const TaskCols = "slug, name, project_slug, status, kind, playbook_slug, parent_slug, forked_from_slug, fork_reason, priority, work_dir, waiting_on, due_date, assignee, permission_mode, model, status_changed_at, session_provider, harness, session_id, session_started, session_last_resumed, session_path, worktree_path, inbox_seen_at, created_at, updated_at, archived_at, deleted_at, auto_run_status, auto_run_pid, auto_run_started, auto_run_finished, auto_run_log"

func ScanTask(row interface{ Scan(dest ...any) error }) (*Task, error) {
	var t Task
	var harness sql.NullString
	err := row.Scan(
		&t.Slug, &t.Name, &t.ProjectSlug, &t.Status, &t.Kind, &t.PlaybookSlug, &t.ParentSlug, &t.ForkedFromSlug, &t.ForkReason,
		&t.Priority, &t.WorkDir,
		&t.WaitingOn, &t.DueDate, &t.Assignee, &t.PermissionMode, &t.Model, &t.StatusChangedAt, &t.SessionProvider, &harness, &t.SessionID,
		&t.SessionStarted, &t.SessionLastResumed, &t.SessionPath, &t.WorktreePath, &t.InboxSeenAt, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.DeletedAt,
		&t.AutoRunStatus, &t.AutoRunPID, &t.AutoRunStarted, &t.AutoRunFinished, &t.AutoRunLog,
	)
	if err != nil {
		return nil, err
	}
	t.Harness = harnessForScan(t.SessionProvider, harness)
	return &t, nil
}

func GetTask(db *sql.DB, slug string) (*Task, error) {
	row := db.QueryRow("SELECT "+TaskCols+" FROM tasks WHERE slug = ?", slug)
	return ScanTask(row)
}

// TaskBySessionID returns the task bound to the given Claude session
// UUID, or sql.ErrNoRows if none. The partial unique index on
// tasks(session_id) WHERE session_id IS NOT NULL guarantees at most
// one row. Empty sid is treated as "no binding" and returns ErrNoRows
// without hitting the DB.
func TaskBySessionID(db *sql.DB, sid string) (*Task, error) {
	if sid == "" {
		return nil, sql.ErrNoRows
	}
	row := db.QueryRow("SELECT "+TaskCols+" FROM tasks WHERE session_id = ? AND deleted_at IS NULL LIMIT 1", sid)
	return ScanTask(row)
}

func ListTasks(db *sql.DB, filter TaskFilter) ([]*Task, error) {
	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	} else if filter.ExcludeDone {
		where = append(where, "status != 'done'")
	}
	if filter.Project != "" {
		where = append(where, "project_slug = ?")
		args = append(args, filter.Project)
	}
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.PlaybookSlug != "" {
		where = append(where, "playbook_slug = ?")
		args = append(args, filter.PlaybookSlug)
	}
	if filter.Priority != "" {
		where = append(where, "priority = ?")
		args = append(args, filter.Priority)
	}
	if filter.Tag != "" {
		where = append(where, "slug IN (SELECT task_slug FROM task_tags WHERE tag = ?)")
		args = append(args, filter.Tag)
	}
	if filter.Since != "" {
		where = append(where, "updated_at >= ?")
		args = append(args, filter.Since)
	}
	if !filter.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if filter.DeletedOnly {
		where = append(where, "deleted_at IS NOT NULL")
	} else if !filter.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	q := "SELECT " + TaskCols + " FROM tasks"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += ` ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 ELSE 3 END, slug`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := ScanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- workdir queries ----------

const WorkdirCols = "path, name, description, git_remote, last_used_at, created_at"

func ScanWorkdir(row interface{ Scan(dest ...any) error }) (*Workdir, error) {
	var w Workdir
	err := row.Scan(&w.Path, &w.Name, &w.Description, &w.GitRemote, &w.LastUsedAt, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func GetWorkdir(db *sql.DB, path string) (*Workdir, error) {
	row := db.QueryRow("SELECT "+WorkdirCols+" FROM workdirs WHERE path = ?", path)
	return ScanWorkdir(row)
}

func ListWorkdirs(db *sql.DB) ([]*Workdir, error) {
	q := "SELECT " + WorkdirCols + " FROM workdirs ORDER BY last_used_at IS NULL, last_used_at DESC, path"
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list workdirs: %w", err)
	}
	defer rows.Close()
	var out []*Workdir
	for rows.Next() {
		w, err := ScanWorkdir(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workdir: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func UpsertWorkdir(db *sql.DB, path, name, description, gitRemote string) error {
	now := NowISO()
	_, err := db.Exec(`
		INSERT INTO workdirs (path, name, description, git_remote, last_used_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			name         = COALESCE(NULLIF(excluded.name, ''),        workdirs.name),
			description  = COALESCE(NULLIF(excluded.description, ''), workdirs.description),
			git_remote   = COALESCE(NULLIF(excluded.git_remote, ''),  workdirs.git_remote),
			last_used_at = excluded.last_used_at
	`, path, NullIfEmpty(name), NullIfEmpty(description), NullIfEmpty(gitRemote), now, now)
	if err != nil {
		return fmt.Errorf("upsert workdir %s: %w", path, err)
	}
	return nil
}

// ---------- playbook models ----------

// Playbook mirrors the playbooks table.
type Playbook struct {
	Slug        string
	Name        string
	ProjectSlug sql.NullString
	WorkDir     string
	// Scheduling (all nullable). ScheduleSpec is the canonical cron/descriptor
	// (see internal/schedule); ScheduleInput is the operator's original phrase.
	// SchedulePausedAt set => schedule retained but not firing. NextFireAt is
	// the computed next fire; NULL when unscheduled or paused.
	ScheduleSpec     sql.NullString
	ScheduleInput    sql.NullString
	SchedulePausedAt sql.NullString
	NextFireAt       sql.NullString
	LastFiredAt      sql.NullString
	LastFireRunSlug  sql.NullString
	CreatedAt        string
	UpdatedAt        string
	ArchivedAt       sql.NullString
	DeletedAt        sql.NullString
}

// PlaybookFilter holds optional filters for ListPlaybooks.
type PlaybookFilter struct {
	Project         string
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
}

// ---------- playbook queries ----------

const PlaybookCols = "slug, name, project_slug, work_dir, schedule_spec, schedule_input, schedule_paused_at, next_fire_at, last_fired_at, last_fire_run_slug, created_at, updated_at, archived_at, deleted_at"

func ScanPlaybook(row interface{ Scan(dest ...any) error }) (*Playbook, error) {
	var p Playbook
	err := row.Scan(
		&p.Slug, &p.Name, &p.ProjectSlug, &p.WorkDir,
		&p.ScheduleSpec, &p.ScheduleInput, &p.SchedulePausedAt, &p.NextFireAt, &p.LastFiredAt, &p.LastFireRunSlug,
		&p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt, &p.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func GetPlaybook(db *sql.DB, slug string) (*Playbook, error) {
	row := db.QueryRow("SELECT "+PlaybookCols+" FROM playbooks WHERE slug = ?", slug)
	return ScanPlaybook(row)
}

func ListPlaybooks(db *sql.DB, filter PlaybookFilter) ([]*Playbook, error) {
	var where []string
	var args []any
	if filter.Project != "" {
		where = append(where, "project_slug = ?")
		args = append(args, filter.Project)
	}
	if !filter.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if filter.DeletedOnly {
		where = append(where, "deleted_at IS NOT NULL")
	} else if !filter.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	q := "SELECT " + PlaybookCols + " FROM playbooks"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY slug"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list playbooks: %w", err)
	}
	defer rows.Close()
	var out []*Playbook
	for rows.Next() {
		p, err := ScanPlaybook(rows)
		if err != nil {
			return nil, fmt.Errorf("scan playbook: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- task tag queries ----------

// NormalizeTag canonicalizes a tag for storage and comparison: trim
// whitespace, lowercase. Returns "" for input that contains nothing
// after trimming — callers should treat "" as invalid.
func NormalizeTag(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// AddTaskTag attaches a tag to a task. Idempotent: re-adding an existing
// (task_slug, tag) pair is a no-op via INSERT OR IGNORE.
func AddTaskTag(db *sql.DB, slug, tag string) error {
	t := NormalizeTag(tag)
	if t == "" {
		return fmt.Errorf("tag is empty")
	}
	_, err := db.Exec(
		`INSERT OR IGNORE INTO task_tags (task_slug, tag, created_at) VALUES (?, ?, ?)`,
		slug, t, NowISO(),
	)
	if err != nil {
		return fmt.Errorf("add tag %s on %s: %w", t, slug, err)
	}
	return nil
}

// RemoveTaskTag detaches a tag from a task. No error if the pair doesn't
// exist; caller can pre-check via GetTaskTags.
func RemoveTaskTag(db *sql.DB, slug, tag string) error {
	t := NormalizeTag(tag)
	if t == "" {
		return fmt.Errorf("tag is empty")
	}
	_, err := db.Exec(`DELETE FROM task_tags WHERE task_slug = ? AND tag = ?`, slug, t)
	if err != nil {
		return fmt.Errorf("remove tag %s from %s: %w", t, slug, err)
	}
	return nil
}

// ClearTaskTags removes all tags from a task.
func ClearTaskTags(db *sql.DB, slug string) error {
	_, err := db.Exec(`DELETE FROM task_tags WHERE task_slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("clear tags for %s: %w", slug, err)
	}
	return nil
}

// GetTaskTags returns the tags on a single task, sorted alphabetically.
func GetTaskTags(db *sql.DB, slug string) ([]string, error) {
	rows, err := db.Query(`SELECT tag FROM task_tags WHERE task_slug = ? ORDER BY tag`, slug)
	if err != nil {
		return nil, fmt.Errorf("get tags for %s: %w", slug, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTaskTagsBatch returns tags for many tasks in one query, keyed by
// task slug. Used by list output to avoid N+1 queries.
func GetTaskTagsBatch(db *sql.DB, slugs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(slugs))
	if len(slugs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(slugs)-1) + "?"
	args := make([]any, 0, len(slugs))
	for _, s := range slugs {
		args = append(args, s)
	}
	q := `SELECT task_slug, tag FROM task_tags WHERE task_slug IN (` + placeholders + `) ORDER BY task_slug, tag`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("batch get tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, tag string
		if err := rows.Scan(&slug, &tag); err != nil {
			return nil, err
		}
		out[slug] = append(out[slug], tag)
	}
	return out, rows.Err()
}

// TagCount is the (tag, task-count) pair returned by ListAllTags.
type TagCount struct {
	Tag   string
	Count int
}

// ListAllTags returns every distinct tag in use, with the number of
// non-archived, non-deleted tasks that carry it. Sorted by count descending, then
// tag ascending — most-used tags first.
func ListAllTags(db *sql.DB) ([]TagCount, error) {
	rows, err := db.Query(`
		SELECT t.tag, COUNT(*) AS n
		FROM task_tags t
		JOIN tasks tk ON tk.slug = t.task_slug
		WHERE tk.archived_at IS NULL AND tk.deleted_at IS NULL
		GROUP BY t.tag
		ORDER BY n DESC, t.tag ASC`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// GitHubEventLogEntry is one processed GitHub event/comment recorded for
// idempotency. EventKey is a stable external key such as
// "pr:owner/repo#123:review_requested" or "review-comment:<node_id>".
type GitHubEventLogEntry struct {
	EventKey  string
	EventKind string
	TaskSlug  string
	RawJSON   string
}

// HasGitHubEvent reports whether eventKey has already been processed.
func HasGitHubEvent(db *sql.DB, eventKey string) (bool, error) {
	key := strings.TrimSpace(eventKey)
	if key == "" {
		return false, fmt.Errorf("github event key is empty")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM github_event_log WHERE event_key = ?`, key).Scan(&n); err != nil {
		return false, fmt.Errorf("check github event %s: %w", key, err)
	}
	return n > 0, nil
}

// RecordGitHubEvent records a processed GitHub event. The returned bool is
// true only for the first insert; duplicate keys return false with nil error.
func RecordGitHubEvent(db *sql.DB, entry GitHubEventLogEntry) (bool, error) {
	key := strings.TrimSpace(entry.EventKey)
	if key == "" {
		return false, fmt.Errorf("github event key is empty")
	}
	kind := strings.TrimSpace(entry.EventKind)
	if kind == "" {
		return false, fmt.Errorf("github event kind is empty")
	}
	var taskSlug any
	if slug := strings.TrimSpace(entry.TaskSlug); slug != "" {
		taskSlug = slug
	}
	res, err := db.Exec(
		`INSERT OR IGNORE INTO github_event_log (event_key, event_kind, task_slug, raw_json, processed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, kind, taskSlug, strings.TrimSpace(entry.RawJSON), NowISO(),
	)
	if err != nil {
		return false, fmt.Errorf("record github event %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record github event %s rows affected: %w", key, err)
	}
	return n > 0, nil
}

// UpsertPlaybook inserts a new playbook or updates an existing row by slug.
// Updates touch name, project_slug, work_dir, updated_at; archived_at is
// not touched here (use a dedicated archive command).
func UpsertPlaybook(db *sql.DB, pb *Playbook) error {
	now := NowISO()
	if pb.CreatedAt == "" {
		pb.CreatedAt = now
	}
	pb.UpdatedAt = now
	_, err := db.Exec(`
		INSERT INTO playbooks (slug, name, project_slug, work_dir, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			name         = excluded.name,
			project_slug = excluded.project_slug,
			work_dir     = excluded.work_dir,
			updated_at   = excluded.updated_at
	`, pb.Slug, pb.Name, pb.ProjectSlug, pb.WorkDir, pb.CreatedAt, pb.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert playbook %s: %w", pb.Slug, err)
	}
	return nil
}

// ---------- playbook schedule operations ----------

// SetPlaybookSchedule attaches (or replaces) a recurring schedule. spec is the
// canonical cron/descriptor, input the operator's original phrase, nextFireAt
// the precomputed next fire (RFC3339). Setting a schedule clears any paused
// flag — re-scheduling a paused playbook is an implicit resume.
func SetPlaybookSchedule(db *sql.DB, slug, spec, input, nextFireAt string) error {
	res, err := db.Exec(
		`UPDATE playbooks SET schedule_spec=?, schedule_input=?, schedule_paused_at=NULL, next_fire_at=?, updated_at=? WHERE slug=?`,
		spec, input, nextFireAt, NowISO(), slug,
	)
	return affectedPlaybookRow(res, err, "set schedule", slug)
}

// ClearPlaybookSchedule removes the schedule entirely (keeps last-fired
// history for display).
func ClearPlaybookSchedule(db *sql.DB, slug string) error {
	res, err := db.Exec(
		`UPDATE playbooks SET schedule_spec=NULL, schedule_input=NULL, schedule_paused_at=NULL, next_fire_at=NULL, updated_at=? WHERE slug=?`,
		NowISO(), slug,
	)
	return affectedPlaybookRow(res, err, "clear schedule", slug)
}

// PausePlaybookSchedule retains the schedule but stops it firing (next_fire_at
// cleared so DuePlaybooks skips it).
func PausePlaybookSchedule(db *sql.DB, slug string) error {
	now := NowISO()
	res, err := db.Exec(
		`UPDATE playbooks SET schedule_paused_at=?, next_fire_at=NULL, updated_at=? WHERE slug=? AND schedule_spec IS NOT NULL`,
		now, now, slug,
	)
	return affectedPlaybookRow(res, err, "pause schedule", slug)
}

// ResumePlaybookSchedule clears the paused flag and arms the next fire.
func ResumePlaybookSchedule(db *sql.DB, slug, nextFireAt string) error {
	res, err := db.Exec(
		`UPDATE playbooks SET schedule_paused_at=NULL, next_fire_at=?, updated_at=? WHERE slug=? AND schedule_spec IS NOT NULL`,
		nextFireAt, NowISO(), slug,
	)
	return affectedPlaybookRow(res, err, "resume schedule", slug)
}

// SetPlaybookNextFire advances only the next-fire time, without stamping a
// fire. Used when the scheduler skips (overlap) or a fire errors, so a single
// due playbook doesn't hot-loop the scheduler.
func SetPlaybookNextFire(db *sql.DB, slug, nextFireAt string) error {
	res, err := db.Exec(
		`UPDATE playbooks SET next_fire_at=?, updated_at=? WHERE slug=?`,
		nextFireAt, NowISO(), slug,
	)
	return affectedPlaybookRow(res, err, "set next fire", slug)
}

// RecordPlaybookFired stamps a fire: records last fire time + run slug and
// advances next_fire_at to the recomputed value.
func RecordPlaybookFired(db *sql.DB, slug, firedAt, nextFireAt, runSlug string) error {
	res, err := db.Exec(
		`UPDATE playbooks SET last_fired_at=?, last_fire_run_slug=?, next_fire_at=?, updated_at=? WHERE slug=?`,
		firedAt, runSlug, nextFireAt, NowISO(), slug,
	)
	return affectedPlaybookRow(res, err, "record fired", slug)
}

// DuePlaybooks returns active (non-archived, non-deleted) playbooks whose
// schedule is armed (spec set, not paused) and whose next_fire_at is at or
// before now.
func DuePlaybooks(db *sql.DB, nowISO string) ([]*Playbook, error) {
	now, err := time.Parse(time.RFC3339, nowISO)
	if err != nil {
		return nil, fmt.Errorf("due playbooks: parse now %q: %w", nowISO, err)
	}
	rows, err := db.Query(
		"SELECT " + PlaybookCols + ` FROM playbooks
		 WHERE archived_at IS NULL AND deleted_at IS NULL
		   AND schedule_spec IS NOT NULL AND schedule_paused_at IS NULL
		   AND next_fire_at IS NOT NULL
		 ORDER BY slug`,
	)
	if err != nil {
		return nil, fmt.Errorf("due playbooks: %w", err)
	}
	defer rows.Close()
	var out []*Playbook
	for rows.Next() {
		p, err := ScanPlaybook(rows)
		if err != nil {
			return nil, fmt.Errorf("due playbooks: scan: %w", err)
		}
		fire, err := time.Parse(time.RFC3339, p.NextFireAt.String)
		if err != nil {
			continue
		}
		if !fire.After(now) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func affectedPlaybookRow(res sql.Result, err error, op, slug string) error {
	if err != nil {
		return fmt.Errorf("%s playbook %s: %w", op, slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s playbook %s: no matching playbook (missing, or no schedule set)", op, slug)
	}
	return nil
}

// ---------- rename operations ----------
//
// The schema doesn't declare ON UPDATE CASCADE on any foreign key, so a
// slug change has to walk every reference explicitly. PRAGMA
// defer_foreign_keys = ON inside the transaction defers validation to
// commit, which lets us write parent and children in any order without
// tripping mid-transaction orphan checks. Filesystem moves of
// ~/.flow/{projects,tasks,playbooks}/<slug>/ are the caller's job — the
// DB layer doesn't know the flow root.

// ErrSlugTaken is returned by Rename* when the destination slug already
// exists in the same entity table.
var ErrSlugTaken = errors.New("slug already taken")

// RenameProject changes a project's slug and cascades the change to every
// table that references projects(slug): tasks.project_slug,
// playbooks.project_slug, and owners.project_slug.
// No-op if old == new.
func RenameProject(db *sql.DB, oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE slug = ?`, newSlug).Scan(&n); err != nil {
		return fmt.Errorf("check target slug: %w", err)
	}
	if n > 0 {
		return ErrSlugTaken
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	now := NowISO()
	if _, err := tx.Exec(`UPDATE projects SET slug=?, updated_at=? WHERE slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("update projects.slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET project_slug=?, updated_at=? WHERE project_slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("cascade tasks.project_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE playbooks SET project_slug=?, updated_at=? WHERE project_slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("cascade playbooks.project_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE owners SET project_slug=?, updated_at=? WHERE project_slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("cascade owners.project_slug: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// RenameTask changes a task's slug and cascades the change to every
// table that references tasks(slug). No-op if old == new.
func RenameTask(db *sql.DB, oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE slug = ?`, newSlug).Scan(&n); err != nil {
		return fmt.Errorf("check target slug: %w", err)
	}
	if n > 0 {
		return ErrSlugTaken
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	now := NowISO()
	if _, err := tx.Exec(`UPDATE tasks SET slug=?, updated_at=? WHERE slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("update tasks.slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET parent_slug=? WHERE parent_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade tasks.parent_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET forked_from_slug=? WHERE forked_from_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade tasks.forked_from_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE task_dependencies SET child_slug=? WHERE child_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade task_dependencies.child_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE task_dependencies SET parent_slug=? WHERE parent_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade task_dependencies.parent_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE task_tags SET task_slug=? WHERE task_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade task_tags.task_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE task_links SET from_slug=? WHERE from_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade task_links.from_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE task_links SET to_slug=? WHERE to_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade task_links.to_slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE agent_runtime_states SET task_slug=? WHERE task_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade agent_runtime_states.task_slug: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// RenamePlaybook changes a playbook's slug and cascades the change to
// every table that references playbooks(slug): tasks.playbook_slug.
// No-op if old == new.
func RenamePlaybook(db *sql.DB, oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM playbooks WHERE slug = ?`, newSlug).Scan(&n); err != nil {
		return fmt.Errorf("check target slug: %w", err)
	}
	if n > 0 {
		return ErrSlugTaken
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	now := NowISO()
	if _, err := tx.Exec(`UPDATE playbooks SET slug=?, updated_at=? WHERE slug=?`, newSlug, now, oldSlug); err != nil {
		return fmt.Errorf("update playbooks.slug: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET playbook_slug=? WHERE playbook_slug=?`, newSlug, oldSlug); err != nil {
		return fmt.Errorf("cascade tasks.playbook_slug: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
