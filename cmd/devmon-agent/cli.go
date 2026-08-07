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
