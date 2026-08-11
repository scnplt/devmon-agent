// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
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

func TestRunAuditCommandMissingSubcommandReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act
	err := runAuditCommand(context.Background(), cfg, nil)

	// Assert
	if err == nil {
		t.Fatal("runAuditCommand() = nil, want an error for a missing subcommand")
	}
}

func TestRunAuditCommandUnknownSubcommandReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act
	err := runAuditCommand(context.Background(), cfg, []string{"bogus"})

	// Assert
	if err == nil {
		t.Fatal("runAuditCommand() = nil, want an error for an unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("runAuditCommand() error = %v, want it to name the unknown subcommand", err)
	}
}

func TestRunAuditCommandDispatchesToList(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act — exercised through the full dispatch path so the switch in
	// runAuditCommand is covered too, not just runAuditList directly.
	err := runAuditCommand(context.Background(), cfg, []string{subcommandList})

	// Assert
	if err != nil {
		t.Fatalf("runAuditCommand(list) unexpected error: %v", err)
	}
}

func TestRunAuditListEmptyStorePrintsHeaderOnly(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	var buf bytes.Buffer

	// Act
	if err := runAuditList(context.Background(), st, &buf, defaultAuditLimit); err != nil {
		t.Fatalf("runAuditList() unexpected error: %v", err)
	}

	// Assert
	got := buf.String()
	if !strings.Contains(got, "WHEN") || !strings.Contains(got, "DEVICE") ||
		!strings.Contains(got, "OPERATION") || !strings.Contains(got, "TARGET") ||
		!strings.Contains(got, "OUTCOME") || !strings.Contains(got, "DETAIL") {
		t.Errorf("runAuditList() output = %q, want a header with every column", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("runAuditList() output = %q, want exactly the header line for an empty store", got)
	}
}

func TestRunAuditListJoinsDeviceName(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	entry := state.AuditEntry{
		DeviceID:  device.ID,
		Operation: "restart",
		Target:    "web",
		Outcome:   state.OutcomeSuccess,
		Detail:    "abc123",
	}
	if err := st.AppendAudit(ctx, entry); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	var buf bytes.Buffer

	// Act
	if err := runAuditList(ctx, st, &buf, defaultAuditLimit); err != nil {
		t.Fatalf("runAuditList() unexpected error: %v", err)
	}

	// Assert
	got := buf.String()
	want := device.ID + " (Pixel 9)"
	if !strings.Contains(got, want) {
		t.Errorf("runAuditList() output = %q, want it to contain the joined device column %q", got, want)
	}
	if !strings.Contains(got, "restart") || !strings.Contains(got, "web") ||
		!strings.Contains(got, state.OutcomeSuccess) || !strings.Contains(got, "abc123") {
		t.Errorf("runAuditList() output = %q, missing an expected column value", got)
	}
}

func TestRunAuditListPrintsBareIDForDeletedDevice(t *testing.T) {
	t.Parallel()

	// Arrange — the device row is deleted after the audit row is written, so
	// the join has nothing to attach a name to. The audit trail must survive
	// the device it describes.
	st := openTestStore(t)
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	entry := state.AuditEntry{
		DeviceID:  device.ID,
		Operation: "delete",
		Target:    "web",
		Outcome:   state.OutcomeSuccess,
		Detail:    "abc123",
	}
	if err := st.AppendAudit(ctx, entry); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	if err := st.DeleteDevice(ctx, device.ID); err != nil {
		t.Fatalf("DeleteDevice() unexpected error: %v", err)
	}
	var buf bytes.Buffer

	// Act
	if err := runAuditList(ctx, st, &buf, defaultAuditLimit); err != nil {
		t.Fatalf("runAuditList() unexpected error: %v", err)
	}

	// Assert
	got := buf.String()
	if !strings.Contains(got, device.ID) {
		t.Errorf("runAuditList() output = %q, want the bare device id %q for a deleted device", got, device.ID)
	}
	if strings.Contains(got, "Pixel 9") {
		t.Errorf("runAuditList() output = %q, want no stale name for a deleted device", got)
	}
}

func TestRunAuditListRespectsLimit(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		entry := state.AuditEntry{
			Operation: "start",
			Target:    "web",
			Outcome:   state.OutcomeSuccess,
		}
		if err := st.AppendAudit(ctx, entry); err != nil {
			t.Fatalf("AppendAudit() unexpected error: %v", err)
		}
	}
	var buf bytes.Buffer

	// Act
	if err := runAuditList(ctx, st, &buf, 1); err != nil {
		t.Fatalf("runAuditList() unexpected error: %v", err)
	}

	// Assert — one header line plus exactly one row.
	got := buf.String()
	lines := strings.Count(got, "\n")
	if lines != 2 {
		t.Errorf("runAuditList() printed %d lines with --limit 1, want 2 (header + 1 row)", lines)
	}
}

func TestRunAuditListCommandParsesLimitFlag(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		entry := state.AuditEntry{
			Operation: "start",
			Target:    "web",
			Outcome:   state.OutcomeSuccess,
		}
		if err := st.AppendAudit(ctx, entry); err != nil {
			t.Fatalf("AppendAudit() unexpected error: %v", err)
		}
	}

	// Act
	err := runAuditListCommand(ctx, st, []string{"--" + auditLimitFlag, "1"})

	// Assert
	if err != nil {
		t.Fatalf("runAuditListCommand() unexpected error: %v", err)
	}
}

func TestAuditDeviceColumnReturnsBareIDWhenUnknown(t *testing.T) {
	t.Parallel()

	// Act
	got := auditDeviceColumn(nil, "deadbeefcafefeed")

	// Assert
	if got != "deadbeefcafefeed" {
		t.Errorf("auditDeviceColumn() = %q, want the bare id when no device matches", got)
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
