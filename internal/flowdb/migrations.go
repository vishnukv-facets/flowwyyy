package flowdb

// This file holds schema migrations and schema_meta helpers,
// split out of db.go. OpenDB (in db.go) calls runMigrations here.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

func runMigrations(db *sql.DB) error {
	// Wipe Brain-orchestration remnants on boot (removed in #34). brain_runs
	// is still the live autonomous-run ledger and is intentionally kept; these
	// three are the dead orchestration tables. Foreign keys are toggled off
	// for the duration in case the remnants FK back to one another.
	//
	// The connector legacy tables (monitor_*/external_messages/
	// slack_oauth_tokens/task_pr_links/automation_rules) are product-owned and
	// dropped by productdb.Ensure — not here — so the core schema stays free
	// of any connector concept.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys for legacy drop: %w", err)
	}
	for _, t := range []string{
		"brain_plans",
		"brain_policy",
		"brain_action_audit",
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

	// playbooks table: the table itself is created via schemaDDL on every
	// OpenDB, but CREATE TABLE IF NOT EXISTS never adds NEW columns to an
	// existing table — so the scheduling columns need explicit ALTERs for DBs
	// that predate them. (Fresh DBs already have them from schemaDDL.)
	for _, col := range []string{
		"schedule_spec", "schedule_input", "schedule_paused_at",
		"next_fire_at", "last_fired_at", "last_fire_run_slug",
	} {
		has, err = columnExists(db, "playbooks", col)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE playbooks ADD COLUMN ` + col + ` TEXT`); err != nil {
				return fmt.Errorf("add playbooks.%s: %w", col, err)
			}
		}
	}

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

// ColumnExists reports whether table has the given column. Exported so a
// registered product migration set (internal/productdb) can gate its own
// ADD COLUMN migrations without re-implementing the PRAGMA probe.
func ColumnExists(db *sql.DB, table, column string) (bool, error) {
	return columnExists(db, table, column)
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
