package flowdb

import "testing"

// AwaitingHumanInput must be NARROWER than Status=="waiting": a session is only
// "blocked on the operator" when it asked a question (elicitation / plan
// approval) or hit a tool-permission prompt. idle_prompt and Codex's
// turn-boundary stop also surface as Status=="waiting" but are NOT blocked on a
// human answer, so they must remain wakeable (false). This is the exact
// distinction the wake-fix depends on.
func TestAgentRuntimeStateAwaitingHumanInput(t *testing.T) {
	cases := []struct {
		name      string
		state     *AgentRuntimeState
		wantBlock bool
	}{
		{"nil", nil, false},
		{"running", &AgentRuntimeState{Status: "running", EventKind: "pre_tool_use"}, false},
		{"idle", &AgentRuntimeState{Status: "idle", EventKind: "stop"}, false},
		{"released", &AgentRuntimeState{Status: "released", EventKind: "session_end"}, false},
		// The dangerous trio that all stored as Status=="waiting":
		{"elicitation (AskUserQuestion)", &AgentRuntimeState{Status: "waiting", EventKind: "elicitation"}, true},
		{"permission_request", &AgentRuntimeState{Status: "waiting", EventKind: "permission_request"}, true},
		{"permission_prompt", &AgentRuntimeState{Status: "waiting", EventKind: "permission_prompt"}, true},
		// ...but these "waiting" states are NOT operator-blocked and must wake:
		{"idle_prompt is wakeable", &AgentRuntimeState{Status: "waiting", EventKind: "idle_prompt"}, false},
		{"codex stop is wakeable", &AgentRuntimeState{Status: "waiting", EventKind: "stop"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.AwaitingHumanInput(); got != tc.wantBlock {
				t.Fatalf("AwaitingHumanInput() = %v, want %v", got, tc.wantBlock)
			}
		})
	}
}

func TestUpsertAgentRuntimeStateLogsSessionBindOnce(t *testing.T) {
	db := openTempDB(t)
	insertProject(t, db, "ledger-proj", "Ledger project", t.TempDir(), "high")
	insertTask(t, db, "ledger-task", "Ledger task", "backlog", "high", t.TempDir(), "ledger-proj")
	ctx, err := CreateWorkContext(db, WorkContext{Title: "Runtime context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}

	if err := UpsertAgentRuntimeState(db, AgentRuntimeStateInput{
		Provider:      "codex",
		SessionID:     "codex-session-1",
		TaskSlug:      "ledger-task",
		WorkContextID: ctx.ID,
		Status:        "running",
		EventKind:     "session_start",
		Seq:           1,
	}); err != nil {
		t.Fatalf("UpsertAgentRuntimeState first: %v", err)
	}
	if err := UpsertAgentRuntimeState(db, AgentRuntimeStateInput{
		Provider:      "codex",
		SessionID:     "codex-session-1",
		TaskSlug:      "ledger-task",
		WorkContextID: ctx.ID,
		Status:        "waiting",
		EventKind:     "stop",
		Seq:           2,
	}); err != nil {
		t.Fatalf("UpsertAgentRuntimeState update: %v", err)
	}

	rows, err := ListWorkEventLog(db, WorkEventLogFilter{EventType: "session_bound", TaskSlug: "ledger-task"})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("session_bound rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.EventID != "session-bound:codex:codex-session-1:ledger-task" {
		t.Fatalf("EventID = %q", got.EventID)
	}
	if got.Provider != "codex" || got.SessionID != "codex-session-1" || got.ProjectSlug != "ledger-proj" || got.WorkContextID != ctx.ID {
		t.Fatalf("missing session provenance: %+v", got)
	}
	if got.ActorKind != "agent" || got.ActorID != "codex:codex-session-1" {
		t.Fatalf("actor = %q/%q, want agent/codex:codex-session-1", got.ActorKind, got.ActorID)
	}
}
