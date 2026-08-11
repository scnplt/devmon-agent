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
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
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
// Method, path, status, and duration only — never headers, never query strings,
// never a body. Headers on this port carry client certificates and, from
// Phase 2, pairing material.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.log.Debug("request served",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
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
