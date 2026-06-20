//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package flowbackup

import (
	"fmt"
	"os"
)

// acquireLock is a no-op on platforms without flock(2). flow's terminal
// backends and connectors target macOS/Linux; on other platforms backups still
// work but are not serialized across processes.
func acquireLock(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("flowbackup: mkdir root for lock: %w", err)
	}
	return func() {}, nil
}
