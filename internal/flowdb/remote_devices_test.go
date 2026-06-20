package flowdb

import (
	"database/sql"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

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
