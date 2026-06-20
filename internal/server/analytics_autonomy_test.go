package server

import (
	"database/sql"
	"testing"
	"time"

	"flow/internal/flowdb"
)

// mkRun builds a brain-run fixture. finished == "" means in-flight.
func mkRun(id, status, started, finished string) *flowdb.BrainRun {
	r := &flowdb.BrainRun{
		RunID:    id,
		Status:   status,
		Provider: "claude",
		Role:     "worker",
	}
	if started != "" {
		r.StartedAt = sql.NullString{String: started, Valid: true}
		r.CreatedAt = started
	}
	if finished != "" {
		r.FinishedAt = sql.NullString{String: finished, Valid: true}
	}
	return r
}

func TestAutonomySeriesVolume(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d") // daily 6/13..6/20
	g := bucketsFor(from, to, unit, now)

	runs := []*flowdb.BrainRun{
		mkRun("a", "completed", "2026-06-15T10:00:00Z", "2026-06-15T10:05:00Z"),
		mkRun("b", "completed", "2026-06-15T14:00:00Z", "2026-06-15T14:20:00Z"),
		mkRun("c", "dead", "2026-06-17T09:00:00Z", "2026-06-17T09:40:00Z"),
		mkRun("d", "running", "2026-06-18T09:00:00Z", ""), // in-flight: started, not completed
	}

	s := autonomySeries(runs, g)
	if s.Key != "runs" {
		t.Fatalf("series key=%s want runs", s.Key)
	}
	started := lineByKey(s, "started")
	completed := lineByKey(s, "completed")
	if started.Key == "" || completed.Key == "" {
		t.Fatalf("missing started/completed lines: %+v", s.Lines)
	}

	if got := pointForDay(started, 2026, 6, 15); got != 2 {
		t.Errorf("started 6/15 = %v want 2", got)
	}
	if got := pointForDay(started, 2026, 6, 18); got != 1 {
		t.Errorf("started 6/18 (in-flight) = %v want 1", got)
	}
	// The in-flight run never lands in the completed line.
	if got := pointForDay(completed, 2026, 6, 18); got != 0 {
		t.Errorf("completed 6/18 = %v want 0 (run still in flight)", got)
	}
	if got := pointForDay(completed, 2026, 6, 15); got != 2 {
		t.Errorf("completed 6/15 = %v want 2", got)
	}
}

func TestComputeAutonomyStats(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	runs := []*flowdb.BrainRun{
		mkRun("a", "completed", "2026-06-15T10:00:00Z", "2026-06-15T10:10:00Z"), // 10 min
		mkRun("b", "completed", "2026-06-15T14:00:00Z", "2026-06-15T14:30:00Z"), // 30 min
		mkRun("c", "dead", "2026-06-17T09:00:00Z", "2026-06-17T09:50:00Z"),      // 50 min, failed
		mkRun("d", "running", "2026-06-18T09:00:00Z", ""),                       // in-flight, excluded
	}

	st := computeAutonomy(runs, g)
	if st.started != 4 {
		t.Errorf("started=%v want 4 (all four began in the window)", st.started)
	}
	if st.finished != 3 {
		t.Errorf("finished=%v want 3 (in-flight excluded)", st.finished)
	}
	if !st.hasFinished {
		t.Fatalf("hasFinished should be true")
	}
	// 2 of 3 finished runs succeeded.
	want := 2.0 / 3.0 * 100
	if st.successPct < want-0.001 || st.successPct > want+0.001 {
		t.Errorf("successPct=%v want ~%.2f", st.successPct, want)
	}
	// Durations 10/30/50 → median 30.
	if !st.p50ok || st.p50Minutes != 30 {
		t.Errorf("p50Minutes=%v (ok=%v) want 30", st.p50Minutes, st.p50ok)
	}
}

func TestComputeAutonomyNoFinishedRuns(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	runs := []*flowdb.BrainRun{
		mkRun("a", "running", "2026-06-18T09:00:00Z", ""),
	}
	st := computeAutonomy(runs, g)
	if st.started != 1 {
		t.Errorf("started=%v want 1", st.started)
	}
	if st.hasFinished || st.p50ok {
		t.Errorf("no finished runs: hasFinished=%v p50ok=%v want false/false", st.hasFinished, st.p50ok)
	}
}
