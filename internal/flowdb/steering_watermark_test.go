package flowdb

import (
	"testing"
)

func TestGetSteeringWatermark_Empty(t *testing.T) {
	db := openTempDB(t)

	ts, err := GetSteeringWatermark(db, "C0123456789")
	if err != nil {
		t.Fatalf("GetSteeringWatermark on empty channel: %v", err)
	}
	if ts != "" {
		t.Errorf("expected empty string, got %q", ts)
	}
}

func TestSetAndGetSteeringWatermark(t *testing.T) {
	db := openTempDB(t)
	ch := "C0123456789"
	want := "1717500000.000100"
	now := NowISO()

	if err := SetSteeringWatermark(db, ch, want, now); err != nil {
		t.Fatalf("SetSteeringWatermark: %v", err)
	}

	got, err := GetSteeringWatermark(db, ch)
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetSteeringWatermark_Overwrite(t *testing.T) {
	db := openTempDB(t)
	ch := "C0123456789"
	now := NowISO()

	if err := SetSteeringWatermark(db, ch, "1717500000.000100", now); err != nil {
		t.Fatalf("first set: %v", err)
	}

	newer := "1717600000.000200"
	if err := SetSteeringWatermark(db, ch, newer, now); err != nil {
		t.Fatalf("second set: %v", err)
	}

	got, err := GetSteeringWatermark(db, ch)
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if got != newer {
		t.Errorf("got %q, want %q", got, newer)
	}

	// Assert only one row exists for this channel.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steering_watermark WHERE channel = ?`, ch).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}
