// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays the audit half of Phase 5's manual checklist: one row per
// mutating request — including the calls policy refuses — attributed to the
// calling device, readable ONLY through the documented host-side
// `devmon-agent audit list` command (D9, D20 of the source plan: the audit
// log is deliberately unreachable over the HTTPS API, so this file drives
// every assertion through harness.ListAudit, never SQLite and never a route
// this agent does not register).
// It covers specifically the "audit list shows one row per attempt..." and
// "revoke the device... audit list gains no row" checklist items.
//
// What this file deliberately does NOT cover: the exact HTTP status per
// operation is contract_lifecycle_test.go and contract_policy_test.go's job;
// this file only asks what the audit trail recorded about those same calls.
// Self-refusal rows (denied_self) need a containerised agent to produce at
// all and are internal/e2e/incontainer's TestAuditRecordsSelfRefusals
// (Task 10).
//
// Audit ROW RETENTION (internal/state/pruner.go) is out of scope for a live
// round trip in this suite: Pruner.Run ticks on a fixed, uncomfigurable
// six-hour interval (defaultPruneInterval in pruner.go) with no test seam to
// shorten it, and D19
// forbids adding one just to make this task's assertions pass sooner. The
// retention bound itself — DEVMON_AUDIT_MAX_ROWS enforced by PruneAudit — is
// already exercised at the unit level (internal/state/audit_test.go's
// TestPruneAuditRemovesRowsWrittenByAppendAudit, internal/state/store_test.go's
// TestPruneAudit, and internal/config/config_test.go's AuditMaxRows cases).
// This is recorded here, rather than silently omitted, because the task that
// found the boundary owns saying so (D19).

// auditListLimit is generous enough that every test in this file, each on its
// own freshly started agent, can assert an EXACT row count rather than
// searching a long list for its own rows.
const auditListLimit = 50

// identityAuditOperations names the operation column values withPairAudit
// and withIdentityAudit write (internal/httpapi/audit.go's opPair, opRenew,
// opUnpairSelf). Since #69, harness.PairDevice's own pairing request writes
// one of these (a "pair" row) before a test's scripted sequence ever runs,
// so a test that asserts an exact row count for its own scripted calls must
// exclude identity rows to stay meaningful regardless of how many pairing or
// renewal events its setup happens to produce.
var identityAuditOperations = map[string]bool{
	"pair":        true,
	"renew":       true,
	"unpair_self": true,
}

// nonIdentityAuditRows filters rows down to the ones NOT written by identity
// bootstrapping (see identityAuditOperations), preserving order. Used by
// every test in this file that asserts an exact count of rows its own
// scripted container-lifecycle calls produced, so the assertion does not
// silently over-count the pairing row PairDevice always writes.
func nonIdentityAuditRows(rows []harness.AuditRow) []harness.AuditRow {
	result := make([]harness.AuditRow, 0, len(rows))
	for _, row := range rows {
		if !identityAuditOperations[row.Operation] {
			result = append(result, row)
		}
	}
	return result
}

// TestAuditRowPerMutatingRequest replays a small scripted sequence — a
// restart that succeeds, a kill refused by default policy, and a delete of a
// container that does not exist — against one agent with no other audit
// traffic, then asserts `audit list` holds EXACTLY three rows, in the
// documented newest-first order (harness.ListAudit / cli.go's GOTCHA:
// ListAudit orders by id DESC), each carrying the right device, operation,
// target, and outcome.
func TestAuditRowPerMutatingRequest(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	const deviceName = "audit-scripted-sequence"
	a := harness.StartAgent(t, harness.AgentOptions{}) // default mode: kill is refused
	d := harness.PairDevice(t, a, deviceName)

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-sequence"})
	waitContainerRunning(t, engine, id)

	const unknownTarget = "devmone2enosuchcontainer00000001"

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusNoContent {
		t.Fatalf("step 1, restart: status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}

	status, _, raw = d.Do(t, http.MethodPost, "/v1/containers/"+id+"/kill", nil)
	if status != http.StatusForbidden {
		t.Fatalf("step 2, kill under default policy: status = %d, want %d; body = %s", status, http.StatusForbidden, raw)
	}

	// Step 3 has to use an operation `default` PERMITS, or it produces a
	// second denied_policy row instead of the not_found one the sequence is
	// asserting: the policy gate runs before the Engine lookup, so a delete
	// under `default` is 403 whether or not the target exists. Restart is
	// permitted under `default`, so it is what reaches the lookup.
	status, _, raw = d.Do(t, http.MethodPost, "/v1/containers/"+unknownTarget+"/restart", nil)
	if status != http.StatusNotFound {
		t.Fatalf("step 3, restart of an unknown container: status = %d, want %d; body = %s", status, http.StatusNotFound, raw)
	}

	// Excludes the pair row d's own PairDevice call writes (identityAuditOperations
	// doc comment: since #69, pairing itself is audited), so this count stays
	// exactly the three scripted lifecycle calls below.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(rows) != 3 {
		t.Fatalf("audit list holds %d non-identity rows, want exactly 3: %+v", len(rows), rows)
	}

	// Newest first: the unknown-container restart lands last and is listed first.
	wantInNewestFirstOrder := []struct {
		step      string
		operation string
		target    string
		outcome   string
	}{
		{"restart of an unknown container", "restart", unknownTarget, "not_found"},
		{"kill under default policy", "kill", id, "denied_policy"},
		{"restart", "restart", id, "success"},
	}

	for i, want := range wantInNewestFirstOrder {
		row := rows[i]
		t.Run(want.step, func(t *testing.T) {
			if !strings.Contains(row.Device, d.ID) {
				t.Errorf("row device = %q, want it to contain the device ID %q", row.Device, d.ID)
			}
			if !strings.Contains(row.Device, deviceName) {
				t.Errorf("row device = %q, want it to contain the device name %q", row.Device, deviceName)
			}
			if row.Operation != want.operation {
				t.Errorf("row operation = %q, want %q", row.Operation, want.operation)
			}
			if row.Target != want.target {
				t.Errorf("row target = %q, want %q", row.Target, want.target)
			}
			if row.Outcome != want.outcome {
				t.Errorf("row outcome = %q, want %q", row.Outcome, want.outcome)
			}
		})
	}
}

// TestAuditRecordsRefusals states one explicit requirement on its own: a
// call the host's policy refuses still leaves a row, with outcome
// denied_policy, not silence. TestAuditRowPerMutatingRequest already proves
// this as one cell of a sequence; this test isolates it so a reader searching
// for "does a refusal get logged" finds a single, minimal answer.
func TestAuditRecordsRefusals(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{}) // default mode refuses kill
	d := harness.PairDevice(t, a, "audit-refusal")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-refusal"})
	waitContainerRunning(t, engine, id)

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/kill", nil)
	if status != http.StatusForbidden {
		t.Fatalf("POST .../kill under default policy: status = %d, want %d; body = %s", status, http.StatusForbidden, raw)
	}

	// Excludes the pair row PairDevice's own call writes (identityAuditOperations
	// doc comment: since #69, pairing itself is audited), so this count stays
	// exactly the one refused request under test.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(rows) != 1 {
		t.Fatalf("audit list holds %d non-identity rows after one refused request, want exactly 1: %+v", len(rows), rows)
	}
	if rows[0].Outcome != "denied_policy" {
		t.Errorf("refused request's row outcome = %q, want %q", rows[0].Outcome, "denied_policy")
	}
	if rows[0].Operation != "kill" {
		t.Errorf("refused request's row operation = %q, want %q", rows[0].Operation, "kill")
	}
}

// TestReadsWriteNoAuditRows drives all eight read routes and both log routes
// against a freshly started agent, then asserts the audit table is still
// empty: only mutating routes carry withAudit (D17), and a suite that never
// checked
// this would not notice a future route accidentally gaining the middleware.
func TestReadsWriteNoAuditRows(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "audit-reads-no-rows")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-reads"})
	waitContainerRunning(t, engine, id)

	imageID := pulledImageID(t, engine, defaultFixtureImageRef)
	netID := createTestNetwork(t, engine, "audit-reads")
	volName := createTestVolume(t, engine, "audit-reads", nil)

	routes := []string{
		"/v1/containers",
		"/v1/containers/" + id,
		"/v1/images",
		"/v1/images/" + bareHex(imageID),
		"/v1/networks",
		"/v1/networks/" + netID,
		"/v1/volumes",
		"/v1/volumes/" + volName,
		"/v1/containers/" + id + "/logs",
	}
	for _, path := range routes {
		if status, _, raw := d.Do(t, http.MethodGet, path, nil); status != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want %d; body = %s", path, status, http.StatusOK, raw)
		}
	}

	// PairDevice's own call writes one identity "pair" row (identityAuditOperations
	// doc comment: since #69, pairing itself is audited); that row is expected
	// and excluded here, so what remains must genuinely be zero — the read and
	// log routes under test.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(rows) != 0 {
		t.Errorf("audit list holds %d non-identity rows after only read and log requests, want 0: %+v", len(rows), rows)
	}
}

// TestRevokedDeviceWritesNoAuditRow asserts a revoked device's retry answers
// 401 and leaves the audit table exactly as it was — an unattributable
// caller must not be able to add a row, and a revoked one is unattributable
// by policy even though it once held a valid certificate (withAudit sits
// inside requireDevice; a 401 from requireDevice never reaches it).
func TestRevokedDeviceWritesNoAuditRow(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: "full"})
	d := harness.PairDevice(t, a, "audit-revoked-retry")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-revoked"})
	waitContainerRunning(t, engine, id)

	// One legitimate mutating request first, so the "unchanged" assertion
	// below is checking a non-empty baseline, not vacuously "0 stays 0".
	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusNoContent {
		t.Fatalf("sanity restart before revocation: status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}
	// Excludes the pair row PairDevice's own call wrote before the sanity
	// restart (identityAuditOperations doc comment: since #69, pairing itself
	// is audited), so "before" is exactly the one legitimate mutating request.
	before := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(before) != 1 {
		t.Fatalf("audit list before revocation holds %d non-identity rows, want exactly 1: %+v", len(before), before)
	}

	harness.RevokeDevice(t, a, d.ID)

	status, _, raw = d.Do(t, http.MethodPost, "/v1/containers/"+id+"/stop", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked device POST .../stop: status = %d, want %d; body = %s", status, http.StatusUnauthorized, raw)
	}

	after := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(after) != len(before) {
		t.Errorf("audit list after a revoked device's retry holds %d non-identity rows, want unchanged from %d: %+v", len(after), len(before), after)
	}
}

// TestAuditRecordsEngineUnavailable is D16's proxy for the audit trail
// itself: the checklist item is not just "a restart with the Engine gone
// answers 502" (contract_lifecycle_test.go's
// TestLifecycleAnswers502WhenEngineIsGone already proves that over the wire)
// but that the row LEFT BEHIND records outcome engine_error, which only
// `audit list` can show.
//
// Owns its own agent and its own proxy, never shared with a t.Parallel()
// test, for the same reason every other proxy-severing test in this suite
// does: severing a proxy another test's assertions depend on would make that
// test's outcome depend on scheduling order.
func TestAuditRecordsEngineUnavailable(t *testing.T) {
	engine := harness.RequireEngine(t)

	realHost, skipReason := harness.EngineHost()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	// The proxy must be listening before the agent starts (D16's gotcha):
	// dockerx.New pings the Engine at startup, and a dead endpoint there is a
	// fatal startup error, not the 502 this test is about.
	proxy := harness.NewProxy(t, realHost)

	a := harness.StartAgent(t, harness.AgentOptions{DockerHost: proxy.Addr(), PolicyMode: "full"})
	d := harness.PairDevice(t, a, "audit-engine-gone")

	// The fixture is managed through engine, a direct connection to the real
	// Engine, never through the severable proxy: only the agent under test
	// talks to the Engine via the proxy.
	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-engine-gone"})
	waitContainerRunning(t, engine, id)

	proxy.Sever(t)

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("POST .../restart with the Engine severed: status = %d, want %d; body = %s", status, http.StatusBadGateway, raw)
	}

	// Excludes the pair row d's own PairDevice call writes (identityAuditOperations
	// doc comment: since #69, pairing itself is audited), so this count stays
	// exactly the one Engine-unavailable attempt under test.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(rows) != 1 {
		t.Fatalf("audit list after one Engine-unavailable attempt holds %d non-identity rows, want exactly 1: %+v", len(rows), rows)
	}
	if rows[0].Outcome != "engine_error" {
		t.Errorf("row outcome with the Engine severed = %q, want %q", rows[0].Outcome, "engine_error")
	}
	if rows[0].Operation != "restart" {
		t.Errorf("row operation with the Engine severed = %q, want %q", rows[0].Operation, "restart")
	}

	proxy.Restore(t)
}

// TestAuditDetailCarriesNoEngineText asserts no row's DETAIL column ever
// contains a socket path, this agent's own state directory path, or
// Engine-shaped error text ("Error response from daemon" is the Engine's own
// phrasing) — the security property behind the checklist's "no error body or
// audit detail carries an Engine message, a host path, or a state path". It
// drives one row of every outcome this file can produce without a
// containerised agent: success, denied_policy, not_found, and (via the
// severed proxy) engine_error.
func TestAuditDetailCarriesNoEngineText(t *testing.T) {
	engine := harness.RequireEngine(t)

	realHost, skipReason := harness.EngineHost()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	proxy := harness.NewProxy(t, realHost)
	a := harness.StartAgent(t, harness.AgentOptions{DockerHost: proxy.Addr(), PolicyMode: "default"})
	d := harness.PairDevice(t, a, "audit-detail-safety")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-detail"})
	waitContainerRunning(t, engine, id)

	const unknownTarget = "devmone2enosuchcontainer00000002"

	if status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil); status != http.StatusNoContent {
		t.Fatalf("restart (success case): status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}
	if status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/kill", nil); status != http.StatusForbidden {
		t.Fatalf("kill under default policy (denied_policy case): status = %d, want %d; body = %s", status, http.StatusForbidden, raw)
	}
	// The not_found row has to come from an operation the policy PERMITS: the
	// policy gate runs before the Engine is ever consulted, so under `default`
	// a delete of an unknown container is 403 denied_policy and never reaches
	// the lookup that would make it 404. Restart is permitted under `default`
	// (contract_policy_test.go's matrix), so it is the route that can actually
	// produce not_found without moving this agent to `full` mode — which would
	// in turn destroy the denied_policy row the kill above depends on.
	if status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+unknownTarget+"/restart", nil); status != http.StatusNotFound {
		t.Fatalf("restart of an unknown container (not_found case): status = %d, want %d; body = %s", status, http.StatusNotFound, raw)
	}

	proxy.Sever(t)
	if status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/stop", nil); status != http.StatusBadGateway {
		t.Fatalf("stop with the Engine severed (engine_error case): status = %d, want %d; body = %s", status, http.StatusBadGateway, raw)
	}
	proxy.Restore(t)

	// Excludes the pair row d's own PairDevice call writes (identityAuditOperations
	// doc comment: since #69, pairing itself is audited), so this count stays
	// exactly the four scripted outcomes below.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	if len(rows) != 4 {
		t.Fatalf("audit list holds %d non-identity rows after the four scripted outcomes, want exactly 4: %+v", len(rows), rows)
	}

	forbidden := []string{
		a.StateDir,
		"docker.sock",
		"Error response from daemon",
		"unix://",
	}
	for _, row := range rows {
		for _, needle := range forbidden {
			if strings.Contains(row.Detail, needle) {
				t.Errorf("row (operation=%s, outcome=%s) detail %q contains forbidden text %q", row.Operation, row.Outcome, row.Detail, needle)
			}
		}
	}
}

// TestAuditSurvivesAgentRestart asserts rows written before a restart are
// still listed after it, on the same state directory: the audit table is
// part of the persisted state store, not an in-memory buffer.
func TestAuditSurvivesAgentRestart(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	stateDir := t.TempDir()
	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	d1 := harness.PairDevice(t, a1, "audit-restart-survival")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "audit-restart"})
	waitContainerRunning(t, engine, id)

	if status, _, raw := d1.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil); status != http.StatusNoContent {
		t.Fatalf("restart before agent restart: status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}

	// Excludes the pair row d1's own PairDevice call wrote (identityAuditOperations
	// doc comment: since #69, pairing itself is audited), so "before" is
	// exactly the one restart under test.
	before := nonIdentityAuditRows(harness.ListAudit(t, a1, auditListLimit))
	if len(before) != 1 {
		t.Fatalf("audit list before the agent restart holds %d non-identity rows, want exactly 1: %+v", len(before), before)
	}

	a1.Stop(t)
	a2 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})

	after := nonIdentityAuditRows(harness.ListAudit(t, a2, auditListLimit))
	if len(after) != len(before) {
		t.Fatalf("audit list after the agent restart holds %d non-identity rows, want unchanged from %d: %+v", len(after), len(before), after)
	}
	if after[0] != before[0] {
		t.Errorf("audit row after restart = %+v, want unchanged from %+v", after[0], before[0])
	}
}

// TestAuditIsNotReachableOverTheAPI asserts D20 is not just a design decision
// but an enforced one: GET /v1/audit and a few plausible near-misses answer
// the same bare 404 an unregistered path gets from net/http.ServeMux, with no
// special "audit route" carve-out to regress into existing later.
func TestAuditIsNotReachableOverTheAPI(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "audit-not-on-api")

	probes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/audit"},
		{http.MethodGet, "/v1/audit/list"},
		{http.MethodGet, "/v1/audit-log"},
		{http.MethodPost, "/v1/audit"},
		{http.MethodDelete, "/v1/audit"},
	}
	for _, p := range probes {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			t.Parallel()
			status, _, raw := d.Do(t, p.method, p.path, nil)
			if status != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want %d; body = %s", p.method, p.path, status, http.StatusNotFound, raw)
			}
		})
	}
}
