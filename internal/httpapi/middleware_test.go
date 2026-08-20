// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/policy"
)

// TestWithRequestLogLogsRoutePattern covers the fix for issue #46 item 3: a
// request to a registered route must log the matched route pattern, never
// the raw request path.
func TestWithRequestLogLogsRoutePattern(t *testing.T) {
	t.Parallel()

	// Arrange
	log, buf := newCapturingLoggerAtLevel(slog.LevelDebug)
	s := testServer(t, policy.ModeDefault)
	s.log = log

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	got := buf.String()
	if !strings.Contains(got, `route=GET\ /v1/status`) && !strings.Contains(got, `route="GET /v1/status"`) {
		t.Errorf("log = %q, want it to contain the matched route pattern GET /v1/status", got)
	}
	if strings.Contains(got, "path=") {
		t.Errorf("log = %q, want no raw \"path\" field", got)
	}
}

// TestWithRequestLogNeverLogsHostilePath covers the other half of issue #46
// item 3: a path carrying a long or hostile segment must never make it into
// the log, matched or not — the ServeMux match fails, withRoute collapses the
// result to unmatchedRoute, and that fixed literal is all that gets logged.
func TestWithRequestLogNeverLogsHostilePath(t *testing.T) {
	t.Parallel()

	// Arrange
	log, buf := newCapturingLoggerAtLevel(slog.LevelDebug)
	s := testServer(t, policy.ModeDefault)
	s.log = log

	hostileSegment := strings.Repeat("A", 2048)
	req := httptest.NewRequest(http.MethodGet, "/v1/status/"+hostileSegment, nil)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	got := buf.String()
	if strings.Contains(got, hostileSegment) {
		t.Errorf("log contains the hostile path segment, want it filtered out entirely: %q", got)
	}
	if !strings.Contains(got, unmatchedRoute) {
		t.Errorf("log = %q, want it to contain the unmatched-route marker %q", got, unmatchedRoute)
	}
}

// TestWithRecoveryLogsRoutePatternOnPanic covers the panic path for issue
// #46 item 3: withRecovery must log the matched route, not r.URL.Path,
// exactly like withRequestLog.
func TestWithRecoveryLogsRoutePatternOnPanic(t *testing.T) {
	t.Parallel()

	// Arrange — a minimal chain that mirrors routes()'s wiring order
	// (withRoute outermost, then withRecovery, then the mux) but with a
	// handler that panics, since no real route in routes() ever does.
	log, buf := newCapturingLogger()
	s := &Server{log: log}

	mux := http.NewServeMux()
	mux.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	patterns := map[string]struct{}{"GET /boom": {}}

	handler := s.withRoute(mux, patterns, s.withRecovery(mux))

	hostileSegment := strings.Repeat("%0A", 100) + "injected"
	req := httptest.NewRequest(http.MethodGet, "/boom?x="+hostileSegment, nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	got := buf.String()
	if !strings.Contains(got, "GET /boom") {
		t.Errorf("log = %q, want it to contain the matched route GET /boom", got)
	}
	if strings.Contains(got, "injected") {
		t.Errorf("log contains hostile query content, want only the fixed route logged: %q", got)
	}
}

// TestRouteFromDefaultsToUnmatched covers routeFrom's fallback: a context
// that never passed through withRoute (as any handler invoked directly in a
// unit test has not) must still yield the fixed unmatchedRoute marker
// rather than panicking or returning an empty string.
func TestRouteFromDefaultsToUnmatched(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)

	if got := routeFrom(req.Context()); got != unmatchedRoute {
		t.Errorf("routeFrom() = %q, want %q", got, unmatchedRoute)
	}
}
