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

// TestStripAllPendingRemoval verifies the operator "clean up flagged" purge:
// the whole Pending-removal section goes (regardless of flag age), live content
// above and any heading after it stays, and a file with no such section is
// returned byte-identical.
func TestStripAllPendingRemoval(t *testing.T) {
	content := strings.Join([]string{
		"# User",
		"",
		"- a live fact (keep)",
		"",
		"## ⚠️ Pending removal",
		"- [flagged 2026-06-10] stale one — why: superseded",
		"- [flagged 2026-06-13] another — why: old",
		"- a non-flagged line in the section",
		"",
		"## Another section",
		"- keep this too",
	}, "\n")

	got, removed := stripAllPendingRemoval(content)
	if removed != 2 {
		t.Fatalf("removed (flagged count) = %d, want 2", removed)
	}
	for _, gone := range []string{"⚠️ Pending removal", "stale one", "another — why: old", "a non-flagged line in the section"} {
		if strings.Contains(got, gone) {
			t.Errorf("purge should have removed %q:\n%s", gone, got)
		}
	}
	for _, keep := range []string{"# User", "a live fact (keep)", "## Another section", "keep this too"} {
		if !strings.Contains(got, keep) {
			t.Errorf("purge should have kept %q:\n%s", keep, got)
		}
	}
}

func TestStripAllPendingRemovalNoSection(t *testing.T) {
	content := "# User\n\n- a fact\n- another fact\n"
	got, removed := stripAllPendingRemoval(content)
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if got != content {
		t.Errorf("content with no Pending-removal section must round-trip unchanged")
	}
}

// TestComputeNextRunInterval is the restart-no-reset guarantee for interval mode:
// an overdue last-run catches up promptly, a recent one waits out the remainder,
// and a fresh worker schedules one interval ahead — never a hard reset to now+24h
// on every boot.
func TestComputeNextRunInterval(t *testing.T) {
	t.Setenv("FLOW_KB_DREAM_SCHEDULE", "")
	t.Setenv("FLOW_KB_DREAM_INTERVAL", "24h")
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.Local)
	d := &kbDreamer{}

	// Fresh (no last run) → one interval out.
	if got := d.computeNextRun(now); !got.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("fresh next = %v, want now+24h", got)
	}

	// Recent last run (1h ago) → resumes at last+24h, NOT reset to now+24h.
	d.lastRun = now.Add(-1 * time.Hour)
	if got := d.computeNextRun(now); !got.Equal(d.lastRun.Add(24 * time.Hour)) {
		t.Errorf("recent next = %v, want lastRun+24h (%v)", got, d.lastRun.Add(24*time.Hour))
	}

	// Overdue last run (25h ago, e.g. across a restart) → catch up soon.
	d.lastRun = now.Add(-25 * time.Hour)
	if got := d.computeNextRun(now); !got.Equal(now.Add(kbDreamCatchupDelay)) {
		t.Errorf("overdue next = %v, want now+catchup (%v)", got, now.Add(kbDreamCatchupDelay))
	}
}

// TestComputeNextRunFixedSchedule verifies the playbook-style fixed schedule:
// a daily clock time resumes at that time (no reset), and a missed day catches up.
func TestComputeNextRunFixedSchedule(t *testing.T) {
	t.Setenv("FLOW_KB_DREAM_SCHEDULE", "daily at 3am")
	d := &kbDreamer{}

	// Before today's 03:00, last run was yesterday's pass → next is today 03:00.
	now := time.Date(2026, 6, 18, 2, 0, 0, 0, time.Local)
	d.lastRun = time.Date(2026, 6, 17, 3, 0, 0, 0, time.Local)
	want := time.Date(2026, 6, 18, 3, 0, 0, 0, time.Local)
	if got := d.computeNextRun(now); !got.Equal(want) {
		t.Errorf("pre-slot next = %v, want today 03:00 (%v)", got, want)
	}

	// Server was down past 03:00 and hasn't run today → catch up soon.
	now = time.Date(2026, 6, 18, 9, 0, 0, 0, time.Local)
	if got := d.computeNextRun(now); !got.Equal(now.Add(kbDreamCatchupDelay)) {
		t.Errorf("missed-slot next = %v, want now+catchup", got)
	}

	// Already ran today after 03:00 → next is tomorrow 03:00.
	d.lastRun = time.Date(2026, 6, 18, 3, 0, 30, 0, time.Local)
	want = time.Date(2026, 6, 19, 3, 0, 0, 0, time.Local)
	if got := d.computeNextRun(now); !got.Equal(want) {
		t.Errorf("post-run next = %v, want tomorrow 03:00 (%v)", got, want)
	}
}

func TestKBDreamScheduleCronFallsBackToInterval(t *testing.T) {
	t.Setenv("FLOW_KB_DREAM_INTERVAL", "6h")
	// Unset → @every fallback, no custom label.
	t.Setenv("FLOW_KB_DREAM_SCHEDULE", "")
	if cron, label, custom := kbDreamScheduleCron(); custom || label != "" || cron != "@every 6h0m0s" {
		t.Errorf("unset: got cron=%q label=%q custom=%v, want @every fallback", cron, label, custom)
	}
	// Set + valid → custom cron + label.
	t.Setenv("FLOW_KB_DREAM_SCHEDULE", "daily at 3am")
	if cron, label, custom := kbDreamScheduleCron(); !custom || label == "" || cron == "" {
		t.Errorf("set: got cron=%q label=%q custom=%v, want custom schedule", cron, label, custom)
	}
	// Set + invalid → falls back to interval (never stalls).
	t.Setenv("FLOW_KB_DREAM_SCHEDULE", "not a real schedule!!")
	if _, _, custom := kbDreamScheduleCron(); custom {
		t.Errorf("invalid schedule should fall back to interval, got custom=true")
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
