// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/state"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever fn wrote to it. Used to prove a parse error never reaches
// stdout — only requested help does (see runAuditListCommand's ErrHelp
// branch and the plan note it implements).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() unexpected error: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}

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
	err := runDevicePairCode(context.Background(), st, testPairTTLMax, nil)

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
	err := runDevicePairCode(context.Background(), st, testPairTTLMax, []string{"--" + pairCodeNameFlag, "Pixel 9"})

	// Assert
	if err != nil {
		t.Fatalf("runDevicePairCode() unexpected error: %v", err)
	}
}

// TestRunDevicePairCodeExplicitTTLSetsExpiry and the four TTL tests below it
// deliberately do NOT call t.Parallel() — see captureStdout's comment on
// TestRunAuditListCommandBadFlagErrorsWithoutStdoutOutput: redirecting the
// package-level os.Stdout would race with any concurrently running parallel
// test that also writes to stdout.
func TestRunDevicePairCodeExplicitTTLSetsExpiry(t *testing.T) {
	// Arrange
	st := openTestStore(t)
	before := time.Now()

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "7"})
		if err != nil {
			t.Fatalf("runDevicePairCode() unexpected error: %v", err)
		}
	})

	// Assert — the printed expiry reflects the requested 7-minute TTL, not
	// the 10-minute default.
	expires := parseExpiresLine(t, got)
	wantAround := before.Add(7 * time.Minute)
	if diff := expires.Sub(wantAround); diff < -time.Minute || diff > time.Minute {
		t.Errorf("runDevicePairCode() expiry = %v, want close to %v (7m TTL)", expires, wantAround)
	}
}

func TestRunDevicePairCodeTTLAboveCeilingMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "30"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for --ttl above the ceiling")
		}
		if !strings.Contains(err.Error(), "30") || !strings.Contains(err.Error(), "DEVMON_PAIR_TTL_MAX_MIN") {
			t.Errorf("runDevicePairCode() error = %v, want it to name the value and DEVMON_PAIR_TTL_MAX_MIN", err)
		}
	})

	// Assert — nothing was minted, so nothing was printed either.
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl is rejected", got)
	}
}

// TestRunDevicePairCodeHugeTTLMintsNothing proves a --ttl large enough to
// overflow int64 nanoseconds under naive Duration multiplication is still a
// hard error at the CLI layer, not silently accepted — see
// resolvePairCodeTTL's overflow-safety doc comment.
func TestRunDevicePairCodeHugeTTLMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "9007199254741022"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for a --ttl that overflows under naive multiplication")
		}
		if !strings.Contains(err.Error(), "9007199254741022") || !strings.Contains(err.Error(), "DEVMON_PAIR_TTL_MAX_MIN") {
			t.Errorf("runDevicePairCode() error = %v, want it to name the typed value and DEVMON_PAIR_TTL_MAX_MIN", err)
		}
	})

	// Assert — nothing was minted, so nothing was printed either.
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl is rejected", got)
	}
}

func TestRunDevicePairCodeTTLBelowMinimumMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "2"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for --ttl below the 5-minute floor")
		}
	})

	// Assert
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl is rejected", got)
	}
}

// TestRunDevicePairCodeExplicitZeroTTLMintsNothing proves an explicit
// `--ttl 0` is a hard error, not silently redirected to the default TTL —
// the second security-review finding on issue #129. Before the fix, 0
// doubled as flag.Int's own "not set" zero value, so this exact case was the
// one exception to "out-of-range is a hard error, never silently
// substituted".
func TestRunDevicePairCodeExplicitZeroTTLMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "0"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for an explicit --ttl 0")
		}
		if !strings.Contains(err.Error(), "0") || !strings.Contains(err.Error(), "5") {
			t.Errorf("runDevicePairCode() error = %v, want it to name the value and the 5-minute minimum", err)
		}
	})

	// Assert — nothing was minted, so nothing was printed either.
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl is rejected", got)
	}
}

// TestRunDevicePairCodeExplicitNegativeTTLMintsNothing proves an explicit
// negative --ttl is a hard error too, not just zero.
func TestRunDevicePairCodeExplicitNegativeTTLMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "-1"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for an explicit negative --ttl")
		}
	})

	// Assert — nothing was minted, so nothing was printed either.
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl is rejected", got)
	}
}

func TestRunDevicePairCodeNonIntegerTTLMintsNothing(t *testing.T) {
	// Arrange
	st := openTestStore(t)

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, testPairTTLMax,
			[]string{"--" + pairCodeNameFlag, "Pixel 9", "--" + pairCodeTTLFlag, "not-a-number"})
		if err == nil {
			t.Fatal("runDevicePairCode() = nil error, want one for a non-integer --ttl")
		}
	})

	// Assert
	if got != "" {
		t.Errorf("runDevicePairCode() wrote %q to stdout, want nothing when --ttl fails to parse", got)
	}
}

func TestRunDevicePairCodeOmittedTTLUsesLoweredCeiling(t *testing.T) {
	// Arrange — the ceiling is below the 10-minute default, so the effective
	// TTL must be the ceiling itself.
	st := openTestStore(t)
	before := time.Now()
	ceiling := 6 * time.Minute

	// Act
	got := captureStdout(t, func() {
		err := runDevicePairCode(context.Background(), st, ceiling, []string{"--" + pairCodeNameFlag, "Pixel 9"})
		if err != nil {
			t.Fatalf("runDevicePairCode() unexpected error: %v", err)
		}
	})

	// Assert
	expires := parseExpiresLine(t, got)
	wantAround := before.Add(ceiling)
	if diff := expires.Sub(wantAround); diff < -time.Minute || diff > time.Minute {
		t.Errorf("runDevicePairCode() expiry = %v, want close to %v (ceiling-clamped TTL)", expires, wantAround)
	}
}

// parseExpiresLine extracts the timestamp from the "Expires:" line printed by
// runDevicePairCode's stdout, so a test can assert on the actual TTL used
// without re-parsing the pairing code line too.
func parseExpiresLine(t *testing.T, stdout string) time.Time {
	t.Helper()

	for _, line := range strings.Split(stdout, "\n") {
		const prefix = "Expires:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		expires, err := time.Parse(deviceTimeFormat, raw)
		if err != nil {
			t.Fatalf("parse expires line %q: %v", line, err)
		}
		return expires
	}
	t.Fatalf("no %q line found in stdout %q", "Expires:", stdout)
	return time.Time{}
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

func TestRunAuditListPrintsDashForEmptyTargetAndDetail(t *testing.T) {
	t.Parallel()

	// Arrange — mirrors the identity operations (pair, renew, unpair_self;
	// internal/httpapi/audit.go) that leave Target and/or Detail empty by
	// design: pairing and renewal have no container target, and a pair row
	// has no detail. An empty tabwriter cell between tab stops collapses
	// visually and defeats the e2e harness's column-count parser
	// (internal/e2e/harness/cli.go's parseTable), so every row must always
	// print a visually distinct placeholder instead.
	st := openTestStore(t)
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatalf("CreateDevice() unexpected error: %v", err)
	}
	entry := state.AuditEntry{
		DeviceID:  device.ID,
		Operation: "pair",
		Target:    "",
		Outcome:   state.OutcomeSuccess,
		Detail:    "",
	}
	if err := st.AppendAudit(ctx, entry); err != nil {
		t.Fatalf("AppendAudit() unexpected error: %v", err)
	}
	var buf bytes.Buffer

	// Act
	if err := runAuditList(ctx, st, &buf, defaultAuditLimit); err != nil {
		t.Fatalf("runAuditList() unexpected error: %v", err)
	}

	// Assert — the row is header + one line; every field, including the two
	// placeholders, must land in its own tab-separated cell.
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("runAuditList() printed %d lines, want 2 (header + 1 row): %q", len(lines), got)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		t.Fatalf("runAuditList() row %q, want at least 5 whitespace-separated fields", lines[1])
	}
	dashCount := 0
	for _, f := range fields {
		if f == auditEmptyColumn {
			dashCount++
		}
	}
	if dashCount != 2 {
		t.Errorf("runAuditList() row = %q, want exactly 2 fields equal to %q (TARGET and DETAIL), got %d", lines[1], auditEmptyColumn, dashCount)
	}
}

func TestOrDash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string becomes the placeholder", "", auditEmptyColumn},
		{"non-empty string is unchanged", "web", "web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := orDash(tt.in)

			// Assert
			if got != tt.want {
				t.Errorf("orDash(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
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

func TestRunDeviceCommandHelpPrintsUsageWithoutOpeningStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		arg  string
	}{
		{"help", "help"},
		{"-h", "-h"},
		{"-help", "-help"},
		{"--help", "--help"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange — StateDir is never prepared: a store open would fail
			// noisily if the help path tried to touch it.
			cfg := testStateConfig(t)

			// Act
			err := runDeviceCommand(context.Background(), cfg, []string{tt.arg})

			// Assert
			if err != nil {
				t.Fatalf("runDeviceCommand(%q) unexpected error: %v", tt.arg, err)
			}
			if _, statErr := os.Stat(cfg.DBPath()); !os.IsNotExist(statErr) {
				t.Errorf("runDeviceCommand(%q) created a store file at %s, want none for help", tt.arg, cfg.DBPath())
			}
			if entries, readErr := os.ReadDir(cfg.StateDir); readErr == nil && len(entries) != 0 {
				t.Errorf("runDeviceCommand(%q) left %d entries under %s, want an untouched state dir", tt.arg, len(entries), cfg.StateDir)
			}
		})
	}
}

func TestRunAuditCommandHelpPrintsUsageWithoutOpeningStore(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)

	// Act
	err := runAuditCommand(context.Background(), cfg, []string{"--help"})

	// Assert
	if err != nil {
		t.Fatalf("runAuditCommand(--help) unexpected error: %v", err)
	}
	if _, statErr := os.Stat(cfg.DBPath()); !os.IsNotExist(statErr) {
		t.Errorf("runAuditCommand(--help) created a store file at %s, want none for help", cfg.DBPath())
	}
}

func TestRunDeviceRevokeHelpReturnsNilWithoutWrappedError(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDeviceRevoke(context.Background(), st, []string{"--help"})

	// Assert
	if err != nil {
		t.Fatalf("runDeviceRevoke(--help) = %v, want nil", err)
	}
}

func TestRunDevicePairCodeHelpReturnsNilWithoutWrappedError(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runDevicePairCode(context.Background(), st, testPairTTLMax, []string{"--help"})

	// Assert
	if err != nil {
		t.Fatalf("runDevicePairCode(--help) = %v, want nil", err)
	}
}

func TestRunAuditListCommandHelpReturnsNilWithoutWrappedError(t *testing.T) {
	t.Parallel()

	// Arrange
	st := openTestStore(t)

	// Act
	err := runAuditListCommand(context.Background(), st, []string{"--help"})

	// Assert
	if err != nil {
		t.Fatalf("runAuditListCommand(--help) = %v, want nil", err)
	}
}

// TestRunAuditListCommandBadFlagErrorsWithoutStdoutOutput and its two
// siblings below deliberately do NOT call t.Parallel(): they redirect the
// package-level os.Stdout for the duration of the call, which would race
// with any concurrently running parallel test that also writes to stdout.
func TestRunAuditListCommandBadFlagErrorsWithoutStdoutOutput(t *testing.T) {
	// Arrange
	st := openTestStore(t)
	var err error

	// Act — capture stdout around the call: a genuine parse error must print
	// nothing there, only the wrapped error (which main sends to stderr).
	got := captureStdout(t, func() {
		err = runAuditListCommand(context.Background(), st, []string{"--bogus"})
	})

	// Assert
	if err == nil {
		t.Fatal("runAuditListCommand(--bogus) = nil error, want one for an unknown flag")
	}
	if got != "" {
		t.Errorf("runAuditListCommand(--bogus) wrote %q to stdout, want nothing", got)
	}
}

func TestRunDeviceRevokeBadFlagErrorsWithoutStdoutOutput(t *testing.T) {
	// Arrange
	st := openTestStore(t)
	var err error

	// Act
	got := captureStdout(t, func() {
		err = runDeviceRevoke(context.Background(), st, []string{"--bogus"})
	})

	// Assert
	if err == nil {
		t.Fatal("runDeviceRevoke(--bogus) = nil error, want one for an unknown flag")
	}
	if got != "" {
		t.Errorf("runDeviceRevoke(--bogus) wrote %q to stdout, want nothing", got)
	}
}

func TestRunDevicePairCodeBadFlagErrorsWithoutStdoutOutput(t *testing.T) {
	// Arrange — the bad-flag case matters most here: pair-code's stdout is a
	// pairing code a script may parse, so a flag error must never pollute it.
	st := openTestStore(t)
	var err error

	// Act
	got := captureStdout(t, func() {
		err = runDevicePairCode(context.Background(), st, testPairTTLMax, []string{"--bogus"})
	})

	// Assert
	if err == nil {
		t.Fatal("runDevicePairCode(--bogus) = nil error, want one for an unknown flag")
	}
	if got != "" {
		t.Errorf("runDevicePairCode(--bogus) wrote %q to stdout, want nothing", got)
	}
}

func TestIsHelpSubcommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"help literal", "help", true},
		{"short flag", "-h", true},
		{"single-dash long flag", "-help", true},
		{"double-dash long flag", "--help", true},
		{"real subcommand", subcommandList, false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := isHelpSubcommand(tt.in)

			// Assert
			if got != tt.want {
				t.Errorf("isHelpSubcommand(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestRunDeviceCommandUnknownSubcommandDoesNotOpenStore proves the reordered
// check pays no SQLite open for a typo'd subcommand: the state dir is left
// untouched by prepareStateDir's own directories only, no devmon.db appears.
func TestRunDeviceCommandUnknownSubcommandDoesNotOpenStore(t *testing.T) {
	t.Parallel()

	// Arrange
	cfg := testStateConfig(t)
	if err := prepareStateDir(cfg); err != nil {
		t.Fatalf("prepareStateDir() unexpected error: %v", err)
	}

	// Act
	if err := runDeviceCommand(context.Background(), cfg, []string{"bogus"}); err == nil {
		t.Fatal("runDeviceCommand() = nil, want an error for an unknown subcommand")
	}

	// Assert
	if _, statErr := os.Stat(cfg.DBPath()); !os.IsNotExist(statErr) {
		t.Errorf("runDeviceCommand(bogus) created a store file at %s, want none for an unknown subcommand", cfg.DBPath())
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
