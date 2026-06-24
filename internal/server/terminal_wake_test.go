package server

import (
	"path/filepath"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

// sessionBooted gates the wake paste on the resumed/woken session having gone
// quiet — the fix for the laptop-sleep→wake race where a paste landed mid-boot
// and vanished while delivery still reported "delivered".
func TestSessionBooted(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 4, 0, 0, time.UTC)
	stable := 1500 * time.Millisecond
	cases := []struct {
		name       string
		sawOutput  bool
		lastOutput time.Time
		want       bool
	}{
		{"no output yet (booting) → not ready", false, time.Time{}, false},
		{"output seen but zero ts → not ready", true, time.Time{}, false},
		{"output still flowing (within stable) → not ready", true, now.Add(-500 * time.Millisecond), false},
		{"output quiesced past stable → ready", true, now.Add(-2 * time.Second), true},
		{"long-idle session (old output) → ready immediately", true, now.Add(-30 * time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionBooted(tc.sawOutput, tc.lastOutput, now, stable); got != tc.want {
				t.Errorf("sessionBooted(%v, %v) = %v, want %v", tc.sawOutput, tc.lastOutput, got, tc.want)
			}
		})
	}
}

// Regression guard for the incident: a wake that arrives while a session is
// blocked on the operator's input must be BUFFERED (persisted), never injected —
// injecting would auto-submit the open prompt and, in the incident, fired an
// unreviewed Slack reply. We seed a session whose recorded runtime state is an
// open AskUserQuestion (elicitation) and assert wakeTask parks the prompt in
// pending_wakes instead of touching the PTY. The session leaving the wait then
// re-opens the gate.
func TestWakeTaskBuffersWhileAwaitingHumanInput(t *testing.T) {
	root := t.TempDir()
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	const slug = "demo"
	const sid = "11111111-1111-4111-8111-111111111111"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, session_id, created_at, updated_at)
		 VALUES (?, ?, 'in-progress', 'medium', ?, 'claude', ?, ?, ?)`,
		slug, "Demo", root, sid, productdb.NowISO(), productdb.NowISO(),
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Record that the session is blocked on an AskUserQuestion (elicitation).
	if err := productdb.UpsertAgentRuntimeState(db, productdb.AgentRuntimeStateInput{
		Provider: "claude", SessionID: sid, TaskSlug: slug,
		Status: "waiting", EventKind: "elicitation",
	}); err != nil {
		t.Fatalf("seed runtime state: %v", err)
	}

	s := New(Config{DB: db, FlowRoot: root})

	if !s.terminals.awaitingHumanInput(slug) {
		t.Fatal("awaitingHumanInput should be true for an open elicitation")
	}
	if err := s.terminals.wakeTask(slug, "new Slack message"); err != nil {
		t.Fatalf("wakeTask: %v", err)
	}
	// Buffered, not injected.
	if pw, ok := s.terminals.wakes.peek(slug); !ok || pw.Prompt != "new Slack message" {
		t.Fatalf("expected the wake to be buffered; peek = %q,%v", pw.Prompt, ok)
	}

	// Leaving the human-input wait re-opens the gate (flushWakes can deliver).
	if err := productdb.UpsertAgentRuntimeState(db, productdb.AgentRuntimeStateInput{
		Provider: "claude", SessionID: sid, TaskSlug: slug,
		Status: "running", EventKind: "post_tool_use",
	}); err != nil {
		t.Fatalf("transition state: %v", err)
	}
	if s.terminals.awaitingHumanInput(slug) {
		t.Fatal("awaitingHumanInput should be false after leaving elicitation")
	}
}
