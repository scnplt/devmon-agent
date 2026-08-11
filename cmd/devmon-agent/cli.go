// SPDX-License-Identifier: AGPL-3.0-only

// Host-side `device` subcommands (D8 in the Phase 2 plan). The image is
// distroless/static:nonroot — there is no shell and no second binary — so
// `docker exec devmon-agent /usr/local/bin/devmon-agent device ...` is the
// only shape that works for an operator managing paired devices.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/state"
)

// Subcommand names, declared as constants so a typo is a compile error.
const (
	subcommandList     = "list"
	subcommandRevoke   = "revoke"
	subcommandPairCode = "pair-code"
)

// pairCodeNameFlag is the --name flag on `device pair-code`.
const pairCodeNameFlag = "name"

// Device states shown in `device list`.
const (
	deviceStateActive  = "active"
	deviceStateRevoked = "revoked"
)

// lastSeenNever is printed for a device whose LastSeenAt is still zero — it
// has never made an authenticated request since it was paired.
const lastSeenNever = "never"

// auditLimitFlag is the --limit flag on `audit list`.
const auditLimitFlag = "limit"

// defaultAuditLimit caps `audit list` when --limit is not given, so a large
// audit table does not scroll an operator's terminal off by default.
const defaultAuditLimit = 100

// Tabwriter layout for `device list`.
const (
	tabwriterMinWidth = 0
	tabwriterPadding  = 2
)

// deviceTimeFormat is used for every timestamp column in `device list` and
// for the pairing code expiry printed by `device pair-code`.
const deviceTimeFormat = time.RFC3339

// runDeviceCommand dispatches a `device <subcommand>` invocation. args holds
// everything after "device" on the command line — the caller (main's run) is
// expected to pass flag.Args()[1:] once flag.Arg(0) == "device" has been
// confirmed.
//
// This opens the SAME SQLite file the running agent has open — intentional,
// and exactly what WAL plus the DSN's _busy_timeout=5000 and _txlock=immediate
// (internal/state/store.go) were configured for. It deliberately never touches
// certs/: a second process creating or loading the CA here could race the
// agent's own startup.
func runDeviceCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("device: missing subcommand (want one of: %s, %s, %s)",
			subcommandList, subcommandRevoke, subcommandPairCode)
	}

	st, err := openDeviceStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	switch args[0] {
	case subcommandList:
		return runDeviceList(ctx, st)
	case subcommandRevoke:
		return runDeviceRevoke(ctx, st, args[1:])
	case subcommandPairCode:
		return runDevicePairCode(ctx, st, args[1:])
	default:
		return fmt.Errorf("device: unknown subcommand %q (want one of: %s, %s, %s)",
			args[0], subcommandList, subcommandRevoke, subcommandPairCode)
	}
}

// openDeviceStore opens the state store without constructing a logging.Sink.
// The CLI must never build a log sink: a pairing code printed to a persisted,
// rotated log file would turn a one-time credential into a durable one. A
// discard-backed logger satisfies state.Open's signature without writing
// anywhere.
func openDeviceStore(ctx context.Context, cfg config.Config) (*state.Store, error) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := state.Open(ctx, cfg.DBPath(), log)
	if err != nil {
		return nil, fmt.Errorf("open state store for device command: %w", err)
	}
	return st, nil
}

// runDeviceList prints every registered device as a table: ID, NAME, PAIRED,
// LAST SEEN, STATE.
func runDeviceList(ctx context.Context, st *state.Store) error {
	devices, err := st.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}

	tw := tabwriter.NewWriter(os.Stdout, tabwriterMinWidth, tabwriterPadding, tabwriterPadding, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tPAIRED\tLAST SEEN\tSTATE"); err != nil {
		return fmt.Errorf("write device list header: %w", err)
	}
	for _, d := range devices {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.Name, d.PairedAt.Format(deviceTimeFormat), formatLastSeen(d.LastSeenAt), deviceStateOf(d)); err != nil {
			return fmt.Errorf("write device list row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write device list: %w", err)
	}
	return nil
}

// formatLastSeen renders a zero LastSeenAt honestly instead of printing the
// zero time value.
func formatLastSeen(t time.Time) string {
	if t.IsZero() {
		return lastSeenNever
	}
	return t.Format(deviceTimeFormat)
}

func deviceStateOf(d state.Device) string {
	if d.IsRevoked() {
		return deviceStateRevoked
	}
	return deviceStateActive
}

// runDeviceRevoke withdraws a device's access. It looks the device up first
// so the confirmation message can name it — state.RevokeDevice itself returns
// no name, only an error.
func runDeviceRevoke(ctx context.Context, st *state.Store, args []string) error {
	fs := flag.NewFlagSet(subcommandRevoke, flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("device revoke: %w", err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("device revoke: expected exactly one device id argument, got %d", fs.NArg())
	}
	id := fs.Arg(0)

	devices, err := st.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices before revoke: %w", err)
	}
	name := deviceNameByID(devices, id)

	if err := st.RevokeDevice(ctx, id); err != nil {
		return fmt.Errorf("revoke device %s: %w", id, err)
	}

	fmt.Printf("revoked %s (%s)\n", id, name)
	return nil
}

func deviceNameByID(devices []state.Device, id string) string {
	for _, d := range devices {
		if d.ID == id {
			return d.Name
		}
	}
	return ""
}

// runDevicePairCode mints a single-use pairing code and prints it to stdout
// ONLY. It must never reach the logger — the log file is persisted and
// rotated, so a pairing code in agent.log would be a durable credential.
func runDevicePairCode(ctx context.Context, st *state.Store, args []string) error {
	fs := flag.NewFlagSet(subcommandPairCode, flag.ContinueOnError)
	name := fs.String(pairCodeNameFlag, "", "name of the device to mint a pairing code for")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("device pair-code: %w", err)
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("device pair-code: --%s is required", pairCodeNameFlag)
	}

	code, expiresAt, err := st.MintPairingCode(ctx, *name)
	if err != nil {
		return fmt.Errorf("mint pairing code: %w", err)
	}

	fmt.Printf("Pairing code: %s\n", code)
	fmt.Printf("Expires:      %s\n", expiresAt.Format(deviceTimeFormat))
	return nil
}

// runAuditCommand dispatches an `audit <subcommand>` invocation, mirroring
// runDeviceCommand line for line. args holds everything after "audit" on the
// command line. `list` is the only subcommand — the audit log is deliberately
// not reachable over the HTTPS API (D20), so this CLI is its only reader.
//
// It opens the SAME SQLite file the running agent has open, exactly as
// runDeviceCommand does, and for the same reasons (see that function's
// comment).
func runAuditCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("audit: missing subcommand (want one of: %s)", subcommandList)
	}

	st, err := openDeviceStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	switch args[0] {
	case subcommandList:
		return runAuditListCommand(ctx, st, args[1:])
	default:
		return fmt.Errorf("audit: unknown subcommand %q (want one of: %s)", args[0], subcommandList)
	}
}

// runAuditListCommand parses the `audit list` flags and prints the result to
// stdout.
func runAuditListCommand(ctx context.Context, st *state.Store, args []string) error {
	fs := flag.NewFlagSet(subcommandList, flag.ContinueOnError)
	limit := fs.Int(auditLimitFlag, defaultAuditLimit, "maximum number of audit rows to print, most recent first")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("audit list: %w", err)
	}
	return runAuditList(ctx, st, os.Stdout, *limit)
}

// runAuditList prints the most recent audit rows as a table: WHEN, DEVICE,
// OPERATION, TARGET, OUTCOME, DETAIL. It takes an io.Writer, unlike
// runDeviceList's direct os.Stdout, so formatting can be asserted in tests.
func runAuditList(ctx context.Context, st *state.Store, w io.Writer, limit int) error {
	entries, err := st.ListAudit(ctx, limit)
	if err != nil {
		return fmt.Errorf("list audit entries: %w", err)
	}

	// Joined here, not per-row, so a large audit table costs one ListDevices
	// call rather than one per row.
	devices, err := st.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices for audit: %w", err)
	}

	tw := tabwriter.NewWriter(w, tabwriterMinWidth, tabwriterPadding, tabwriterPadding, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WHEN\tDEVICE\tOPERATION\tTARGET\tOUTCOME\tDETAIL"); err != nil {
		return fmt.Errorf("write audit list header: %w", err)
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.OccurredAt.Format(deviceTimeFormat), auditDeviceColumn(devices, e.DeviceID), e.Operation, e.Target, e.Outcome, e.Detail); err != nil {
			return fmt.Errorf("write audit list row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write audit list: %w", err)
	}
	return nil
}

// auditDeviceColumn joins a device ID to its current name, mirroring
// runDeviceRevoke's lookup. A device whose row was deleted still has audit
// rows — the bare ID is printed rather than failing, because the audit trail
// outliving the device is the point.
func auditDeviceColumn(devices []state.Device, id string) string {
	name := deviceNameByID(devices, id)
	if name == "" {
		return id
	}
	return fmt.Sprintf("%s (%s)", id, name)
}
