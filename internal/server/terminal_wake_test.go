package server

import (
	"testing"
	"time"
)

// sessionBooted gates the wake paste on the resumed/woken session having gone
// quiet — the fix for the laptop-sleep→wake race where a paste landed mid-boot
// and vanished while delivery still reported "delivered".
func TestSessionBooted(t *testing.T) {
	now := time.Date(2026, 6, 18, 10, 4, 0, 0, time.UTC)
	stable := 1500 * time.Millisecond
	cases := []struct {
		name       string
		sawOutput  bool
		lastOutput time.Time
		want       bool
	}{
		{"no output yet (booting) → not ready", false, time.Time{}, false},
		{"output seen but zero ts → not ready", true, time.Time{}, false},
		{"output still flowing (within stable) → not ready", true, now.Add(-500 * time.Millisecond), false},
		{"output quiesced past stable → ready", true, now.Add(-2 * time.Second), true},
		{"long-idle session (old output) → ready immediately", true, now.Add(-30 * time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionBooted(tc.sawOutput, tc.lastOutput, now, stable); got != tc.want {
				t.Errorf("sessionBooted(%v, %v) = %v, want %v", tc.sawOutput, tc.lastOutput, got, tc.want)
			}
		})
	}
}
