package server

import "testing"

// Adhoc Ask Flow sessions must be listable (for the tray), survive in the
// registry until explicitly closed, and be removed by the close action — this
// is what lets the tray persist across navigation/reload and end sessions.
func TestFloatingSessions_RegisterListAndClose(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	srv.terminals.registerFloatingLaunch(terminalLaunch{Slug: "overview-aaa", Provider: "claude"}, "Ask Flow")
	srv.terminals.registerFloatingLaunch(terminalLaunch{Slug: "overview-bbb", Provider: "codex"}, "Triage day")

	sessions := srv.terminals.floatingSessions()
	if len(sessions) != 2 {
		t.Fatalf("floatingSessions = %d, want 2", len(sessions))
	}
	byID := map[string]floatingSessionInfo{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	if byID["overview-aaa"].Provider != "claude" || byID["overview-aaa"].Title != "Ask Flow" {
		t.Fatalf("overview-aaa = %+v", byID["overview-aaa"])
	}
	if byID["overview-bbb"].Provider != "codex" || byID["overview-bbb"].Title != "Triage day" {
		t.Fatalf("overview-bbb = %+v", byID["overview-bbb"])
	}
	if byID["overview-aaa"].Running {
		t.Fatal("session should not report Running before its PTY is attached")
	}

	// The tray reads the list off the UiData snapshot.
	data, err := srv.buildUIData()
	if err != nil {
		t.Fatalf("buildUIData: %v", err)
	}
	if len(data.FloatingSessions) != 2 {
		t.Fatalf("uiData.FLOATING_SESSIONS = %d, want 2", len(data.FloatingSessions))
	}

	// Closing via the action surface removes exactly that session.
	resp, status := srv.runAction(actionRequest{Kind: "close-floating-terminal", Slug: "overview-aaa"})
	if status != 200 || !resp.OK {
		t.Fatalf("close action resp=%+v status=%d", resp, status)
	}
	left := srv.terminals.floatingSessions()
	if len(left) != 1 || left[0].ID != "overview-bbb" {
		t.Fatalf("after close, sessions = %+v", left)
	}

	// Idempotent: closing an already-gone id still succeeds so the UI can prune.
	resp, status = srv.runAction(actionRequest{Kind: "close-floating-terminal", Slug: "overview-zzz"})
	if status != 200 || !resp.OK {
		t.Fatalf("idempotent close resp=%+v status=%d", resp, status)
	}
}
