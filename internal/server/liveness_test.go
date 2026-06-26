package server

import "testing"

// A FRESH codex session (launched with a prompt, not `codex resume <id>`)
// carries no session id on its command line, so it must be detected as live by
// its working dir (-C) instead — otherwise the reconciler flips an
// actively-running session to "dead".
func TestScanLiveSessionsTracksCodexWorkdirs(t *testing.T) {
	oldScanner := reconcileScanner
	defer func() { reconcileScanner = oldScanner }()

	reconcileScanner = func() ([]byte, error) {
		return []byte(
			"201 node /opt/homebrew/bin/codex --no-alt-screen -C /repo/.codex/worktrees/anikoto-provider You are the execution session\n",
		), nil
	}

	live, err := scanLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if !live.codexDirLive("/repo/.codex/worktrees/anikoto-provider") {
		t.Fatal("fresh codex session should be live via its -C working dir")
	}
	if live.codexDirLive("/repo/.codex/worktrees/other") {
		t.Fatal("an unrelated dir must not be reported live")
	}
	// No session id on the command line → id-based match must stay false.
	if live.has("codex", "019eff1e-55e2-7f03-8540-75445627827b") {
		t.Fatal("fresh codex has no id on cmdline; id match must be false")
	}
}

func TestScanLiveSessionsTracksCodexResumeIDs(t *testing.T) {
	oldScanner := reconcileScanner
	defer func() { reconcileScanner = oldScanner }()

	liveID := "11111111-1111-4111-8111-111111111111"
	deadID := "22222222-2222-4222-8222-222222222222"
	reconcileScanner = func() ([]byte, error) {
		return []byte(
			"101 codex resume --include-non-interactive --no-alt-screen -C /repo " + liveID + "\n" +
				"102 codex --no-alt-screen -C /other fresh prompt without session id\n",
		), nil
	}

	live, err := scanLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if !live.has("codex", liveID) {
		t.Fatalf("codex resume session %s should be live", liveID)
	}
	if live.has("codex", deadID) {
		t.Fatalf("codex session %s should not be live just because another codex process exists", deadID)
	}
}
