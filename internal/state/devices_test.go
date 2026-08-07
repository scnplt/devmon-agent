package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateDeviceThenLookupBySerial(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(90 * 24 * time.Hour)
	if err := s.RecordDeviceCert(ctx, device.ID, "abc123", notBefore, notAfter); err != nil {
		t.Fatalf("RecordDeviceCert() unexpected error: %v", err)
	}

	// Act
	found, err := s.DeviceByCertSerial(ctx, "abc123")

	// Assert
	if err != nil {
		t.Fatalf("DeviceByCertSerial() unexpected error: %v", err)
	}
	if found.ID != device.ID {
		t.Errorf("DeviceByCertSerial() ID = %q, want %q", found.ID, device.ID)
	}
	if found.Name != "Pixel 9" {
		t.Errorf("DeviceByCertSerial() Name = %q, want %q", found.Name, "Pixel 9")
	}
	if found.IsRevoked() {
		t.Error("IsRevoked() = true for a freshly paired device, want false")
	}
}

func TestDeviceByCertSerialReturnsRevokedDevice(t *testing.T) {
	t.Parallel()

	// Arrange — a revoked device's certificate must still resolve, so the
	// caller (requireDevice) can distinguish "revoked" from "unknown" and log
	// which one happened.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Lost Phone")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	notBefore := time.Now()
	if err := s.RecordDeviceCert(ctx, device.ID, "revoked-serial", notBefore, notBefore.Add(time.Hour)); err != nil {
		t.Fatalf("RecordDeviceCert() unexpected error: %v", err)
	}
	if err := s.RevokeDevice(ctx, device.ID); err != nil {
		t.Fatalf("RevokeDevice() unexpected error: %v", err)
	}

	// Act
	found, err := s.DeviceByCertSerial(ctx, "revoked-serial")

	// Assert
	if err != nil {
		t.Fatalf("DeviceByCertSerial() unexpected error: %v", err)
	}
	if !found.IsRevoked() {
		t.Error("IsRevoked() = false for a revoked device, want true")
	}
}

func TestDeviceByCertSerialUnknownReturnsErrDeviceNotFound(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	_, err := s.DeviceByCertSerial(context.Background(), "never-issued")

	// Assert
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("DeviceByCertSerial() error = %v, want ErrDeviceNotFound", err)
	}
}

func TestRevokeDeviceIsIdempotent(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	if err := s.RevokeDevice(ctx, device.ID); err != nil {
		t.Fatalf("first RevokeDevice() unexpected error: %v", err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	firstRevokedAt := devices[0].RevokedAt

	// Act — revoke again; revoked_at must not move.
	if err := s.RevokeDevice(ctx, device.ID); err != nil {
		t.Fatalf("second RevokeDevice() unexpected error: %v", err)
	}

	// Assert
	devices, err = s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if !devices[0].RevokedAt.Equal(firstRevokedAt) {
		t.Errorf("RevokedAt changed on a second revoke: %v -> %v", firstRevokedAt, devices[0].RevokedAt)
	}
}

func TestRevokeDeviceUnknownIDReturnsErrDeviceNotFound(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	err := s.RevokeDevice(context.Background(), "0123456789abcdef")

	// Assert
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("RevokeDevice() error = %v, want ErrDeviceNotFound", err)
	}
}

func TestRenameDeviceUpdatesName(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "pending")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}

	// Act
	if err := s.RenameDevice(ctx, device.ID, "Pixel 9"); err != nil {
		t.Fatalf("RenameDevice() unexpected error: %v", err)
	}

	// Assert
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if devices[0].Name != "Pixel 9" {
		t.Errorf("Name = %q, want %q", devices[0].Name, "Pixel 9")
	}
}

func TestRenameDeviceUnknownIDReturnsErrDeviceNotFound(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	err := s.RenameDevice(context.Background(), "0123456789abcdef", "anything")

	// Assert
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("RenameDevice() error = %v, want ErrDeviceNotFound", err)
	}
}

func TestDeleteDeviceRemovesRow(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Rollback Candidate")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}

	// Act
	if err := s.DeleteDevice(ctx, device.ID); err != nil {
		t.Fatalf("DeleteDevice() unexpected error: %v", err)
	}

	// Assert
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	for _, d := range devices {
		if d.ID == device.ID {
			t.Errorf("device %s still present after DeleteDevice()", device.ID)
		}
	}
}

func TestSupersedePriorCertsKeepsRowsValid(t *testing.T) {
	t.Parallel()

	// Arrange — two certificates for the same device, mirroring a renewal.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Renewing Phone")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	now := time.Now()
	if err := s.RecordDeviceCert(ctx, device.ID, "old-serial", now, now.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("RecordDeviceCert(old) unexpected error: %v", err)
	}
	if err := s.RecordDeviceCert(ctx, device.ID, "new-serial", now, now.Add(180*24*time.Hour)); err != nil {
		t.Fatalf("RecordDeviceCert(new) unexpected error: %v", err)
	}

	// Act
	if err := s.SupersedePriorCerts(ctx, device.ID, "new-serial"); err != nil {
		t.Fatalf("SupersedePriorCerts() unexpected error: %v", err)
	}

	// Assert — the old certificate must still resolve to the device (D6): a
	// lost renewal response must not strand it.
	old, err := s.DeviceByCertSerial(ctx, "old-serial")
	if err != nil {
		t.Fatalf("DeviceByCertSerial(old) unexpected error: %v", err)
	}
	if old.ID != device.ID {
		t.Errorf("DeviceByCertSerial(old) ID = %q, want %q", old.ID, device.ID)
	}
	current, err := s.DeviceByCertSerial(ctx, "new-serial")
	if err != nil {
		t.Fatalf("DeviceByCertSerial(new) unexpected error: %v", err)
	}
	if current.ID != device.ID {
		t.Errorf("DeviceByCertSerial(new) ID = %q, want %q", current.ID, device.ID)
	}
}

func TestTouchDeviceThrottlesWithinResolutionWindow(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	if err := s.TouchDevice(ctx, device.ID); err != nil {
		t.Fatalf("first TouchDevice() unexpected error: %v", err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	firstSeenAt := devices[0].LastSeenAt
	if firstSeenAt.IsZero() {
		t.Fatal("LastSeenAt is zero after TouchDevice(), want a timestamp")
	}

	// Act — a second touch immediately after must be a no-op: the stored
	// value is not older than lastSeenResolution, so exactly one write
	// (the first) should have happened.
	if err := s.TouchDevice(ctx, device.ID); err != nil {
		t.Fatalf("second TouchDevice() unexpected error: %v", err)
	}

	// Assert
	devices, err = s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if !devices[0].LastSeenAt.Equal(firstSeenAt) {
		t.Errorf("LastSeenAt changed on a second touch within %s: %v -> %v",
			lastSeenResolution, firstSeenAt, devices[0].LastSeenAt)
	}
}

func TestTouchDeviceWritesAgainAfterResolutionWindow(t *testing.T) {
	t.Parallel()

	// Arrange — seed last_seen_at directly, older than the resolution window,
	// rather than sleeping the test for 60 real seconds.
	s := openStore(t, tempDBPath(t))
	ctx := context.Background()
	device, err := s.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	stale := time.Now().Add(-2 * lastSeenResolution)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`, stale.Unix(), device.ID,
	); err != nil {
		t.Fatalf("seed stale last_seen_at: %v", err)
	}

	// Act
	if err := s.TouchDevice(ctx, device.ID); err != nil {
		t.Fatalf("TouchDevice() unexpected error: %v", err)
	}

	// Assert
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if devices[0].LastSeenAt.Unix() == stale.Unix() {
		t.Error("LastSeenAt unchanged after the resolution window elapsed, want a refreshed timestamp")
	}
}

func TestListDevicesEmptyStore(t *testing.T) {
	t.Parallel()

	// Arrange
	s := openStore(t, tempDBPath(t))

	// Act
	devices, err := s.ListDevices(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("ListDevices() = %d devices on an empty store, want 0", len(devices))
	}
}
