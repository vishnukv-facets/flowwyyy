package flowdb

import "testing"

func TestAttentionFeedbackRecordAndList(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	item := FeedItem{
		ID:              "feed-1",
		Source:          "slack",
		ThreadKey:       "C1:100.1",
		SuggestedAction: "reply",
		Confidence:      0.83,
		Draft:           "Original draft",
		Channel:         "C1",
		ChannelType:     "channel",
		Author:          "U_REVIEWER",
		CreatedAt:       "2026-06-05T10:00:00Z",
	}
	fb := AttentionFeedbackFromFeed(item, "send_reply", "approved", "Original draft, edited", "2026-06-05T11:00:00Z")
	if err := RecordAttentionFeedback(db, fb); err != nil {
		t.Fatalf("RecordAttentionFeedback: %v", err)
	}

	got, err := ListAttentionFeedback(db, AttentionFeedbackFilter{})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	row := got[0]
	if row.FeedItemID != "feed-1" || row.Source != "slack" || row.Channel != "C1" ||
		row.Author != "U_REVIEWER" || row.ThreadType != "channel" {
		t.Errorf("source snapshot mismatch: %+v", row)
	}
	if row.SuggestedAction != "reply" || row.FinalAction != "send_reply" || row.Outcome != "approved" {
		t.Errorf("action fields mismatch: %+v", row)
	}
	if row.ConfidenceBand != "0.80-0.89" {
		t.Errorf("ConfidenceBand = %q, want 0.80-0.89", row.ConfidenceBand)
	}
	if row.DraftBefore != "Original draft" || row.DraftAfter != "Original draft, edited" || row.DraftEditDelta == "" {
		t.Errorf("draft edit fields not captured: %+v", row)
	}
}

func TestAttentionFeedbackReportByChannelAndConfidenceBand(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	rows := []AttentionFeedback{
		{ID: "a", FeedItemID: "fa", Source: "slack", Channel: "C1", Author: "U1", ThreadType: "channel", ThreadKey: "C1:1", SuggestedAction: "reply", FinalAction: "send_reply", Outcome: "approved", Confidence: 0.91, ConfidenceBand: "0.90-1.00", CreatedAt: "2026-06-05T10:00:00Z"},
		{ID: "b", FeedItemID: "fb", Source: "slack", Channel: "C1", Author: "U2", ThreadType: "channel", ThreadKey: "C1:2", SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed", Confidence: 0.72, ConfidenceBand: "0.70-0.79", CreatedAt: "2026-06-05T11:00:00Z"},
		{ID: "c", FeedItemID: "fc", Source: "slack", Channel: "C2", Author: "U1", ThreadType: "channel", ThreadKey: "C2:3", SuggestedAction: "forward", FinalAction: "forward", Outcome: "approved", Confidence: 0.74, ConfidenceBand: "0.70-0.79", CreatedAt: "2026-06-05T12:00:00Z"},
	}
	for _, row := range rows {
		if err := RecordAttentionFeedback(db, row); err != nil {
			t.Fatalf("RecordAttentionFeedback %s: %v", row.ID, err)
		}
	}

	byChannel, err := AttentionFeedbackReport(db, "channel")
	if err != nil {
		t.Fatalf("AttentionFeedbackReport channel: %v", err)
	}
	if len(byChannel) != 2 {
		t.Fatalf("channel groups = %d, want 2: %+v", len(byChannel), byChannel)
	}
	if byChannel[0].Group != "C1" || byChannel[0].Total != 2 || byChannel[0].Approved != 1 || byChannel[0].Dismissed != 1 ||
		byChannel[0].ApprovalRate != 0.5 || byChannel[0].DismissRate != 0.5 {
		t.Errorf("C1 aggregate mismatch: %+v", byChannel[0])
	}

	byBand, err := AttentionFeedbackReport(db, "confidence_band")
	if err != nil {
		t.Fatalf("AttentionFeedbackReport confidence_band: %v", err)
	}
	if len(byBand) != 2 || byBand[0].Group != "0.70-0.79" || byBand[0].Total != 2 {
		t.Errorf("confidence band aggregate mismatch: %+v", byBand)
	}
}

// A teammate's DM (thread_type im/mpim) must NEVER be auto-suppressed from card
// dismissals — dismissing a few low-value cards is "I don't need to act", not
// "silence this person". The learned policy is for broadcast-channel noise only,
// so DM feedback drives neither channel nor author suppression.
func TestLearnedAttentionPolicyNeverSuppressesDirectMessages(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	// 4/4 dismissed in a DM — well past the total>=3 & 0.8 dismiss-rate threshold
	// that would suppress a broadcast channel.
	for i := 0; i < 4; i++ {
		if err := RecordAttentionFeedback(db, AttentionFeedback{
			ID: "dm-dismiss-" + string(rune('a'+i)), FeedItemID: "fdm", Source: "slack",
			Channel: "D_TEAMMATE", Author: "U_TEAMMATE", ThreadType: "im", ThreadKey: "D_TEAMMATE:1",
			SuggestedAction: "digest_only", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.5, ConfidenceBand: "0.50-0.59", CreatedAt: "2026-06-10T10:00:00Z",
		}); err != nil {
			t.Fatalf("record dm dismiss %d: %v", i, err)
		}
	}

	policy, err := LearnedAttentionPolicyFromFeedback(db, LearnedAttentionPolicyOptions{MinFeedback: 3})
	if err != nil {
		t.Fatalf("LearnedAttentionPolicyFromFeedback: %v", err)
	}
	if policy.SuppressThreads["D_TEAMMATE:1"] {
		t.Errorf("a DM thread must never be learned-suppressed (would silence the teammate): %+v", policy.SuppressThreads)
	}
	if policy.SuppressAuthors["U_TEAMMATE"] {
		t.Errorf("an author must not be learned-suppressed from DM-only dismissals: %+v", policy.SuppressAuthors)
	}
}

func TestLearnedAttentionPolicySuppressesDismissedSourcesAndAdjustsThresholds(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := RecordAttentionFeedback(db, AttentionFeedback{
			ID: "dismiss-channel-" + string(rune('a'+i)), FeedItemID: "fdc", Source: "slack",
			Channel: "C_NOISE", Author: "U_NOISE", ThreadType: "channel", ThreadKey: "C_NOISE:1",
			SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.86, ConfidenceBand: "0.80-0.89", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("record dismiss %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := RecordAttentionFeedback(db, AttentionFeedback{
			ID: "approve-action-" + string(rune('a'+i)), FeedItemID: "fap", Source: "slack",
			Channel: "C_SIGNAL", Author: "U_SIGNAL", ThreadType: "channel", ThreadKey: "C_SIGNAL:1",
			SuggestedAction: "forward", FinalAction: "forward", Outcome: "approved",
			Confidence: 0.76, ConfidenceBand: "0.70-0.79", CreatedAt: "2026-06-05T11:00:00Z",
		}); err != nil {
			t.Fatalf("record approve %d: %v", i, err)
		}
	}

	policy, err := LearnedAttentionPolicyFromFeedback(db, LearnedAttentionPolicyOptions{MinFeedback: 3})
	if err != nil {
		t.Fatalf("LearnedAttentionPolicyFromFeedback: %v", err)
	}
	if !policy.SuppressThreads["C_NOISE:1"] {
		t.Errorf("expected thread C_NOISE:1 to be learned as a suppressed thread: %+v", policy.SuppressThreads)
	}
	if !policy.SuppressAuthors["U_NOISE"] {
		t.Errorf("expected U_NOISE to be learned as a suppressed author: %+v", policy.SuppressAuthors)
	}
	if got := policy.ThresholdAdjustments["forward"]; got >= 0 {
		t.Errorf("forward threshold adjustment = %v, want a negative adjustment after repeated approvals", got)
	}
	if got := policy.ThresholdAdjustments["reply"]; got <= 0 {
		t.Errorf("reply threshold adjustment = %v, want a positive adjustment after repeated dismissals", got)
	}
}

// operator_handled calibration rows are stored and listable, but invisible to the
// operator-facing report and the learned-policy derivation, so they never dilute
// approval/dismiss denominators or trip suppression/threshold logic.
func TestOperatorHandledExcludedFromAggregations(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	for i, ts := range []string{"01", "02", "03", "04", "05"} {
		fb := AttentionFeedback{
			ID: "oh" + ts, FeedItemID: "f" + ts, Source: "slack", Channel: "C_NOISE", Author: "U_NOISE",
			ThreadType: "channel", ThreadKey: "C_NOISE:1", SuggestedAction: "reply",
			FinalAction: "operator_reply", Outcome: OutcomeOperatorHandled, Confidence: 0.5,
			CreatedAt: "2026-06-05T10:00:" + ts + "Z",
		}
		if err := RecordAttentionFeedback(db, fb); err != nil {
			t.Fatalf("record row %d: %v", i, err)
		}
	}

	if rows, err := ListAttentionFeedback(db, AttentionFeedbackFilter{}); err != nil || len(rows) != 5 {
		t.Fatalf("ListAttentionFeedback len=%d err=%v, want 5 stored rows", len(rows), err)
	}
	rep, err := AttentionFeedbackReport(db, "suggested_action")
	if err != nil {
		t.Fatalf("AttentionFeedbackReport: %v", err)
	}
	if len(rep) != 0 {
		t.Errorf("report = %+v, want empty (all rows are operator_handled)", rep)
	}
	policy, err := LearnedAttentionPolicyFromFeedback(db, LearnedAttentionPolicyOptions{})
	if err != nil {
		t.Fatalf("LearnedAttentionPolicyFromFeedback: %v", err)
	}
	if len(policy.SuppressAuthors) != 0 || len(policy.SuppressThreads) != 0 || len(policy.ThresholdAdjustments) != 0 {
		t.Errorf("learned policy = %+v, want empty (operator_handled rows excluded)", policy)
	}
}

func TestRecentAgentReplyDrafts(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	seed := func(id, thread, final, draft, at string) {
		if err := RecordAttentionFeedback(db, AttentionFeedback{
			ID: id, FeedItemID: id, Source: "slack", ThreadKey: thread, SuggestedAction: "reply",
			FinalAction: final, Outcome: "approved", DraftAfter: draft, CreatedAt: at,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("s1", "C1:1", "send_reply", "draft A", "2026-06-05T10:00:00Z")
	seed("s2", "C1:1", "sent", "draft B", "2026-06-05T11:00:00Z")
	seed("d1", "C1:1", "dismiss", "", "2026-06-05T12:00:00Z")                 // not a reply → excluded
	seed("s3", "C2:9", "send_reply", "other thread", "2026-06-05T11:30:00Z") // other thread → excluded

	got, err := RecentAgentReplyDrafts(db, "C1:1", "")
	if err != nil {
		t.Fatalf("RecentAgentReplyDrafts: %v", err)
	}
	if len(got) != 2 || got[0] != "draft B" || got[1] != "draft A" {
		t.Errorf("drafts = %v, want [draft B, draft A] newest-first", got)
	}
	since, err := RecentAgentReplyDrafts(db, "C1:1", "2026-06-05T10:30:00Z")
	if err != nil {
		t.Fatalf("RecentAgentReplyDrafts(since): %v", err)
	}
	if len(since) != 1 || since[0] != "draft B" {
		t.Errorf("drafts since = %v, want [draft B] only", since)
	}
}
