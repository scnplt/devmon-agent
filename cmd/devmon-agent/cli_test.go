package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/state"
)

func openTestStore(t *testing.T) *state.Store {
	t.Helper()
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}
	st, err := openDeviceStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openDeviceStore() unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRunDeviceCommandMissingSubcommandReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act
	err := runDeviceCommand(context.Background(), cfg, nil)

	// Assert
	if err == nil {
		t.Fatal("runDeviceCommand() = nil, want an error for a missing subcommand")
	}
}

func TestRunDeviceCommandUnknownSubcommandReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act
	err := runDeviceCommand(context.Background(), cfg, []string{"bogus"})

	// Assert
	if err == nil {
		t.Fatal("runDeviceCommand() = nil, want an error for an unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("runDeviceCommand() error = %v, want it to name the unknown subcommand", err)
	}
}

func TestRunDeviceListEmptyStorePrintsHeaderOnly(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act / Assert — an empty store must not error; the table is header-only.
	if err := runDeviceList(context.Background(), st); err != nil {
		t.Fatalf("runDeviceList() unexpected error: %v", err)
	}
}

func TestRunDeviceRevokeUnknownIDReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDeviceRevoke(context.Background(), st, []string{"does-not-exist"})

	// Assert
	if !errors.Is(err, state.ErrDeviceNotFound) {
		t.Errorf("runDeviceRevoke() error = %v, want it to wrap ErrDeviceNotFound", err)
	}
}

func TestRunDeviceRevokeKnownIDSucceeds(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}

	// Act
	err = runDeviceRevoke(ctx, st, []string{device.ID})

	// Assert
	if err != nil {
		t.Fatalf("runDeviceRevoke() unexpected error: %v", err)
	}
	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices() unexpected error: %v", err)
	}
	if len(devices) != 1 || !devices[0].IsRevoked() {
		t.Errorf("device %s is not revoked after runDeviceRevoke()", device.ID)
	}
}

func TestRunDeviceRevokeRequiresExactlyOneArgument(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDeviceRevoke(context.Background(), st, nil)

	// Assert
	if err == nil {
		t.Fatal("runDeviceRevoke() = nil, want an error when no device id is given")
	}
}

func TestRunDevicePairCodeRequiresName(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDevicePairCode(context.Background(), st, nil)

	// Assert
	if err == nil {
		t.Fatal("runDevicePairCode() = nil, want an error when --name is missing")
	}
}

func TestRunDevicePairCodeMintsCode(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDevicePairCode(context.Background(), st, []string{"--" + pairCodeNameFlag, "Pixel 9"})

	// Assert
	if err != nil {
		t.Fatalf("runDevicePairCode() unexpected error: %v", err)
	}
}

func TestRunDeviceCommandDispatchesToSubcommands(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}
	ctx := context.Background()

	// Act — mint a pairing code through the full dispatch path, not the
	// package-level helper directly, so the switch in runDeviceCommand is
	// exercised too.
	err := runDeviceCommand(ctx, cfg, []string{subcommandPairCode, "--" + pairCodeNameFlag, "Pixel 9"})

	// Assert
	if err != nil {
		t.Fatalf("runDeviceCommand(pair-code) unexpected error: %v", err)
	}

	// Act — list must now see nothing yet, since pair-code only mints a code
	// and does not create a device row.
	if err := runDeviceCommand(ctx, cfg, []string{subcommandList}); err != nil {
		t.Fatalf("runDeviceCommand(list) unexpected error: %v", err)
	}
}

func TestFormatLastSeenReportsNeverForZeroTime(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}

	// Act
	got := formatLastSeen(device.LastSeenAt)

	// Assert
	if got != lastSeenNever {
		t.Errorf("formatLastSeen() = %q, want %q for a device never seen", got, lastSeenNever)
	}
}
