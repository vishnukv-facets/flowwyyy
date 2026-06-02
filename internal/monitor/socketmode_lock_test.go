//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package monitor

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireSocketModeLock_MutualExclusion is the core of the regression
// guard: a second acquire on the same lock path (separate file descriptor,
// as a separate process would have) must observe the lock held and decline.
// This is exactly the collision that let multiple flow servers split — and
// drop — Slack events.
func TestAcquireSocketModeLock_MutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sm.lock")

	f1, ok1, err := acquireSocketModeLock(path)
	if err != nil {
		t.Fatalf("first acquire err = %v", err)
	}
	if !ok1 || f1 == nil {
		t.Fatalf("first acquire should succeed; ok=%v file=%v", ok1, f1)
	}

	f2, ok2, err := acquireSocketModeLock(path)
	if err != nil {
		t.Fatalf("second acquire err = %v", err)
	}
	if ok2 || f2 != nil {
		t.Fatalf("second acquire should be declined while held; ok=%v file=%v", ok2, f2)
	}

	// Releasing the first frees the slot for a subsequent acquire — a clean
	// restart must be able to reclaim ownership.
	releaseSocketModeLock(f1)
	f3, ok3, err := acquireSocketModeLock(path)
	if err != nil {
		t.Fatalf("third acquire err = %v", err)
	}
	if !ok3 || f3 == nil {
		t.Fatalf("third acquire should succeed after release; ok=%v file=%v", ok3, f3)
	}
	releaseSocketModeLock(f3)
}

func TestSocketModeLockPath_KeyedByToken(t *testing.T) {
	a := socketModeLockPath("xapp-aaa")
	b := socketModeLockPath("xapp-bbb")
	if a == b {
		t.Fatalf("distinct app tokens must map to distinct lock paths; both = %q", a)
	}
	if a != socketModeLockPath("xapp-aaa") {
		t.Fatal("same app token must map to a stable lock path")
	}
	if strings.Contains(a, "xapp-aaa") {
		t.Fatalf("lock path %q must not embed the raw app token", a)
	}
}

// TestSlackListener_SkipsWhenAnotherProcessHoldsLock verifies the wiring:
// with Socket Mode fully configured but the singleton slot already held,
// Start() must NOT open a second connection — it reports Suppressed() and
// stays not-Running.
func TestSlackListener_SkipsWhenAnotherProcessHoldsLock(t *testing.T) {
	// Isolate the cache dir so we never touch the operator's real lock.
	// os.UserCacheDir derives from $HOME on darwin and $XDG_CACHE_HOME/$HOME
	// on linux; set both.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("FLOW_SLACK_APP_TOKEN", "xapp-guard")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-guard")
	t.Setenv("FLOW_SLACK_TOKEN", "")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("FLOW_SLACK_SOCKET_MODE", "1")

	if !SocketModeEnabled() {
		t.Fatal("precondition: SocketModeEnabled() should be true with the test tokens")
	}

	// Simulate another flow process already owning the slot.
	held, ok, err := acquireSocketModeLock(socketModeLockPath(SlackAppToken()))
	if err != nil || !ok {
		t.Fatalf("pre-acquire lock: ok=%v err=%v", ok, err)
	}
	defer releaseSocketModeLock(held)

	l := NewSlackListener(NewDispatcher(nil, nil))
	if err := l.Start(); err != nil {
		t.Fatalf("Start err = %v", err)
	}
	defer l.Stop()

	if l.Running() {
		t.Fatal("listener must not open a second Socket Mode connection while another process holds the lock")
	}
	if !l.Suppressed() {
		t.Fatal("listener should report Suppressed() when the slot is already held")
	}
}
