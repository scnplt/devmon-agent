package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// msgClientCertRequired is the terse rejection served to any unauthenticated
// caller on a guarded route. It says nothing about why.
const msgClientCertRequired = "client certificate required"

// requireDevice rejects any request without a verified client certificate.
//
// This is the other half of the single-port design: because the listener uses
// VerifyClientCertIfGiven rather than RequireAndVerifyClientCert (see
// internal/tlsconf), a connection with no client certificate reaches HTTP. This
// middleware is what stops it from reaching a protected route.
//
// Phase 1 has no CA, so every request is rejected — and no route is wrapped in
// it yet, because none exists. Phase 2 fills in the device lookup against
// state.Store and starts guarding real handlers.
func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
			return
		}
		// Phase 2: resolve r.TLS.PeerCertificates[0] to a non-revoked device and
		// call next. Until then a presented certificate cannot be verified
		// against anything, so it fails closed.
		_ = next
		s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
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
