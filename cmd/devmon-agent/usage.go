// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// devmonAliasName is the symlink name the Dockerfile installs alongside the
// real binary (`ln -s devmon-agent devmon`), so an operator can run
// `docker exec devmon-agent devmon` instead of the full binary path.
const devmonAliasName = "devmon"

// rootUsage is shown for `devmon` with no args, `devmon help`,
// `-h`/`-help`/`--help`, and any invocation under the `devmon` alias name. It
// must stay verbatim in sync with the plan's help screen — operators run this
// via `docker exec` and it is the only discovery mechanism the image offers.
const rootUsage = `DevMon agent — mTLS-authenticated Docker control agent.

Usage:
  devmon <command> [flags]   operator CLI (docker exec devmon-agent devmon ...)
  devmon-agent               run the agent daemon (container entrypoint)

Commands:
  device list                     list paired devices
  device revoke <id>              revoke a device's access
  device pair-code --name <name>  mint a single-use pairing code
  audit list [--limit N]          print recent audit entries (default 100)
  health                          probe the running agent's listener
  help                            show this help

Flags:
  --version   print version information and exit

Run 'devmon <command> --help' for details on a command.
`

// deviceUsage is shown for `device --help` and for `--help` on any of its
// three subcommands — a single screen instead of six near-identical ones,
// since `--help` is detected before the subcommand itself is dispatched.
const deviceUsage = `Usage: devmon device <subcommand> [flags]

Subcommands:
  list                     list paired devices
  revoke <id>              revoke a device's access
  pair-code --name <name>  mint a single-use pairing code
`

// auditUsage is shown for `audit --help`.
const auditUsage = `Usage: devmon audit list [--limit N]

--limit N   maximum number of audit rows to print, most recent first (default 100)
`

// healthUsage is shown for `health --help`. health takes no arguments — it
// backs the Dockerfile's HEALTHCHECK, which invokes it on a fixed 30-second
// interval for the life of the container.
const healthUsage = `Usage: devmon health

Probes the running agent's own listener and exits 0 if healthy, 1 otherwise.
Takes no arguments. This is what the image's HEALTHCHECK runs.
`

// printRootUsage writes rootUsage to w. A write failure here is not
// actionable — w is stdout or stderr — so it is intentionally ignored, as
// the other usage printers below also do.
func printRootUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, rootUsage)
}

// printDeviceUsage writes deviceUsage to w.
func printDeviceUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, deviceUsage)
}

// printAuditUsage writes auditUsage to w.
func printAuditUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, auditUsage)
}

// printHealthUsage writes healthUsage to w.
func printHealthUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, healthUsage)
}

// isDevmonAlias reports whether argv0 — typically os.Args[0] — is an
// invocation of the `devmon` symlink the Dockerfile installs next to the real
// binary, rather than devmon-agent itself. A no-args run under that name
// shows help instead of starting the daemon, since `devmon` (unlike
// `devmon-agent`) is never the container's ENTRYPOINT.
func isDevmonAlias(argv0 string) bool {
	base := strings.ToLower(filepath.Base(argv0))
	base = strings.TrimSuffix(base, ".exe")
	return base == devmonAliasName
}

// helpRequested reports whether any of args asks for help. It scans every
// remaining argument rather than only args[0], so `device revoke --help`
// shows the device usage screen instead of treating "--help" as a device id
// — "--help" was never a valid id.
func helpRequested(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}
