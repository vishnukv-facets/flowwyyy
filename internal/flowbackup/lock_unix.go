//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package flowbackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockName is a top-level dotfile inside the flow root. The whitelist
// .gitignore's `/*` rule ignores it, so it is never committed; it lives outside
// .git so the same lock guards repo *creation* too (no chicken-and-egg).
const lockName = ".flow-backup.lock"

// acquireLock takes an exclusive advisory lock (flock) on <root>/.flow-backup.lock,
// serializing checkpoints across processes (e.g. the server dreamer vs a
// `flow done` in another tab). It retries briefly when another process holds the
// lock rather than failing immediately. Returns an unlock func the caller defers.
func acquireLock(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("flowbackup: mkdir root for lock: %w", err)
	}
	path := filepath.Join(root, lockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("flowbackup: open lock: %w", err)
	}
	// Bounded retry (~5s) on contention; flock is released automatically if a
	// holder dies, so we never wedge permanently.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("flowbackup: flock: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("flowbackup: backup lock busy after retry")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
