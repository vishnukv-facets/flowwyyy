// Package db implements the SQLite data layer for flow.
package flowdb

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPermissionMode is used when callers do not explicitly choose one.
const DefaultPermissionMode = "auto"

// schemaDDL is the full DDL for flow.db. Each statement is idempotent
// (CREATE ... IF NOT EXISTS) so OpenDB can run this on every startup.
//
// Note on NULL-safe equality: SQLite's `IS` operator treats NULLs as
// equal (NULL IS NULL → true, 'x' IS 'x' → true). Code that needs
// optimistic-lock updates against a preSessionID that may be NULL
// should use `WHERE session_id IS ?` rather than `= ?`.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS projects (
    slug          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','done')),
    priority      TEXT NOT NULL DEFAULT 'medium' CHECK (priority IN ('high','medium','low')),
    work_dir      TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    archived_at   TEXT,
    deleted_at    TEXT
);

CREATE TABLE IF NOT EXISTS playbooks (
    slug          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    project_slug  TEXT REFERENCES projects(slug),
    work_dir      TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    archived_at   TEXT,
    deleted_at    TEXT
);

CREATE TABLE IF NOT EXISTS tasks (
    slug                  TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    project_slug          TEXT REFERENCES projects(slug),
    status                TEXT NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog','in-progress','done')),
    kind                  TEXT NOT NULL DEFAULT 'regular' CHECK (kind IN ('regular','playbook_run')),
    playbook_slug         TEXT REFERENCES playbooks(slug),
    parent_slug           TEXT REFERENCES tasks(slug),
    forked_from_slug      TEXT REFERENCES tasks(slug),
    fork_reason           TEXT,
    priority              TEXT NOT NULL DEFAULT 'medium' CHECK (priority IN ('high','medium','low')),
    work_dir              TEXT NOT NULL,
    waiting_on            TEXT,
    due_date              TEXT,
    assignee              TEXT,
    permission_mode       TEXT NOT NULL DEFAULT 'auto' CHECK (permission_mode IN ('default','auto','bypass')),
	    model                 TEXT,
	    status_changed_at     TEXT,
	    session_provider      TEXT NOT NULL DEFAULT 'claude' CHECK (session_provider IN ('claude','codex')),
	    harness               TEXT,
	    session_id            TEXT,
    session_started       TEXT,
    session_last_resumed  TEXT,
    session_path          TEXT,
    worktree_path         TEXT,
    inbox_seen_at         TEXT,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    archived_at           TEXT,
    deleted_at            TEXT,
    auto_run_status       TEXT,
    auto_run_pid          INTEGER,
    auto_run_started      TEXT,
    auto_run_finished     TEXT,
    auto_run_log          TEXT,
    CHECK (status IN ('backlog','done') OR session_id IS NOT NULL OR (session_provider = 'codex' AND status = 'in-progress'))
);

CREATE TABLE IF NOT EXISTS brain_plans (
    id             TEXT PRIMARY KEY,
    title          TEXT NOT NULL,
    query          TEXT NOT NULL DEFAULT '',
    summary        TEXT NOT NULL DEFAULT '',
    source         TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','approved','executing','blocked','completed','cancelled','rejected','deferred','failed')),
    project_slug   TEXT REFERENCES projects(slug),
    work_dir       TEXT,
    branch_policy  TEXT,
    error          TEXT,
    approved_at    TEXT,
    executed_at    TEXT,
    completed_at   TEXT,
    cancelled_at   TEXT,
    rejected_at    TEXT,
    blocked_at     TEXT,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    plan_json      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS brain_runs (
    run_id          TEXT PRIMARY KEY,
    family_slug     TEXT NOT NULL,
    task_slug       TEXT NOT NULL,
    plan_id         TEXT,
    role            TEXT NOT NULL CHECK (role IN ('worker','validator','steward','orchestrator')),
    provider        TEXT NOT NULL CHECK (provider IN ('claude','codex')),
    requested_model TEXT,
    requested_tier  TEXT,
    resolved_model  TEXT,
    permission_mode TEXT NOT NULL DEFAULT 'auto' CHECK (permission_mode IN ('default','auto','bypass')),
    status          TEXT NOT NULL CHECK (status IN ('queued','running','completed','dead','error','cancelled','blocked')),
    pid             INTEGER,
    session_id      TEXT,
    log_path        TEXT,
    input_summary   TEXT,
    output_json     TEXT,
    evidence_json   TEXT,
    error_text      TEXT,
    started_at      TEXT,
    finished_at     TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS brain_policy (
    action     TEXT PRIMARY KEY,
    mode       TEXT NOT NULL CHECK (mode IN ('auto','approval_required')),
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS brain_action_audit (
    id            TEXT PRIMARY KEY,
    action        TEXT NOT NULL,
    target_type   TEXT NOT NULL,
    target_id     TEXT NOT NULL,
    actor         TEXT NOT NULL,
    policy        TEXT NOT NULL,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    result        TEXT NOT NULL,
    error_text    TEXT,
    created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workdirs (
    path          TEXT PRIMARY KEY,
    name          TEXT,
    description   TEXT,
    git_remote    TEXT,
    last_used_at  TEXT,
    created_at    TEXT NOT NULL
);

	CREATE TABLE IF NOT EXISTS task_tags (
	    task_slug   TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE,
	    tag         TEXT NOT NULL,
	    created_at  TEXT NOT NULL,
	    PRIMARY KEY (task_slug, tag)
	);

	CREATE TABLE IF NOT EXISTS owners (
	    slug              TEXT PRIMARY KEY,
	    name              TEXT NOT NULL,
	    work_dir          TEXT NOT NULL,
	    project_slug      TEXT REFERENCES projects(slug),
	    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','retired')),
	    every             TEXT NOT NULL,
	    next_wake_at      TEXT,
	    last_tick_at      TEXT,
	    last_tick_status  TEXT,
	    tick_pid          INTEGER,
	    tick_started      TEXT,
	    harness           TEXT,
	    created_at        TEXT NOT NULL,
	    updated_at        TEXT NOT NULL,
	    archived_at       TEXT
	);

	CREATE TABLE IF NOT EXISTS github_event_log (
    event_key    TEXT PRIMARY KEY,
    event_kind   TEXT NOT NULL,
    task_slug    TEXT REFERENCES tasks(slug) ON DELETE SET NULL,
    raw_json     TEXT,
    processed_at TEXT NOT NULL
);

-- github_webhook_deliveries is the raw delivery audit/idempotency log keyed on
-- X-GitHub-Delivery. It sits in front of github_event_log: delivery_id guards
-- against GitHub redelivering the same payload, while github_event_log dedupes
-- at the normalized-event level (and across the polling transport).
CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
    delivery_id  TEXT PRIMARY KEY,
    event_type   TEXT NOT NULL,
    action       TEXT,
    status       TEXT NOT NULL,
    error        TEXT,
    task_slug    TEXT,
    event_count  INTEGER NOT NULL DEFAULT 0,
    received_at  TEXT NOT NULL,
    processed_at TEXT
);

CREATE TABLE IF NOT EXISTS agent_runtime_states (
    provider     TEXT NOT NULL CHECK (provider IN ('claude','codex')),
    session_id   TEXT NOT NULL,
    task_slug    TEXT REFERENCES tasks(slug) ON DELETE SET NULL,
    status       TEXT NOT NULL CHECK (status IN ('running','waiting','idle','dead','released')),
    event_kind   TEXT NOT NULL,
    message      TEXT,
    updated_at   TEXT NOT NULL,
    last_seq     INTEGER NOT NULL DEFAULT 0,
    raw_json     TEXT,
    PRIMARY KEY (provider, session_id)
);

-- Many-to-many task dependencies. A child can depend on N parents;
-- the start-blocker logic requires all non-deleted parents to be done.
-- tasks.parent_slug is kept as a denormalized first-parent mirror for
-- backwards compat with older code paths and ad-hoc queries.
CREATE TABLE IF NOT EXISTS task_dependencies (
    child_slug   TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE,
    parent_slug  TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (child_slug, parent_slug),
    CHECK (child_slug <> parent_slug)
);

CREATE TABLE IF NOT EXISTS task_links (
    from_slug   TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE ON UPDATE CASCADE,
    to_slug     TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE ON UPDATE CASCADE,
    from_kind   TEXT NOT NULL CHECK (from_kind IN ('brief','update')),
    source_file TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (from_slug, to_slug, from_kind, source_file)
);

CREATE TABLE IF NOT EXISTS schema_meta (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    applied_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS search_docs (
    id            INTEGER PRIMARY KEY,
    doc_key       TEXT NOT NULL UNIQUE,
    scope         TEXT NOT NULL CHECK (scope IN ('brief','update','transcript','memory')),
    entity_type   TEXT NOT NULL CHECK (entity_type IN ('task','project','playbook','memory')),
    entity_slug   TEXT NOT NULL,
    title         TEXT NOT NULL,
    source_path   TEXT NOT NULL,
    source_mtime  TEXT NOT NULL,
    content       TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attention_feed (
    id                 TEXT PRIMARY KEY,
    source             TEXT NOT NULL,
    thread_key         TEXT NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    suggested_action   TEXT NOT NULL,
    matched_task       TEXT,
    suggested_project  TEXT,
    suggested_priority TEXT,
    urgency            TEXT,
    is_vip             INTEGER NOT NULL DEFAULT 0,
    confidence         REAL NOT NULL DEFAULT 0,
    draft              TEXT,
    reason             TEXT,
    context_json       TEXT,
    channel            TEXT,
    channel_type       TEXT,
    author             TEXT,
    ts                 TEXT,
    team_id            TEXT,
    url                TEXT,
    status             TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new','acted','dismissed','snoozed','deferred')),
    snooze_until       TEXT,
    linked_task        TEXT,
    retriaging_at      TEXT,
    created_at         TEXT NOT NULL,
    acted_at           TEXT
);

CREATE TABLE IF NOT EXISTS attention_feedback (
    id                 TEXT PRIMARY KEY,
    feed_item_id       TEXT NOT NULL,
    source             TEXT NOT NULL,
    channel            TEXT,
    author             TEXT,
    thread_type        TEXT,
    thread_key         TEXT NOT NULL,
    suggested_action   TEXT NOT NULL,
    final_action       TEXT NOT NULL,
    outcome            TEXT NOT NULL,
    confidence         REAL NOT NULL DEFAULT 0,
    confidence_band    TEXT NOT NULL,
    draft_before       TEXT,
    draft_after        TEXT,
    draft_edit_delta   TEXT,
    created_at         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attention_handoffs (
    id                 TEXT PRIMARY KEY,
    feed_item_id       TEXT NOT NULL,
    sender             TEXT NOT NULL,
    receiver           TEXT NOT NULL,
    context            TEXT NOT NULL,
    requested_verdict  TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','declined','timeout')),
    reason             TEXT,
    requested_at       TEXT NOT NULL,
    expires_at         TEXT NOT NULL,
    responded_at       TEXT
);

CREATE TABLE IF NOT EXISTS steering_trace (
    id                TEXT PRIMARY KEY,
    created_at        TEXT NOT NULL,
    origin            TEXT NOT NULL DEFAULT 'live',
    source            TEXT NOT NULL DEFAULT '',
    channel           TEXT,
    channel_type      TEXT,
    author            TEXT,
    thread_key        TEXT,
    text_preview      TEXT,
    disposition       TEXT NOT NULL,
    stage_reached     TEXT NOT NULL,
    drop_reason       TEXT,
    stage1_relevant   INTEGER,
    stage1_reason     TEXT,
    stage2_action     TEXT,
    stage2_confidence REAL,
    stage3_action     TEXT,
    stage3_confidence REAL,
    final_action      TEXT,
    final_confidence  REAL,
    feed_item_id      TEXT,
    error             TEXT,
    autonomy_action   TEXT,
    autonomy_decision TEXT,
    autonomy_reason   TEXT,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    model             TEXT,
    ts                TEXT,
    team_id           TEXT,
    url               TEXT
);

-- Operator-set permanent suppressions for the attention router. scope is
-- 'channel' (Slack channel id / owner/repo), 'author' (Slack user id / GitHub
-- login), or 'thread' (a thread key). Stage 0 drops any event matching a row
-- here, so "perma drop" from a feed card takes effect on the next event.
CREATE TABLE IF NOT EXISTS steering_mutes (
    scope      TEXT NOT NULL,
    value      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (scope, value)
);

CREATE TABLE IF NOT EXISTS steering_watermark (
    channel    TEXT PRIMARY KEY,
    last_ts    TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Two FTS indexes over search_docs, partitioned by scope. Transcripts are
-- enormous (whole-session JSONL — ~100x the size of all briefs/updates/memories
-- combined) and searched only on demand, so they live in their own index:
-- search_docs_fts (briefs/updates/memories) stays tiny and instant for the ⌘K
-- palette, while search_docs_tx_fts carries the heavy transcript content and is
-- only queried when transcript scope is requested. The triggers route each row
-- to exactly one index by scope; scope is immutable per doc_key, so the delete
-- side can safely target the matching index using old.scope.
CREATE VIRTUAL TABLE IF NOT EXISTS search_docs_fts USING fts5(
    title,
    content,
    content='search_docs',
    content_rowid='id',
    tokenize='unicode61'
);

CREATE VIRTUAL TABLE IF NOT EXISTS search_docs_tx_fts USING fts5(
    title,
    content,
    content='search_docs',
    content_rowid='id',
    tokenize='unicode61'
);

CREATE TRIGGER IF NOT EXISTS search_docs_ai AFTER INSERT ON search_docs BEGIN
    INSERT INTO search_docs_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope = 'transcript';
END;

CREATE TRIGGER IF NOT EXISTS search_docs_ad AFTER DELETE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(search_docs_tx_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope = 'transcript';
END;

CREATE TRIGGER IF NOT EXISTS search_docs_au AFTER UPDATE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(search_docs_tx_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope = 'transcript';
    INSERT INTO search_docs_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope = 'transcript';
END;

CREATE INDEX IF NOT EXISTS idx_tasks_project    ON tasks(project_slug);
CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_updated_at ON tasks(updated_at);
CREATE INDEX IF NOT EXISTS idx_task_tags_tag    ON task_tags(tag);
CREATE INDEX IF NOT EXISTS idx_github_event_log_task ON github_event_log(task_slug);
CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_task ON agent_runtime_states(task_slug);
CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_updated ON agent_runtime_states(updated_at);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_parent ON task_dependencies(parent_slug);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_child ON task_dependencies(child_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_to ON task_links(to_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_from ON task_links(from_slug);
CREATE INDEX IF NOT EXISTS idx_brain_runs_family_started ON brain_runs(family_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_task_started ON brain_runs(task_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_status_updated ON brain_runs(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_action_audit_target ON brain_action_audit(target_type, target_id, created_at);
CREATE INDEX IF NOT EXISTS idx_steering_trace_feed ON steering_trace(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created ON steering_trace(created_at);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created_id ON steering_trace(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_created_id ON steering_trace(disposition, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_source_created_id ON steering_trace(source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_source_created_id ON steering_trace(disposition, source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_funnel ON steering_trace(created_at, disposition, stage_reached);
CREATE INDEX IF NOT EXISTS idx_brain_plans_status_updated ON brain_plans(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_plans_created ON brain_plans(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_plans_project ON brain_plans(project_slug);
CREATE INDEX IF NOT EXISTS idx_search_docs_scope ON search_docs(scope);
CREATE INDEX IF NOT EXISTS idx_search_docs_entity ON search_docs(entity_type, entity_slug);
CREATE INDEX IF NOT EXISTS idx_attention_feed_status ON attention_feed(status);
CREATE INDEX IF NOT EXISTS idx_attention_feed_thread ON attention_feed(thread_key);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_feed ON attention_feedback(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_created ON attention_feedback(created_at);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_channel ON attention_feedback(channel);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_author ON attention_feedback(author);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_action ON attention_feedback(suggested_action);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_band ON attention_feedback(confidence_band);
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_feed ON attention_handoffs(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_receiver ON attention_handoffs(receiver);
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_status ON attention_handoffs(status);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition ON steering_trace(disposition);

CREATE TABLE IF NOT EXISTS chats (
    slug             TEXT PRIMARY KEY,
    title            TEXT NOT NULL,
    provider         TEXT NOT NULL,
    origin           TEXT NOT NULL,
    session_id       TEXT,
    created_at       TEXT NOT NULL,
    last_activity_at TEXT NOT NULL,
    archived_at      TEXT,
    deleted_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_chats_last_activity ON chats(last_activity_at DESC);

CREATE TABLE IF NOT EXISTS kb_capture (
    session_id   TEXT PRIMARY KEY,
    slug         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    cursor       INTEGER NOT NULL DEFAULT 0,
    captured_at  TEXT NOT NULL
);
	`

// indexesPostMigrate are indexes that depend on columns added by
// runMigrations. Running them in schemaDDL before migrations would fail
// against an existing pre-migration DB ("no such column"), so they live
// here and run AFTER migrations land.
const indexesPostMigrate = `
CREATE INDEX IF NOT EXISTS idx_tasks_kind          ON tasks(kind);
CREATE INDEX IF NOT EXISTS idx_tasks_playbook_slug ON tasks(playbook_slug);
CREATE INDEX IF NOT EXISTS idx_playbooks_project   ON playbooks(project_slug);
CREATE INDEX IF NOT EXISTS idx_projects_deleted_at ON projects(deleted_at);
CREATE INDEX IF NOT EXISTS idx_playbooks_deleted_at ON playbooks(deleted_at);
CREATE INDEX IF NOT EXISTS idx_tasks_deleted_at ON tasks(deleted_at);
CREATE INDEX IF NOT EXISTS idx_tasks_forked_from ON tasks(forked_from_slug);
`

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
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func runMigrations(db *sql.DB) error {
	// Wipe legacy inbox/monitor/slack/github tables on boot. The feature
	// was removed; any existing user install still has data sitting in
	// these tables that the current code no longer touches. Foreign keys
	// are toggled off for the duration so child-table drops don't trip
	// parent-table constraints — several of these tables FK back to
	// monitor_events / external_messages which are in the same drop set.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys for legacy drop: %w", err)
	}
	for _, t := range []string{
		"external_message_actions",
		"external_messages",
		"monitor_event_actions",
		"monitor_notifications",
		"monitor_notification_states",
		"monitor_sync_state",
		"monitor_fetch_state",
		"monitor_events",
		"automation_rules",
		"task_pr_links",
		"slack_oauth_tokens",
	} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
			return fmt.Errorf("drop legacy %s: %w", t, err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("re-enable foreign_keys after legacy drop: %w", err)
	}

	has, err := columnExists(db, "workdirs", "description")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE workdirs ADD COLUMN description TEXT`); err != nil {
			return fmt.Errorf("add workdirs.description: %w", err)
		}
	}
	has, err = columnExists(db, "tasks", "due_date")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN due_date TEXT`); err != nil {
			return fmt.Errorf("add tasks.due_date: %w", err)
		}
	}
	has, err = columnExists(db, "tasks", "status_changed_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN status_changed_at TEXT`); err != nil {
			return fmt.Errorf("add tasks.status_changed_at: %w", err)
		}
	}

	has, err = columnExists(db, "steering_trace", "stage1_reason")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN stage1_reason TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.stage1_reason: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "ts")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN ts TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.ts: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "team_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN team_id TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.team_id: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "url")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN url TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.url: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "autonomy_action")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN autonomy_action TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.autonomy_action: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "autonomy_decision")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN autonomy_decision TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.autonomy_decision: %w", err)
		}
	}
	has, err = columnExists(db, "steering_trace", "autonomy_reason")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE steering_trace ADD COLUMN autonomy_reason TEXT`); err != nil {
			return fmt.Errorf("add steering_trace.autonomy_reason: %w", err)
		}
	}

	has, err = columnExists(db, "attention_feed", "linked_task")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN linked_task TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.linked_task: %w", err)
		}
	}

	has, err = columnExists(db, "attention_feed", "channel")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN channel TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.channel: %w", err)
		}
	}
	has, err = columnExists(db, "attention_feed", "channel_type")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN channel_type TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.channel_type: %w", err)
		}
	}
	has, err = columnExists(db, "attention_feed", "author")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN author TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.author: %w", err)
		}
	}
	has, err = columnExists(db, "attention_feed", "ts")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN ts TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.ts: %w", err)
		}
	}
	has, err = columnExists(db, "attention_feed", "team_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN team_id TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.team_id: %w", err)
		}
	}
	has, err = columnExists(db, "attention_feed", "url")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN url TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.url: %w", err)
		}
	}

	has, err = columnExists(db, "attention_feed", "retriaging_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE attention_feed ADD COLUMN retriaging_at TEXT`); err != nil {
			return fmt.Errorf("add attention_feed.retriaging_at: %w", err)
		}
	}

	// playbooks table: created via schemaDDL on every OpenDB, so no ALTER needed
	// for the table itself. Just ensure tasks columns are present.

	has, err = columnExists(db, "tasks", "kind")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'regular'`); err != nil {
			return fmt.Errorf("add tasks.kind: %w", err)
		}
		// Note: SQLite doesn't allow CHECK constraints on ADD COLUMN; the
		// CHECK is only enforced for fresh tables (see schemaDDL). Application
		// code should validate enum values before insert.
	}

	has, err = columnExists(db, "tasks", "playbook_slug")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN playbook_slug TEXT REFERENCES playbooks(slug)`); err != nil {
			return fmt.Errorf("add tasks.playbook_slug: %w", err)
		}
	}

	has, err = columnExists(db, "tasks", "assignee")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN assignee TEXT`); err != nil {
			return fmt.Errorf("add tasks.assignee: %w", err)
		}
	}

	has, err = columnExists(db, "tasks", "permission_mode")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'auto' CHECK (permission_mode IN ('default','auto','bypass'))`); err != nil {
			return fmt.Errorf("add tasks.permission_mode: %w", err)
		}
	}

	has, err = columnExists(db, "tasks", "model")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN model TEXT`); err != nil {
			return fmt.Errorf("add tasks.model: %w", err)
		}
	}

	has, err = columnExists(db, "tasks", "session_provider")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN session_provider TEXT NOT NULL DEFAULT 'claude'`); err != nil {
			return fmt.Errorf("add tasks.session_provider: %w", err)
		}
		// SQLite cannot add the CHECK constraint during ALTER TABLE; fresh
		// databases get it from schemaDDL and application code validates
		// writes for migrated databases.
	}

	// tasks.harness: nullable runtime pin for the pluggable harness layer.
	// Existing rows can remain NULL; ScanTask coalesces them to session_provider.
	has, err = columnExists(db, "tasks", "harness")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN harness TEXT`); err != nil {
			return fmt.Errorf("add tasks.harness: %w", err)
		}
	}

	has, err = columnExists(db, "tasks", "worktree_path")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN worktree_path TEXT`); err != nil {
			return fmt.Errorf("add tasks.worktree_path: %w", err)
		}
	}

	for _, table := range []string{"projects", "playbooks", "tasks"} {
		has, err = columnExists(db, table, "deleted_at")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN deleted_at TEXT`, table)); err != nil {
				return fmt.Errorf("add %s.deleted_at: %w", table, err)
			}
		}
	}

	// last_seq column on agent_runtime_states for monotonic ordering of
	// hook events. The agent harness only stamps RFC3339 timestamps which
	// collide on bursty writes; the agent-side seq (time.UnixNano) breaks
	// ties unambiguously.
	has, err = columnExists(db, "agent_runtime_states", "last_seq")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE agent_runtime_states ADD COLUMN last_seq INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add agent_runtime_states.last_seq: %w", err)
		}
	}

	// parent_slug + inbox_seen_at: parent-child task linkage (flow spawn)
	// and SessionStart inbox detection (flow tell). Both nullable; no
	// table rebuild needed.
	has, err = columnExists(db, "tasks", "parent_slug")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN parent_slug TEXT REFERENCES tasks(slug)`); err != nil {
			return fmt.Errorf("add tasks.parent_slug: %w", err)
		}
	}
	has, err = columnExists(db, "tasks", "forked_from_slug")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN forked_from_slug TEXT REFERENCES tasks(slug)`); err != nil {
			return fmt.Errorf("add tasks.forked_from_slug: %w", err)
		}
	}
	has, err = columnExists(db, "tasks", "fork_reason")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN fork_reason TEXT`); err != nil {
			return fmt.Errorf("add tasks.fork_reason: %w", err)
		}
	}
	has, err = columnExists(db, "tasks", "inbox_seen_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN inbox_seen_at TEXT`); err != nil {
			return fmt.Errorf("add tasks.inbox_seen_at: %w", err)
		}
	}

	// session_path caches the absolute path to a session's transcript jsonl
	// file. Populated at session capture (Codex) or first-use lookup; read
	// on every UI tick to skip the otherwise-expensive recursive walk of
	// ~/.codex/sessions when resolving a Codex session id to its file. Null
	// for tasks that have never had a session captured.
	has, err = columnExists(db, "tasks", "session_path")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN session_path TEXT`); err != nil {
			return fmt.Errorf("add tasks.session_path: %w", err)
		}
	}

	// agent_runtime_states.status: add 'released' value (graceful SessionEnd
	// vs 'idle' for between-turn pause). SQLite cannot widen a CHECK
	// constraint in place — see migrateAgentRuntimeStatesAllowReleased.
	if err := migrateAgentRuntimeStatesAllowReleased(db); err != nil {
		return fmt.Errorf("migrate agent_runtime_states.status: %w", err)
	}

	// Session-id invariant: any non-backlog Claude task must have a
	// session_id. Codex owns session-id allocation, so a freshly launched
	// Codex task may be in-progress while flow captures the id from Codex's
	// session store. Old DBs need a table rebuild.
	if err := migrateTasksSessionInvariant(db); err != nil {
		return fmt.Errorf("migrate session invariant: %w", err)
	}

	// Indexes that depend on columns added above. Safe to run after every
	// migration pass — CREATE INDEX IF NOT EXISTS is idempotent, and by
	// this point all referenced columns exist.
	if _, err := db.Exec(indexesPostMigrate); err != nil {
		return fmt.Errorf("create post-migrate indexes: %w", err)
	}

	// Session-id uniqueness: dedupe any tasks sharing a session_id, then
	// create the partial unique index. Runs after the basic indexes
	// because it has its own dedupe-first contract; a naive
	// CREATE UNIQUE INDEX in indexesPostMigrate would fail on a DB with
	// pre-existing duplicates.
	if err := migrateTasksSessionIDUnique(db); err != nil {
		return fmt.Errorf("migrate session-id uniqueness: %w", err)
	}

	// task_dependencies: many-to-many dependency table. Backfill from
	// the legacy single-parent column once so existing tasks keep their
	// dep wiring after the upgrade. Idempotent via INSERT OR IGNORE.
	if err := migrateTaskDependencies(db); err != nil {
		return fmt.Errorf("migrate task_dependencies: %w", err)
	}
	if err := migrateSplitHierarchyDependency(db); err != nil {
		return fmt.Errorf("migrate split hierarchy/dependency: %w", err)
	}
	if err := migrateSearchDocsMemoryScope(db); err != nil {
		return fmt.Errorf("migrate search docs memory scope: %w", err)
	}
	if err := migrateSearchDocsTranscriptFTS(db); err != nil {
		return fmt.Errorf("migrate search docs transcript fts: %w", err)
	}
	// Autonomous-run bookkeeping columns (feat: flow do --auto). Added
	// AFTER the session-invariant rebuild so that rebuild (which only
	// copies the pre-existing column set) doesn't need to know about
	// them — they land here via plain ALTER on the rebuilt table. All
	// nullable, no CHECK (SQLite can't add CHECK via ALTER; status enum
	// is validated in application code).
	for _, col := range []struct{ name, ddl string }{
		{"auto_run_status", "ALTER TABLE tasks ADD COLUMN auto_run_status TEXT"},
		{"auto_run_pid", "ALTER TABLE tasks ADD COLUMN auto_run_pid INTEGER"},
		{"auto_run_started", "ALTER TABLE tasks ADD COLUMN auto_run_started TEXT"},
		{"auto_run_finished", "ALTER TABLE tasks ADD COLUMN auto_run_finished TEXT"},
		{"auto_run_log", "ALTER TABLE tasks ADD COLUMN auto_run_log TEXT"},
	} {
		has, err := columnExists(db, "tasks", col.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(col.ddl); err != nil {
				return fmt.Errorf("add tasks.%s: %w", col.name, err)
			}
		}
	}
	return nil
}

// migrateSearchDocsTranscriptFTS splits transcripts out of the main FTS index
// into their own (search_docs_tx_fts). Before this, every search — even a quick
// ⌘K title lookup — had FTS scan the ~100 MB of transcript content sharing the
// index, so common terms took seconds. We detect the old layout by the routing
// trigger (the schema DDL may have already created an empty tx index via
// IF NOT EXISTS, so table presence isn't a reliable signal), then rebuild both
// indexes and repopulate them from the intact search_docs content — no file
// re-walk needed.
func migrateSearchDocsTranscriptFTS(db *sql.DB) error {
	var triggerSQL string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='search_docs_ai'`).Scan(&triggerSQL)
	if errors.Is(err, sql.ErrNoRows) {
		// No search trigger yet (fresh DB) — the schema DDL already built the
		// dual-index layout.
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Contains(triggerSQL, "search_docs_tx_fts") {
		return nil // already split
	}
	_, err = db.Exec(`
DROP TRIGGER IF EXISTS search_docs_ai;
DROP TRIGGER IF EXISTS search_docs_ad;
DROP TRIGGER IF EXISTS search_docs_au;
DROP TABLE IF EXISTS search_docs_fts;
DROP TABLE IF EXISTS search_docs_tx_fts;

CREATE VIRTUAL TABLE search_docs_fts USING fts5(
    title, content, content='search_docs', content_rowid='id', tokenize='unicode61'
);
CREATE VIRTUAL TABLE search_docs_tx_fts USING fts5(
    title, content, content='search_docs', content_rowid='id', tokenize='unicode61'
);

CREATE TRIGGER search_docs_ai AFTER INSERT ON search_docs BEGIN
    INSERT INTO search_docs_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope = 'transcript';
END;

CREATE TRIGGER search_docs_ad AFTER DELETE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(search_docs_tx_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope = 'transcript';
END;

CREATE TRIGGER search_docs_au AFTER UPDATE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(search_docs_tx_fts, rowid, title, content)
    SELECT 'delete', old.id, old.title, old.content WHERE old.scope = 'transcript';
    INSERT INTO search_docs_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope <> 'transcript';
    INSERT INTO search_docs_tx_fts(rowid, title, content)
    SELECT new.id, new.title, new.content WHERE new.scope = 'transcript';
END;

INSERT INTO search_docs_fts(rowid, title, content)
    SELECT id, title, content FROM search_docs WHERE scope <> 'transcript';
INSERT INTO search_docs_tx_fts(rowid, title, content)
    SELECT id, title, content FROM search_docs WHERE scope = 'transcript';
`)
	return err
}

// migrateSearchDocsMemoryScope widens the rebuildable FTS cache to include
// memory docs. Because search_docs is derived from markdown/transcript files,
// dropping and recreating the cache is safer than a SQLite table rebuild that
// preserves stale index rows.
func migrateSearchDocsMemoryScope(db *sql.DB) error {
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='search_docs'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Contains(ddl, "'memory'") {
		return nil
	}
	_, err = db.Exec(`
DROP TRIGGER IF EXISTS search_docs_ai;
DROP TRIGGER IF EXISTS search_docs_ad;
DROP TRIGGER IF EXISTS search_docs_au;
DROP TABLE IF EXISTS search_docs_fts;
DROP TABLE IF EXISTS search_docs;

CREATE TABLE search_docs (
    id            INTEGER PRIMARY KEY,
    doc_key       TEXT NOT NULL UNIQUE,
    scope         TEXT NOT NULL CHECK (scope IN ('brief','update','transcript','memory')),
    entity_type   TEXT NOT NULL CHECK (entity_type IN ('task','project','playbook','memory')),
    entity_slug   TEXT NOT NULL,
    title         TEXT NOT NULL,
    source_path   TEXT NOT NULL,
    source_mtime  TEXT NOT NULL,
    content       TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE VIRTUAL TABLE search_docs_fts USING fts5(
    title,
    content,
    content='search_docs',
    content_rowid='id',
    tokenize='unicode61'
);

CREATE TRIGGER search_docs_ai AFTER INSERT ON search_docs BEGIN
    INSERT INTO search_docs_fts(rowid, title, content)
    VALUES (new.id, new.title, new.content);
END;

CREATE TRIGGER search_docs_ad AFTER DELETE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, old.content);
END;

CREATE TRIGGER search_docs_au AFTER UPDATE ON search_docs BEGIN
    INSERT INTO search_docs_fts(search_docs_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, old.content);
    INSERT INTO search_docs_fts(rowid, title, content)
    VALUES (new.id, new.title, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_search_docs_scope ON search_docs(scope);
CREATE INDEX IF NOT EXISTS idx_search_docs_entity ON search_docs(entity_type, entity_slug);
`)
	return err
}

// migrateTaskDependencies ensures the task_dependencies table exists and
// backfills it from tasks.parent_slug. Idempotent — INSERT OR IGNORE means
// a row already present from a prior run or from a fresh CREATE TABLE is
// left untouched. tasks.parent_slug stays in place as a write-through
// mirror of the first parent for backwards compatibility.
func migrateTaskDependencies(db *sql.DB) error {
	// schemaDDL also creates this table on a fresh DB; the CREATE here is
	// belt-and-braces for an older DB that has every other migration but
	// hasn't yet been opened with the new schemaDDL.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS task_dependencies (
		    child_slug   TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE,
		    parent_slug  TEXT NOT NULL REFERENCES tasks(slug) ON DELETE CASCADE,
		    created_at   TEXT NOT NULL,
		    PRIMARY KEY (child_slug, parent_slug),
		    CHECK (child_slug <> parent_slug)
		);
		CREATE INDEX IF NOT EXISTS idx_task_dependencies_parent ON task_dependencies(parent_slug);
		CREATE INDEX IF NOT EXISTS idx_task_dependencies_child ON task_dependencies(child_slug);
	`); err != nil {
		return fmt.Errorf("create task_dependencies: %w", err)
	}
	// The session-invariant rebuild can transiently drop tasks.parent_slug
	// from older DBs; on the next runMigrations pass it gets re-added. Skip
	// the backfill if the column isn't present yet — there are no rows to
	// migrate from anyway, and the subsequent open will retry.
	has, err := columnExists(db, "tasks", "parent_slug")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	// Once the hierarchy/dependency split migration has run, tasks.parent_slug
	// means pure organizational hierarchy (non-blocking) and must NOT be
	// backfilled into task_dependencies. Backfilling after the split would
	// turn hierarchy parents into blocking dependencies, defeating the split.
	split, err := schemaMetaHas(db, "hierarchy_dependency_split")
	if err != nil {
		return err
	}
	if split {
		return nil
	}
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO task_dependencies (child_slug, parent_slug, created_at)
		SELECT slug, parent_slug, COALESCE(created_at, datetime('now'))
		FROM tasks
		WHERE parent_slug IS NOT NULL AND TRIM(parent_slug) <> ''
		  AND slug <> parent_slug
	`); err != nil {
		return fmt.Errorf("backfill task_dependencies: %w", err)
	}
	return nil
}

// migrateSplitHierarchyDependency runs once per DB. It nulls tasks.parent_slug
// values that merely mirror an existing task_dependencies edge — the artifact
// of the pre-split era when "parent" meant both hierarchy and blocking
// dependency. After this, parent_slug means hierarchy only and task_dependencies
// means blocking only. Gated by a schema_meta marker so a legitimately-set
// hierarchy edge that later happens to coincide with a dependency is never
// clobbered on a subsequent open.
func migrateSplitHierarchyDependency(db *sql.DB) error {
	done, err := schemaMetaHas(db, "hierarchy_dependency_split")
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	// Null only the legacy mirrors: parent_slug values that duplicate an
	// existing task_dependencies edge (the pre-split artifact). A fresh DB has
	// none, so this UPDATE is a no-op there. We then stamp the marker
	// UNCONDITIONALLY — even on an empty DB — so the window in which
	// migrateTaskDependencies could backfill a newly-written hierarchy
	// parent_slug into a blocking dependency is closed by construction on the
	// very first open, rather than relying on a task happening to exist yet.
	if _, err := db.Exec(`
		UPDATE tasks SET parent_slug = NULL
		WHERE parent_slug IS NOT NULL
		  AND EXISTS (
		      SELECT 1 FROM task_dependencies d
		      WHERE d.child_slug = tasks.slug AND d.parent_slug = tasks.parent_slug
		  )
	`); err != nil {
		return fmt.Errorf("null legacy parent_slug mirrors: %w", err)
	}
	return schemaMetaSet(db, "hierarchy_dependency_split")
}

// migrateTasksSessionIDUnique creates the partial unique index on
// tasks(session_id) WHERE session_id IS NOT NULL. Older DBs may have
// two tasks sharing a session_id (the old `flow update task
// --session-id` flag could silently overwrite a binding without
// clearing the prior owner; or a user manually edited the row). A
// flat CREATE UNIQUE INDEX would fail on those DBs, so this function
// first deduplicates by:
//
//  1. Listing every session_id that appears on 2+ tasks.
//  2. For each such session_id, ordering the carrier tasks by
//     updated_at DESC, slug ASC. The first row keeps the binding.
//  3. The remaining rows get session_id=NULL, session_started=NULL,
//     and status='backlog' (the only state legal for a NULL
//     session_id under the invariant). A stderr summary explains
//     which task kept the session and which were demoted.
//
// Idempotent: probes sqlite_master for the index first; subsequent
// calls are no-ops once the index exists.
func migrateTasksSessionIDUnique(db *sql.DB) error {
	var existing sql.NullString
	err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_tasks_session_id'`,
	).Scan(&existing)
	if err == nil && existing.Valid {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("probe unique index: %w", err)
	}

	rows, err := db.Query(
		`SELECT session_id FROM tasks
		 WHERE session_id IS NOT NULL
		 GROUP BY session_id
		 HAVING COUNT(*) > 1
		 ORDER BY session_id`,
	)
	if err != nil {
		return fmt.Errorf("scan duplicates: %w", err)
	}
	var dupedSIDs []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return err
		}
		dupedSIDs = append(dupedSIDs, sid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(dupedSIDs) > 0 {
		fmt.Fprintf(os.Stderr,
			"flow migration: deduplicating %d session_id(s) shared across multiple tasks (only one task may carry a given session_id):\n",
			len(dupedSIDs))
		now := NowISO()
		for _, sid := range dupedSIDs {
			tRows, err := db.Query(
				`SELECT slug, status FROM tasks
				 WHERE session_id = ?
				 ORDER BY updated_at DESC, slug ASC`,
				sid,
			)
			if err != nil {
				return fmt.Errorf("scan duplicate group %s: %w", sid, err)
			}
			type tRow struct{ slug, status string }
			var carriers []tRow
			for tRows.Next() {
				var t tRow
				if err := tRows.Scan(&t.slug, &t.status); err != nil {
					tRows.Close()
					return err
				}
				carriers = append(carriers, t)
			}
			if err := tRows.Err(); err != nil {
				tRows.Close()
				return err
			}
			tRows.Close()
			if len(carriers) < 2 {
				continue
			}
			winner := carriers[0]
			fmt.Fprintf(os.Stderr,
				"  %s: keeping on %s (was %s); demoting to backlog with NULL session_id:\n",
				sid, winner.slug, winner.status)
			for _, l := range carriers[1:] {
				fmt.Fprintf(os.Stderr, "    - %s (was %s)\n", l.slug, l.status)
				if _, err := db.Exec(
					`UPDATE tasks SET session_id=NULL, session_started=NULL, status='backlog', updated_at=? WHERE slug=?`,
					now, l.slug,
				); err != nil {
					return fmt.Errorf("demote duplicate %s: %w", l.slug, err)
				}
			}
		}
	}

	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_session_id ON tasks(session_id) WHERE session_id IS NOT NULL`,
	); err != nil {
		return fmt.Errorf("create unique index: %w", err)
	}
	return nil
}

// migrateTasksSessionInvariant rebuilds the tasks table to enforce the
// session binding invariant. In-progress Claude tasks must carry session_id;
// Codex tasks may briefly be in-progress with no session_id while flow
// captures the id that Codex generated. Terminal done tasks may be closed by
// external automation (for example, a merged GitHub PR) without a local agent
// transcript. SQLite does not support changing a CHECK constraint in place, so
// the documented CREATE-new, copy, DROP-old, RENAME procedure is used.
// Existing non-Codex in-progress violators are demoted to backlog first (with
// a stderr summary), since there is no way to invent a session_id for them
// after the fact.
func migrateTasksSessionInvariant(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'`,
	).Scan(&ddl); err != nil {
		return fmt.Errorf("inspect tasks ddl: %w", err)
	}
	if strings.Contains(ddl, "status IN ('backlog','done') OR session_id IS NOT NULL") {
		return nil
	}

	type violator struct{ slug, prevStatus string }
	var vs []violator
	rows, err := db.Query(
		`SELECT slug, status FROM tasks
		 WHERE status = 'in-progress'
		   AND session_id IS NULL
		   AND NOT (session_provider = 'codex' AND status = 'in-progress')`,
	)
	if err != nil {
		return fmt.Errorf("scan violators: %w", err)
	}
	for rows.Next() {
		var v violator
		if err := rows.Scan(&v.slug, &v.prevStatus); err != nil {
			rows.Close()
			return err
		}
		vs = append(vs, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(vs) > 0 {
		fmt.Fprintf(os.Stderr,
			"flow migration: demoting %d task(s) without session_id back to backlog "+
				"(in-progress tasks require session_id under the new invariant):\n",
			len(vs))
		for _, v := range vs {
			fmt.Fprintf(os.Stderr, "  %s (was %s)\n", v.slug, v.prevStatus)
		}
		if _, err := db.Exec(
			`UPDATE tasks
			 SET status='backlog', updated_at=?
			 WHERE status = 'in-progress'
			   AND session_id IS NULL
			   AND NOT (session_provider = 'codex' AND status = 'in-progress')`,
			NowISO(),
		); err != nil {
			return fmt.Errorf("demote violators: %w", err)
		}
	}

	// SQLite-recommended ALTER-by-rebuild procedure. PRAGMA must be set
	// outside the transaction.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable fk: %w", err)
	}
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()

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

	if _, err := tx.Exec(`
		CREATE TABLE tasks_new (
			slug                  TEXT PRIMARY KEY,
			name                  TEXT NOT NULL,
			project_slug          TEXT REFERENCES projects(slug),
			status                TEXT NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog','in-progress','done')),
			kind                  TEXT NOT NULL DEFAULT 'regular' CHECK (kind IN ('regular','playbook_run')),
			playbook_slug         TEXT REFERENCES playbooks(slug),
			parent_slug           TEXT REFERENCES tasks(slug),
			forked_from_slug      TEXT REFERENCES tasks(slug),
			fork_reason           TEXT,
			priority              TEXT NOT NULL DEFAULT 'medium' CHECK (priority IN ('high','medium','low')),
			work_dir              TEXT NOT NULL,
			waiting_on            TEXT,
			due_date              TEXT,
			assignee              TEXT,
			permission_mode       TEXT NOT NULL DEFAULT 'auto' CHECK (permission_mode IN ('default','auto','bypass')),
				model                 TEXT,
				status_changed_at     TEXT,
				session_provider      TEXT NOT NULL DEFAULT 'claude' CHECK (session_provider IN ('claude','codex')),
				harness               TEXT,
				session_id            TEXT,
			session_started       TEXT,
			session_last_resumed  TEXT,
			session_path          TEXT,
			worktree_path         TEXT,
			inbox_seen_at         TEXT,
			created_at            TEXT NOT NULL,
			updated_at            TEXT NOT NULL,
			archived_at           TEXT,
			deleted_at            TEXT,
			CHECK (status IN ('backlog','done') OR session_id IS NOT NULL OR (session_provider = 'codex' AND status = 'in-progress'))
		)`); err != nil {
		return fmt.Errorf("create tasks_new: %w", err)
	}

	if _, err := tx.Exec(`
			INSERT INTO tasks_new (
				slug, name, project_slug, status, kind, playbook_slug, parent_slug, forked_from_slug, fork_reason, priority,
				work_dir, waiting_on, due_date, assignee, permission_mode, model, status_changed_at,
				session_provider, harness, session_id, session_started, session_last_resumed,
				session_path, worktree_path, inbox_seen_at, created_at, updated_at, archived_at, deleted_at
			)
			SELECT
				slug, name, project_slug, status, kind, playbook_slug, parent_slug, forked_from_slug, fork_reason, priority,
				work_dir, waiting_on, due_date, assignee, permission_mode, model, status_changed_at,
				COALESCE(NULLIF(session_provider, ''), 'claude'), harness, session_id, session_started, session_last_resumed,
				session_path, worktree_path, inbox_seen_at, created_at, updated_at, archived_at, deleted_at
			FROM tasks`); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE tasks`); err != nil {
		return fmt.Errorf("drop old tasks: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE tasks_new RENAME TO tasks`); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	// Indexes from schemaDDL that lived on the dropped table.
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_tasks_project    ON tasks(project_slug)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_updated_at ON tasks(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_forked_from ON tasks(forked_from_slug)`,
	} {
		if _, err := tx.Exec(idx); err != nil {
			return fmt.Errorf("recreate index: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// migrateAgentRuntimeStatesAllowReleased widens the status CHECK
// constraint on agent_runtime_states to include 'released'. SQLite
// cannot alter a CHECK constraint in place, so we rebuild the table via
// the documented CREATE-new + copy + DROP + RENAME procedure. Idempotent:
// probes the current DDL for the 'released' literal and skips if already
// present.
func migrateAgentRuntimeStatesAllowReleased(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='agent_runtime_states'`,
	).Scan(&ddl); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("inspect agent_runtime_states ddl: %w", err)
	}
	if strings.Contains(ddl, "'released'") {
		return nil
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable fk: %w", err)
	}
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()

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

	if _, err := tx.Exec(`
		CREATE TABLE agent_runtime_states_new (
			provider     TEXT NOT NULL CHECK (provider IN ('claude','codex')),
			session_id   TEXT NOT NULL,
			task_slug    TEXT REFERENCES tasks(slug) ON DELETE SET NULL,
			status       TEXT NOT NULL CHECK (status IN ('running','waiting','idle','dead','released')),
			event_kind   TEXT NOT NULL,
			message      TEXT,
			updated_at   TEXT NOT NULL,
			last_seq     INTEGER NOT NULL DEFAULT 0,
			raw_json     TEXT,
			PRIMARY KEY (provider, session_id)
		)`); err != nil {
		return fmt.Errorf("create agent_runtime_states_new: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO agent_runtime_states_new (
			provider, session_id, task_slug, status, event_kind, message,
			updated_at, last_seq, raw_json
		)
		SELECT
			provider, session_id, task_slug, status, event_kind, message,
			updated_at, COALESCE(last_seq, 0), raw_json
		FROM agent_runtime_states`); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE agent_runtime_states`); err != nil {
		return fmt.Errorf("drop old: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE agent_runtime_states_new RENAME TO agent_runtime_states`); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_task ON agent_runtime_states(task_slug)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_updated ON agent_runtime_states(updated_at)`,
	} {
		if _, err := tx.Exec(idx); err != nil {
			return fmt.Errorf("recreate index: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// schemaMetaHas reports whether a one-shot migration marker has been recorded.
// Used to gate data migrations that cannot be inferred from schema structure.
func schemaMetaHas(db *sql.DB, key string) (bool, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// schemaMetaSet records a one-shot migration marker. Idempotent.
func schemaMetaSet(db *sql.DB, key string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO schema_meta (key, value, applied_at) VALUES (?, '1', ?)`,
		key, NowISO(),
	)
	return err
}

// GetMeta reads a value-bearing key from schema_meta (vs the one-shot markers
// schemaMetaHas/Set, which only store '1'). Returns "" with nil error when the
// key is absent, so callers can treat unset and empty alike.
func GetMeta(db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta %s: %w", key, err)
	}
	return v, nil
}

// SetMeta upserts a value-bearing schema_meta key, refreshing applied_at.
func SetMeta(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO schema_meta (key, value, applied_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, applied_at = excluded.applied_at`,
		key, value, NowISO(),
	)
	if err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	return nil
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
	CreatedAt   string
	UpdatedAt   string
	ArchivedAt  sql.NullString
	DeletedAt   sql.NullString
}

// PlaybookFilter holds optional filters for ListPlaybooks.
type PlaybookFilter struct {
	Project         string
	IncludeArchived bool
	IncludeDeleted  bool
	DeletedOnly     bool
}

// ---------- playbook queries ----------

const PlaybookCols = "slug, name, project_slug, work_dir, created_at, updated_at, archived_at, deleted_at"

func ScanPlaybook(row interface{ Scan(dest ...any) error }) (*Playbook, error) {
	var p Playbook
	err := row.Scan(&p.Slug, &p.Name, &p.ProjectSlug, &p.WorkDir, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt, &p.DeletedAt)
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
// table that references projects(slug): tasks.project_slug and
// playbooks.project_slug. No-op if old == new.
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
