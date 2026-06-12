package monitor

import (
	"fmt"
	"os"
	"time"
)

// NewStderrLogger returns a printf-style logger that writes timestamped,
// prefixed lines to stderr (the server's ~/.flow/logs/ui-serve.log). The leading
// RFC3339 timestamp (local, with offset) makes log recency unambiguous — so a
// line like "weekly limit" can be told apart as current vs hours-stale at a
// glance. prefix should carry its own trailing space, e.g. "[steering] ".
func NewStderrLogger(prefix string) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprint(os.Stderr, stderrLogLine(time.Now(), prefix, format, args...))
	}
}

// stderrLogLine formats one timestamped log line (with trailing newline). Split
// from NewStderrLogger so the formatting is deterministically testable.
func stderrLogLine(now time.Time, prefix, format string, args ...any) string {
	return now.Format(time.RFC3339) + " " + prefix + fmt.Sprintf(format, args...) + "\n"
}
