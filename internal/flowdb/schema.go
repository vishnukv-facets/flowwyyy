package flowdb

// This file holds the DDL constants for flow.db, split out of db.go.
// These are raw-string consts and need no imports.

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
    slug                TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    project_slug        TEXT REFERENCES projects(slug),
    work_dir            TEXT NOT NULL,
    schedule_spec       TEXT,
    schedule_input      TEXT,
    schedule_paused_at  TEXT,
    next_fire_at        TEXT,
    last_fired_at       TEXT,
    last_fire_run_slug  TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    archived_at         TEXT,
    deleted_at          TEXT
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
	    effort                TEXT,
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

CREATE TABLE IF NOT EXISTS clickup_event_log (
    event_key    TEXT PRIMARY KEY,
    event_kind   TEXT NOT NULL,
    task_slug    TEXT REFERENCES tasks(slug) ON DELETE SET NULL,
    raw_json     TEXT,
    processed_at TEXT NOT NULL
);

-- clickup_webhook_deliveries is keyed on Flow's normalized delivery id. ClickUp
-- recommends webhook_id:history_item_id for events with history entries; events
-- without history use a deterministic webhook:event:task fallback.
CREATE TABLE IF NOT EXISTS clickup_webhook_deliveries (
    delivery_id  TEXT PRIMARY KEY,
    event_type   TEXT NOT NULL,
    task_id      TEXT,
    webhook_id   TEXT,
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

-- Buffered wake prompts: a wake (inbox nudge, operator-approved reply, steerer
-- turn) that arrived while a session was blocked on the operator's input is
-- parked here instead of being injected into — and auto-submitting — the open
-- prompt. Persisted (not in-memory) so a "flow ui serve" restart never loses a
-- buffered wake. Drained FIFO by id once the session leaves the human-input
-- wait. See internal/server/terminal_wake.go (flushWakes).
CREATE TABLE IF NOT EXISTS pending_wakes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    slug       TEXT NOT NULL,
    prompt     TEXT NOT NULL,
    not_before TEXT,
    created_at TEXT NOT NULL
);

-- Provider rate-limit hold queue. When Claude/Codex is out of tokens, automatic
-- connector ingestion and automatic task opens are persisted here instead of
-- dispatching immediately. The server drains ready rows after the provider
-- reset time and replays them through the normal dispatch/open paths.
CREATE TABLE IF NOT EXISTS rate_limit_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL CHECK (kind IN ('slack_event','github_event','open_task')),
    provider     TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL,
    run_after    TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','done','error')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rate_limit_queue_ready ON rate_limit_queue(status, run_after, id);

-- Outbound Slack sends to channels OUTSIDE the operator's org (Slack Connect /
-- cross-workspace) are parked here for the operator's explicit approval instead
-- of going out directly — the external-channel send gate. Every path (manual
-- CLI, agent session, auto-permit, steerer) routes through the server send
-- handler, which enqueues an external send as 'pending'; only the operator's
-- inbox approval actually posts it. See internal/server/slack_send.go.
CREATE TABLE IF NOT EXISTS pending_sends (
    id            TEXT PRIMARY KEY,
    channel       TEXT NOT NULL,
    channel_label TEXT,
    thread_ts     TEXT,
    text          TEXT NOT NULL,
    identity      TEXT,
    file_path     TEXT,
    post_at       INTEGER NOT NULL DEFAULT 0,
    reason        TEXT,
    origin        TEXT,
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    TEXT NOT NULL,
    decided_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_pending_sends_status ON pending_sends(status, created_at);

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

-- Persistent per-thread "running understanding" for the attention router
-- (task steerer-thread-memory). Unlike attention_feed — whose coalescing upsert
-- overwrites every verdict field per event — this row ACCUMULATES across events
-- for one thread_key: the current decision, a rolling summary, the operator
-- actions taken on the thread, the operator's own replies seen, and the
-- last-seen source ts. It lives in its own table so it outlives any single card
-- and dismissal/re-surface cycles; the feed's coalescing behavior is unchanged.
-- PK on thread_key covers every lookup.
CREATE TABLE IF NOT EXISTS attention_thread_state (
    thread_key         TEXT PRIMARY KEY,
    source             TEXT NOT NULL DEFAULT '',
    current_action     TEXT,
    current_confidence REAL NOT NULL DEFAULT 0,
    current_reason     TEXT,
    summary            TEXT NOT NULL DEFAULT '',
    operator_actions   TEXT NOT NULL DEFAULT '[]',
    operator_replies   TEXT NOT NULL DEFAULT '[]',
    operator_corrections TEXT NOT NULL DEFAULT '[]',
    event_count        INTEGER NOT NULL DEFAULT 0,
    last_seen_ts       TEXT,
    first_seen_at      TEXT NOT NULL,
    updated_at         TEXT NOT NULL
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
CREATE INDEX IF NOT EXISTS idx_clickup_event_log_task ON clickup_event_log(task_slug);
CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_task ON agent_runtime_states(task_slug);
CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_updated ON agent_runtime_states(updated_at);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_parent ON task_dependencies(parent_slug);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_child ON task_dependencies(child_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_to ON task_links(to_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_from ON task_links(from_slug);
CREATE INDEX IF NOT EXISTS idx_brain_runs_family_started ON brain_runs(family_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_task_started ON brain_runs(task_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_status_updated ON brain_runs(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_feed ON steering_trace(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created ON steering_trace(created_at);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created_id ON steering_trace(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_created_id ON steering_trace(disposition, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_source_created_id ON steering_trace(source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_source_created_id ON steering_trace(disposition, source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_funnel ON steering_trace(created_at, disposition, stage_reached);
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
    deleted_at       TEXT,
    muted_at         TEXT
);

CREATE INDEX IF NOT EXISTS idx_chats_last_activity ON chats(last_activity_at DESC);

CREATE TABLE IF NOT EXISTS kb_capture (
    session_id   TEXT PRIMARY KEY,
    slug         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    cursor       INTEGER NOT NULL DEFAULT 0,
    captured_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS remote_devices (
    id            TEXT PRIMARY KEY,
    label         TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    last_seen_at  TEXT,
    revoked_at    TEXT
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
