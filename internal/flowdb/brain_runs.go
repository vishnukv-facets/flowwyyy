package flowdb

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const brainRunLegacyPrefix = "legacy:auto-run:"

// BrainRun mirrors the persistent run ledger. The NULL-able fields are kept in
// sql.Null* form so the app layer can preserve "unknown" vs empty-string
// distinctions when it writes or updates a row.
type BrainRun struct {
	RunID          string
	FamilySlug     string
	TaskSlug       string
	PlanID         sql.NullString
	Role           string
	Provider       string
	RequestedModel sql.NullString
	RequestedTier  sql.NullString
	ResolvedModel  sql.NullString
	PermissionMode string
	Status         string
	PID            sql.NullInt64
	SessionID      sql.NullString
	LogPath        sql.NullString
	InputSummary   sql.NullString
	OutputJSON     sql.NullString
	EvidenceJSON   sql.NullString
	ErrorText      sql.NullString
	StartedAt      sql.NullString
	FinishedAt     sql.NullString
	CreatedAt      string
	UpdatedAt      string

	// Legacy marks a synthesized compatibility row backed by tasks.auto_run_*.
	Legacy bool
}

const BrainRunCols = "run_id, family_slug, task_slug, plan_id, role, provider, requested_model, requested_tier, resolved_model, permission_mode, status, pid, session_id, log_path, input_summary, output_json, evidence_json, error_text, started_at, finished_at, created_at, updated_at"

func ScanBrainRun(row interface{ Scan(dest ...any) error }) (*BrainRun, error) {
	var r BrainRun
	err := row.Scan(
		&r.RunID, &r.FamilySlug, &r.TaskSlug, &r.PlanID, &r.Role, &r.Provider,
		&r.RequestedModel, &r.RequestedTier, &r.ResolvedModel, &r.PermissionMode,
		&r.Status, &r.PID, &r.SessionID, &r.LogPath, &r.InputSummary,
		&r.OutputJSON, &r.EvidenceJSON, &r.ErrorText, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpsertBrainRun persists a run row or updates the mutable fields on conflict.
// created_at is preserved on existing rows so the original insertion time stays
// stable across lifecycle updates.
func UpsertBrainRun(db *sql.DB, run *BrainRun) error {
	if run == nil {
		return errors.New("brain run is nil")
	}
	if strings.TrimSpace(run.RunID) == "" {
		return errors.New("brain run id is required")
	}
	if strings.TrimSpace(run.FamilySlug) == "" {
		return errors.New("brain run family slug is required")
	}
	if strings.TrimSpace(run.TaskSlug) == "" {
		return errors.New("brain run task slug is required")
	}
	if strings.TrimSpace(run.Role) == "" {
		return errors.New("brain run role is required")
	}
	if strings.TrimSpace(run.Provider) == "" {
		return errors.New("brain run provider is required")
	}
	if strings.TrimSpace(run.PermissionMode) == "" {
		run.PermissionMode = DefaultPermissionMode
	}
	if strings.TrimSpace(run.Status) == "" {
		return errors.New("brain run status is required")
	}
	if strings.TrimSpace(run.CreatedAt) == "" {
		run.CreatedAt = NowISO()
	}
	if strings.TrimSpace(run.UpdatedAt) == "" {
		run.UpdatedAt = run.CreatedAt
	}
	_, err := db.Exec(
		`INSERT INTO brain_runs (
			run_id, family_slug, task_slug, plan_id, role, provider,
			requested_model, requested_tier, resolved_model, permission_mode,
			status, pid, session_id, log_path, input_summary, output_json,
			evidence_json, error_text, started_at, finished_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			family_slug = excluded.family_slug,
			task_slug = excluded.task_slug,
			plan_id = excluded.plan_id,
			role = excluded.role,
			provider = excluded.provider,
			requested_model = excluded.requested_model,
			requested_tier = excluded.requested_tier,
			resolved_model = excluded.resolved_model,
			permission_mode = excluded.permission_mode,
			status = excluded.status,
			pid = excluded.pid,
			session_id = excluded.session_id,
			log_path = excluded.log_path,
			input_summary = excluded.input_summary,
			output_json = excluded.output_json,
			evidence_json = excluded.evidence_json,
			error_text = excluded.error_text,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			updated_at = excluded.updated_at`,
		run.RunID, run.FamilySlug, run.TaskSlug, run.PlanID, run.Role, run.Provider,
		run.RequestedModel, run.RequestedTier, run.ResolvedModel, run.PermissionMode,
		run.Status, run.PID, run.SessionID, run.LogPath, run.InputSummary,
		run.OutputJSON, run.EvidenceJSON, run.ErrorText, run.StartedAt, run.FinishedAt,
		run.CreatedAt, run.UpdatedAt,
	)
	return err
}

func GetBrainRun(db *sql.DB, runID string) (*BrainRun, error) {
	row := db.QueryRow(`SELECT `+BrainRunCols+` FROM brain_runs WHERE run_id = ?`, runID)
	run, err := ScanBrainRun(row)
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || !strings.HasPrefix(runID, brainRunLegacyPrefix) {
		return nil, err
	}
	taskSlug := strings.TrimPrefix(runID, brainRunLegacyPrefix)
	task, tErr := GetTask(db, taskSlug)
	if tErr != nil {
		return nil, tErr
	}
	root, rootErr := TaskFamilyRoot(db, taskSlug)
	if rootErr != nil {
		return nil, rootErr
	}
	legacy := brainRunFromLegacyTask(task, root)
	if legacy == nil {
		return nil, sql.ErrNoRows
	}
	return legacy, nil
}

// TaskFamilyRoot walks parent_slug to find the top-most known ancestor for the
// supplied task slug. If an intermediate parent row is missing, the current
// known task becomes the family root so a broken hierarchy still groups its
// descendants together.
func TaskFamilyRoot(db *sql.DB, taskSlug string) (string, error) {
	taskSlug = strings.TrimSpace(taskSlug)
	if taskSlug == "" {
		return "", errors.New("task slug is required")
	}
	current := taskSlug
	first := true
	seen := map[string]struct{}{}
	for {
		if _, ok := seen[current]; ok {
			return "", fmt.Errorf("task family cycle detected at %q", current)
		}
		seen[current] = struct{}{}
		var parent sql.NullString
		err := db.QueryRow(`SELECT parent_slug FROM tasks WHERE slug = ?`, current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			if first {
				return "", fmt.Errorf("task %q not found", taskSlug)
			}
			return current, nil
		}
		if err != nil {
			return "", err
		}
		first = false
		if !parent.Valid || strings.TrimSpace(parent.String) == "" {
			return current, nil
		}
		current = strings.TrimSpace(parent.String)
	}
}

func listTaskFamily(db *sql.DB, rootSlug string) ([]*Task, error) {
	rootSlug = strings.TrimSpace(rootSlug)
	if rootSlug == "" {
		return nil, errors.New("family root slug is required")
	}
	rows, err := db.Query(
		`WITH RECURSIVE family(slug) AS (
			SELECT slug FROM tasks WHERE slug = ?
			UNION
			SELECT t.slug FROM tasks t
			JOIN family f ON t.parent_slug = f.slug
		)
		SELECT `+TaskCols+` FROM tasks
		WHERE slug IN (SELECT slug FROM family)
		ORDER BY created_at, slug`,
		rootSlug,
	)
	if err != nil {
		return nil, fmt.Errorf("list task family: %w", err)
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := ScanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func ListBrainRunsForFamily(db *sql.DB, familySlug string, limit int) ([]*BrainRun, error) {
	familySlug = strings.TrimSpace(familySlug)
	if familySlug == "" {
		return nil, errors.New("family slug is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT `+BrainRunCols+` FROM brain_runs
		 WHERE family_slug = ?
		 ORDER BY COALESCE(started_at, created_at) DESC, created_at DESC, run_id DESC`,
		familySlug,
	)
	if err != nil {
		return nil, fmt.Errorf("list brain runs: %w", err)
	}
	defer rows.Close()
	var runs []*BrainRun
	hasRun := map[string]bool{}
	for rows.Next() {
		run, err := ScanBrainRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
		hasRun[run.TaskSlug] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tasks, err := listTaskFamily(db, familySlug)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if hasRun[task.Slug] {
			continue
		}
		if legacy := brainRunFromTask(task, familySlug); legacy != nil {
			runs = append(runs, legacy)
		}
	}

	sort.SliceStable(runs, func(i, j int) bool {
		ti := brainRunSortTime(runs[i])
		tj := brainRunSortTime(runs[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if runs[i].CreatedAt != runs[j].CreatedAt {
			return runs[i].CreatedAt > runs[j].CreatedAt
		}
		return runs[i].RunID > runs[j].RunID
	})
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// ListBrainRunsSince returns ledger rows whose start time (started_at, falling
// back to created_at) is at or after `since` (an RFC3339 string), most-recent
// first. Unlike ListBrainRunsForFamily it does NOT synthesize legacy
// task-backed rows — it reads the live brain_runs ledger directly, which is
// what the analytics time-series needs (a row per real run, including those
// still in flight). An empty `since` returns every row.
func ListBrainRunsSince(db *sql.DB, since string) ([]*BrainRun, error) {
	since = strings.TrimSpace(since)
	q := `SELECT ` + BrainRunCols + ` FROM brain_runs`
	var args []any
	if since != "" {
		q += ` WHERE COALESCE(NULLIF(started_at, ''), created_at) >= ?`
		args = append(args, since)
	}
	q += ` ORDER BY COALESCE(NULLIF(started_at, ''), created_at) DESC, created_at DESC, run_id DESC`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list brain runs since: %w", err)
	}
	defer rows.Close()
	var runs []*BrainRun
	for rows.Next() {
		run, err := ScanBrainRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func brainRunSortTime(run *BrainRun) time.Time {
	if run == nil {
		return time.Time{}
	}
	for _, raw := range []string{run.StartedAt.String, run.CreatedAt, run.UpdatedAt} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func brainRunFromTask(task *Task, familySlug string) *BrainRun {
	if task == nil || !task.AutoRunStatus.Valid || strings.TrimSpace(task.AutoRunStatus.String) == "" {
		return nil
	}
	run := &BrainRun{
		RunID:          brainRunLegacyID(task.Slug),
		FamilySlug:     familySlug,
		TaskSlug:       task.Slug,
		Role:           "worker",
		Provider:       task.SessionProvider,
		PermissionMode: task.PermissionMode,
		Status:         task.AutoRunStatus.String,
		CreatedAt:      task.CreatedAt,
		UpdatedAt:      task.UpdatedAt,
		Legacy:         true,
	}
	if task.Model.Valid && strings.TrimSpace(task.Model.String) != "" {
		run.RequestedModel = sql.NullString{String: task.Model.String, Valid: true}
		run.ResolvedModel = sql.NullString{String: task.Model.String, Valid: true}
	}
	if task.AutoRunPID.Valid {
		run.PID = sql.NullInt64{Int64: task.AutoRunPID.Int64, Valid: true}
	}
	if task.SessionID.Valid && strings.TrimSpace(task.SessionID.String) != "" {
		run.SessionID = sql.NullString{String: task.SessionID.String, Valid: true}
	}
	if task.AutoRunLog.Valid && strings.TrimSpace(task.AutoRunLog.String) != "" {
		run.LogPath = sql.NullString{String: task.AutoRunLog.String, Valid: true}
	}
	if task.AutoRunStarted.Valid && strings.TrimSpace(task.AutoRunStarted.String) != "" {
		run.StartedAt = sql.NullString{String: task.AutoRunStarted.String, Valid: true}
		run.CreatedAt = task.AutoRunStarted.String
	}
	if task.AutoRunFinished.Valid && strings.TrimSpace(task.AutoRunFinished.String) != "" {
		run.FinishedAt = sql.NullString{String: task.AutoRunFinished.String, Valid: true}
	}
	run.InputSummary = sql.NullString{String: "legacy auto-run compatibility row", Valid: true}
	return run
}

func brainRunFromLegacyTask(task *Task, familySlug string) *BrainRun {
	if task == nil {
		return nil
	}
	return brainRunFromTask(task, familySlug)
}

func brainRunLegacyID(taskSlug string) string {
	return brainRunLegacyPrefix + strings.TrimSpace(taskSlug)
}
