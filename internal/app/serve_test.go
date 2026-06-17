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

// stopExistingFlowServer must only kill a confirmed flow ui-serve — never an
// unrelated service that happens to hold the port. This guards that decision.
func TestIsFlowUIServeCommand(t *testing.T) {
	cases := []struct {
		cmdline string
		want    bool
	}{
		{"/Users/v/facets/codebases/flowwyyy/flow ui serve --host 127.0.0.1 --port 8787", true},
		{"flow ui serve --bg", true},
		{"node /srv/app/server.js --port 8787", false},
		{"postgres -p 8787", false},
		{"/usr/local/bin/flow do some-task", false}, // a flow process, but not ui-serve
		{"", false},
	}
	for _, tc := range cases {
		if got := isFlowUIServeCommand(tc.cmdline); got != tc.want {
			t.Errorf("isFlowUIServeCommand(%q) = %v, want %v", tc.cmdline, got, tc.want)
		}
	}
}
