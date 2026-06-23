package flowdb

import (
	"database/sql"
	"fmt"
)

// RemoteDevice is a phone (or other client) paired for remote access. Its token
// is stored only as a SHA-256 hex hash; the plaintext is shown to the device
// once at pairing and never persisted.
type RemoteDevice struct {
	ID         string
	Label      string
	TokenHash  string
	CreatedAt  string
	ExpiresAt  string
	LastSeenAt sql.NullString
	RevokedAt  sql.NullString
}

const RemoteDeviceCols = "id, label, token_hash, created_at, expires_at, last_seen_at, revoked_at"

func ScanRemoteDevice(row interface{ Scan(dest ...any) error }) (*RemoteDevice, error) {
	var d RemoteDevice
	err := row.Scan(&d.ID, &d.Label, &d.TokenHash, &d.CreatedAt, &d.ExpiresAt, &d.LastSeenAt, &d.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func InsertRemoteDevice(db *sql.DB, id, label, tokenHash, createdAt, expiresAt string) error {
	_, err := db.Exec(
		`INSERT INTO remote_devices (`+RemoteDeviceCols+`) VALUES (?, ?, ?, ?, ?, NULL, NULL)`,
		id, label, tokenHash, createdAt, expiresAt)
	if err != nil {
		return fmt.Errorf("insert remote device %s: %w", id, err)
	}
	return nil
}

// GetRemoteDeviceByTokenHash returns the device row for a token hash, or
// sql.ErrNoRows. It does NOT filter revoked/expired rows — the caller decides,
// so validation logic lives in one place (see validRemoteDeviceToken).
func GetRemoteDeviceByTokenHash(db *sql.DB, tokenHash string) (*RemoteDevice, error) {
	row := db.QueryRow("SELECT "+RemoteDeviceCols+" FROM remote_devices WHERE token_hash = ?", tokenHash)
	return ScanRemoteDevice(row)
}

func ListRemoteDevices(db *sql.DB) ([]*RemoteDevice, error) {
	rows, err := db.Query("SELECT " + RemoteDeviceCols + " FROM remote_devices ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list remote devices: %w", err)
	}
	defer rows.Close()
	var out []*RemoteDevice
	for rows.Next() {
		d, err := ScanRemoteDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan remote device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func RevokeRemoteDevice(db *sql.DB, id, now string) error {
	_, err := db.Exec("UPDATE remote_devices SET revoked_at = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("revoke remote device %s: %w", id, err)
	}
	return nil
}

// DeleteRemoteDevice permanently removes a paired-device row by id. Revoke
// disables a device's token but leaves the row visible; delete clears it from
// the list entirely. Deleting a still-active device also invalidates its token
// (GetRemoteDeviceByTokenHash then returns sql.ErrNoRows), so delete is a strict
// superset of revoke.
func DeleteRemoteDevice(db *sql.DB, id string) error {
	_, err := db.Exec("DELETE FROM remote_devices WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete remote device %s: %w", id, err)
	}
	return nil
}

func TouchRemoteDeviceLastSeen(db *sql.DB, id, now string) error {
	_, err := db.Exec("UPDATE remote_devices SET last_seen_at = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("touch remote device %s: %w", id, err)
	}
	return nil
}
