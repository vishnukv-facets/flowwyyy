package server

import "testing"

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
