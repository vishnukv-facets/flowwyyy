package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// socketModeLockPath returns the path of the advisory lock file that
// serializes Slack Socket Mode ownership across flow processes.
//
// Slack delivers each Socket Mode event to exactly ONE connected socket.
// If two flow processes both open a connection with the same app token,
// Slack splits events between them — and any process pointed at a different
// FLOW_ROOT (a stray smoke-test server, a worktree build) routes "its"
// share of events into the wrong task inboxes, where they are silently
// dropped. To prevent that, only the first flow process to claim this lock
// starts a listener.
//
// The path is keyed by a hash of the app token (so distinct Slack apps
// don't block each other, and the secret never appears in a filename) and
// lives under the per-user cache dir — deliberately NOT under FLOW_ROOT,
// because the colliding processes can each have a different FLOW_ROOT. The
// cache dir is stable per-user and independent of $TMPDIR, so every flow
// process the user launches contends for the same lock.
func socketModeLockPath(appToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(appToken)))
	name := "slack-socketmode-" + hex.EncodeToString(sum[:])[:16] + ".lock"
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "flow")
	// Best-effort: if MkdirAll fails, the OpenFile in acquire surfaces the
	// error and the caller fails open (starts anyway with a warning).
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, name)
}
