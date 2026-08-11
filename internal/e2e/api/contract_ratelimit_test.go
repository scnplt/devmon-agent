// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays Phase 7's Rate-Limiting Contract from the outside: the
// two pre-authentication tiers (status, pair), the per-device guarded tier
// (D6), recovery once Retry-After elapses, and the audit-table property D7
// exists for — a throttled call writes no row.
// hardening-and-oss-release.plan.md's "Rate-Limiting Contract" section.
//
// Every test in this file starts its own agent: the three DEVMON_RATE_*
// variables are startup configuration, so sharing an agent across cases
// would make one test's limit leak into another's assertion.
//
// maxRateLimitIterations bounds every polling loop in this file. Agent.
// waitReady already spends tokens from the status tier's bucket before a
// test body runs (harness/agent.go:328-353), so no case here may assert an
// exact request count before the first 429 — only that one arrives within
// this bound. None of these cases sets the status tier to 1: doing so risks
// waitReady itself failing before the agent is ever considered started.
const maxRateLimitIterations = 20

// assertRateLimitedBody asserts the 429 wire shape the contract promises:
// an integer Retry-After of at least one second and the single, tier-
// agnostic error body (raw callers never see which bucket they hit — that
// is operator information, logged, not returned).
func assertRateLimitedBody(t *testing.T, hdr http.Header, raw []byte) {
	t.Helper()

	retryAfter, err := strconv.Atoi(hdr.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer: %v", hdr.Get("Retry-After"), err)
	}
	if retryAfter < 1 {
		t.Errorf("Retry-After = %d, want >= 1", retryAfter)
	}

	const wantBody = `{"error":"rate limit exceeded"}`
	if got := string(raw); got != wantBody {
		t.Errorf("429 body = %s, want %s", got, wantBody)
	}
}

// TestRateLimitStatusTierThrottles asserts GET /v1/status, called with no
// client certificate, eventually answers 429 once DEVMON_RATE_STATUS_PER_MIN
// is set low, and that the 429 carries the documented wire shape.
func TestRateLimitStatusTierThrottles(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		Env: map[string]string{"DEVMON_RATE_STATUS_PER_MIN": "5"},
	})
	d := harness.PairDevice(t, a, "ratelimit-status")
	anon := unauthenticatedClient(d)

	got429 := false
	var hdr http.Header
	var raw []byte
	for i := 0; i < maxRateLimitIterations; i++ {
		var status int
		status, hdr, raw = doRaw(t, anon, http.MethodGet, a.BaseURL+"/v1/status")
		if status == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("GET /v1/status = %d, want %d or %d", status, http.StatusOK, http.StatusTooManyRequests)
		}
	}
	if !got429 {
		t.Fatalf("GET /v1/status never answered 429 within %d iterations (status tier = 5/min)", maxRateLimitIterations)
	}
	assertRateLimitedBody(t, hdr, raw)
}

// TestRateLimitPairTierIsSeparateFromStatusTier asserts the pair and status
// tiers are independent buckets, not one shared per-port ceiling: exhausting
// DEVMON_RATE_PAIR_PER_MIN must leave /v1/status still serving 200 on the
// same connection.
func TestRateLimitPairTierIsSeparateFromStatusTier(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		Env: map[string]string{"DEVMON_RATE_PAIR_PER_MIN": "1"},
	})
	// PairDevice's own successful pairing spends the pair tier's single burst
	// token; the loop below drives the (already near-empty) bucket to a 429
	// with a junk code. It also gives this test a pinned CA to build the
	// unauthenticated status client from, without a second InsecureSkipVerify
	// use (D7 in the harness package comment).
	d := harness.PairDevice(t, a, "ratelimit-pair-separate")

	key := harness.GenerateDeviceKey(t)
	csrPEM := harness.DeviceCSRPEM(t, key, "devmon-e2e-ratelimit-pair")

	got429 := false
	for i := 0; i < maxRateLimitIterations; i++ {
		status, _ := harness.TryPairDevice(t, a, "junk-pairing-code-000000", csrPEM)
		if status == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("POST /v1/pair never answered 429 within %d iterations (pair tier = 1/min)", maxRateLimitIterations)
	}

	anon := unauthenticatedClient(d)
	status, _, raw := doRaw(t, anon, http.MethodGet, a.BaseURL+"/v1/status")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/status after exhausting the pair tier: status = %d, want %d (buckets are per tier, not per port); body = %s",
			status, http.StatusOK, raw)
	}
}

// TestRateLimitGuardedTierIsPerDevice asserts the guarded tier keys on
// device ID (D6), not IP: throttling one paired device must leave a second
// device, on the same agent and the same IP, served immediately.
func TestRateLimitGuardedTierIsPerDevice(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		Env: map[string]string{"DEVMON_RATE_GUARDED_PER_SEC": "1"},
	})
	deviceA := harness.PairDevice(t, a, "ratelimit-guarded-a")
	deviceB := harness.PairDevice(t, a, "ratelimit-guarded-b")

	got429 := false
	for i := 0; i < maxRateLimitIterations; i++ {
		status, _, raw := deviceA.Do(t, http.MethodGet, "/v1/containers", nil)
		if status == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("GET /v1/containers (device A) = %d, want %d or %d; body = %s",
				status, http.StatusOK, http.StatusTooManyRequests, raw)
		}
	}
	if !got429 {
		t.Fatalf("device A never received a 429 within %d iterations (guarded tier = 1/sec)", maxRateLimitIterations)
	}

	status, _, raw := deviceB.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers (device B) while device A is throttled: status = %d, want %d; body = %s",
			status, http.StatusOK, raw)
	}
}

// TestRateLimitGuardedTierRecoversAfterRetryAfter asserts a throttled device
// is served again once the advertised Retry-After elapses. This is the only
// sleep permitted in this file, and it sleeps the ADVERTISED duration, never
// a guessed one — a guessed duration would pass or fail depending on
// scheduling noise, not on whether recovery actually works.
func TestRateLimitGuardedTierRecoversAfterRetryAfter(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		Env: map[string]string{"DEVMON_RATE_GUARDED_PER_SEC": "1"},
	})
	d := harness.PairDevice(t, a, "ratelimit-guarded-recovery")

	got429 := false
	var retryAfterHeader string
	for i := 0; i < maxRateLimitIterations; i++ {
		status, hdr, raw := d.Do(t, http.MethodGet, "/v1/containers", nil)
		if status == http.StatusTooManyRequests {
			got429 = true
			retryAfterHeader = hdr.Get("Retry-After")
			break
		}
		if status != http.StatusOK {
			t.Fatalf("GET /v1/containers = %d, want %d or %d; body = %s",
				status, http.StatusOK, http.StatusTooManyRequests, raw)
		}
	}
	if !got429 {
		t.Fatalf("device never received a 429 within %d iterations (guarded tier = 1/sec)", maxRateLimitIterations)
	}

	retryAfter, err := strconv.Atoi(retryAfterHeader)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer: %v", retryAfterHeader, err)
	}

	time.Sleep(time.Duration(retryAfter) * time.Second)

	status, _, raw := d.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/containers after waiting the advertised Retry-After (%ds): status = %d, want %d; body = %s",
			retryAfter, status, http.StatusOK, raw)
	}
}

// TestRateLimitedRequestsWriteNoAuditRow is D7 in action: a device that
// trips the guarded tier on a mutating route must leave no audit row for the
// throttled attempts, only for the requests that actually reached the
// Engine. withDeviceLimit sits before withAudit precisely so a device cannot
// use throttled retries to push real history out of a size-bounded audit
// table (internal/httpapi/ratelimit.go).
func TestRateLimitedRequestsWriteNoAuditRow(t *testing.T) {
	t.Parallel()
	engine := harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{
		PolicyMode: "full",
		Env:        map[string]string{"DEVMON_RATE_GUARDED_PER_SEC": "1"},
	})
	d := harness.PairDevice(t, a, "ratelimit-audit-no-row")

	id := harness.StartFixture(t, engine, harness.FixtureOptions{NameSuffix: "ratelimit-audit"})
	waitContainerRunning(t, engine, id)

	successCount := 0
	got429 := false
	for i := 0; i < maxRateLimitIterations; i++ {
		status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
		switch status {
		case http.StatusNoContent:
			successCount++
		case http.StatusTooManyRequests:
			got429 = true
		default:
			t.Fatalf("POST .../restart = %d, want %d or %d; body = %s",
				status, http.StatusNoContent, http.StatusTooManyRequests, raw)
		}
		if got429 {
			break
		}
	}
	if !got429 {
		t.Fatalf("restart never answered 429 within %d iterations (guarded tier = 1/sec)", maxRateLimitIterations)
	}
	if successCount == 0 {
		t.Fatalf("no restart succeeded before the limiter tripped; nothing to compare the audit table against")
	}

	rows := harness.ListAudit(t, a, auditListLimit)
	if len(rows) != successCount {
		t.Fatalf("audit list holds %d rows, want exactly %d (one per restart that actually executed, none for the throttled 429s): %+v",
			len(rows), successCount, rows)
	}
}
