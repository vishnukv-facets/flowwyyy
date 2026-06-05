package monitor

import (
	"context"
	"testing"

	"flow/internal/flowdb"
)

// The Samarthya case end-to-end at the dispatcher layer: a task tracks a
// #channel thread, but the answer arrives as a message in a *different*
// conversation (a DM) that forwards the original thread message. The shared-ref
// fields route it home — inbox append + waiting_on cleared — without the steerer.
func TestDispatcher_ForwardedMessageRoutesViaSharedRef(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()

	// Task is anchored on the engineering-team thread and is blocked.
	seedSlackTask(t, db, "eng-task", "C_eng:1700000000.000100")
	if _, err := db.Exec(`UPDATE tasks SET waiting_on = ? WHERE slug = ?`,
		"Samarthya to grant security-manager", "eng-task"); err != nil {
		t.Fatalf("set waiting_on: %v", err)
	}

	fs := &fakeSteerer{}
	d := NewDispatcher(db, nil)
	d.Steerer = fs

	// Samarthya forwards the thread message into a DM and answers there. The DM
	// conversation itself is untracked; the ref points back at the eng thread.
	msg := InboundEvent{
		Kind:        "message",
		ChannelType: "im",
		Channel:     "D_dm",
		TS:          "1700000999.000100",
		ThreadTS:    "1700000999.000100",
		UserID:      "U_samarthya",
		Text:        "Made you the owner",
		RefChannel:  "C_eng",
		RefThreadTS: "1700000000.000100",
		RefTS:       "1700000500.000300",
	}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	entries, err := ReadInboxEntries("eng-task")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("forwarded reply should append to the tracked task inbox, got %d", len(entries))
	}
	if entries[0].Event.Text != "Made you the owner" {
		t.Errorf("inbox entry text = %q", entries[0].Event.Text)
	}
	if len(fs.events) != 0 {
		t.Errorf("a ref-routed message must NOT also be steered, got %d", len(fs.events))
	}
	task, err := flowdb.GetTask(db, "eng-task")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.WaitingOn.String != "" {
		t.Errorf("waiting_on = %q, want cleared by the external forwarded reply", task.WaitingOn.String)
	}
}

// When the forwarded reference points at a thread no task tracks, ref-routing
// must miss cleanly and the message falls through to the steerer (no silent drop).
func TestDispatcher_ForwardedMessageNoMatchFallsToSteerer(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()

	fs := &fakeSteerer{}
	d := NewDispatcher(db, nil)
	d.Steerer = fs

	msg := InboundEvent{
		Kind:        "message",
		ChannelType: "im",
		Channel:     "D_dm",
		TS:          "1700000999.000100",
		ThreadTS:    "1700000999.000100",
		UserID:      "U_other",
		Text:        "fyi",
		RefChannel:  "C_unknown",
		RefThreadTS: "1700000000.000999",
		RefTS:       "1700000000.000999",
	}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fs.events) != 1 {
		t.Errorf("unmatched forwarded message should reach the steerer, got %d", len(fs.events))
	}
}

// The operator's own forwarded reply wakes the session (inbox) but does NOT
// clear waiting_on — they're the one who was waiting.
func TestDispatcher_ForwardedSelfReplyDoesNotResolveWait(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	db := dispatcherTestDB(t)
	_, _, _, restore := stubDispatcherIO(t)
	defer restore()

	seedSlackTask(t, db, "eng-task", "C_eng:1700000000.000100")
	if _, err := db.Exec(`UPDATE tasks SET waiting_on = ? WHERE slug = ?`,
		"someone to reply", "eng-task"); err != nil {
		t.Fatalf("set waiting_on: %v", err)
	}

	d := NewDispatcher(db, nil)
	d.Steerer = &fakeSteerer{}

	msg := InboundEvent{
		Kind: "message", ChannelType: "im", Channel: "D_dm",
		TS: "1.1", ThreadTS: "1.1", UserID: "U_me", Text: "still waiting",
		RefChannel: "C_eng", RefThreadTS: "1700000000.000100", RefTS: "1700000500.000300",
	}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	task, _ := flowdb.GetTask(db, "eng-task")
	if task.WaitingOn.String == "" {
		t.Error("operator's own forwarded reply must not clear their own waiting_on")
	}
}
