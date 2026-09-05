// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file exercises DEVMON_PROTECTED_CONTAINERS: an optional, startup-only
// list of container names or IDs that every lifecycle route refuses in every
// policy mode, independent of the agent's own self-exclusion (which needs a
// containerised agent and lives in internal/e2e/incontainer). It replays the
// plan's checklist for the new knob: refusal by every reference form the
// operator could have listed (name, short ID, full ID), a second,
// deliberately unprotected fixture proving the refusal is target-specific and
// not a blanket denial, the "protected": true projection in both list and
// inspect, reads and logs staying unaffected, and the audit trail recording
// outcome denied_protected with the target exactly as the client supplied it.
//
// What this file deliberately does NOT cover: a self-exclusion-style
// "TestProtectedContainerDoesNotWeakenSelfRule" is not meaningful here — this
// package's agent is a host binary with no container of its own, so it has no
// self rule for a protected-list entry to interact with; that interaction
// (self wins when a container is listed as both self and protected) is a
// dockerx-level unit test (internal/dockerx/lifecycle_test.go's
// TestLifecycleSelfWinsOverProtected), not an e2e concern. Likewise, one
// agent protecting a SIBLING agent's own container needs two containerised
// agents and is explicitly out of scope for this phase (plan's NOT building
// list).

// protectedContainerBody is the exact error body internal/httpapi/reads.go
// serves for dockerx.ErrProtectedContainer. Declared locally rather than
// imported (this suite's own convention, mirrored from
// internal/e2e/incontainer's selfExclusionBody): a wording change must be
// something this suite notices, which it cannot do if it shares the constant
// with the server that produces it.
const protectedContainerBody = "container is protected by host configuration"

// outcomeDeniedProtected is the audit outcome internal/httpapi/reads.go
// records for a protected-container refusal (internal/state/audit.go's
// OutcomeDeniedProtected constant). Declared locally for the same reason as
// protectedContainerBody.
const outcomeDeniedProtected = "denied_protected"

// protectedRefFormOrder fixes the iteration order over the three reference
// forms so subtest names and the audit-order assertion below stay stable
// across runs.
var protectedRefFormOrder = []string{"name", "short", "full"}

// refForms returns id's three reference forms: its container name with
// Docker's leading "/" stripped (dockerx.ValidateRef's pattern rejects a
// leading slash, so the raw InspectResponse.Name would fail validation before
// the protected-container check ever runs), its 12-character short ID, and
// its 64-character full ID. Built from ContainerInspect, never from `docker
// ps` output, mirroring internal/e2e/incontainer/contract_selfexclusion_test.go's
// helper of the same name in the sibling package.
func refForms(t *testing.T, e *client.Client, id string) map[string]string {
	t.Helper()

	if len(id) != 64 {
		t.Fatalf("container ID %q is not a 64-character full ID", id)
	}
	res, err := e.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect container %s: %v", id, err)
	}
	name := strings.TrimPrefix(res.Container.Name, "/")

	return map[string]string{
		"name":  name,
		"short": id[:12],
		"full":  id,
	}
}

// protectedLifecycleRoute is one of the five mutating routes under test.
type protectedLifecycleRoute struct {
	operation string
	method    string
	path      func(ref string) string
}

var protectedLifecycleRoutes = []protectedLifecycleRoute{
	{"start", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/start" }},
	{"restart", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/restart" }},
	{"stop", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/stop" }},
	{"kill", http.MethodPost, func(ref string) string { return "/v1/containers/" + ref + "/kill" }},
	{"delete", http.MethodDelete, func(ref string) string { return "/v1/containers/" + ref }},
}

// protectedCall records one refused attempt's operation and the exact
// reference string the test sent, so the audit assertion below can check the
// trail against what was actually requested rather than a guess.
type protectedCall struct {
	operation string
	target    string
}

// TestProtectedContainerRefusedByEveryForm is the headline metric for this
// knob: a full-mode agent — the most permissive policy tier there is —
// refuses all five lifecycle routes against a container named in
// DEVMON_PROTECTED_CONTAINERS, by every one of the three reference forms a
// client could plausibly send. It then proves the refusal is specific to that
// target (a second, unprotected fixture created AFTER the agent started still
// answers normally), that both read projections mark the protected row, that
// reads and logs are unaffected, that the audit trail records
// denied_protected with the target exactly as supplied, and that the Engine
// itself (not the agent) still shows the protected container running,
// untouched by any of the fifteen refused attempts.
//
// The environment is fixed at agent start (config is read once, D5), so the
// fixture must exist and its name must be known BEFORE StartAgent runs.
func TestProtectedContainerRefusedByEveryForm(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	protectedID := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "protected"})
	waitContainerRunning(t, engine, protectedID)
	protectedRefs := refForms(t, engine, protectedID)

	a := harness.StartAgent(t, harness.AgentOptions{
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_PROTECTED_CONTAINERS": protectedRefs["name"]},
	})
	d := harness.PairDevice(t, a, "protected-refused-every-form")

	// Fifteen cells, all the same answer: none may reach the Engine, so the
	// container's running state never changes regardless of which route or
	// reference form is tried.
	var calls []protectedCall
	for _, route := range protectedLifecycleRoutes {
		for _, form := range protectedRefFormOrder {
			ref := protectedRefs[form]
			calls = append(calls, protectedCall{operation: route.operation, target: ref})

			t.Run(route.operation+"/"+form, func(t *testing.T) {
				status, obj := d.JSON(t, route.method, route.path(ref))
				if status != http.StatusForbidden {
					t.Fatalf("%s %s (protected, by %s): status = %d, want %d; body = %v",
						route.method, route.path(ref), form, status, http.StatusForbidden, obj)
				}
				if obj["error"] != protectedContainerBody {
					t.Errorf("%s %s (protected, by %s): error = %v, want %q",
						route.method, route.path(ref), form, obj["error"], protectedContainerBody)
				}
			})
		}
	}

	// Falsifiability: a second fixture, created only now — after the agent's
	// environment (and therefore its protected list) is already fixed — is
	// NOT a member of that list and must behave exactly like any other
	// fixture. If every mutating request against this agent failed
	// regardless of target, the fifteen 403s above would prove nothing about
	// the protected list specifically.
	unprotectedID := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "unprotected"})
	waitContainerRunning(t, engine, unprotectedID)

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+unprotectedID+"/restart", nil)
	if status != http.StatusNoContent {
		t.Fatalf("POST .../restart on an unprotected fixture created after the agent started: status = %d, want %d; body = %s",
			status, http.StatusNoContent, raw)
	}
	assertNoBody(t, raw)

	// Both read projections mark the protected row, and only the protected
	// row.
	items := readEnvelope(t, d, "/v1/containers?all=true")
	protectedRow := findByKey(t, items, "id", protectedID)
	if protectedRow["protected"] != true {
		t.Errorf("protected fixture list row: protected = %v, want true", protectedRow["protected"])
	}
	unprotectedRow := findByKey(t, items, "id", unprotectedID)
	if unprotectedRow["protected"] != false {
		t.Errorf("unprotected fixture list row: protected = %v, want false", unprotectedRow["protected"])
	}

	status, detail := d.JSON(t, http.MethodGet, "/v1/containers/"+protectedID)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers/%s: status = %d, want %d", protectedID, status, http.StatusOK)
	}
	if detail["protected"] != true {
		t.Errorf("protected fixture inspect: protected = %v, want true", detail["protected"])
	}

	// Reads and logs are unaffected: the protected list narrows mutation
	// only, mirroring self-exclusion's own scope.
	status, _, raw = d.Do(t, http.MethodGet, "/v1/containers/"+protectedID+"/logs", nil)
	if status != http.StatusOK {
		t.Errorf("GET .../logs on the protected fixture: status = %d, want %d; body = %s", status, http.StatusOK, raw)
	}

	// The audit trail: exactly the fifteen refused calls carry outcome
	// denied_protected, each with the target recorded exactly as this test
	// supplied it. Identity rows (the pair request PairDevice itself makes)
	// are excluded, mirroring contract_audit_test.go's own convention.
	rows := nonIdentityAuditRows(harness.ListAudit(t, a, auditListLimit))
	var deniedRows []harness.AuditRow
	for _, row := range rows {
		if row.Outcome == outcomeDeniedProtected {
			deniedRows = append(deniedRows, row)
		}
	}
	if len(deniedRows) != len(calls) {
		t.Fatalf("audit list holds %d denied_protected rows, want exactly %d: %+v", len(deniedRows), len(calls), deniedRows)
	}
	// ListAudit orders newest first (harness/cli.go's documented GOTCHA), and
	// calls were made oldest first, so the comparison walks calls in reverse.
	for i, row := range deniedRows {
		want := calls[len(calls)-1-i]
		if row.Operation != want.operation {
			t.Errorf("denied_protected row %d: operation = %q, want %q", i, row.Operation, want.operation)
		}
		if row.Target != want.target {
			t.Errorf("denied_protected row %d: target = %q, want %q", i, row.Target, want.target)
		}
	}

	// The metric itself, asked of the ENGINE, never the agent: still running,
	// untouched by any of the fifteen refused attempts.
	waitContainerRunning(t, engine, protectedID)
}

// TestProtectedContainerByShortIDEnv asserts an operator may list a
// container's 12-character short ID instead of its name, and a request that
// addresses the SAME container by NAME is still refused — the matching rule
// resolves the request's target to an ID before comparing (dockerx/protected.go),
// so an entry recorded in the environment as an ID form still protects every
// reference form of that container, not just the one the operator typed.
func TestProtectedContainerByShortIDEnv(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	protectedID := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "protected-short-id"})
	waitContainerRunning(t, engine, protectedID)
	refs := refForms(t, engine, protectedID)

	a := harness.StartAgent(t, harness.AgentOptions{
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_PROTECTED_CONTAINERS": refs["short"]},
	})
	d := harness.PairDevice(t, a, "protected-short-id-env")

	status, obj := d.JSON(t, http.MethodPost, "/v1/containers/"+refs["name"]+"/stop")
	if status != http.StatusForbidden {
		t.Fatalf("POST .../stop (by name, protected via a 12-hex short ID in the env): status = %d, want %d; body = %v",
			status, http.StatusForbidden, obj)
	}
	if obj["error"] != protectedContainerBody {
		t.Errorf("POST .../stop (by name, protected via a 12-hex short ID in the env): error = %v, want %q",
			obj["error"], protectedContainerBody)
	}
}
