package server

import (
	"testing"
	"time"

	"flow/internal/flowdb"
)

func steeringFixture() []flowdb.SteeringTraceLite {
	return []flowdb.SteeringTraceLite{
		{CreatedAt: "2026-06-15T10:00:00Z", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 100},
		{CreatedAt: "2026-06-15T11:00:00Z", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 300},
		{CreatedAt: "2026-06-15T12:00:00Z", Disposition: "dropped", StageReached: "stage1", LatencyMS: 50},
		{CreatedAt: "2026-06-17T09:00:00Z", Disposition: "error", StageReached: "stage2", LatencyMS: 500},
		{CreatedAt: "2026-06-01T09:00:00Z", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 999}, // out of window
	}
}

func TestSteeringFunnelWindowTotals(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	f := steeringFunnel(steeringFixture(), g)
	if f.Observed != 4 {
		t.Errorf("Observed=%d want 4 (6/01 out of window)", f.Observed)
	}
	if f.Surfaced != 2 {
		t.Errorf("Surfaced=%d want 2", f.Surfaced)
	}
	if f.Errors != 1 {
		t.Errorf("Errors=%d want 1", f.Errors)
	}
	// One dropped at stage1.
	var droppedTotal float64
	for _, seg := range f.Dropped {
		droppedTotal += seg.Value
	}
	if droppedTotal != 1 {
		t.Errorf("dropped total=%v want 1", droppedTotal)
	}
}

func TestSteeringSeriesVolumeAndLatency(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)

	vol, latency := steeringSeries(steeringFixture(), g)
	if vol.Key != "steering" || latency.Key != "steering_latency" {
		t.Fatalf("series keys = %s/%s want steering/steering_latency", vol.Key, latency.Key)
	}

	observed := lineByKey(vol, "observed")
	surfaced := lineByKey(vol, "surfaced")
	if got := pointForDay(observed, 2026, 6, 15); got != 3 {
		t.Errorf("observed 6/15 = %v want 3", got)
	}
	if got := pointForDay(surfaced, 2026, 6, 15); got != 2 {
		t.Errorf("surfaced 6/15 = %v want 2", got)
	}

	// p50 latency on 6/15 over {100,300,50} → 100.
	p50 := lineByKey(latency, "p50")
	if got := pointForDay(p50, 2026, 6, 15); got != 100 {
		t.Errorf("p50 latency 6/15 = %v want 100", got)
	}
	// 6/16 has no rows → 0.
	if got := pointForDay(p50, 2026, 6, 16); got != 0 {
		t.Errorf("p50 latency 6/16 = %v want 0 (no rows)", got)
	}
}
