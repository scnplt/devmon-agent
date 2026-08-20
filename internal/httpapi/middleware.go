// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/scnplt/devmon-agent/internal/state"
)

// msgClientCertRequired is the terse rejection served to any unauthenticated
// caller on a guarded route. It says nothing about why.
const msgClientCertRequired = "client certificate required"

// deviceCtxKey is the context key under which requireDevice stores the
// resolved Device. An empty struct type, not a string: a string key can
// collide with another package's context value, an empty struct type cannot.
type deviceCtxKey struct{}

// routeCtxKey is the context key under which withRoute stashes the matched
// route pattern for withRecovery and withRequestLog to read.
type routeCtxKey struct{}

// unmatchedRoute is what withRoute stashes for a request that matched no
// registered pattern — a fixed literal, never anything derived from the
// request. See withRoute for why.
const unmatchedRoute = "(unmatched)"

// routeFrom returns the route pattern withRoute resolved for this request,
// or unmatchedRoute if none was stashed (e.g. a context that never passed
// through withRoute, as in a unit test that calls a handler directly).
func routeFrom(ctx context.Context) string {
	if route, ok := ctx.Value(routeCtxKey{}).(string); ok {
		return route
	}
	return unmatchedRoute
}

// withRoute resolves the route pattern once, outside both withRecovery and
// withRequestLog, and stashes it in the request context under routeCtxKey.
//
// mux.Handler only inspects the request to find a match; it does not serve
// it, so dispatch still happens exactly once, inside next, when next's chain
// eventually reaches mux.ServeHTTP.
//
// The resolved pattern is checked against patterns — the closed set of
// literals routes() registered — rather than trusted verbatim. Two cases
// need that: an unmatched request, where mux.Handler returns an empty
// pattern, and an internally-generated redirect (e.g. a path ServeMux
// cleans before matching), where the net/http docs say Handler returns "the
// path that will match after following the redirect" — a value built from
// the request path, not from the registered pattern set, and therefore not
// safe to log unfiltered. Membership in patterns is what keeps the logged
// value bounded and non-attacker-controlled in both cases; anything not in
// the set collapses to unmatchedRoute.
func (s *Server) withRoute(mux *http.ServeMux, patterns map[string]struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if _, ok := patterns[pattern]; !ok {
			pattern = unmatchedRoute
		}
		ctx := context.WithValue(r.Context(), routeCtxKey{}, pattern)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// DeviceFrom returns the Device requireDevice resolved for this request, if
// any. Handlers behind requireDevice may call this without a second lookup.
func DeviceFrom(ctx context.Context) (state.Device, bool) {
	device, ok := ctx.Value(deviceCtxKey{}).(state.Device)
	return device, ok
}

// requireDevice rejects any request without a verified client certificate
// belonging to an active, registered device.
//
// This is the other half of the single-port design: because the listener uses
// VerifyClientCertIfGiven rather than RequireAndVerifyClientCert (see
// internal/tlsconf), a connection with no client certificate reaches HTTP. This
// middleware is what stops it from reaching a protected route.
//
// Every rejection reason — no certificate, unknown serial, revoked device —
// answers with the identical msgClientCertRequired body and 401 status. The
// real reason is logged for the operator; a scanner probing the port learns
// nothing that distinguishes the cases.
func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
			return
		}

		serial := r.TLS.PeerCertificates[0].SerialNumber.Text(16)

		device, err := s.st.DeviceByCertSerial(r.Context(), serial)
		if err != nil {
			if !errors.Is(err, state.ErrDeviceNotFound) {
				s.log.Error("resolve device by certificate serial",
					slog.String("serial", serial),
					slog.Any("err", err),
				)
			} else {
				s.log.Warn("rejected request with unknown certificate serial",
					slog.String("serial", serial),
				)
			}
			s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
			return
		}
		if device.IsRevoked() {
			s.log.Warn("rejected request from revoked device",
				slog.String("device_id", device.ID),
			)
			s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
			return
		}

		// A failed last-seen write is bookkeeping, not authorisation — it must
		// never fail the request.
		if err := s.st.TouchDevice(r.Context(), device.ID); err != nil {
			s.log.Warn("touch device last seen",
				slog.String("device_id", device.ID),
				slog.Any("err", err),
			)
		}

		ctx := context.WithValue(r.Context(), deviceCtxKey{}, device)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRecovery turns a panicking handler into a 500 without leaking a stack
// trace to the client. The trace goes to the agent's own log, where the operator
// can read it and a scanner cannot.
//
// It logs the matched route pattern, resolved upstream by withRoute, rather
// than r.URL.Path: the path is attacker-controlled up to the header budget
// (maxHeaderBytes), and withIPLimit's doc comment (ratelimit.go) already
// explains why attacker-controlled input has no place in this log — a
// scanner must not be able to write arbitrary volume into the operator's
// own diagnostics. The route pattern is drawn from the closed, fixed set
// routes() registers, so it carries the same diagnostic value without that
// risk.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					slog.String("method", r.Method),
					slog.String("route", routeFrom(r.Context())),
					slog.Any("panic", rec),
				)
				s.writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withRequestLog records one Debug line per request.
//
// Method, route, status, and duration only — never headers, never query
// strings, never a body. Headers on this port carry client certificates and,
// from Phase 2, pairing material.
//
// "route" is the matched route pattern (see withRoute), not r.URL.Path: the
// same reasoning as withRecovery above applies here — the path is
// attacker-controlled and unbounded relative to the log, the route pattern
// is neither. See withIPLimit in ratelimit.go for the rate-limiter's version
// of this same rule.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.log.Debug("request served",
			slog.String("method", r.Method),
			slog.String("route", routeFrom(r.Context())),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// statusRecorder captures the status code so it can be logged after the handler
// has returned.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the wrapped ResponseWriter to http.ResponseController.
//
// Without this, ResponseController cannot find the underlying writer's Flush
// or SetWriteDeadline and returns http.ErrNotSupported for both — because
// statusRecorder embeds the ResponseWriter *interface*, whose method set is
// only Header/Write/WriteHeader. The SSE stream in logs.go depends on both:
// one to deliver each line, the other to escape the server's 30s WriteTimeout.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
