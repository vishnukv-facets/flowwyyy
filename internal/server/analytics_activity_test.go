package server

import (
	"database/sql"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func atDay(y, m, d int) string {
	return time.Date(y, time.Month(m), d, 10, 0, 0, 0, time.Local).Format(time.RFC3339)
}

func mkTask(slug, status, created, changed string) *flowdb.Task {
	t := &flowdb.Task{Slug: slug, Status: status, CreatedAt: created}
	if changed != "" {
		t.StatusChangedAt = sql.NullString{String: changed, Valid: true}
	}
	return t
}

func lineByKey(s Series, key string) Line {
	for _, l := range s.Lines {
		if l.Key == key {
			return l
		}
	}
	return Line{}
}

func pointForDay(l Line, y, m, d int) float64 {
	label := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local).Format("2006-01-02")
	for _, p := range l.Points {
		if p.T == label {
			return p.V
		}
	}
	return -1 // not found
}

func TestActivitySeriesBucketsCreatedAndDone(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now) // daily buckets 6/13..6/20

	tasks := []*flowdb.Task{
		mkTask("a", "done", atDay(2026, 6, 14), atDay(2026, 6, 16)),       // created 6/14, done 6/16
		mkTask("b", "in-progress", atDay(2026, 6, 14), ""),               // created 6/14
		mkTask("c", "done", atDay(2026, 6, 1), atDay(2026, 6, 20)),        // created before window, done 6/20 (live bucket)
		mkTask("d", "backlog", atDay(2026, 6, 10), ""),                   // created before window — not counted
	}

	s := activitySeries(tasks, g)
	if s.Key != "throughput" {
		t.Fatalf("series key=%s want throughput", s.Key)
	}
	created := lineByKey(s, "created")
	done := lineByKey(s, "done")

	if got := pointForDay(created, 2026, 6, 14); got != 2 {
		t.Errorf("created 6/14 = %v want 2 (a,b)", got)
	}
	if got := pointForDay(created, 2026, 6, 20); got != 0 {
		t.Errorf("created 6/20 = %v want 0", got)
	}
	if got := pointForDay(done, 2026, 6, 16); got != 1 {
		t.Errorf("done 6/16 = %v want 1 (a)", got)
	}
	if got := pointForDay(done, 2026, 6, 20); got != 1 {
		t.Errorf("done 6/20 = %v want 1 (c, in live bucket)", got)
	}
	if got := pointForDay(done, 2026, 6, 15); got != 0 {
		t.Errorf("done 6/15 = %v want 0", got)
	}
}

func TestCycleTimeMedianDays(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "30d")
	g := bucketsFor(from, to, unit, now)

	tasks := []*flowdb.Task{
		mkTask("x1", "done", atDay(2026, 6, 10), atDay(2026, 6, 12)), // 2d
		mkTask("x2", "done", atDay(2026, 6, 10), atDay(2026, 6, 14)), // 4d
		mkTask("x3", "done", atDay(2026, 6, 10), atDay(2026, 6, 16)), // 6d
		mkTask("ip", "in-progress", atDay(2026, 6, 10), ""),         // ignored
	}
	med, ok := cycleTimeMedianDays(tasks, g)
	if !ok {
		t.Fatal("expected ok=true with done tasks in window")
	}
	if med != 4 {
		t.Errorf("median=%v want 4 (median of 2,4,6)", med)
	}

	if _, ok := cycleTimeMedianDays(nil, g); ok {
		t.Errorf("empty input should give ok=false")
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{2, 4, 6}); m != 4 {
		t.Errorf("median odd = %v want 4", m)
	}
	if m := median([]float64{2, 4, 6, 8}); m != 5 {
		t.Errorf("median even = %v want 5", m)
	}
	if m := median([]float64{7}); m != 7 {
		t.Errorf("median single = %v want 7", m)
	}
}
