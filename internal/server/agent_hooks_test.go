package server

import (
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"

	_ "modernc.org/sqlite"
)

func TestMaybeRegisterDMThread(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	db, err := flowdb.OpenDB(t.TempDir() + "/flow.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Bucket-O tag writes route through `flow update task --tag` (no flow binary
	// in this unit test). Stub the writer to perform the same DB effect so the
	// persistence assertion below still exercises the real tag derivation.
	srv := New(Config{DB: db})
	orig := taskTagWriter
	t.Cleanup(func() { taskTagWriter = orig })
	taskTagWriter = func(_ *Server, slug, tag string) error { return flowdb.AddTaskTag(db, slug, tag) }

	now := productdb.NowISO()
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, permission_mode, session_provider, status_changed_at, created_at, updated_at)
		 VALUES ('coinswitch','coinswitch','backlog','high',?, 'default','claude',?,?,?)`,
		t.TempDir(), now, now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// An agent DM send (thread-scoped) registers a slack-thread tag on the DM
	// channel + thread root — reusing the thread model so routing/backfill work.
	tag, ok := srv.maybeRegisterDMThread("PostToolUse", "coinswitch", map[string]any{
		"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
		"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1780480392.819809", "text": "hi"},
	})
	if !ok {
		t.Fatalf("expected registration, got ok=false")
	}
	if tag != "slack-thread:d03lh2rczmg:1780480392.819809" {
		t.Fatalf("tag = %q, want slack-thread:d03lh2rczmg:1780480392.819809", tag)
	}
	tags, err := productdb.GetTaskTags(db, "coinswitch")
	if err != nil {
		t.Fatalf("GetTaskTags: %v", err)
	}
	found := false
	for _, tg := range tags {
		if tg == "slack-thread:d03lh2rczmg:1780480392.819809" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DM-thread tag not persisted; tags=%v", tags)
	}

	// A send to a normal channel must NOT auto-register (origin thread is
	// already monitored; other channels are out of scope).
	if _, ok := srv.maybeRegisterDMThread("PostToolUse", "coinswitch", map[string]any{
		"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
		"tool_input": map[string]any{"channel": "C0B3L0D8QG1", "thread_ts": "1779359538.629579"},
	}); ok {
		t.Fatalf("channel send should not auto-register")
	}
}

func TestSlackDMSendFromHook(t *testing.T) {
	cases := []struct {
		name        string
		event       string
		payload     map[string]any
		wantChannel string
		wantThread  string
		wantOK      bool
	}{
		{
			name:  "claude send to DM with thread_ts",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1780480392.819809", "text": "hi"},
			},
			wantChannel: "D03LH2RCZMG", wantThread: "1780480392.819809", wantOK: true,
		},
		{
			name:  "codex send-message to DM with thread_ts",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "slack-send-message",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1780480392.819809"},
			},
			wantChannel: "D03LH2RCZMG", wantThread: "1780480392.819809", wantOK: true,
		},
		{
			name:  "fresh top-level DM: thread root from tool_response ts",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":     "mcp__claude_ai_Slack__slack_send_message",
				"tool_input":    map[string]any{"channel": "D03LH2RCZMG", "text": "kickoff"},
				"tool_response": map[string]any{"ok": true, "channel": "D03LH2RCZMG", "ts": "1780500000.000100"},
			},
			wantChannel: "D03LH2RCZMG", wantThread: "1780500000.000100", wantOK: true,
		},
		{
			name:  "send to a real channel (not DM) is ignored",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
				"tool_input": map[string]any{"channel": "C0B3L0D8QG1", "thread_ts": "1779359538.629579"},
			},
			wantOK: false,
		},
		{
			name:  "draft is not a send",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_send_message_draft",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1.0"},
			},
			wantOK: false,
		},
		{
			name:  "read tool to a DM is not a send",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_read_thread",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1.0"},
			},
			wantOK: false,
		},
		{
			name:  "not a PostToolUse event",
			event: "PreToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "thread_ts": "1.0"},
			},
			wantOK: false,
		},
		{
			name:  "DM send with no resolvable thread root is skipped",
			event: "PostToolUse",
			payload: map[string]any{
				"tool_name":  "mcp__claude_ai_Slack__slack_send_message",
				"tool_input": map[string]any{"channel": "D03LH2RCZMG", "text": "no thread"},
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, thr, ok := slackDMSendFromHook(tc.event, tc.payload)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ch=%q thr=%q)", ok, tc.wantOK, ch, thr)
			}
			if ok && (ch != tc.wantChannel || thr != tc.wantThread) {
				t.Fatalf("got (%q,%q), want (%q,%q)", ch, thr, tc.wantChannel, tc.wantThread)
			}
		})
	}
}

// Codex has no Notification/Elicitation/TeammateIdle hook (those are
// Claude-only), so its only "I've yielded to the user" signal is Stop. A Codex
// Stop therefore means "turn finished, awaiting your input" and must surface as
// "waiting" so the notification bell/toast fires — exactly like Claude does via
// its dedicated waiting events. Claude's Stop stays a quiet turn boundary.
func TestAgentHookRuntimeStatusCodexStopWaitsForUser(t *testing.T) {
	if got := agentHookRuntimeStatus("stop", "codex"); got != "waiting" {
		t.Fatalf("codex stop status = %q, want waiting", got)
	}
	if got := agentHookRuntimeStatus("stop", "claude"); got != "idle" {
		t.Fatalf("claude stop status = %q, want idle", got)
	}
	// Codex session_start is not a waiting state — it just launched.
	if got := agentHookRuntimeStatus("session_start", "codex"); got != "idle" {
		t.Fatalf("codex session_start status = %q, want idle", got)
	}
	// A genuine waiting event (Codex tool permission) stays waiting.
	if got := agentHookRuntimeStatus("permission_request", "codex"); got != "waiting" {
		t.Fatalf("codex permission_request status = %q, want waiting", got)
	}
}
