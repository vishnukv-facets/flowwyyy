package server

import (
	"strings"
	"testing"
	"time"
)

// TestKBShouldWake covers the gate that decides whether an idle, changed,
// off-cooldown live session should be woken for a KB checkpoint.
func TestKBShouldWake(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	const (
		idle     = 8 * time.Minute
		cooldown = 30 * time.Minute
		minDelta = int64(600)
	)
	zero := time.Time{}

	cases := []struct {
		name       string
		mtime      time.Time // last transcript activity
		capturedAt time.Time
		cursor     int64
		maxOffset  int64
		want       bool
	}{
		{"never swept, idle, big delta", now.Add(-10 * time.Minute), zero, 0, 5000, true},
		{"still active (fresh mtime)", now.Add(-1 * time.Minute), zero, 0, 5000, false},
		{"idle but within cooldown", now.Add(-10 * time.Minute), now.Add(-5 * time.Minute), 0, 5000, false},
		{"idle, past cooldown, big delta", now.Add(-10 * time.Minute), now.Add(-40 * time.Minute), 1000, 5000, true},
		{"idle, past cooldown, delta below min", now.Add(-10 * time.Minute), now.Add(-40 * time.Minute), 4800, 5000, false},
		{"no transcript yet", now.Add(-10 * time.Minute), zero, 0, 0, false},
		{"idle exactly at threshold", now.Add(-idle), zero, 0, 5000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kbShouldWake(now, tc.mtime, tc.capturedAt, tc.cursor, tc.maxOffset, minDelta, idle, cooldown)
			if got != tc.want {
				t.Errorf("kbShouldWake = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMaxTranscriptByteOffset(t *testing.T) {
	entries := []TranscriptEntry{
		{Type: "user", ByteOffset: 100},
		{Type: "assistant", ByteOffset: 980},
		{Type: "tool_use", ByteOffset: 540},
	}
	if got := maxTranscriptByteOffset(entries); got != 980 {
		t.Errorf("maxTranscriptByteOffset = %d, want 980", got)
	}
	if got := maxTranscriptByteOffset(nil); got != 0 {
		t.Errorf("maxTranscriptByteOffset(nil) = %d, want 0", got)
	}
}

func TestKBDistillEnabledDefault(t *testing.T) {
	t.Setenv("FLOW_KB_DISTILL_ENABLED", "")
	if !kbDistillEnabled() {
		t.Errorf("default should be enabled")
	}
	t.Setenv("FLOW_KB_DISTILL_ENABLED", "0")
	if kbDistillEnabled() {
		t.Errorf("=0 should disable")
	}
}

func TestKBCheckpointPromptReusesSkillRules(t *testing.T) {
	got := kbCheckpointPrompt("/custom/flowroot")
	for _, want := range []string{"KB checkpoint", "§4.10", "/custom/flowroot/kb/*.md", "silently", "DURABLE"} {
		if !strings.Contains(got, want) {
			t.Errorf("kbCheckpointPrompt missing %q", want)
		}
	}
	// Empty root falls back to ~/.flow (never an empty path in the instruction).
	if !strings.Contains(kbCheckpointPrompt(""), "~/.flow/kb/*.md") {
		t.Errorf("empty root should fall back to ~/.flow/kb/*.md")
	}
}
