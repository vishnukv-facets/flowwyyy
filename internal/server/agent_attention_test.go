package server

import (
	"flow/internal/flowdb"
	"os"
	"path/filepath"
	"testing"
)

func TestContextWindowForProvider(t *testing.T) {
	if got := contextWindowForProvider("claude"); got != 1000000 {
		t.Fatalf("claude context window = %d, want 1000000", got)
	}
	if got := contextWindowForProvider("codex"); got != 200000 {
		t.Fatalf("codex context window = %d, want 200000", got)
	}
}

func TestSessionTranscriptUsageStats(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","timestamp":"2026-05-16T12:00:00Z","message":{"role":"assistant","usage":{"input_tokens":10,"cache_read_input_tokens":20,"output_tokens":5},"content":[{"type":"text","text":"Done"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := sessionTranscriptUsageStats(claudePath)
	if claude.TokensUsed != 35 || claude.LastTimestamp != "2026-05-16T12:00:00Z" {
		t.Fatalf("claude stats = %+v, want 35 tokens and timestamp", claude)
	}

	codexPath := filepath.Join(dir, "codex.jsonl")
	codexLine := `{"type":"event_msg","timestamp":"2026-05-16T12:01:00Z","payload":{"type":"token_count","info":{"model_context_window":258400,"last_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":25,"reasoning_output_tokens":5,"total_tokens":180},"total_token_usage":{"total_tokens":999}}}}`
	if err := os.WriteFile(codexPath, []byte(codexLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codex := sessionTranscriptUsageStats(codexPath)
	if codex.TokensUsed != 180 || codex.TokensMax != 258400 || codex.LastTimestamp != "2026-05-16T12:01:00Z" {
		t.Fatalf("codex stats = %+v, want reported usage/window/timestamp", codex)
	}
}

func TestAttentionDetectionCatchesPermissionsAndQuestions(t *testing.T) {
	kind, excerpt := attentionFromText("Would you like to run the following command?\n$ kill 123\n1. Yes, proceed")
	if kind != "permission" || excerpt == "" {
		t.Fatalf("permission attention = kind %q excerpt %q", kind, excerpt)
	}
	action, question, kind := latestTranscriptAction([]uiTranscript{{Type: "assistant", Text: "Should I proceed with the deploy?"}})
	if kind != "question" || question == "" || action == "" {
		t.Fatalf("question action=%q question=%q kind=%q", action, question, kind)
	}
}

func TestTerminalAttentionTracksOnlyUnansweredPermission(t *testing.T) {
	unanswered := "Would you like to run the following command?\n$ kill 38173\n> 1. Yes, proceed (y)\n2. Yes, and don't ask again\nPress enter to confirm or esc to cancel"
	kind, excerpt := terminalAttentionFromText(unanswered)
	if kind != "permission" || excerpt == "" {
		t.Fatalf("unanswered terminal attention = kind %q excerpt %q, want permission", kind, excerpt)
	}

	answered := unanswered + "\nBash(kill 38173)\n  ok command completed"
	kind, excerpt = terminalAttentionFromText(answered)
	if kind != "" || excerpt != "" {
		t.Fatalf("answered terminal attention = kind %q excerpt %q, want none", kind, excerpt)
	}
}

func TestTerminalAttentionClearsAnsweredClaudeQuestion(t *testing.T) {
	scrollback := "\x1b[38;2;153;153;153mSave both files as drafted above?\x1b[39m\n" +
		"User has answered your questions: Save both\n" +
		"Both saved:\n" +
		"- /Users/vishnukv/.flow/tasks/harness/updates/2026-05-16-phase-1-2-spec.md\n" +
		"- /Users/vishnukv/.flow/tasks/harness/plan.md\n" +
		"* Worked for 9m 10s\n" +
		"* recap: Goal captured. Next: implement phase 1 step 1.\n" +
		"> "
	kind, excerpt := terminalAttentionFromText(scrollback)
	if kind != "" || excerpt != "" {
		t.Fatalf("answered Claude question attention = kind %q excerpt %q, want none", kind, excerpt)
	}
}

func TestTerminalScrollbackClearsStaleTranscriptAttention(t *testing.T) {
	srv := &Server{}
	srv.terminals = newTerminalHub(srv)
	srv.terminals.sessions["commit-local-changes"] = &terminalSession{
		slug:       "commit-local-changes",
		scrollback: []byte("Would you like to run the following command?\n$ git add internal/\n1. Yes, proceed\nBash(git add internal/)\n  ok 94 files changed"),
	}
	promptTranscript := []uiTranscript{{Type: "assistant", Text: "Would you like to run the following command?\n$ git add internal/\n1. Yes, proceed"}}

	insights := srv.sessionInsightsForTask(TaskView{Slug: "commit-local-changes"}, "claude", promptTranscript)
	if insights.AskedQuestion || insights.AttentionKind != "" {
		t.Fatalf("insights = %+v, want stale transcript attention cleared by terminal progress", insights)
	}
}

func TestAgentAttentionNotifications(t *testing.T) {
	notifs := agentAttentionNotifications([]uiAgent{
		{
			Slug:       "switcher",
			Name:       "Switcher",
			Provider:   "codex",
			Status:     "waiting",
			SessionID:  "019e2f71-82f0-76b0-9353-fbc4a662d442",
			LastAction: "permission requested",
			WaitingFor: &uiWaitingFor{Kind: "permission", Why: "Would you like to run the following command?"},
		},
	})
	if len(notifs) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifs))
	}
	if notifs[0].Level != "approval" || notifs[0].Status != "unread" || notifs[0].Source != "agent" {
		t.Fatalf("notification = %+v, want unread agent approval", notifs[0])
	}
}

func TestAgentAttentionNotificationCanBeDismissed(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{cfg: Config{DB: db}}
	agents := []uiAgent{
		{
			Slug:      "switcher",
			Name:      "Switcher",
			Provider:  "codex",
			Status:    "waiting",
			SessionID: "019e2f71-82f0-76b0-9353-fbc4a662d442",
			WaitingFor: &uiWaitingFor{
				Kind: "permission",
				Why:  "Would you like to run the following command?",
			},
		},
	}
	if got := srv.uiMonitor(agents).Unread; got != 1 {
		t.Fatalf("unread before dismiss = %d, want 1", got)
	}

	resp, status := srv.updateNotification(actionRequest{Kind: "notification-dismiss", Target: "agent-switcher-permission"})
	if status != 200 || !resp.OK {
		t.Fatalf("dismiss response = %#v status %d", resp, status)
	}
	monitor := srv.uiMonitor(agents)
	if monitor.Unread != 0 || len(monitor.Notifications) != 0 {
		t.Fatalf("monitor after dismiss = %+v, want no agent notification", monitor)
	}
}
