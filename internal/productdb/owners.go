package productdb

// owners.go is productdb's flowdb-free READ layer over the owners table
// (Bucket O — official flow owns the table and the `flow owner` verbs:
// start/pause/retire/next). Reads mirror flowdb exactly; every owner mutation
// (activate/pause/retire/set-next-wake/create/update/delete) goes through `flow
// owner` exec, never directly — so no write helpers live here.

import (
	"database/sql"
	"fmt"
	"strings"
)

// Owner mirrors the owners table (twin of flowdb.Owner).
type Owner struct {
	Slug           string
	Name           string
	WorkDir        string
	ProjectSlug    sql.NullString
	Status         string
	Every          string
	NextWakeAt     sql.NullString
	LastTickAt     sql.NullString
	LastTickStatus sql.NullString
	TickPID        sql.NullInt64
	TickStarted    sql.NullString
	Harness        string
	CreatedAt      string
	UpdatedAt      string
	ArchivedAt     sql.NullString
}

type OwnerFilter struct {
	Status          string
	IncludeArchived bool
}

const OwnerCols = "slug, name, work_dir, project_slug, status, every, next_wake_at, last_tick_at, last_tick_status, tick_pid, tick_started, harness, created_at, updated_at, archived_at"

func ScanOwner(row interface{ Scan(dest ...any) error }) (*Owner, error) {
	var o Owner
	var harness sql.NullString
	err := row.Scan(
		&o.Slug, &o.Name, &o.WorkDir, &o.ProjectSlug, &o.Status, &o.Every,
		&o.NextWakeAt, &o.LastTickAt, &o.LastTickStatus, &o.TickPID, &o.TickStarted, &harness,
		&o.CreatedAt, &o.UpdatedAt, &o.ArchivedAt,
	)
	if err != nil {
		return nil, err
	}
	if harness.Valid && strings.TrimSpace(harness.String) != "" {
		o.Harness = harness.String
	} else {
		o.Harness = "claude"
	}
	return &o, nil
}

func GetOwner(db *sql.DB, slug string) (*Owner, error) {
	row := db.QueryRow("SELECT "+OwnerCols+" FROM owners WHERE slug = ?", slug)
	return ScanOwner(row)
}

func ListOwners(db *sql.DB, filter OwnerFilter) ([]*Owner, error) {
	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if !filter.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	q := "SELECT " + OwnerCols + " FROM owners"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY slug"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list owners: %w", err)
	}
	defer rows.Close()
	var out []*Owner
	for rows.Next() {
		o, err := ScanOwner(rows)
		if err != nil {
			return nil, fmt.Errorf("scan owner: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
