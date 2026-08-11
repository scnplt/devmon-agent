// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// rateLimitedServer builds a Server with deliberately tiny, deterministic
// rate-limit tiers, so a test can drive a bucket to exhaustion in a handful
// of requests instead of dozens.
func rateLimitedServer(t *testing.T, statusPerMin, pairPerMin, guardedPerSec int) *Server {
	t.Helper()
	cfg := config.Config{
		StateDir:          t.TempDir(),
		ListenAddr:        ":8443",
		PolicyMode:        policy.ModeDefault,
		RateStatusPerMin:  statusPerMin,
		RatePairPerMin:    pairPerMin,
		RateGuardedPerSec: guardedPerSec,
	}
	return NewServer(cfg, nil, nil, nil, nil, testLogger())
}

// requestFromIP builds an unauthenticated request whose RemoteAddr carries
// ip, for driving the IP-keyed tiers without a real network connection.
func requestFromIP(method, path, ip string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip + ":54321"
	return req
}

// deviceContext returns a context carrying a resolved Device with id, as
// requireDevice would set it, for driving withDeviceLimit directly without
// a real client certificate or *state.Store.
func deviceContext(id string) context.Context {
	return context.WithValue(context.Background(), deviceCtxKey{}, state.Device{ID: id})
}

// assertRateLimited404Body asserts the 429 wire shape the Rate-Limiting
// Contract promises: the exact terse body, Cache-Control: no-store, and an
// integer Retry-After of at least one second.
func assertRateLimitedResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"rate limit exceeded"}` {
		t.Errorf("body = %q, want the exact terse rate-limit body", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	retryAfter := rec.Header().Get(headerRetryAfter)
	n, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer: %v", retryAfter, err)
	}
	if n < 1 {
		t.Errorf("Retry-After = %d, want >= 1", n)
	}
}

// TestStatusTierBurstThen429 drives a status-tier bucket of burst 1 to
// exhaustion: the first request succeeds, the second is refused with the
// full 429 contract shape.
func TestStatusTierBurstThen429(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 1, 5, 20)

	// Act — first request within burst.
	rec1 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec1, requestFromIP(http.MethodGet, "/v1/status", "203.0.113.1"))
	// Act — second request past burst.
	rec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec2, requestFromIP(http.MethodGet, "/v1/status", "203.0.113.1"))

	// Assert
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}
	assertRateLimitedResponse(t, rec2)
}

// TestStatusTierIsPerIP proves two client IPs hold independent buckets: the
// first IP's burst is exhausted, but a second IP is still served.
func TestStatusTierIsPerIP(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 1, 5, 20)

	// Act — exhaust the burst for the first IP.
	s.routes().ServeHTTP(httptest.NewRecorder(), requestFromIP(http.MethodGet, "/v1/status", "203.0.113.10"))
	exhausted := httptest.NewRecorder()
	s.routes().ServeHTTP(exhausted, requestFromIP(http.MethodGet, "/v1/status", "203.0.113.10"))

	// Act — a different IP, same tier.
	otherIP := httptest.NewRecorder()
	s.routes().ServeHTTP(otherIP, requestFromIP(http.MethodGet, "/v1/status", "203.0.113.11"))

	// Assert
	if exhausted.Code != http.StatusTooManyRequests {
		t.Fatalf("first IP second request status = %d, want 429", exhausted.Code)
	}
	if otherIP.Code != http.StatusOK {
		t.Errorf("second IP status = %d, want 200 (independent bucket)", otherIP.Code)
	}
}

// TestPairTierIsSeparateFromStatusTier proves the two pre-auth tiers are
// separate buckets, not one shared per-IP bucket: exhausting the status
// tier for an IP leaves the pair tier still serving that same IP.
func TestPairTierIsSeparateFromStatusTier(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 1, 1, 20)
	const ip = "203.0.113.20"

	// Act — exhaust the status tier for ip.
	s.routes().ServeHTTP(httptest.NewRecorder(), requestFromIP(http.MethodGet, "/v1/status", ip))
	statusExhausted := httptest.NewRecorder()
	s.routes().ServeHTTP(statusExhausted, requestFromIP(http.MethodGet, "/v1/status", ip))
	if statusExhausted.Code != http.StatusTooManyRequests {
		t.Fatalf("status tier not exhausted: got %d, want 429", statusExhausted.Code)
	}

	// Act — the pair tier, same IP, must still be served rather than
	// refused by a bucket shared with status.
	pairRec := httptest.NewRecorder()
	s.routes().ServeHTTP(pairRec, requestFromIP(http.MethodPost, "/v1/pair", ip))

	// Assert — the request fails decoding (empty body), but it must not be
	// the limiter that refused it.
	if pairRec.Code == http.StatusTooManyRequests {
		t.Errorf("pair request after status exhaustion = 429, want the pair tier to be independent")
	}
}

// TestDeviceTierBurstThen429 drives a device bucket of burst 2 (guardedPerSec
// 1 x guardedBurstMultiplier 2) to exhaustion directly through
// withDeviceLimit, without a real client certificate.
func TestDeviceTierBurstThen429(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 30, 5, 1)
	var ran int
	handler := s.withDeviceLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran++
		w.WriteHeader(http.StatusOK)
	}))

	// Act — two requests within the burst of 2.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(deviceContext("device-a"))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (within burst)", i, rec.Code)
		}
	}

	// Act — a third request, past the burst.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(deviceContext("device-a"))
	handler.ServeHTTP(rec, req)

	// Assert
	assertRateLimitedResponse(t, rec)
	if ran != 2 {
		t.Errorf("next ran %d times, want exactly 2", ran)
	}
}

// TestDeviceTierIsPerDevice proves two device IDs hold independent buckets
// (D6): throttling one device leaves a different device served immediately.
func TestDeviceTierIsPerDevice(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 30, 5, 1)
	handler := s.withDeviceLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Act — exhaust device A's burst of 2.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(deviceContext("device-a"))
		handler.ServeHTTP(rec, req)
	}
	deviceA := httptest.NewRecorder()
	handler.ServeHTTP(deviceA, httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(deviceContext("device-a")))

	// Act — a different device, same tier.
	deviceB := httptest.NewRecorder()
	handler.ServeHTTP(deviceB, httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(deviceContext("device-b")))

	// Assert
	if deviceA.Code != http.StatusTooManyRequests {
		t.Fatalf("device A status = %d, want 429", deviceA.Code)
	}
	if deviceB.Code != http.StatusOK {
		t.Errorf("device B status = %d, want 200 (independent bucket)", deviceB.Code)
	}
}

// TestWithDeviceLimitRejectsWithoutDeviceInContext is the mandatory GOTCHA:
// withDeviceLimit only ever runs behind requireDevice. If it somehow runs
// without a resolved device in context, it must reject with 500 and never
// call next — never silently degrade to no limiter.
func TestWithDeviceLimitRejectsWithoutDeviceInContext(t *testing.T) {
	t.Parallel()

	// Arrange
	s := rateLimitedServer(t, 30, 5, 20)
	var ran bool
	handler := s.withDeviceLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/containers", nil))

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ran {
		t.Error("next ran despite no device resolved in context")
	}
}

// TestClientIP covers net.SplitHostPort's normal and degenerate cases.
func TestClientIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{name: "ipv4 with port", remoteAddr: "1.2.3.4:5678", want: "1.2.3.4"},
		{name: "ipv6 with port", remoteAddr: "[::1]:5678", want: "::1"},
		{name: "unsplittable value", remoteAddr: "garbage", want: "garbage"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
			req.RemoteAddr = tt.remoteAddr

			// Act
			got := clientIP(req)

			// Assert
			if got != tt.want {
				t.Errorf("clientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

// TestClientIPIgnoresForwardedFor is D5's regression test: a client-supplied
// X-Forwarded-For must never change the key clientIP returns.
func TestClientIPIgnoresForwardedFor(t *testing.T) {
	t.Parallel()

	// Arrange
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")

	// Act
	got := clientIP(req)

	// Assert
	if got != "1.2.3.4" {
		t.Errorf("clientIP() = %q, want RemoteAddr host 1.2.3.4; X-Forwarded-For must be ignored", got)
	}
}
