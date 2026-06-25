package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func TestLookupRollupForTaskClassifiesContextReads(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "tasks", "mine", "brief.md"), 8)
	writeSizedFile(t, filepath.Join(root, "tasks", "mine", "updates", "u.md"), 4)
	writeSizedFile(t, filepath.Join(root, "tasks", "other", "brief.md"), 12)
	writeSizedFile(t, filepath.Join(root, "kb", "org.md"), 16)

	var stats transcriptUsageStats
	lines := []string{
		`{"type":"assistant","timestamp":"2026-06-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"flow show task mine"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"flow show project flowwyyy"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"flow transcript other"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:04Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.ToSlash(filepath.Join(root, "kb", "org.md")) + `"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.ToSlash(filepath.Join(root, "tasks", "mine", "brief.md")) + `"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:06Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.ToSlash(filepath.Join(root, "tasks", "other", "brief.md")) + `"}}]}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:07Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"git status --short"}}]}}`,
	}
	for _, line := range lines {
		accumulateTranscriptUsage(&stats, []byte(line))
	}
	if got := len(stats.LookupEvents); got != 6 {
		t.Fatalf("lookup events = %d, want 6", got)
	}

	roll := lookupRollupForTask(stats.LookupEvents, "mine", root)
	day := roll.LookupsByDay["2026-06-01"]
	if day[lookupResume] != 1 || day[lookupReference] != 1 || day[lookupCrossTask] != 2 || day[lookupKB] != 1 {
		t.Fatalf("lookup counts = %+v, want resume1 reference1 cross_task2 kb1", day)
	}
	// Context bytes: resume own brief+updates (12) + reference own brief (8)
	// + transcript cross-task proxy own brief (8) + actual sibling read (12)
	// + actual KB read (16) = 56 bytes ~= 14 tokens.
	if got := roll.ContextTokensByDay["2026-06-01"]; got != 14 {
		t.Fatalf("context tokens = %d, want 14", got)
	}
}

func TestBuildAnalyticsPayloadValueStats(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	q, _ := parseAnalyticsRange(nil, now)
	in := analyticsInputs{
		usages: []taskTokenUsage{{
			LookupsByDay: map[string]map[string]int{
				"2026-06-14": {lookupResume: 2, lookupReference: 1, lookupCrossTask: 1, lookupKB: 1},
				"2026-06-01": {lookupResume: 9},
			},
			ContextTokensByDay: map[string]int64{"2026-06-14": 1200, "2026-06-01": 9999},
		}},
		runs: []*flowdb.BrainRun{
			mkRun("r1", "completed", "2026-06-14T10:00:00Z", "2026-06-14T10:05:00Z"),
			mkRun("r2", "dead", "2026-06-15T10:00:00Z", "2026-06-15T10:05:00Z"),
			mkRun("old", "completed", "2026-06-01T10:00:00Z", "2026-06-01T10:05:00Z"),
		},
		valueConstants: defaultValueConstants(),
	}

	p := buildAnalyticsPayload(in, q, now)
	if p.Value == nil {
		t.Fatal("expected value stats")
	}
	if p.Value.LookupsTotal != 5 {
		t.Fatalf("lookups total = %v, want 5", p.Value.LookupsTotal)
	}
	if p.Value.ContextTokens != 1200 {
		t.Fatalf("context tokens = %v, want 1200", p.Value.ContextTokens)
	}
	if p.Value.AutomationRuns != 2 {
		t.Fatalf("automation runs = %v, want 2", p.Value.AutomationRuns)
	}
	if p.Value.ContextSwitchHours != 0.25 {
		t.Fatalf("context switch hours = %v, want 0.25", p.Value.ContextSwitchHours)
	}
	if p.Value.AutomationHours < 0.666 || p.Value.AutomationHours > 0.667 {
		t.Fatalf("automation hours = %v, want ~0.667", p.Value.AutomationHours)
	}
	if p.Value.TotalDollars < 91.66 || p.Value.TotalDollars > 91.67 {
		t.Fatalf("total dollars = %v, want ~91.67", p.Value.TotalDollars)
	}
	if len(p.Value.Series) != 1 || p.Value.Series[0].Key != "value_lookups" {
		t.Fatalf("value series = %+v, want value_lookups", p.Value.Series)
	}
	if len(p.Value.Breakdowns) != 1 || p.Value.Breakdowns[0].Key != "lookup_kind" {
		t.Fatalf("value breakdowns = %+v, want lookup_kind", p.Value.Breakdowns)
	}
}

func TestLoadValueConstantsDefaultsAndOverride(t *testing.T) {
	dir := t.TempDir()
	if got := loadValueConstants(filepath.Join(dir, "missing.json")); got != defaultValueConstants() {
		t.Fatalf("missing constants = %+v, want defaults", got)
	}
	path := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(path, []byte(`{"minutes_per_unattended_run":30,"dollar_per_hour":150}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadValueConstants(path)
	if got.MinutesPerUnattendedRun != 30 || got.MinutesPerContextSwitch != 5 || got.DollarPerHour != 150 {
		t.Fatalf("override constants = %+v, want run=30 switch=5 dollars=150", got)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
