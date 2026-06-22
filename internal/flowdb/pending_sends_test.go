package flowdb

import "testing"

// Pending sends are the external-channel send gate's store: an external send is
// created 'pending', listed for the operator, then marked 'sent' or 'discarded'
// when they decide. Persisted so a queued send survives a restart.
func TestPendingSendsLifecycle(t *testing.T) {
	db := openTestDB(t)

	if got, err := ListPendingSends(db, "pending"); err != nil || len(got) != 0 {
		t.Fatalf("empty list: got %d err %v", len(got), err)
	}

	in := PendingSend{
		ID:           "ps1",
		Channel:      "C_EXT",
		ChannelLabel: "#partner-shared",
		ThreadTS:     "1700.1",
		Text:         "hello partner",
		Identity:     "user",
		Reason:       "Slack Connect (external org present)",
		Origin:       "task:slack-c0b3",
	}
	if err := CreatePendingSend(db, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := ListPendingSends(db, "pending")
	if err != nil || len(got) != 1 {
		t.Fatalf("list pending: got %d err %v", len(got), err)
	}
	if got[0].Channel != "C_EXT" || got[0].Text != "hello partner" || got[0].Reason == "" {
		t.Fatalf("round-trip mismatch: %+v", got[0])
	}
	if got[0].Status != "pending" {
		t.Fatalf("status = %q, want pending", got[0].Status)
	}

	ps, ok, err := GetPendingSend(db, "ps1")
	if err != nil || !ok || ps.ThreadTS != "1700.1" {
		t.Fatalf("get: ok=%v err=%v ts=%q", ok, err, ps.ThreadTS)
	}

	if err := SetPendingSendStatus(db, "ps1", "sent"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got, _ := ListPendingSends(db, "pending"); len(got) != 0 {
		t.Fatalf("after send, pending list should be empty, got %d", len(got))
	}
	ps, ok, _ = GetPendingSend(db, "ps1")
	if !ok || ps.Status != "sent" || ps.DecidedAt == "" {
		t.Fatalf("after send: status=%q decided=%q", ps.Status, ps.DecidedAt)
	}
}
