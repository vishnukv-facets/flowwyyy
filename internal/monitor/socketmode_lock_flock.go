//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package monitor

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// acquireSocketModeLock takes an exclusive, non-blocking advisory lock on
// path via flock(2). On success it returns the open file (true) — the
// caller MUST keep it open to hold the lock; closing it or exiting the
// process releases it (flock locks are tied to the open file and the OS
// reclaims them on death, so a crashed flow never wedges the slot).
//
// When another process already holds the lock it returns (nil, false, nil)
// — an expected, non-error outcome. A non-nil error signals an unexpected
// filesystem/syscall failure; callers should fail OPEN on it rather than
// disabling Slack entirely.
func acquireSocketModeLock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("monitor: open socket-mode lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil // held by another process — expected
		}
		return nil, false, fmt.Errorf("monitor: flock socket-mode lock: %w", err)
	}
	return f, true, nil
}

// releaseSocketModeLock unlocks and closes a lock file previously returned
// by acquireSocketModeLock. Safe to call with a nil file.
func releaseSocketModeLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
