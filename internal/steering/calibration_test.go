package steering

import (
	"testing"

	"flow/internal/flowdb"
)

func TestConfidenceCalibratorGroundedAndFallback(t *testing.T) {
	bins := []flowdb.AttentionCalibrationBin{
		// make_task 0.80-0.89: agreed = 2 approved + 1 operator_handled = 3; total 4
		// (>= minSamples). Calibrated = 3/4 = 0.75.
		{Action: "make_task", ConfidenceBand: "0.80-0.89", Approved: 2, Negative: 1, OperatorHandled: 1},
		// forward 0.50-0.59: total 2 < minSamples → ungrounded, raw returned.
		{Action: "forward", ConfidenceBand: "0.50-0.59", Approved: 1, Negative: 1},
		// reply 0.20-0.29: all zero → no signal, skipped entirely.
		{Action: "reply", ConfidenceBand: "0.20-0.29"},
	}
	cal := NewConfidenceCalibrator(bins, 0)

	if got, grounded := cal.Calibrate(ActionMakeTask, 0.85); !grounded || got != 0.75 {
		t.Errorf("Calibrate(make_task, 0.85) = %v, %v; want 0.75, true", got, grounded)
	}
	if got, grounded := cal.Calibrate(ActionForward, 0.55); grounded || got != 0.55 {
		t.Errorf("Calibrate(forward, 0.55) = %v, %v; want 0.55, false (below min samples)", got, grounded)
	}
	if got, grounded := cal.Calibrate(ActionReply, 0.25); grounded || got != 0.25 {
		t.Errorf("Calibrate(reply, 0.25) = %v, %v; want 0.25, false (no cell)", got, grounded)
	}

	var nilCal *ConfidenceCalibrator
	if got, grounded := nilCal.Calibrate(ActionMakeTask, 0.42); grounded || got != 0.42 {
		t.Errorf("nil Calibrate = %v, %v; want 0.42, false", got, grounded)
	}
}

func TestConfidenceCalibratorCells(t *testing.T) {
	bins := []flowdb.AttentionCalibrationBin{
		{Action: "forward", ConfidenceBand: "0.50-0.59", Approved: 1, Negative: 1},
		{Action: "make_task", ConfidenceBand: "0.80-0.89", Approved: 3, Negative: 0},
	}
	cells := NewConfidenceCalibrator(bins, 3).Cells()
	if len(cells) != 2 {
		t.Fatalf("len(cells) = %d, want 2", len(cells))
	}
	// sorted by action: forward < make_task.
	if cells[0].Action != ActionForward || cells[1].Action != ActionMakeTask {
		t.Fatalf("cells not sorted by action: %+v", cells)
	}
	if cells[0].Grounded || !cells[1].Grounded {
		t.Errorf("grounded flags wrong: forward should be ungrounded, make_task grounded: %+v", cells)
	}
	if cells[1].Calibrated != 1.0 {
		t.Errorf("make_task calibrated = %v, want 1.0", cells[1].Calibrated)
	}
}
