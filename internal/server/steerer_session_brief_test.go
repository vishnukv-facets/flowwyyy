package server

import (
	"strings"
	"testing"
)

// The prime must name the surface tool, the action vocabulary, the surface-only
// autonomy boundary, and context_only handling — the load-bearing contract.
func TestSteererSessionBriefContract(t *testing.T) {
	b := steererSessionBrief()
	for _, want := range []string{
		"flow attention surface",
		"make_task", "forward", "capture_kb", "digest_only", "drop",
		"context_only",
		"operator acted directly",
		"--context-only --thread-key",
		"never", // surface-only: never auto-send a reply
		"thread_key",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("steerer brief missing %q", want)
		}
	}
}
