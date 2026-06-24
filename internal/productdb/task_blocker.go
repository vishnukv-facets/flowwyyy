package productdb

// task_blocker.go is the flowdb-free READ-side twin of flowdb's task-start
// gating. It reads task_dependencies (Bucket F — flowwyyy-owned) joined with
// tasks (Bucket O) to decide whether a task may start. It is read-only: the
// dependency-edge writes (AddTaskDependency, …) stay in `flow` (exec), so only
// the blocker evaluation lives here, mirroring flowdb exactly.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PendingParent is one parent task that is preventing the child from starting
// (status != 'done' or row is missing/deleted). Twin of flowdb.PendingParent.
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
// Twin of flowdb.TaskStartBlocker (same Error() text).
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
// dependencies: the child cannot start until every non-deleted parent is done.
// Dependencies are read exclusively from the task_dependencies table; the
// tasks.parent_slug hierarchy column is non-blocking and is ignored here.
//
// Special case: when waiting_on text was written at intake describing a
// dependency and the parent later transitions to done, the note no longer
// reflects reality. We treat it as resolved when ALL parents (≥1) are
// done/not-deleted AND the note mentions at least one parent slug. Unrelated
// waiting_on notes continue to block. Twin of flowdb.TaskStartBlockerFor.
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
// for the given child, joined with tasks for status/name/deleted info. Twin of
// flowdb.loadParentsForBlocker.
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

// EnsureTaskStartable fails when task dependencies or blockers say the task
// should not be started. Twin of flowdb.EnsureTaskStartable.
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
