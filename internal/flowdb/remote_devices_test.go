package flowdb

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// openTestDB is shared across flowdb tests (defined in pending_wakes_test.go).

func TestRemoteDeviceCRUD(t *testing.T) {
	db := openTestDB(t)
	now := NowISO()
	exp := time.Now().Add(12 * time.Hour).Format(time.RFC3339)
	if err := InsertRemoteDevice(db, "dev1", "iPhone", "hashAAA", now, exp); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := GetRemoteDeviceByTokenHash(db, "hashAAA")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "dev1" || got.Label != "iPhone" || got.ExpiresAt != exp {
		t.Fatalf("unexpected device: %+v", got)
	}
	list, err := ListRemoteDevices(db)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if err := RevokeRemoteDevice(db, "dev1", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err = GetRemoteDeviceByTokenHash(db, "hashAAA")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if !got.RevokedAt.Valid {
		t.Fatalf("expected revoked_at set after revoke")
	}
}

func TestDeleteRemoteDevice(t *testing.T) {
	db := openTestDB(t)
	now := NowISO()
	exp := time.Now().Add(12 * time.Hour).Format(time.RFC3339)
	if err := InsertRemoteDevice(db, "dev1", "iPhone", "hashAAA", now, exp); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := DeleteRemoteDevice(db, "dev1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Row is gone from the list entirely (not just revoked).
	list, err := ListRemoteDevices(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 devices after delete, got %d", len(list))
	}
	// Its token no longer resolves — delete is a superset of revoke.
	if _, err := GetRemoteDeviceByTokenHash(db, "hashAAA"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows after delete, got %v", err)
	}
	// Deleting a missing id is a no-op, not an error.
	if err := DeleteRemoteDevice(db, "nope"); err != nil {
		t.Fatalf("delete missing id should be a no-op: %v", err)
	}
}
