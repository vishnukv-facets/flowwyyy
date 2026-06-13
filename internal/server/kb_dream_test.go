package server

import (
	"strings"
	"testing"
	"time"
)

// TestPruneExpiredPendingRemoval is the safety-critical test: the deterministic
// auto-remove must delete ONLY flagged bullets older than maxAge, leave fresh
// flags and all other content untouched, and never touch live entries.
func TestPruneExpiredPendingRemoval(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	const maxAge = 30 * 24 * time.Hour

	content := strings.Join([]string{
		"# User",
		"",
		"- 2026-05-01 — a live, valid fact (keep)",
		"- 2026-06-13 — another live fact (keep)",
		"",
		"## ⚠️ Pending removal",
		"- [flagged 2026-04-01] very old stale fact — why: superseded", // 74 days → remove
		"- [flagged 2026-06-10] recently flagged — why: maybe stale",   // 4 days → keep
		"- [flagged 2026-05-14] exactly 31 days — why: old",            // 31 days → remove
		"- not a flagged bullet, leave it",
	}, "\n")

	got, removed := pruneExpiredPendingRemoval(content, now, maxAge)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	// Expired ones gone.
	if strings.Contains(got, "very old stale fact") || strings.Contains(got, "exactly 31 days") {
		t.Errorf("expired flagged entries should be removed:\n%s", got)
	}
	// Fresh flag + live facts + non-bullet line preserved.
	for _, keep := range []string{
		"a live, valid fact (keep)",
		"another live fact (keep)",
		"recently flagged — why: maybe stale",
		"## ⚠️ Pending removal",
		"not a flagged bullet, leave it",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("should have kept %q in:\n%s", keep, got)
		}
	}
}

func TestPruneExpiredPendingRemovalNoChange(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	content := "# User\n\n- 2026-06-01 — a fact\n- 2026-06-10 — another fact\n"
	got, removed := pruneExpiredPendingRemoval(content, now, 30*24*time.Hour)
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if got != content {
		t.Errorf("content with no flagged bullets must be returned unchanged")
	}
}

func TestPruneExpiredPendingRemovalBadDate(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	// Unparseable flagged date → preserved (never silently drop on a parse error).
	content := "## ⚠️ Pending removal\n- [flagged not-a-date] x — why: y\n"
	_, removed := pruneExpiredPendingRemoval(content, now, time.Hour)
	if removed != 0 {
		t.Errorf("unparseable flagged date must be preserved, removed=%d", removed)
	}
}

func TestKBDreamEnabledDefault(t *testing.T) {
	t.Setenv("FLOW_KB_DREAM_ENABLED", "")
	if !kbDreamEnabled() {
		t.Errorf("default should be enabled")
	}
	t.Setenv("FLOW_KB_DREAM_ENABLED", "off")
	if kbDreamEnabled() {
		t.Errorf("off should disable")
	}
}
