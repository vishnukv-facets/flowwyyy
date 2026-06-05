package flowdb

import "testing"

func TestSteeringMutesRoundTrip(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	if err := AddSteeringMute(db, MuteScopeChannel, "C_noise"); err != nil {
		t.Fatalf("add channel: %v", err)
	}
	if err := AddSteeringMute(db, MuteScopeChannel, "C_noise"); err != nil {
		t.Fatalf("add channel (idempotent): %v", err) // INSERT OR IGNORE
	}
	if err := AddSteeringMute(db, MuteScopeAuthor, "U_bot"); err != nil {
		t.Fatalf("add author: %v", err)
	}
	if err := AddSteeringMute(db, MuteScopeThread, "C_x:1.1"); err != nil {
		t.Fatalf("add thread: %v", err)
	}

	m, err := ListSteeringMutes(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !m.Channels["C_noise"] || !m.Authors["U_bot"] || !m.Threads["C_x:1.1"] {
		t.Errorf("mutes = %+v, want all three present", m)
	}

	if err := RemoveSteeringMute(db, MuteScopeChannel, "C_noise"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m2, _ := ListSteeringMutes(db)
	if m2.Channels["C_noise"] {
		t.Error("channel mute should be gone after remove")
	}
	if !m2.Authors["U_bot"] {
		t.Error("author mute should remain")
	}

	if err := AddSteeringMute(db, "channel", ""); err == nil {
		t.Error("empty value must error")
	}
}
