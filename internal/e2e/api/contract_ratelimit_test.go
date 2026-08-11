// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"net/http"
	"strconv"
	"strings"
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

	// writeJSON (internal/httpapi/respond.go) encodes every response body with
	// json.Encoder, which always appends a trailing newline — true of every
	// error response this API serves, not a quirk of the 429. Trim it before
	// the exact-match comparison so the assertion still pins the body to
	// exactly this terse message and nothing else.
	const wantBody = `{"error":"rate limit exceeded"}`
	if got := strings.TrimSpace(string(raw)); got != wantBody {
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
// trips the guarded tier must leave no audit row for the throttled attempt,
// only for the requests that actually reached the Engine. withDeviceLimit
// sits before withAudit precisely so a device cannot use throttled retries
// to push real history out of a size-bounded audit table
// (internal/httpapi/ratelimit.go).
//
// The guarded tier is one bucket per device, shared across every guarded
// route (D6), not one bucket per route. So this test drains it with fast
// GET /v1/containers calls rather than with the mutating route itself: a
// real restart against Engine 29 routinely takes longer than the 1/sec
// refill interval, so a loop of restarts alone never outpaces the refill
// and the bucket never empties — that was the original defect here. Reads
// are milliseconds and reliably outrun a 1/sec refill, so they can drain the
// same shared bucket that the restart call will then find empty.
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

	// One known-good restart first, while the bucket still has its burst
	// token, so there is a baseline audit row count to compare the throttled
	// attempt against.
	status, _, raw := d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusNoContent {
		t.Fatalf("baseline POST .../restart = %d, want %d; body = %s", status, http.StatusNoContent, raw)
	}
	rowsBefore := harness.ListAudit(t, a, auditListLimit)

	// Drain the device's shared guarded-tier bucket with cheap reads, which
	// outrun the 1/sec refill; the mutating call below reuses the same
	// bucket (maxRateLimitIterations bounds this loop like every other in
	// this file).
	got429 := false
	for i := 0; i < maxRateLimitIterations; i++ {
		status, _, raw := d.Do(t, http.MethodGet, "/v1/containers", nil)
		if status == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("GET /v1/containers = %d, want %d or %d; body = %s",
				status, http.StatusOK, http.StatusTooManyRequests, raw)
		}
	}
	if !got429 {
		t.Fatalf("draining GET /v1/containers never answered 429 within %d iterations (guarded tier = 1/sec)", maxRateLimitIterations)
	}

	// The bucket is now empty and shared across routes, so the very next
	// guarded call — this time the mutating one — must be throttled too.
	status, _, raw = d.Do(t, http.MethodPost, "/v1/containers/"+id+"/restart", nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("POST .../restart immediately after draining the shared bucket = %d, want %d; body = %s",
			status, http.StatusTooManyRequests, raw)
	}

	rowsAfter := harness.ListAudit(t, a, auditListLimit)
	if len(rowsAfter) != len(rowsBefore) {
		t.Fatalf("audit list holds %d rows after the throttled restart, want %d (unchanged: the throttled restart must write nothing): %+v",
			len(rowsAfter), len(rowsBefore), rowsAfter)
	}
}
