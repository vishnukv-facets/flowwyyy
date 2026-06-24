package productdb

// read.go is flowwyyy's OWN read layer over the shared flow.db core tables
// (tasks, projects, task_tags). It mirrors the equivalent flowdb queries
// EXACTLY — same columns, scan order, filter logic, and ordering — so that
// flowwyyy reads identical rows without importing flowdb (Phase-3 ownership
// model, seam §11). Parity is enforced by read_test.go, which seeds via flowdb
// and asserts these return identical results.
//
// Writes to these core tables are NOT here: tasks/projects/task_tags are
// official-flow-owned (Bucket O), so flowwyyy mutates them by exec-ing `flow`,
// never by writing directly. Only flowwyyy-owned (Bucket F) tables get write
// helpers in this package.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NowISO returns the current time formatted as RFC3339 — the flowdb-free twin of
// flowdb.NowISO (same format, same local-time semantics).
func NowISO() string {
	return time.Now().Format(time.RFC3339)
}

// NormalizeTag canonicalizes a tag (lowercase, trimmed) — twin of
// flowdb.NormalizeTag.
func NormalizeTag(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NullIfEmpty returns nil for an empty string (so it binds as SQL NULL) and the
// string otherwise — twin of flowdb.NullIfEmpty. Shared by the productdb
// connector/attention write helpers.
func NullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------- models (mirror the shared schema) ----------

// Project mirrors the projects table (twin of flowdb.Project).
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

// Task mirrors the tasks table (twin of flowdb.Task). Field set + order match
// flowdb.Task so a cutover is a near-mechanical flowdb.X → productdb.X swap.
type Task struct {
	Slug               string
	Name               string
	ProjectSlug        sql.NullString
	Status             string
	Kind               string
	PlaybookSlug       sql.NullString
	ParentSlug         sql.NullString
	ForkedFromSlug     sql.NullString
	ForkReason         sql.NullString
	Priority           string
	WorkDir            string
	WaitingOn          sql.NullString
	DueDate            sql.NullString
	Assignee           sql.NullString
	PermissionMode     string
	Model              sql.NullString
	StatusChangedAt    sql.NullString
	SessionProvider    string
	Harness            string
	SessionID          sql.NullString
	SessionStarted     sql.NullString
	SessionLastResumed sql.NullString
	SessionPath        sql.NullString
	WorktreePath       sql.NullString
	InboxSeenAt        sql.NullString
	CreatedAt          string
	UpdatedAt          string
	ArchivedAt         sql.NullString
	DeletedAt          sql.NullString
	AutoRunStatus      sql.NullString
	AutoRunPID         sql.NullInt64
	AutoRunStarted     sql.NullString
	AutoRunFinished    sql.NullString
	AutoRunLog         sql.NullString
}

// TaskFilter mirrors flowdb.TaskFilter.
type TaskFilter struct {
	Status          string
	Project         string
	Priority        string
	Kind            string
	PlaybookSlug    string
	Tag             string
	Since           string
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
	ExcludeDone     bool
}

// ProjectFilter mirrors flowdb.ProjectFilter.
type ProjectFilter struct {
	Status          string
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
}

// ---------- harness/provider normalization (twins of the flowdb helpers) ----------

func normalizeSessionProvider(provider string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(provider)) {
	case "", "claude", "claude-code", "claudecode":
		return "claude", nil
	case "codex", "codex-cli":
		return "codex", nil
	default:
		return "", fmt.Errorf("session provider must be claude|codex, got %q", provider)
	}
}

func normalizeHarnessName(harness string) (string, error) {
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
		if normalized, err := normalizeHarnessName(harness.String); err == nil {
			return normalized
		}
		return strings.TrimSpace(strings.ToLower(harness.String))
	}
	normalized, err := normalizeSessionProvider(sessionProvider)
	if err != nil {
		return "claude"
	}
	return normalized
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

// ---------- task tags ----------

// GetTaskTags returns the tags on a single task, sorted alphabetically — twin of
// flowdb.GetTaskTags.
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

// GetTaskTagsBatch returns tags for many tasks in one query, keyed by task slug
// — twin of flowdb.GetTaskTagsBatch (avoids N+1).
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
