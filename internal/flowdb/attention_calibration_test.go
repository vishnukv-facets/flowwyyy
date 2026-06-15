package flowdb

import "testing"

func TestAttentionCalibrationBins(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	rows := []AttentionFeedback{
		// make_task, band 0.80-0.89: 2 approved, 1 dismissed.
		{FeedItemID: "f1", Source: "slack", ThreadKey: "C:1", SuggestedAction: "make_task", FinalAction: "make_task", Outcome: "approved", Confidence: 0.85},
		{FeedItemID: "f2", Source: "slack", ThreadKey: "C:2", SuggestedAction: "make_task", FinalAction: "make_task", Outcome: "approved", Confidence: 0.81},
		{FeedItemID: "f3", Source: "slack", ThreadKey: "C:3", SuggestedAction: "make_task", FinalAction: "dismiss", Outcome: "dismissed", Confidence: 0.88},
		// forward, band 0.90-1.00: an operator_handled calibration-only row — must be
		// INCLUDED here even though the feedback report/learned-policy exclude it.
		{FeedItemID: "f4", Source: "slack", ThreadKey: "C:4", SuggestedAction: "forward", FinalAction: "operator_reply", Outcome: OutcomeOperatorHandled, Confidence: 0.95},
		// reply, band 0.50-0.59: 1 muted (negative).
		{FeedItemID: "f5", Source: "slack", ThreadKey: "C:5", SuggestedAction: "reply", FinalAction: "mute_thread", Outcome: "muted", Confidence: 0.55},
	}
	for _, r := range rows {
		if err := RecordAttentionFeedback(db, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	bins, err := AttentionCalibrationBins(db)
	if err != nil {
		t.Fatalf("AttentionCalibrationBins: %v", err)
	}
	got := map[string]AttentionCalibrationBin{}
	for _, b := range bins {
		got[b.Action+"|"+b.ConfidenceBand] = b
	}

	if b := got["make_task|0.80-0.89"]; b.Approved != 2 || b.Negative != 1 || b.OperatorHandled != 0 {
		t.Errorf("make_task bin = %+v, want approved=2 negative=1 operator_handled=0", b)
	}
	if b := got["forward|0.90-0.99"]; b.OperatorHandled != 1 || b.Approved != 0 || b.Negative != 0 {
		t.Errorf("forward bin = %+v, want operator_handled=1", b)
	}
	if b := got["reply|0.50-0.59"]; b.Negative != 1 || b.Approved != 0 {
		t.Errorf("reply bin = %+v, want negative=1", b)
	}
}
