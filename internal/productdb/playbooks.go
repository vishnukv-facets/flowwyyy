package productdb

// playbooks.go is productdb's flowdb-free READ layer over the playbooks table
// (Bucket O — official flow owns the table and the `flow add/run/update playbook`
// verbs). Reads mirror the flowdb queries exactly; the write/schedule helpers
// (UpsertPlaybook, SetPlaybookSchedule, …) are intentionally absent — flowwyyy
// mutates playbooks by exec-ing `flow`, never directly.

import (
	"database/sql"
	"fmt"
	"strings"
)

// Playbook mirrors the playbooks table (twin of flowdb.Playbook).
type Playbook struct {
	Slug        string
	Name        string
	ProjectSlug sql.NullString
	WorkDir     string
	// Scheduling (all nullable). ScheduleSpec is the canonical cron/descriptor;
	// ScheduleInput is the operator's original phrase. SchedulePausedAt set =>
	// schedule retained but not firing. NextFireAt is the computed next fire;
	// NULL when unscheduled or paused.
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
