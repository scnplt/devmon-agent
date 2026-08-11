// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import (
	"bytes"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Host-side `device` and `audit` subcommands, run as subprocesses against the
// same DEVMON_STATE_DIR the agent under test has open (D8, D9). This is the
// documented operator path — reaching into SQLite directly would test an
// interface no operator has, and would keep passing if the CLI itself broke.

// pairingCodePrefix is the exact prefix cmd/devmon-agent/cli.go prints the
// minted code with. Parsed by prefix, never logged past the prefix check.
const pairingCodePrefix = "Pairing code: "

// tableColumnSplit matches the tabwriter's inter-column gap. The command's
// own padding is 2, so two-or-more spaces is always a column boundary and
// never part of a value — a value like "id (name)" or "LAST SEEN" contains
// only single spaces.
var tableColumnSplit = regexp.MustCompile(`\s{2,}`)

// DeviceRow is one line of `device list`, parsed from its table output.
type DeviceRow struct {
	ID         string
	Name       string
	PairedAt   string
	LastSeenAt string
	State      string
}

// AuditRow is one line of `audit list`, parsed from its table output.
type AuditRow struct {
	OccurredAt string
	Device     string
	Operation  string
	Target     string
	Outcome    string
	Detail     string
}

// MintPairingCode runs `device pair-code --name <name>` against a. The code
// must be minted while the agent is running (D8): doing it before startup
// would pass, but would stop testing the shared-state-store requirement D8
// exists for. The parsed code is never logged, including on a parse failure —
// only the number of lines seen is reported.
func MintPairingCode(t *testing.T, a *Agent, name string) string {
	t.Helper()

	out := runCLI(t, a, "device", "pair-code", "--name", name)
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, pairingCodePrefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, pairingCodePrefix))
		}
	}
	t.Fatalf("device pair-code: did not find a %q line in %d lines of output", pairingCodePrefix, len(lines))
	return ""
}

// ListDevices runs `device list` on the host against a and parses its
// tabwriter table. a's state directory must be readable by the host test
// user — a state directory written by a containerised agent (UID 65532,
// files at 0600) is not; use ListDevicesInContainer for that case.
func ListDevices(t *testing.T, a *Agent) []DeviceRow {
	t.Helper()
	return parseDeviceRows(t, runCLI(t, a, "device", "list"))
}

// RevokeDevice runs `device revoke <id>` against a.
func RevokeDevice(t *testing.T, a *Agent, id string) {
	t.Helper()
	runCLI(t, a, "device", "revoke", id)
}

// ListAudit runs `audit list --limit <limit>` on the host against a and
// parses its tabwriter table. Same host-readability caveat as ListDevices;
// use ListAuditInContainer for a containerised agent's state directory.
// Columns are split on runs of two-or-more spaces, not single spaces:
// DETAIL can be empty, and DEVICE contains "id (name)" with a single space
// inside it.
func ListAudit(t *testing.T, a *Agent, limit int) []AuditRow {
	t.Helper()
	return parseAuditRows(t, runCLI(t, a, "audit", "list", "--limit", strconv.Itoa(limit)))
}

// parseDeviceRows parses `device list`'s tabwriter table into DeviceRow
// values. Shared by the host-side (ListDevices) and in-container
// (ListDevicesInContainer) call sites so their assertions never drift apart.
func parseDeviceRows(t *testing.T, out string) []DeviceRow {
	t.Helper()

	rows := parseTable(t, out, 5)
	result := make([]DeviceRow, 0, len(rows))
	for _, f := range rows {
		result = append(result, DeviceRow{
			ID:         f[0],
			Name:       f[1],
			PairedAt:   f[2],
			LastSeenAt: f[3],
			State:      f[4],
		})
	}
	return result
}

// parseAuditRows parses `audit list`'s tabwriter table into AuditRow values.
// Shared by the host-side (ListAudit) and in-container (ListAuditInContainer)
// call sites so their assertions never drift apart.
func parseAuditRows(t *testing.T, out string) []AuditRow {
	t.Helper()

	rows := parseTable(t, out, 6)
	result := make([]AuditRow, 0, len(rows))
	for _, f := range rows {
		result = append(result, AuditRow{
			OccurredAt: f[0],
			Device:     f[1],
			Operation:  f[2],
			Target:     f[3],
			Outcome:    f[4],
			Detail:     f[5],
		})
	}
	return result
}

// parseTable splits a tabwriter table's stdout into rows of exactly
// wantColumns fields, skipping the header line. A DETAIL (or similarly
// trailing) column that is empty still yields the right column count because
// the header row always has one more gap than the shortest possible row —
// callers that need to tolerate a genuinely blank last column pad it
// themselves before calling this.
func parseTable(t *testing.T, out string, wantColumns int) [][]string {
	t.Helper()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(lines)-1)
	for _, line := range lines[1:] { // skip the header
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := tableColumnSplit.Split(line, wantColumns)
		if len(fields) != wantColumns {
			t.Fatalf("parse table row %q: got %d columns, want %d", line, len(fields), wantColumns)
		}
		for i, f := range fields {
			fields[i] = strings.TrimSpace(f)
		}
		rows = append(rows, fields)
	}
	return rows
}

// runCLI builds (once per package) and runs one `devmon-agent <args...>`
// subcommand invocation against the given agent's state directory, with an
// explicitly built environment (D12 applies to CLI subprocesses too). It
// fails the test on a non-zero exit and returns combined stdout only — the
// production CLI never writes a pairing code or key material to stderr, but
// callers of this helper must still never log its return value past the
// specific prefix they are looking for.
func runCLI(t *testing.T, a *Agent, args ...string) string {
	t.Helper()

	bin := BuildBinary(t)
	env := buildAgentEnv(map[string]string{
		"DEVMON_STATE_DIR":   a.StateDir,
		"DEVMON_PUBLIC_ADDR": "127.0.0.1", // required by config.Load; unused by the device/audit dispatch
	})

	// #nosec G204 -- bin is the path this package built via buildBinary; args
	// are fixed subcommand literals supplied by this package's own callers,
	// never derived from external input.
	cmd := exec.Command(bin, args...)
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("devmon-agent %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}
