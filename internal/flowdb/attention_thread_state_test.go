package flowdb

import "testing"

func TestGetThreadStateEmpty(t *testing.T) {
	db := openTempDB(t)
	got, ok, err := GetThreadState(db, "C1:1.1")
	if err != nil {
		t.Fatalf("GetThreadState: %v", err)
	}
	if ok {
		t.Fatalf("expected no row, got %+v", got)
	}
}

func TestRecordThreadDecisionAccumulates(t *testing.T) {
	db := openTempDB(t)
	key := "C1:1.1"

	// Event 1 — establishes the row.
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "reply", Confidence: 0.6,
		Reason: "first look", Summary: "customer asks about pricing",
		LastSeenTS: "1000.1", At: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	// Event 2 — blank summary must NOT blank out the good one; decision overwrites.
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "forward", Confidence: 0.7,
		Reason: "matches task", Summary: "",
		LastSeenTS: "1001.2", At: "2026-06-05T10:05:00Z",
	}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	// Event 3 — newer decision + new summary.
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "make_task", Confidence: 0.9,
		Reason: "needs tracking", Summary: "pricing escalation, owner assigned",
		LastSeenTS: "1002.3", At: "2026-06-05T10:10:00Z",
	}); err != nil {
		t.Fatalf("record 3: %v", err)
	}

	s, ok, err := GetThreadState(db, key)
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if s.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", s.EventCount)
	}
	if s.CurrentAction != "make_task" || s.CurrentConfidence != 0.9 || s.CurrentReason != "needs tracking" {
		t.Errorf("current decision = %q/%v/%q, want make_task/0.9/needs tracking", s.CurrentAction, s.CurrentConfidence, s.CurrentReason)
	}
	if s.Summary != "pricing escalation, owner assigned" {
		t.Errorf("Summary = %q, want event-3 summary", s.Summary)
	}
	if s.LastSeenTS != "1002.3" {
		t.Errorf("LastSeenTS = %q, want 1002.3", s.LastSeenTS)
	}
	if s.FirstSeenAt != "2026-06-05T10:00:00Z" {
		t.Errorf("FirstSeenAt = %q, want the event-1 time (preserved)", s.FirstSeenAt)
	}
}

func TestRecordThreadDecisionBlankSummaryCarriesForward(t *testing.T) {
	db := openTempDB(t)
	key := "D1:2000.0"
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "reply", Summary: "the good summary",
		At: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "reply", Summary: "",
		At: "2026-06-05T10:01:00Z",
	}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	s, _, err := GetThreadState(db, key)
	if err != nil {
		t.Fatalf("GetThreadState: %v", err)
	}
	if s.Summary != "the good summary" {
		t.Errorf("Summary = %q, want carried-forward summary", s.Summary)
	}
}

func TestAppendThreadOperatorActionAndReply(t *testing.T) {
	db := openTempDB(t)
	key := "C2:3000.0"
	// Seed a decision so the row exists; the appends must accumulate onto it.
	if err := RecordThreadDecision(db, ThreadDecision{
		ThreadKey: key, Source: "slack", Action: "reply", At: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := AppendThreadOperatorAction(db, key, ThreadOperatorAction{
		At: "2026-06-05T10:05:00Z", Action: "forward", Outcome: "approved", LinkedTask: "oauth-budget",
	}); err != nil {
		t.Fatalf("append action 1: %v", err)
	}
	if err := AppendThreadOperatorAction(db, key, ThreadOperatorAction{
		At: "2026-06-05T10:06:00Z", Action: "dismiss", Outcome: "dismissed",
	}); err != nil {
		t.Fatalf("append action 2: %v", err)
	}
	if err := AppendThreadOperatorReply(db, key, ThreadOperatorReply{
		At: "2026-06-05T10:07:00Z", TS: "3001.0", Author: "U_ME", Text: "handled it, thanks",
	}); err != nil {
		t.Fatalf("append reply: %v", err)
	}

	s, ok, err := GetThreadState(db, key)
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if len(s.OperatorActions) != 2 {
		t.Fatalf("OperatorActions = %d, want 2: %+v", len(s.OperatorActions), s.OperatorActions)
	}
	if s.OperatorActions[0].Action != "forward" || s.OperatorActions[0].LinkedTask != "oauth-budget" {
		t.Errorf("action[0] = %+v", s.OperatorActions[0])
	}
	if s.OperatorActions[1].Action != "dismiss" || s.OperatorActions[1].Outcome != "dismissed" {
		t.Errorf("action[1] = %+v", s.OperatorActions[1])
	}
	if len(s.OperatorReplies) != 1 || s.OperatorReplies[0].Text != "handled it, thanks" || s.OperatorReplies[0].Author != "U_ME" {
		t.Errorf("OperatorReplies = %+v", s.OperatorReplies)
	}
	// The decision-seeded fields must survive the appends.
	if s.CurrentAction != "reply" || s.EventCount != 1 {
		t.Errorf("decision fields clobbered by append: action=%q count=%d", s.CurrentAction, s.EventCount)
	}
}

func TestAppendThreadCreatesRowWhenMissing(t *testing.T) {
	db := openTempDB(t)
	key := "C3:4000.0"
	// No prior decision — an operator action lands on an un-carded thread.
	if err := AppendThreadOperatorAction(db, key, ThreadOperatorAction{
		At: "2026-06-05T10:00:00Z", Action: "dismiss", Outcome: "dismissed",
	}); err != nil {
		t.Fatalf("append on missing row: %v", err)
	}
	s, ok, err := GetThreadState(db, key)
	if err != nil || !ok {
		t.Fatalf("GetThreadState ok=%v err=%v", ok, err)
	}
	if len(s.OperatorActions) != 1 || s.FirstSeenAt == "" {
		t.Errorf("defensive row not created correctly: %+v", s)
	}
}
