package server

import (
	"flow/internal/productdb"
	"testing"
	"time"
)

// atLocalH builds an RFC3339 timestamp at a specific local hour.
func atLocalH(y, m, d, h int) string {
	return time.Date(y, time.Month(m), d, h, 0, 0, 0, time.Local).Format(time.RFC3339)
}

func TestProjectEffortBreakdown(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	usages := []taskTokenUsage{
		{Provider: "claude", ProjectSlug: "alpha", TokensByDay: map[string]int{"2026-06-15": 1000}},
		{Provider: "codex", ProjectSlug: "beta", TokensByDay: map[string]int{"2026-06-16": 500}},
		{Provider: "claude", ProjectSlug: "alpha", TokensByDay: map[string]int{"2026-06-17": 200, "2026-06-01": 999}}, // 6/01 out
		{Provider: "claude", ProjectSlug: "", TokensByDay: map[string]int{"2026-06-15": 50}},                          // no project
	}
	names := map[string]string{"alpha": "Alpha Project"}

	b := projectEffortBreakdown(usages, names, g)
	if b.Key != "project_effort" {
		t.Fatalf("key = %s want project_effort", b.Key)
	}
	seg := map[string]float64{}
	label := map[string]string{}
	for _, s := range b.Segments {
		seg[s.Key] = s.Value
		label[s.Key] = s.Label
	}
	if seg["alpha"] != 1200 {
		t.Errorf("alpha tokens = %v want 1200 (1000+200; 999 out of window)", seg["alpha"])
	}
	if label["alpha"] != "Alpha Project" {
		t.Errorf("alpha label = %q want display name", label["alpha"])
	}
	if seg["beta"] != 500 {
		t.Errorf("beta tokens = %v want 500", seg["beta"])
	}
	// Largest first.
	if b.Segments[0].Key != "alpha" {
		t.Errorf("first segment = %s want alpha (largest)", b.Segments[0].Key)
	}
}

func TestTaskSourceBreakdown(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	tasks := []*productdb.Task{
		mkTask("s1", "backlog", atLocalH(2026, 6, 15, 10), ""),
		mkTask("g1", "done", atLocalH(2026, 6, 16, 10), atLocalH(2026, 6, 16, 12)),
		mkTask("m1", "in-progress", atLocalH(2026, 6, 17, 10), ""),
		mkTask("old", "backlog", atLocalH(2026, 6, 1, 10), ""), // out of window
	}
	tags := map[string][]string{
		"s1":  {"slack-reply"},
		"g1":  {"github", "gh-pr:o/r#1"},
		"m1":  {},
		"old": {"slack-reply"},
	}

	b := taskSourceBreakdown(tasks, tags, g)
	if b.Key != "task_source" {
		t.Fatalf("key = %s want task_source", b.Key)
	}
	seg := map[string]float64{}
	for _, s := range b.Segments {
		seg[s.Key] = s.Value
	}
	if seg["slack"] != 1 || seg["github"] != 1 || seg["manual"] != 1 {
		t.Errorf("source counts = %+v want slack/github/manual each 1 (old is out of window)", seg)
	}
}

func TestSourceConversions(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	traces := []productdb.SteeringTraceLite{
		{CreatedAt: atLocalH(2026, 6, 15, 10), Source: "slack", Disposition: "surfaced", FinalAction: "make_task"},
		{CreatedAt: atLocalH(2026, 6, 15, 11), Source: "slack", Disposition: "dropped"},
		{CreatedAt: atLocalH(2026, 6, 16, 10), Source: "github", Disposition: "surfaced", FinalAction: "reply"},
		{CreatedAt: atLocalH(2026, 6, 1, 10), Source: "slack", Disposition: "surfaced", FinalAction: "make_task"}, // out
	}

	conv := sourceConversions(traces, g)
	bySrc := map[string]SourceConversion{}
	for _, c := range conv {
		bySrc[c.Source] = c
	}
	if bySrc["slack"].Observed != 2 || bySrc["slack"].Surfaced != 1 || bySrc["slack"].Tasks != 1 {
		t.Errorf("slack conversion = %+v want observed2/surfaced1/tasks1", bySrc["slack"])
	}
	if bySrc["github"].Observed != 1 || bySrc["github"].Surfaced != 1 || bySrc["github"].Tasks != 0 {
		t.Errorf("github conversion = %+v want observed1/surfaced1/tasks0", bySrc["github"])
	}
	// Most-active source first.
	if len(conv) == 0 || conv[0].Source != "slack" {
		t.Errorf("expected slack first (most observed), got %+v", conv)
	}
}
