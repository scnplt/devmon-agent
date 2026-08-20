// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays Phase 1's manual checklist item "GET /v1/status carries
// exactly the documented allowlist, POST is rejected, and no route leaks
// anything to a caller with no client certificate" — the one endpoint every
// scanner on the internet can already reach.

// unauthenticatedClient builds an http.Client that trusts only the CA an
// already-paired Device was pinned to, but presents no client certificate of
// its own — the shape of a caller that has never paired. Deriving trust from
// a real pairing response, rather than from InsecureSkipVerify, keeps this
// file off D7's forbidden path: after pairing has happened once against an
// agent, no client in this suite may skip verification again. The two
// permitted uses of InsecureSkipVerify are PairDevice's own bootstrap
// request and Agent's readiness probe, both in the harness package.
func unauthenticatedClient(d *harness.Device) *http.Client {
	cfg := d.TLSConfig()
	cfg.Certificates = nil
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}

// getRaw issues an unauthenticated GET/POST-style request and returns the
// status, header, and raw body, mirroring Device.Do's shape for a caller
// that has no certificate.
func doRaw(t *testing.T, c *http.Client, method, url string) (status int, hdr http.Header, raw []byte) {
	t.Helper()

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, raw
}

// TestStatusAllowlist asserts GET /v1/status, called with no client
// certificate at all, answers exactly the five documented fields and
// nothing else — status.go's strict allowlist (internal/httpapi/status.go).
// An extra key here is an unreviewed disclosure on the one port every
// scanner on the internet can already reach.
func TestStatusAllowlist(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "status-allowlist")
	anon := unauthenticatedClient(d)

	status, hdr, raw := doRaw(t, anon, http.MethodGet, a.BaseURL+"/v1/status")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/status status = %d, want %d", status, http.StatusOK)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode /v1/status response: %v", err)
	}
	harness.AssertExactKeys(t, obj, []string{
		"api_version", "agent_version", "policy_mode", "server_time", "ca_fingerprint",
	})

	if obj["api_version"] != "v1" {
		t.Errorf("api_version = %v, want %q", obj["api_version"], "v1")
	}
	if obj["ca_fingerprint"] != d.CAFingerprint {
		t.Errorf("ca_fingerprint = %v, want %s (the same CA this device pinned)", obj["ca_fingerprint"], d.CAFingerprint)
	}
}

// TestStatusAdvertisesPolicyMode starts one agent per policy tier and
// asserts /v1/status reports the tier the operator configured — including
// the unset case, which must report "default" (policy.ParseMode's
// documented empty-string behaviour), not an empty string or the read-only
// floor.
func TestStatusAdvertisesPolicyMode(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	tests := []struct {
		name       string
		policyMode string // AgentOptions.PolicyMode; "" leaves DEVMON_POLICY_MODE unset
		want       string
	}{
		{name: "read-only mode reports read-only", policyMode: "read-only", want: "read-only"},
		{name: "full mode reports full", policyMode: "full", want: "full"},
		{name: "unset mode reports default", policyMode: "", want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: tt.policyMode})
			d := harness.PairDevice(t, a, "status-policy-"+tt.want)
			anon := unauthenticatedClient(d)

			status, _, raw := doRaw(t, anon, http.MethodGet, a.BaseURL+"/v1/status")
			if status != http.StatusOK {
				t.Fatalf("GET /v1/status status = %d, want %d", status, http.StatusOK)
			}

			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode /v1/status response: %v", err)
			}
			if obj["policy_mode"] != tt.want {
				t.Errorf("policy_mode = %v, want %q", obj["policy_mode"], tt.want)
			}
		})
	}
}

// TestStatusRejectsOtherMethods asserts POST /v1/status answers 405: the
// route is registered "GET /v1/status", and Go 1.22+'s method-aware mux
// pattern must not silently also match every other verb.
func TestStatusRejectsOtherMethods(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "status-method")
	anon := unauthenticatedClient(d)

	status, _, _ := doRaw(t, anon, http.MethodPost, a.BaseURL+"/v1/status")
	if status != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/status status = %d, want %d", status, http.StatusMethodNotAllowed)
	}
}

// TestUnauthenticatedRequestsLeakNothing asserts a guarded route, called
// with no client certificate, answers with the terse rejection the wire
// contract promises — and only that: no state directory path, no hostname,
// no Go type name, no stack trace. requireDevice (internal/httpapi/middleware.go)
// is supposed to say nothing about *why* a caller was rejected; this test
// is the client-side proof of that promise.
func TestUnauthenticatedRequestsLeakNothing(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d := harness.PairDevice(t, a, "status-leak")
	anon := unauthenticatedClient(d)

	status, _, raw := doRaw(t, anon, http.MethodGet, a.BaseURL+"/v1/containers")
	if status != http.StatusUnauthorized && status != http.StatusNotFound {
		t.Fatalf("GET /v1/containers with no client certificate: status = %d, want %d or %d",
			status, http.StatusUnauthorized, http.StatusNotFound)
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode error response: %v (body = %s)", err, raw)
	}
	harness.AssertExactKeys(t, obj, []string{"error"})
	if obj["error"] != "client certificate required" {
		t.Errorf(`error = %v, want "client certificate required"`, obj["error"])
	}

	body := string(raw)
	forbidden := []string{a.StateDir, "internal/", ".go:", "goroutine "}
	for _, needle := range forbidden {
		if strings.Contains(body, needle) {
			t.Errorf("rejection body leaks %q: %s", needle, body)
		}
	}
}
