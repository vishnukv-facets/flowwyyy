package steering

import (
	"database/sql"
	"sort"

	"flow/internal/productdb"
)

// defaultCalibrationMinSamples is the minimum number of resolved outcomes in a
// (action × band) cell before its observed agreement rate is trusted as the
// calibrated confidence. Below it, the raw model number is returned unchanged —
// a cold or thin history must never distort the score. Matches the learned-policy
// MinFeedback default so the two feedback-driven loops agree on "enough data".
const defaultCalibrationMinSamples = 3

// ConfidenceCalibrator maps a raw model confidence for an action to the
// empirically observed probability the operator agreed with that action, learned
// from attention_feedback grouped by (action × confidence band). It answers the
// question the raw number only pretended to: in this band, how often did the
// operator actually go along with this action?
//
// Like LearnedAttentionPolicyFromFeedback it is derived on read and never mutates
// operator settings. SCOPE: this only DERIVES and SURFACES the calibrated score;
// wiring it into the autonomy gate is [[steerer-autonomy-and-kb]]'s job, so this
// type is the seam that task plugs into — it is intentionally side-effect-free.
type ConfidenceCalibrator struct {
	minSamples int
	cells      map[cellKey]calibrationCell
}

// cellKey identifies one (action × confidence band) calibration cell.
type cellKey struct {
	action Action
	band   string
}

type calibrationCell struct {
	agreed int
	total  int
}

// CalibrationCell is the read-only audit view of one learned (action × band) cell.
type CalibrationCell struct {
	Action     Action
	Band       string
	Agreed     int
	Total      int
	Calibrated float64 // agreed/total — the empirical agreement rate in this band
	Grounded   bool    // total >= minSamples (else Calibrate falls back to raw)
}

// NewConfidenceCalibrator folds feedback bins into per-(action,band) agreement
// cells. agreed = approved + operator_handled; total adds the negatives
// (dismissed/muted). Operator-handled rows count as agreement: a card the
// operator resolved by replying in the thread themselves confirms the steerer was
// right to surface it (drop verdicts never create a card, so these rows only ever
// carry a surfacing action). minSamples <= 0 uses defaultCalibrationMinSamples.
//
// ponytail: histogram-bin calibration (observed rate per band). Isotonic/Platt
// scaling would smooth sparse bands, but binning is the lower-risk start the brief
// called for; revisit only if bands stay too sparse to be useful.
func NewConfidenceCalibrator(bins []productdb.AttentionCalibrationBin, minSamples int) *ConfidenceCalibrator {
	if minSamples <= 0 {
		minSamples = defaultCalibrationMinSamples
	}
	cells := make(map[cellKey]calibrationCell, len(bins))
	for _, b := range bins {
		agreed := b.Approved + b.OperatorHandled
		total := agreed + b.Negative
		if total == 0 {
			continue
		}
		cells[cellKey{Action(b.Action), b.ConfidenceBand}] = calibrationCell{agreed: agreed, total: total}
	}
	return &ConfidenceCalibrator{minSamples: minSamples, cells: cells}
}

// LoadConfidenceCalibrator reads the feedback bins and builds a calibrator with
// the default min-sample floor.
func LoadConfidenceCalibrator(db *sql.DB) (*ConfidenceCalibrator, error) {
	bins, err := productdb.AttentionCalibrationBins(db)
	if err != nil {
		return nil, err
	}
	return NewConfidenceCalibrator(bins, 0), nil
}

// Calibrate returns the calibrated confidence for an action at a raw confidence —
// the observed agreement rate in that action's matching confidence band — and
// whether it was grounded in >= minSamples resolved outcomes. When ungrounded it
// returns the raw value unchanged, so the calibrated score only ever overrides
// the model when there is real evidence to override it with. A nil calibrator is
// a valid no-op (returns raw, false).
func (c *ConfidenceCalibrator) Calibrate(action Action, raw float64) (float64, bool) {
	if c == nil {
		return raw, false
	}
	cell, ok := c.cells[cellKey{action, productdb.ConfidenceBand(raw)}]
	if !ok || cell.total < c.minSamples {
		return raw, false
	}
	return float64(cell.agreed) / float64(cell.total), true
}

// Cells returns every learned cell, sorted (action, band), for audit/inspection.
func (c *ConfidenceCalibrator) Cells() []CalibrationCell {
	if c == nil {
		return nil
	}
	out := make([]CalibrationCell, 0, len(c.cells))
	for key, cell := range c.cells {
		out = append(out, CalibrationCell{
			Action:     key.action,
			Band:       key.band,
			Agreed:     cell.agreed,
			Total:      cell.total,
			Calibrated: float64(cell.agreed) / float64(cell.total),
			Grounded:   cell.total >= c.minSamples,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action < out[j].Action
		}
		return out[i].Band < out[j].Band
	})
	return out
}
