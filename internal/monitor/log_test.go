package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestStderrLogLineHasTimestampPrefixAndMessage(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 21, 0, 0, time.UTC)
	got := stderrLogLine(now, "[steering] ", "deep triage failed for %s", "C1:1.1")
	want := "2026-06-12T12:21:00Z [steering] deep triage failed for C1:1.1\n"
	if got != want {
		t.Fatalf("stderrLogLine = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "2026-06-12T12:21:00Z ") {
		t.Errorf("line must start with an RFC3339 timestamp: %q", got)
	}
}
