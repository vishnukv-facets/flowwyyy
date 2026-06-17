package server

import (
	"testing"
	"time"
)

func TestShouldCompact(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	const (
		idle     = 8 * time.Minute
		cooldown = 30 * time.Minute
	)
	zero := time.Time{}

	cases := []struct {
		name        string
		mtime       time.Time
		compactedAt time.Time
		tokensUsed  int
		tokensMax   int
		want        bool
	}{
		{"threshold and idle fires", now.Add(-10 * time.Minute), zero, 600000, 1000000, true},
		{"below threshold skips", now.Add(-10 * time.Minute), zero, 599999, 1000000, false},
		{"mid-turn skips", now.Add(-1 * time.Minute), zero, 800000, 1000000, false},
		{"cooldown holds", now.Add(-10 * time.Minute), now.Add(-10 * time.Minute), 800000, 1000000, false},
		{"after cooldown fires", now.Add(-10 * time.Minute), now.Add(-31 * time.Minute), 800000, 1000000, true},
		{"missing max skips", now.Add(-10 * time.Minute), zero, 800000, 0, false},
		{"idle exactly at threshold fires", now.Add(-idle), zero, 600000, 1000000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldCompact(now, tc.mtime, tc.compactedAt, tc.tokensUsed, tc.tokensMax, 60, idle, cooldown)
			if got != tc.want {
				t.Errorf("shouldCompact = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSteererCompactUsage(t *testing.T) {
	t.Run("uses transcript context window when present", func(t *testing.T) {
		used, max := steererCompactUsage("codex", transcriptUsageStats{TokensUsed: 120000, TokensMax: 200000})
		if used != 120000 || max != 200000 {
			t.Errorf("usage = (%d,%d), want (120000,200000)", used, max)
		}
	})
	t.Run("falls back to token panel model window", func(t *testing.T) {
		used, max := steererCompactUsage("claude", transcriptUsageStats{TokensUsed: 600000, Model: "claude-opus-4-8"})
		if used != 600000 || max != 1000000 {
			t.Errorf("usage = (%d,%d), want (600000,1000000)", used, max)
		}
	})
	t.Run("clamps max to used like token panel", func(t *testing.T) {
		used, max := steererCompactUsage("codex", transcriptUsageStats{TokensUsed: 250000, TokensMax: 200000})
		if used != 250000 || max != 250000 {
			t.Errorf("usage = (%d,%d), want (250000,250000)", used, max)
		}
	})
}
