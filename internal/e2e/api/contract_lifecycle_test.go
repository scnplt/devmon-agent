// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays the whole Phase 5 curl script — restart, stop, start,
// idempotent start, kill refused, delete refused in default mode; then, in
// full mode, delete-while-running conflicts, stop, then delete succeeds — plus
// the invalid/unknown-reference, Engine-unavailable, and wrong-method cells
// of the same checklist.
//
// What this file deliberately does NOT cover: the three-mode × operation
// matrix lives in contract_policy_test.go, one row of which
// (TestDeleteRunningContainerConflicts here) it deliberately does not repeat;
// the audit trail these same requests leave behind is contract_audit_test.go
// (Task 8); and the self-exclusion guarantee — the headline metric —
// needs a containerised agent and lives in internal/e2e/incontainer
// (Tasks 9-11).

// lifecycleContractTimeout bounds a single Engine poll in this file and in
// contract_policy_test.go, which shares waitContainerRunning.
const lifecycleContractTimeout = 20 * time.Second

// assertNoBody asserts a 204 response carries no body. A client that tries to
// parse JSON out of a No Content response is a bug this contract exists to
// prevent (Task 7's gotcha).
func assertNoBody(t *testing.T, raw []byte) {
	t.Helper()
	if len(raw) != 0 {
		t.Errorf("204 response has a non-empty body: %q", raw)
	}
}

// waitContainerRunning polls the Engine (never the agent) until id reports a
// running state, bounded by a deadline rather than a fixed sleep. The 409
// conflict case and every restart/stop/kill cell need a container that is
// GENUINELY running: ContainerStart returns before the container has
// necessarily reached that state (Task 7's gotcha).
func waitContainerRunning(t *testing.T, engine *client.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(lifecycleContractTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		cancel()
		if err == nil && res.Container.State != nil && res.Container.State.Running {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("container %s did not reach running state within %s", id, lifecycleContractTimeout)
}

// TestLifecycleHappyPath reproduces the default-mode half of the curl script
// verbatim: restart, stop, start, a second (idempotent) start, kill refused,
// delete refused — against a DEFAULT-mode agent started with
// DEVMON_POLICY_MODE absent, not set to "default": the metric is about
// the operator who configures nothing, and setting the variable would test a
// different claim (Task 7's gotcha; the explicit value is covered by
// TestPolicyMatrix's "default" row in contract_policy_test.go).
func TestLifecycleHappyPath(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{}) // PolicyMode "" -> DEVMON_POLICY_MODE unset
	d := harness.PairDevice(t, a, "lifecycle-happy-path")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "happy-path"})
	waitContainerRunning(t, engine, id)

	steps := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"restart", http.MethodPost, "/v1/containers/" + id + "/restart", http.StatusNoContent},
		{"stop", http.MethodPost, "/v1/containers/" + id + "/stop", http.StatusNoContent},
		{"start", http.MethodPost, "/v1/containers/" + id + "/start", http.StatusNoContent},
		{"start again (idempotent)", http.MethodPost, "/v1/containers/" + id + "/start", http.StatusNoContent},
		{"kill refused by default policy", http.MethodPost, "/v1/containers/" + id + "/kill", http.StatusForbidden},
		{"delete refused by default policy", http.MethodDelete, "/v1/containers/" + id, http.StatusForbidden},
	}

	// Deliberately sequential (no t.Parallel per step): the sequence itself,
	// in order, is the contract being replayed — restart must land before
	// stop, stop before start, and so on, exactly as the curl script runs it.
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			status, _, raw := d.Do(t, step.method, step.path, nil)
			if status != step.want {
				t.Fatalf("%s %s status = %d, want %d; body = %s", step.method, step.path, status, step.want, raw)
			}
			if status == http.StatusNoContent {
				assertNoBody(t, raw)
			}
		})
	}
}

// TestDeleteRunningContainerConflicts reproduces the full-mode half of the
// curl script: deleting a running container conflicts (409) and leaves it
// untouched; stopping it then succeeds; deleting it then succeeds and it is
// gone even from ?all=true.
func TestDeleteRunningContainerConflicts(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: "full"})
	d := harness.PairDevice(t, a, "lifecycle-delete-conflict")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "delete-conflict"})
	waitContainerRunning(t, engine, id)

	status, obj := d.JSON(t, http.MethodDelete, "/v1/containers/"+id)
	if status != http.StatusConflict {
		t.Fatalf("DELETE %s while running: status = %d, want %d", id, status, http.StatusConflict)
	}
	if obj["error"] != "container is running" {
		t.Errorf("DELETE conflict error = %v, want %q", obj["error"], "container is running")
	}
	// Untouched by the refused attempt: still running, not just still present.
	waitContainerRunning(t, engine, id)

	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/stop", nil)
	if status != http.StatusNoContent {
		t.Fatalf("POST .../stop status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}
	assertNoBody(t, raw)

	status, _, raw = d.Do(t, http.MethodDelete, "/v1/containers/"+id, nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE %s after stop: status = %d, want %d; body = %s", id, status, http.StatusNoContent, raw)
	}
	assertNoBody(t, raw)

	items := readEnvelope(t, d, "/v1/containers?all=true")
	for _, raw := range items {
		if asObject(t, raw)["id"] == id {
			t.Fatalf("deleted container %s still present in ?all=true list", id)
		}
	}
}

// TestLifecycleRejectsInvalidAndUnknownRefs asserts a lifecycle route answers
// 400 for a reference that fails dockerx.ValidateRef (never reaching the
// Engine) and 404 for a well-formed but nonexistent one, in a mode that
// permits the operation — otherwise the policy gate, not the reference
// check, would be what a reader sees produce the status.
func TestLifecycleRejectsInvalidAndUnknownRefs(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "lifecycle-invalid-refs")

	const unknown = "devmone2enosuchcontainer00000000"

	tests := []struct {
		name string
		path string
		want int
	}{
		{"invalid ref", "/v1/containers/%2e%2e/start", http.StatusBadRequest},
		{"unknown ref", "/v1/containers/" + unknown + "/start", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, _, raw := d.Do(t, http.MethodPost, tt.path, nil)
			if status != tt.want {
				t.Errorf("POST %s status = %d, want %d; body = %s", tt.path, status, tt.want, raw)
			}
		})
	}
}

// TestLifecycleAnswers502WhenEngineIsGone is D16's proxy in action for a
// mutating route: with the proxy severed, a restart yields 502 and the agent
// itself stays reachable throughout, proven by asking the Engine directly
// (not the agent) and by /v1/status continuing to answer.
//
// This test owns its own agent and its own proxy, never shared with a
// t.Parallel() test, for the same reason TestReadsAnswer502WhenEngineIsGone
// in contract_reads_test.go does: severing a proxy another test depends on
// would make that test's outcome depend on scheduling order.
func TestLifecycleAnswers502WhenEngineIsGone(t *testing.T) {
	engine := harness.RequireEngine(t)

	realHost, skipReason := harness.EngineHost()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	// The proxy must be listening BEFORE the agent starts: dockerx.New pings
	// the Engine at startup, and a dead endpoint there is a fatal startup
	// error, not the 502 this test is about (D16's gotcha).
	proxy := harness.NewProxy(t, realHost)

	a := harness.StartAgent(t, harness.AgentOptions{DockerHost: proxy.Addr(), PolicyMode: "full"})
	d := harness.PairDevice(t, a, "lifecycle-engine-gone")

	// The fixture is managed through engine, a direct connection to the real
	// Engine (harness.RequireEngine), never through the proxy: only the agent
	// under test talks to the Engine via the severable proxy.
	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "engine-gone"})
	waitContainerRunning(t, engine, id)

	// Falsifiability / sanity: with the proxy intact, the same restart the
	// test is about to repeat against a severed proxy succeeds normally. If
	// this pre-check itself failed, the 502 assertion below would prove
	// nothing about the proxy.
	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusNoContent {
		t.Fatalf("sanity check before severing the proxy: POST .../restart status = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}
	waitContainerRunning(t, engine, id)

	proxy.Sever(t)

	status, obj := d.JSON(t, http.MethodPost, "/v1/containers/"+id+"/restart")
	if status != http.StatusBadGateway {
		t.Errorf("POST .../restart with the Engine severed: status = %d, want %d", status, http.StatusBadGateway)
	} else if obj["error"] != "docker engine unavailable" {
		t.Errorf("POST .../restart with the Engine severed: error = %v, want %q", obj["error"], "docker engine unavailable")
	}

	// The agent itself is still up: /v1/status does not depend on the
	// Engine, so it continuing to answer confirms the agent's own process —
	// not just its HTTP port — survived the severed Engine.
	if status, _, _ := d.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Errorf("GET /v1/status with the Engine severed: status = %d, want %d", status, http.StatusOK)
	}

	proxy.Restore(t)
}

// TestLifecycleRejectsWrongMethod asserts GET on a lifecycle route answers
// 405: the routes are registered as "POST .../start" etc, and Go 1.22+'s
// method-aware mux pattern must not silently also match GET.
func TestLifecycleRejectsWrongMethod(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "lifecycle-wrong-method")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "wrong-method"})

	status, _, raw := d.Do(t, http.MethodGet, "/v1/containers/"+id+"/start", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("GET .../start status = %d, want %d; body = %s", status, http.StatusMethodNotAllowed, raw)
	}
}
