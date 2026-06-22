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
