package product

import (
	"strings"
	"testing"

	"flow/internal/steering"
)

func TestRenderAttentionCalibration(t *testing.T) {
	cells := []steering.CalibrationCell{
		{Action: steering.ActionMakeTask, Band: "0.80-0.89", Agreed: 3, Total: 4, Calibrated: 0.75, Grounded: true},
		{Action: steering.ActionForward, Band: "0.50-0.59", Agreed: 1, Total: 2, Calibrated: 0.5, Grounded: false},
	}
	out := renderAttentionCalibration(cells)
	if !strings.Contains(out, "make_task") || !strings.Contains(out, "0.80-0.89") || !strings.Contains(out, "75%") {
		t.Errorf("calibration render missing grounded make_task row:\n%s", out)
	}
	if !strings.Contains(out, "raw fallback") {
		t.Errorf("ungrounded row should be flagged as raw fallback:\n%s", out)
	}
	if empty := renderAttentionCalibration(nil); !strings.Contains(empty, "No attention feedback") {
		t.Errorf("empty render = %q", empty)
	}
}
