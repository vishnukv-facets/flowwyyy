// Package productdb owns the flowwyyy product layer's tables on the shared
// flow.db. These are the connector / Attention Router / steering / chat /
// remote-device tables — everything that the core `flow` engine has no concept
// of. It registers a flowdb.MigrationSet (via init) so that any binary which
// imports productdb gets the product schema created additively after the core
// schema; a core-only binary that never imports it creates a core-only DB.
//
// Scope note: only the product *DDL + migrations* live here. The product
// CRUD/query code still lives in flowdb for now (it moves to productdb in a
// later task), so this package intentionally has no read/write helpers yet.
package productdb

import (
	"database/sql"
	"fmt"

	"flow/internal/flowdb"
)

// init registers the product migration set with flowdb. flowdb.OpenDB applies
// it after the core schema + core migrations, so Ensure may reference core
// tables (e.g. github_event_log.task_slug REFERENCES tasks).
func init() {
	flowdb.RegisterMigrations(flowdb.MigrationSet{Domain: "flowwyyy", Apply: Ensure})
}

// Ensure creates and migrates the product tables on db. It is idempotent and
// safe to run on every OpenDB: CREATE ... IF NOT EXISTS for fresh DBs, gated
// ADD COLUMN migrations for older DBs, and a one-shot drop of dead connector
// tables. All product tables are additive over existing installs.
func Ensure(db *sql.DB) error {
	// Wipe legacy connector tables (the removed inbox/monitor/slack/github
	// poller feature). Any pre-existing install still carries data the current
	// code no longer touches. Foreign keys are toggled off for the duration so
	// child-table drops don't trip parent-table constraints — several of these
	// FK back to monitor_events / external_messages in the same drop set.
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

	// Fresh-DB table + index creation. Idempotent; a no-op on installs that
	// already have these tables.
	if _, err := db.Exec(productSchemaDDL); err != nil {
		return fmt.Errorf("apply product schema: %w", err)
	}

	// ADD COLUMN migrations for older DBs whose product tables predate these
	// columns. (CREATE TABLE IF NOT EXISTS never adds new columns to an
	// existing table, so fresh DBs get them from productSchemaDDL above and
	// these are no-ops there.)
	for _, m := range []struct{ table, column, alter string }{
		{"steering_trace", "stage1_reason", `ALTER TABLE steering_trace ADD COLUMN stage1_reason TEXT`},
		{"steering_trace", "ts", `ALTER TABLE steering_trace ADD COLUMN ts TEXT`},
		{"steering_trace", "team_id", `ALTER TABLE steering_trace ADD COLUMN team_id TEXT`},
		{"steering_trace", "url", `ALTER TABLE steering_trace ADD COLUMN url TEXT`},
		{"steering_trace", "autonomy_action", `ALTER TABLE steering_trace ADD COLUMN autonomy_action TEXT`},
		{"steering_trace", "autonomy_decision", `ALTER TABLE steering_trace ADD COLUMN autonomy_decision TEXT`},
		{"steering_trace", "autonomy_reason", `ALTER TABLE steering_trace ADD COLUMN autonomy_reason TEXT`},
		{"attention_feed", "linked_task", `ALTER TABLE attention_feed ADD COLUMN linked_task TEXT`},
		{"attention_feed", "channel", `ALTER TABLE attention_feed ADD COLUMN channel TEXT`},
		{"attention_feed", "channel_type", `ALTER TABLE attention_feed ADD COLUMN channel_type TEXT`},
		{"attention_feed", "author", `ALTER TABLE attention_feed ADD COLUMN author TEXT`},
		{"attention_feed", "ts", `ALTER TABLE attention_feed ADD COLUMN ts TEXT`},
		{"attention_feed", "team_id", `ALTER TABLE attention_feed ADD COLUMN team_id TEXT`},
		{"attention_feed", "url", `ALTER TABLE attention_feed ADD COLUMN url TEXT`},
		{"attention_feed", "retriaging_at", `ALTER TABLE attention_feed ADD COLUMN retriaging_at TEXT`},
		{"chats", "muted_at", `ALTER TABLE chats ADD COLUMN muted_at TEXT`},
		// Operator corrections on a thread's running understanding (the "correct
		// the steerer" button): authoritative operator-supplied context, fed
		// into deep triage as ground truth. JSON array, same shape as
		// operator_actions/replies.
		{"attention_thread_state", "operator_corrections", `ALTER TABLE attention_thread_state ADD COLUMN operator_corrections TEXT NOT NULL DEFAULT '[]'`},
	} {
		has, err := flowdb.ColumnExists(db, m.table, m.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(m.alter); err != nil {
				return fmt.Errorf("add %s.%s: %w", m.table, m.column, err)
			}
		}
	}

	return nil
}

// productSchemaDDL is the full DDL for the product tables, mirroring the
// fresh-DB column set. Statements are idempotent (CREATE ... IF NOT EXISTS).
// Indexes reference only base columns, so they are safe to create alongside the
// tables (before the ADD COLUMN migrations above).
const productSchemaDDL = `
CREATE TABLE IF NOT EXISTS github_event_log (
    event_key    TEXT PRIMARY KEY,
    event_kind   TEXT NOT NULL,
    task_slug    TEXT REFERENCES tasks(slug) ON DELETE SET NULL,
    raw_json     TEXT,
    processed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_github_event_log_task ON github_event_log(task_slug);

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
CREATE INDEX IF NOT EXISTS idx_attention_feed_status ON attention_feed(status);
CREATE INDEX IF NOT EXISTS idx_attention_feed_thread ON attention_feed(thread_key);

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
CREATE INDEX IF NOT EXISTS idx_attention_feedback_feed ON attention_feedback(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_created ON attention_feedback(created_at);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_channel ON attention_feedback(channel);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_author ON attention_feedback(author);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_action ON attention_feedback(suggested_action);
CREATE INDEX IF NOT EXISTS idx_attention_feedback_band ON attention_feedback(confidence_band);

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
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_feed ON attention_handoffs(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_receiver ON attention_handoffs(receiver);
CREATE INDEX IF NOT EXISTS idx_attention_handoffs_status ON attention_handoffs(status);

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
CREATE INDEX IF NOT EXISTS idx_steering_trace_feed ON steering_trace(feed_item_id);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created ON steering_trace(created_at);
CREATE INDEX IF NOT EXISTS idx_steering_trace_created_id ON steering_trace(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_created_id ON steering_trace(disposition, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_source_created_id ON steering_trace(source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition_source_created_id ON steering_trace(disposition, source, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_steering_trace_funnel ON steering_trace(created_at, disposition, stage_reached);
CREATE INDEX IF NOT EXISTS idx_steering_trace_disposition ON steering_trace(disposition);

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
