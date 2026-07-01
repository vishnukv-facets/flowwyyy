package server

import (
	"testing"
	"time"
)

func TestContextWindowForModel(t *testing.T) {
	if got := contextWindowForModel("claude", "claude-opus-4-7"); got != 1000000 {
		t.Fatalf("opus-4-7 = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude", "claude-opus-4-6"); got != 1000000 {
		t.Fatalf("opus-4-6 = %d, want 1000000", got)
	}
	// The bug this fixes: opus-4-8 (and any future opus-4.6+) must be 1M, not the
	// 200k that the old hardcoded 4-6/4-7 check produced (which clamped the bar to
	// a bogus "381k/381k").
	if got := contextWindowForModel("claude", "claude-opus-4-8"); got != 1000000 {
		t.Fatalf("opus-4-8 = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude", "claude-opus-4-8-20260101"); got != 1000000 {
		t.Fatalf("dated opus-4-8 = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude", "claude-opus-4-8[1m]"); got != 1000000 {
		t.Fatalf("opus-4-8[1m] = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude", "claude-sonnet-5"); got != 1000000 {
		t.Fatalf("sonnet-5 = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude", "claude-sonnet-5-20260701"); got != 1000000 {
		t.Fatalf("dated sonnet-5 = %d, want 1000000", got)
	}
	// Older Opus 4 (pre-4.6) stays 200k; the [1m] tag still bumps it.
	if got := contextWindowForModel("claude", "claude-opus-4-1"); got != 200000 {
		t.Fatalf("opus-4-1 = %d, want 200000", got)
	}
	if got := contextWindowForModel("claude", "claude-sonnet-4-5"); got != 200000 {
		t.Fatalf("sonnet-4-5 = %d, want 200000", got)
	}
	if got := contextWindowForModel("claude", "claude-haiku-4-5"); got != 200000 {
		t.Fatalf("haiku-4-5 = %d, want 200000", got)
	}
	if got := contextWindowForModel("claude", ""); got != 1000000 {
		t.Fatalf("empty model claude = %d, want 1000000 (provider default)", got)
	}
	if got := contextWindowForModel("codex", "gpt-5"); got != 200000 {
		t.Fatalf("codex = %d, want 200000", got)
	}
}

func TestRuntimeStateStaleForRunning(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-10 * time.Second).Format(time.RFC3339)
	stillFresh := now.Add(-60 * time.Second).Format(time.RFC3339)
	stale := now.Add(-2 * time.Minute).Format(time.RFC3339)
	veryStale := now.Add(-10 * time.Minute).Format(time.RFC3339)

	if runtimeStateStaleForRunning(fresh, fresh) {
		t.Fatalf("fresh hook + fresh transcript should not be stale")
	}
	if runtimeStateStaleForRunning(stillFresh, stale) {
		t.Fatalf("hook still under 90s should not be stale even if transcript is")
	}
	if runtimeStateStaleForRunning(stale, fresh) {
		t.Fatalf("stale hook but fresh transcript (active tool call) should not be demoted")
	}
	if !runtimeStateStaleForRunning(stale, stale) {
		t.Fatalf("hook stale + transcript stale should be flagged stale")
	}
	if !runtimeStateStaleForRunning(veryStale, "") {
		t.Fatalf("very stale hook with no transcript info should be flagged stale")
	}
	if !runtimeStateStaleForRunning(veryStale, veryStale) {
		t.Fatalf("very stale both should be flagged stale")
	}
}
