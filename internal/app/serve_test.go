package app

import (
	"testing"
)

// The backgrounded `ui serve` must re-exec the binary that's currently
// running, not a bare "flow" PATH lookup — otherwise `./flow ui serve --bg`
// launches a stale installed build with old embedded UI assets.
func TestPreferredUIFlowBinaryUsesCurrentExecutable(t *testing.T) {
	if got := preferredUIFlowBinary("/tmp/worktree/bin/flow"); got != "/tmp/worktree/bin/flow" {
		t.Fatalf("preferredUIFlowBinary() = %q, want /tmp/worktree/bin/flow", got)
	}
}

func TestPreferredUIFlowBinaryFallsBackWhenEmpty(t *testing.T) {
	if got := preferredUIFlowBinary("  "); got != "flow" {
		t.Fatalf("preferredUIFlowBinary() = %q, want flow", got)
	}
}
