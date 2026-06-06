package server

import "testing"

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
