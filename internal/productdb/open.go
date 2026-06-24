package productdb

// open.go is flowwyyy's OWN database entry point for the two-binary world
// (Phase-3, seam §11). The product binary (flowwyyy) opens the shared ~/.flow
// flow.db that the official `flow` binary created via `flow init`, and layers
// on every table official flow LACKS:
//
//   - the 6 "core-gap" tables (brain_runs, task_dependencies, task_links,
//     agent_runtime_states, pending_wakes, search_docs + its FTS indexes) —
//     tables flowwyyy's fork added that upstream flow has no concept of, and
//   - the 13 product tables (attention_*/steering_*/github_*/chats/
//     remote_devices/pending_sends/kb_capture) created by Ensure.
//
// It does NOT create the Bucket-O tables (tasks/projects/playbooks/owners/
// workdirs/task_tags/schema_meta): those are owned and migrated by official
// flow's `flow init`, so flowwyyy reads them but never re-declares their schema.
// CREATE ... IF NOT EXISTS keeps Open idempotent against both a fresh
// official-flow DB and an in-repo dev DB that flowdb already fully populated.

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Open opens (or creates) the flow.db at path and ensures every flowwyyy-owned
// (Bucket-F) table exists. Connection setup mirrors flowdb.OpenDB exactly —
// busy_timeout(30000) on both the DSN and an explicit PRAGMA so every pooled
// connection inherits it, plus foreign_keys=ON — so concurrent access between
// the flow and flowwyyy binaries on the same file behaves identically.
//
// It is the product binary's single DB entry point: replaces flowdb.OpenDB for
// flowwyyy. The Bucket-O tables are assumed present (created by `flow init`);
// Open never declares them.
func Open(path string) (*sql.DB, error) {
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
	// Core-gap tables first (they FK to tasks, which official flow already
	// created), then the product tables via Ensure (legacy drops + ADD COLUMN
	// migrations). Both idempotent.
	if _, err := db.Exec(coreGapSchemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply core-gap schema: %w", err)
	}
	if err := Ensure(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure product schema: %w", err)
	}
	return db, nil
}

// coreGapSchemaDDL declares the tables flowwyyy's fork added that official flow
// does not ship — the "6 core-gap" set from seam §11. Verbatim copies of the
// matching statements in flowdb's coreSchemaDDL (same columns, constraints,
// indexes, FTS config, and triggers) so the shared schema stays identical
// whichever binary created the table. Bucket-O declarations are deliberately
// excluded. Each statement is idempotent (CREATE ... IF NOT EXISTS).
const coreGapSchemaDDL = `
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

CREATE TABLE IF NOT EXISTS pending_wakes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    slug       TEXT NOT NULL,
    prompt     TEXT NOT NULL,
    created_at TEXT NOT NULL
);

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

CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_task ON agent_runtime_states(task_slug);
CREATE INDEX IF NOT EXISTS idx_agent_runtime_states_updated ON agent_runtime_states(updated_at);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_parent ON task_dependencies(parent_slug);
CREATE INDEX IF NOT EXISTS idx_task_dependencies_child ON task_dependencies(child_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_to ON task_links(to_slug);
CREATE INDEX IF NOT EXISTS idx_task_links_from ON task_links(from_slug);
CREATE INDEX IF NOT EXISTS idx_brain_runs_family_started ON brain_runs(family_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_task_started ON brain_runs(task_slug, started_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_brain_runs_status_updated ON brain_runs(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_search_docs_scope ON search_docs(scope);
CREATE INDEX IF NOT EXISTS idx_search_docs_entity ON search_docs(entity_type, entity_slug);
`
