package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// deviceIDBytes is the amount of randomness behind a device ID: 8 bytes hex
// encode to 16 lowercase hex characters, matching the schema comment.
const deviceIDBytes = 8

// lastSeenResolution is the minimum gap between two last_seen_at writes for
// the same device (D11). Writing on every request, against a pool capped at
// MaxOpenConns(1), would serialise reads behind a write for no operational
// benefit.
const lastSeenResolution = 60 * time.Second

// Device is one paired client. Certificates live in a separate table so a
// renewal is an INSERT, not a destructive UPDATE (see D6 in the Phase 2 plan).
type Device struct {
	ID         string
	Name       string
	PairedAt   time.Time
	LastSeenAt time.Time // zero when never seen
	RevokedAt  time.Time // zero when active
}

// IsRevoked reports whether the device's access has been withdrawn.
func (d Device) IsRevoked() bool {
	return !d.RevokedAt.IsZero()
}

// CreateDevice registers a new device and returns its record. The ID is 8
// random bytes hex-encoded via crypto/rand — never math/rand, which is not
// safe for identifiers an attacker might try to predict or collide.
func (s *Store) CreateDevice(ctx context.Context, name string) (Device, error) {
	raw := make([]byte, deviceIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return Device{}, fmt.Errorf("generate device id: %w", err)
	}
	id := hex.EncodeToString(raw)
	pairedAt := time.Now()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, paired_at) VALUES (?, ?, ?)`,
		id, name, pairedAt.Unix(),
	)
	if err != nil {
		return Device{}, fmt.Errorf("create device %s: %w", id, err)
	}

	return Device{ID: id, Name: name, PairedAt: pairedAt.UTC()}, nil
}

// RenameDevice updates a device's display name.
//
// It exists for the pairing flow (see internal/httpapi/pair.go): a device
// row must exist before its pairing code can be redeemed, because
// RedeemPairingCode needs the device's ID, but the code — never the
// request — is the only legitimate source of the device's name. The row is
// created with a placeholder name and renamed once RedeemPairingCode returns
// the real one.
func (s *Store) RenameDevice(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("rename device %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rename device %s: rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// DeleteDevice removes a device and, via ON DELETE CASCADE, every certificate
// issued to it. It exists only for the pairing rollback path: when issuance
// fails after the device row was created, the orphan must not persist.
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete device %s: %w", id, err)
	}
	return nil
}

// RecordDeviceCert stores a newly issued certificate against its owning
// device.
func (s *Store) RecordDeviceCert(ctx context.Context, deviceID, serial string, notBefore, notAfter time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO device_certs (serial, device_id, not_before, not_after, issued_at)
		 VALUES (?, ?, ?, ?, ?)`,
		serial, deviceID, notBefore.Unix(), notAfter.Unix(), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record certificate %s for device %s: %w", serial, deviceID, err)
	}
	return nil
}

// SupersedePriorCerts marks every certificate the device holds, other than
// keepSerial, as superseded. It never deletes rows or shortens validity (D6):
// a superseded certificate stays valid until its own not_after, so a lost
// renewal response cannot strand the device.
func (s *Store) SupersedePriorCerts(ctx context.Context, deviceID, keepSerial string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE device_certs SET superseded_at = ?
		 WHERE device_id = ? AND serial != ? AND superseded_at IS NULL`,
		time.Now().Unix(), deviceID, keepSerial,
	)
	if err != nil {
		return fmt.Errorf("supersede prior certificates for device %s: %w", deviceID, err)
	}
	return nil
}

// DeviceByCertSerial resolves the device that owns a certificate serial. It
// returns the device even when revoked, so the caller can distinguish
// "unknown serial" from "revoked device" and log precisely which happened.
func (s *Store) DeviceByCertSerial(ctx context.Context, serial string) (Device, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.name, d.paired_at, d.last_seen_at, d.revoked_at
		 FROM device_certs c JOIN devices d ON d.id = c.device_id
		 WHERE c.serial = ?`,
		serial,
	)
	device, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrDeviceNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("look up device by certificate serial %s: %w", serial, err)
	}
	return device, nil
}

// ListDevices returns every registered device, most recently paired first.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, paired_at, last_seen_at, revoked_at
		 FROM devices ORDER BY paired_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var devices []Device
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device row: %w", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return devices, nil
}

// RevokeDevice withdraws a device's access. It is idempotent: revoking an
// already-revoked device succeeds without changing its revoked_at.
func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM devices WHERE id = ?)`, id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check device %s exists: %w", id, err)
	}
	if !exists {
		return ErrDeviceNotFound
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("revoke device %s: %w", id, err)
	}
	return nil
}

// TouchDevice records that a device was just seen, but only when the stored
// value is older than lastSeenResolution (D11), so a burst of requests costs
// at most one write per resolution window.
func (s *Store) TouchDevice(ctx context.Context, id string) error {
	now := time.Now()
	threshold := now.Add(-lastSeenResolution).Unix()

	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ?
		 WHERE id = ? AND (last_seen_at IS NULL OR last_seen_at < ?)`,
		now.Unix(), id, threshold,
	)
	if err != nil {
		return fmt.Errorf("touch device %s: %w", id, err)
	}
	return nil
}

// rowScanner abstracts over *sql.Row and *sql.Rows, both of which implement
// Scan with an identical signature.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanDevice reads one devices row, converting the nullable Unix-second
// columns into zero-valued time.Time when absent.
func scanDevice(row rowScanner) (Device, error) {
	var (
		id, name              string
		pairedAt              int64
		lastSeenAt, revokedAt sql.NullInt64
	)
	if err := row.Scan(&id, &name, &pairedAt, &lastSeenAt, &revokedAt); err != nil {
		return Device{}, err
	}

	device := Device{
		ID:       id,
		Name:     name,
		PairedAt: time.Unix(pairedAt, 0).UTC(),
	}
	if lastSeenAt.Valid {
		device.LastSeenAt = time.Unix(lastSeenAt.Int64, 0).UTC()
	}
	if revokedAt.Valid {
		device.RevokedAt = time.Unix(revokedAt.Int64, 0).UTC()
	}
	return device, nil
}
