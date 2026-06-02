//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package monitor

import "os"

// acquireSocketModeLock is a no-op on platforms without flock(2). flow's
// Socket Mode listener runs on unix in practice; non-unix builds fall
// through to the prior single-process behavior (no cross-process guard).
func acquireSocketModeLock(string) (*os.File, bool, error) { return nil, true, nil }

// releaseSocketModeLock is the no-op counterpart to acquireSocketModeLock.
func releaseSocketModeLock(*os.File) {}
