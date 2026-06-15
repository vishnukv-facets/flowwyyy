package flowdb

import (
	"database/sql"
	"fmt"
)

// AttentionCalibrationBin is the observed-outcome tally for one
// (suggested_action × confidence band) cell of attention_feedback. It is the raw
// material for confidence calibration: the empirical P(operator agreed) in a band
// is what a model-emitted confidence in that band SHOULD have meant.
//
// Unlike AttentionFeedbackReport / LearnedAttentionPolicyFromFeedback, this
// query deliberately INCLUDES the operator_handled calibration rows (the operator
// resolved a surfaced thread by replying in it themselves) — folding the
// operator's hand actions back in is exactly the calibration signal the
// steerer-operator-reply-learning task emits for this one to consume.
type AttentionCalibrationBin struct {
	Action          string
	ConfidenceBand  string
	Approved        int // outcome IN (approved, sent)
	Negative        int // outcome IN (dismissed, muted)
	OperatorHandled int // outcome == operator_handled (calibration-only rows)
}

// AttentionCalibrationBins returns one tally per (suggested_action × confidence
// band), sorted (action, band). Rows with no action or no band are skipped — they
// carry no calibration signal. Includes operator_handled rows on purpose (see the
// type doc); callers decide how to weight them.
func AttentionCalibrationBins(db *sql.DB) ([]AttentionCalibrationBin, error) {
	if db == nil {
		return nil, fmt.Errorf("flowdb: attention calibration bins requires db")
	}
	rows, err := db.Query(`
		SELECT suggested_action,
		       confidence_band,
		       SUM(CASE WHEN outcome IN ('approved','sent') THEN 1 ELSE 0 END) AS approved,
		       SUM(CASE WHEN outcome IN ('dismissed','muted') THEN 1 ELSE 0 END) AS negative,
		       SUM(CASE WHEN outcome = '` + OutcomeOperatorHandled + `' THEN 1 ELSE 0 END) AS operator_handled
		FROM attention_feedback
		WHERE suggested_action != '' AND confidence_band != ''
		GROUP BY suggested_action, confidence_band
		ORDER BY suggested_action ASC, confidence_band ASC`)
	if err != nil {
		return nil, fmt.Errorf("flowdb: attention calibration bins: %w", err)
	}
	defer rows.Close()

	var out []AttentionCalibrationBin
	for rows.Next() {
		var b AttentionCalibrationBin
		if err := rows.Scan(&b.Action, &b.ConfidenceBand, &b.Approved, &b.Negative, &b.OperatorHandled); err != nil {
			return nil, fmt.Errorf("flowdb: scan attention calibration bin: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
