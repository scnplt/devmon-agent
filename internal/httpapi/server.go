// SPDX-License-Identifier: AGPL-3.0-only

// Package httpapi serves the agent's HTTPS API on its single listening port.
package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/ratelimit"
	"github.com/scnplt/devmon-agent/internal/state"
)

// Server timeouts and limits. All named, because each one is a security or
// availability decision rather than a tunable.
const (
	// readHeaderTimeout is the one that matters most on an internet-facing port:
	// without it a Slowloris client holds a connection open indefinitely by
	// dribbling headers. gosec G114 flags its absence.
	readHeaderTimeout = 10 * time.Second

	readTimeout    = 30 * time.Second
	writeTimeout   = 30 * time.Second
	idleTimeout    = 120 * time.Second
	maxHeaderBytes = 16 << 10 // 16 KiB

	// shutdownGrace bounds how long in-flight requests may finish after SIGTERM.
	// Docker's own stop timeout is 10s by default, so this must not be the thing
	// that turns a clean stop into a SIGKILL.
	shutdownGrace = 5 * time.Second

	// maxConcurrentStreams bounds simultaneous live log streams. Each holds a
	// goroutine, an Engine connection, and a socket for its entire life, so an
	// unbounded count is a file-descriptor exhaustion the agent inflicts on the
	// host it exists to protect. A constant rather than an env var: the PRD's
	// rule is that every extra startup setting is surface the operator has to
	// understand at install time, and eight concurrent log views on one phone is
	// already beyond any real use.
	maxConcurrentStreams = 8
)

// Server owns the HTTPS listener and its routes.
type Server struct {
	cfg config.Config
	st  *state.Store
	ca  *certs.CA
	// dc is the Docker read surface the eight read routes depend on.
	dc  DockerReader
	log *slog.Logger
	// streams bounds concurrent live log streams (D10). Buffered to
	// maxConcurrentStreams and initialised here rather than lazily in the
	// handler: a nil channel blocks forever on send and never succeeds on a
	// non-blocking select, which would answer every stream request with 503.
	streams chan struct{}

	// unauthGlobal is the shared, unkeyed backstop bucket every
	// pre-authentication request checks first (D8).
	unauthGlobal *rate.Limiter

	// statusLimits and pairLimits key one bucket per client IP for the two
	// unauthenticated routes; statusLimit and pairLimit are their
	// configured per-second rates, kept alongside the registries because
	// *ratelimit.Registry does not expose the rate it was built with and
	// withIPLimit needs it to compute Retry-After.
	statusLimits *ratelimit.Registry
	statusLimit  rate.Limit
	pairLimits   *ratelimit.Registry
	pairLimit    rate.Limit

	// deviceLimits keys one bucket per device ID for every guarded route
	// (D6); deviceLimit is its configured per-second rate, for the same
	// reason statusLimit and pairLimit are kept.
	deviceLimits *ratelimit.Registry
	deviceLimit  rate.Limit

	http *http.Server
}

// NewServer wires the API. tlsCfg carries the server certificate, so the
// listener never re-reads it from disk. ca is retained on Server — not just
// its fingerprint — because guarded routes added in a later phase issue and
// renew device certificates from it; the status handler derives the public
// fingerprint from it on each call. ca may be nil in tests that do not
// exercise certificate issuance; handleStatus tolerates that by serving an
// empty fingerprint. dc may likewise be nil in tests that do not exercise the
// Docker read routes; every read handler tolerates that by serving 502
// instead of panicking.
func NewServer(cfg config.Config, st *state.Store, ca *certs.CA, dc DockerReader, tlsCfg *tls.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, ca: ca, dc: dc, log: log, streams: make(chan struct{}, maxConcurrentStreams)}

	s.unauthGlobal = rate.NewLimiter(rate.Limit(unauthGlobalPerSec), unauthGlobalBurst)

	// floorRatePerX floors a config value at its package default when it is
	// below 1. config.Load's own minRatePerX bound means it can never
	// produce such a value; this exists solely so a zero-value
	// config.Config — the shape cmd/devmon-agent/main.go's and this
	// package's own tests build — never yields a limiter that admits
	// nothing.
	floorRatePerX := func(v, def int) int {
		if v < 1 {
			return def
		}
		return v
	}

	statusPerMin := floorRatePerX(cfg.RateStatusPerMin, defaultRateStatusPerMin)
	pairPerMin := floorRatePerX(cfg.RatePairPerMin, defaultRatePairPerMin)
	guardedPerSec := floorRatePerX(cfg.RateGuardedPerSec, defaultRateGuardedPerSec)

	// Burst equals the whole per-minute count on the pre-auth tiers, so a
	// client that legitimately checks status a few times in a row is not
	// throttled for behaving normally.
	s.statusLimit = rate.Limit(statusPerMin) / secondsPerMinute
	s.statusLimits = ratelimit.NewRegistry(s.statusLimit, statusPerMin, rateLimitMaxKeys)
	s.pairLimit = rate.Limit(pairPerMin) / secondsPerMinute
	s.pairLimits = ratelimit.NewRegistry(s.pairLimit, pairPerMin, rateLimitMaxKeys)

	s.deviceLimit = rate.Limit(guardedPerSec)
	s.deviceLimits = ratelimit.NewRegistry(s.deviceLimit, guardedPerSec*guardedBurstMultiplier, rateLimitMaxKeys)

	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.routes(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// routes builds the mux and wraps it in the middleware chain.
//
// No catch-all "/" handler is registered: an unmatched path gets ServeMux's
// bare 404, which tells an unauthenticated scanner nothing.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// The Go 1.22+ method pattern matters. Registering "/v1/status" alone would
	// also match POST, DELETE, and everything else.
	//
	// Rate-limit order (the Rate-Limiting Contract): the global unauthenticated
	// backstop runs first, then the route's own per-IP tier.
	mux.Handle("GET /v1/status",
		s.withGlobalUnauthLimit(s.withIPLimit(s.statusLimits, s.statusLimit, "status", http.HandlerFunc(s.handleStatus))))

	// Unauthenticated by design (D2): the device has no certificate yet, so
	// the pairing code itself is what authenticates this one call.
	mux.Handle("POST /v1/pair",
		s.withGlobalUnauthLimit(s.withIPLimit(s.pairLimits, s.pairLimit, "pair", http.HandlerFunc(s.handlePair))))

	// Guarded: both act on the calling device's own identity, resolved by
	// requireDevice from its client certificate — never from the request.
	// withDeviceLimit sits immediately inside requireDevice, before anything
	// else, exactly as it does for the read/logs/mutate helpers below.
	mux.Handle("POST /v1/device/renew", s.requireDevice(s.withDeviceLimit(http.HandlerFunc(s.handleRenew))))
	mux.Handle("DELETE /v1/device/self", s.requireDevice(s.withDeviceLimit(http.HandlerFunc(s.handleUnpairSelf))))

	// Read operations. Every one is guarded three times: requireDevice proves
	// who is calling, withDeviceLimit bounds how often, requireOp proves the
	// host's startup policy permits it.
	read := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireDevice(s.withDeviceLimit(s.requireOp(policy.OpRead, h))))
	}
	read("GET /v1/containers", s.handleListContainers)
	read("GET /v1/containers/{id}", s.handleInspectContainer)
	read("GET /v1/images", s.handleListImages)
	read("GET /v1/images/{id}", s.handleInspectImage)
	read("GET /v1/networks", s.handleListNetworks)
	read("GET /v1/networks/{id}", s.handleInspectNetwork)
	read("GET /v1/volumes", s.handleListVolumes)
	read("GET /v1/volumes/{name}", s.handleInspectVolume)

	// Log routes. Same triple guard as the read routes, with policy.OpLogs —
	// which, like OpRead, every mode permits (see internal/policy/mode.go).
	logs := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireDevice(s.withDeviceLimit(s.requireOp(policy.OpLogs, h))))
	}
	logs("GET /v1/containers/{id}/logs", s.handleContainerLogs)
	logs("GET /v1/containers/{id}/logs/stream", s.handleStreamContainerLogs)

	// Mutating operations. Four guards, and the order is load-bearing (D7,
	// D15): requireDevice proves who is calling, withDeviceLimit bounds how
	// often before anything is recorded, withAudit records the attempt
	// whatever happens to it, requireOp proves the host's startup policy
	// permits it.
	mutate := func(pattern string, op policy.Operation, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireDevice(s.withDeviceLimit(s.withAudit(op, s.requireOp(op, h)))))
	}
	mutate("POST /v1/containers/{id}/start", policy.OpStart, s.handleStartContainer)
	mutate("POST /v1/containers/{id}/restart", policy.OpRestart, s.handleRestartContainer)
	mutate("POST /v1/containers/{id}/stop", policy.OpStop, s.handleStopContainer)
	mutate("POST /v1/containers/{id}/kill", policy.OpKill, s.handleKillContainer)
	mutate("DELETE /v1/containers/{id}", policy.OpDelete, s.handleRemoveContainer)

	return s.withRecovery(s.withRequestLog(mux))
}

// Run serves until ctx is cancelled, then drains for up to shutdownGrace.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		// Both arguments empty: the certificate comes from TLSConfig.
		// Passing paths here would re-read from disk and bypass everything
		// internal/certs guarantees about generation and permissions.
		errCh <- s.http.ListenAndServeTLS("", "")
	}()

	select {
	case err := <-errCh:
		// ErrServerClosed means someone called Shutdown or Close. Treating it as
		// a failure would make every clean SIGTERM exit non-zero, and Docker
		// would record the container as failed.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve https on %s: %w", s.cfg.ListenAddr, err)
		}
		return nil
	case <-ctx.Done():
		return s.shutdown()
	}
}

func (s *Server) shutdown() error {
	s.log.Info("shutting down http server")

	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to drain against.
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(sctx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
