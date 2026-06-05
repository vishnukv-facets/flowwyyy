package monitor

import (
	"database/sql"
	"testing"

	"flow/internal/flowdb"
)

func seedWaitingTask(t *testing.T, db *sql.DB, slug, waiting string) {
	t.Helper()
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, waiting_on, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'medium', ?, 'default', 'claude', ?, ?, ?, ?)`,
		slug, "waiting task", t.TempDir(), waiting, now, now, now,
	); err != nil {
		t.Fatalf("seed waiting task %s: %v", slug, err)
	}
}

func taskWaitingOn(t *testing.T, db *sql.DB, slug string) string {
	t.Helper()
	var w sql.NullString
	if err := db.QueryRow(`SELECT waiting_on FROM tasks WHERE slug=?`, slug).Scan(&w); err != nil {
		t.Fatalf("read waiting_on %s: %v", slug, err)
	}
	return w.String
}

func TestAutoResolveWaitingOn(t *testing.T) {
	db := dispatcherTestDB(t)
	self := []string{"U_ME"}

	seedWaitingTask(t, db, "ext", "Anshul to reply")
	if !autoResolveWaitingOn(db, "ext", "U_OTHER", self) {
		t.Error("external reply should resolve waiting_on")
	}
	if w := taskWaitingOn(t, db, "ext"); w != "" {
		t.Errorf("waiting_on = %q, want cleared", w)
	}

	seedWaitingTask(t, db, "selfreply", "Anshul to reply")
	if autoResolveWaitingOn(db, "selfreply", "u_me", self) { // case-insensitive self match
		t.Error("operator's own reply must NOT resolve their wait")
	}
	if w := taskWaitingOn(t, db, "selfreply"); w == "" {
		t.Error("self reply should leave waiting_on intact")
	}

	seedWaitingTask(t, db, "botreply", "Anshul to reply")
	if autoResolveWaitingOn(db, "botreply", "", self) {
		t.Error("empty author (bot/system) must not resolve")
	}

	seedWaitingTask(t, db, "nowait", "")
	if autoResolveWaitingOn(db, "nowait", "U_OTHER", self) {
		t.Error("no waiting_on → nothing to resolve")
	}
}

func TestAutoResolveWaitingOnGateOff(t *testing.T) {
	t.Setenv("FLOW_STEERING_AUTO_RESOLVE_WAITING", "0")
	db := dispatcherTestDB(t)
	seedWaitingTask(t, db, "gated", "Anshul to reply")
	if autoResolveWaitingOn(db, "gated", "U_OTHER", []string{"U_ME"}) {
		t.Error("gate off → must not resolve")
	}
	if w := taskWaitingOn(t, db, "gated"); w == "" {
		t.Error("gate off should leave waiting_on intact")
	}
}
