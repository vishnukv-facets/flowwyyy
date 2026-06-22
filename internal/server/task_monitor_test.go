package server

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func TestTaskNeedsMonitor(t *testing.T) {
	cases := []struct {
		name string
		task *flowdb.Task
		tags []string
		want bool
	}{
		{"slack-reply backlog", &flowdb.Task{Status: "backlog"}, []string{"slack-reply"}, true},
		{"gh-pr in-progress", &flowdb.Task{Status: "in-progress"}, []string{"gh-pr:o/r#1"}, true},
		{"gh-issue backlog", &flowdb.Task{Status: "backlog"}, []string{"gh-issue:o/r#9"}, true},
		{"slack-thread in-progress", &flowdb.Task{Status: "in-progress"}, []string{"slack-thread:C1:123.45"}, true},
		{"worktree but no origin tag", &flowdb.Task{Status: "backlog", WorktreePath: nullStr("/tmp/wt")}, nil, false},
		{"worktree + plain slack label only", &flowdb.Task{Status: "in-progress", WorktreePath: nullStr("/tmp/wt")}, []string{"bugfix", "cli", "slack"}, false},
		{"no origin no worktree", &flowdb.Task{Status: "backlog"}, []string{"ui", "p1"}, false},
		{"gh-pr but done", &flowdb.Task{Status: "done"}, []string{"gh-pr:o/r#1"}, false},
		{"gh-pr but archived", &flowdb.Task{Status: "backlog", ArchivedAt: nullStr("2026-05-01T00:00:00Z")}, []string{"gh-pr:o/r#1"}, false},
		{"gh-pr but deleted", &flowdb.Task{Status: "backlog", DeletedAt: nullStr("2026-05-01T00:00:00Z")}, []string{"gh-pr:o/r#1"}, false},
		{"nil task", nil, []string{"slack-reply"}, false},
	}
	for _, c := range cases {
		if got := taskNeedsMonitor(c.task, c.tags); got != c.want {
			t.Errorf("%s: taskNeedsMonitor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRespawnGate(t *testing.T) {
	g := newRespawnGate(50 * time.Millisecond)
	if !g.allow("a") {
		t.Fatal("first allow(a) should be true")
	}
	if g.allow("a") {
		t.Fatal("second allow(a) within window should be false (debounced)")
	}
	if !g.allow("b") {
		t.Fatal("allow(b) should be true — debounce is per-slug")
	}
	time.Sleep(60 * time.Millisecond)
	if !g.allow("a") {
		t.Fatal("allow(a) after window should be true again")
	}

	var nilGate *respawnGate
	if !nilGate.allow("x") {
		t.Fatal("nil gate should allow")
	}
}

// TestWakeSharedTaskInjectsIntoTmux covers the restart-gap wake fix: when no
// browser PTY is attached (e.g. right after a `flow ui serve` restart) but the
// agent is still alive in its detached tmux session, the wake must be injected
// straight into tmux via send-keys — bracketed paste then a delayed Enter,
// mirroring wakeTask. An unknown slug (no tmux session) must NOT claim a wake.
func TestWakeSharedTaskInjectsIntoTmux(t *testing.T) {
	oldLook, oldCmd := sharedTerminalLookPath, sharedTerminalCommand
	resetSharedTerminalAvailable()
	t.Cleanup(func() {
		sharedTerminalLookPath = oldLook
		sharedTerminalCommand = oldCmd
		resetSharedTerminalAvailable()
	})
	sharedTerminalLookPath = func(string) (string, error) { return "/usr/bin/tmux", nil }

	live := sharedTerminalSessionName("my-task") // the only session that "exists"
	var mu sync.Mutex
	var cmds [][]string
	sharedTerminalCommand = func(args ...string) ([]byte, error) {
		mu.Lock()
		cmds = append(cmds, append([]string(nil), args...))
		mu.Unlock()
		if len(args) > 0 && args[0] == "has-session" {
			if args[len(args)-1] == live {
				return nil, nil
			}
			return nil, fmt.Errorf("missing session")
		}
		return nil, nil
	}

	h := &terminalHub{}

	if !h.wakeSharedTask("my-task", "wake up: 2 new events") {
		t.Fatal("wakeSharedTask should return true when the tmux session exists")
	}

	wantPaste := "\x1b[200~wake up: 2 new events\x1b[201~"
	pasteSeen, enterSeen := false, false
	deadline := time.Now().Add(2 * time.Second) // wait for the delayed (250ms) Enter
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, c := range cmds {
			if len(c) == 5 && c[0] == "send-keys" && c[2] == live && c[3] == "-l" && c[4] == wantPaste {
				pasteSeen = true
			}
			if len(c) == 4 && c[0] == "send-keys" && c[2] == live && c[3] == "Enter" {
				enterSeen = true
			}
		}
		done := pasteSeen && enterSeen
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !pasteSeen {
		t.Error("expected a bracketed-paste send-keys carrying the wake prompt")
	}
	if !enterSeen {
		t.Error("expected a delayed Enter send-keys to submit the prompt")
	}

	if h.wakeSharedTask("ghost-task", "nope") {
		t.Error("wakeSharedTask should return false when no tmux session exists for the slug")
	}
}

func insertMonitorTask(t *testing.T, db *sql.DB, slug, status, worktree string, tags ...string) {
	t.Helper()
	now := "2026-05-28T10:00:00Z"
	var wt any
	if worktree != "" {
		wt = worktree
	}
	// The tasks CHECK constraint requires in-progress claude tasks to carry a
	// session_id; give one (slug-derived → unique).
	var sid any
	if status == "in-progress" {
		sid = slug + "-session"
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, priority, work_dir, worktree_path, session_id, created_at, updated_at, session_provider)
		 VALUES (?, ?, ?, 'regular', 'medium', '/tmp', ?, ?, ?, ?, 'claude')`,
		slug, slug, status, wt, sid, now, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if err := flowdb.AddTaskTag(db, slug, tag); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMonitorReconcilerConverges(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", root)
	srv := New(Config{DB: db, FlowRoot: root, Version: "test"})
	t.Cleanup(func() {
		for _, slug := range srv.inboxMonitors.runningSlugs() {
			srv.inboxMonitors.stop(slug)
		}
	})

	insertMonitorTask(t, db, "slack-task", "backlog", "", "slack-reply")
	insertMonitorTask(t, db, "ghpr-task", "in-progress", "", "gh-pr:o/r#1")
	insertMonitorTask(t, db, "branch-task", "backlog", "/tmp/wt") // worktree but no PR → not monitored
	insertMonitorTask(t, db, "plain-task", "backlog", "")
	insertMonitorTask(t, db, "done-ghpr", "done", "", "gh-pr:o/r#2")

	r := newMonitorReconciler(srv)
	r.tick()

	want := map[string]bool{
		"slack-task":  true,
		"ghpr-task":   true,
		"branch-task": false,
		"plain-task":  false,
		"done-ghpr":   false,
	}
	for slug, w := range want {
		if got := srv.inboxMonitors.running(slug); got != w {
			t.Errorf("after tick: running(%q) = %v, want %v", slug, got, w)
		}
	}

	// When a monitored task finishes, the next tick must stop its monitor.
	if _, err := db.Exec(`UPDATE tasks SET status = 'done' WHERE slug = 'slack-task'`); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if srv.inboxMonitors.running("slack-task") {
		t.Error("monitor for slack-task should stop once the task is done")
	}
	if !srv.inboxMonitors.running("ghpr-task") {
		t.Error("ghpr-task monitor should still be running")
	}
}

// TestNudgeSessionUsesSharedTmux is the manual-nudge twin of the inbox-monitor
// restart-gap fix: a session whose agent is alive but has no browser PTY attached
// in THIS server process (the post-`flow ui serve`-restart state) is still
// reachable through its detached tmux session. nudgeSession must inject there —
// mirroring deliverInboxEvents — instead of misreading it as a native/external
// terminal and refusing with "open it there to send".
func TestNudgeSessionUsesSharedTmux(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", root)

	const sid = "11111111-1111-4111-8111-111111111111"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, priority, work_dir, session_id, created_at, updated_at, session_provider)
		 VALUES ('ext-tmux','Ext tmux','in-progress','regular','medium','/tmp',?, '2026-05-28T10:00:00Z','2026-05-28T10:00:00Z','claude')`,
		sid,
	); err != nil {
		t.Fatal(err)
	}

	// The agent appears alive in the OS process table — without the tmux step
	// below, nudgeSession would classify this as an external terminal and refuse.
	oldPS := psRunner
	psRunner = func() ([]byte, error) { return []byte("12345 claude --session-id " + sid + "\n"), nil }
	t.Cleanup(func() { psRunner = oldPS })

	// A detached tmux session exists for the slug, so the wake can be injected.
	oldLook, oldCmd := sharedTerminalLookPath, sharedTerminalCommand
	resetSharedTerminalAvailable()
	t.Cleanup(func() {
		sharedTerminalLookPath = oldLook
		sharedTerminalCommand = oldCmd
		resetSharedTerminalAvailable()
	})
	sharedTerminalLookPath = func(string) (string, error) { return "/usr/bin/tmux", nil }
	live := sharedTerminalSessionName("ext-tmux")
	sharedTerminalCommand = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "has-session" {
			if args[len(args)-1] == live {
				return nil, nil
			}
			return nil, fmt.Errorf("missing session")
		}
		return nil, nil
	}

	// caches nil → cachedLiveAgentSessions calls psRunner directly; empty hub →
	// no browser PTY attached (terminals.running is false).
	s := &Server{cfg: Config{DB: db}, terminals: &terminalHub{}}

	resp, code := s.nudgeSession("ext-tmux", "ping manan and ask if they replied?")
	if !resp.OK {
		t.Fatalf("nudge refused (code %d): %q — should inject via the detached tmux session", code, resp.Message)
	}
}
