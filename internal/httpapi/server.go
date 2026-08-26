// SPDX-License-Identifier: AGPL-3.0-only

// Package httpapi serves the agent's HTTPS API on its single listening port.
package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
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
	// host it exists to protect. A constant rather than an env var: every extra
	// startup setting is surface the operator has to understand at install time,
	// and eight concurrent log views on one phone is already beyond any real use.
	// This global ceiling is unchanged by the per-device cap below (issue #80);
	// only how the eight are shared across devices changes.
	maxConcurrentStreams = 8

	// maxStreamsPerDevice bounds how many of maxConcurrentStreams a single
	// device may hold at once (issue #80). Without this, one device holding
	// every stream slot answers 503 for every other paired device — the
	// budget was global-only and had no notion of who was calling. Three is
	// generous for a couple of log views open on one phone, while still
	// keeping the global ceiling reachable in practice: three devices at
	// their own cap sum past eight, so the host-wide limit stays meaningful
	// and testable rather than becoming unreachable dead code.
	maxStreamsPerDevice = 3
)

// There is deliberately no maxEventStreamsPerDevice constant: eventRegistry
// admits exactly one live event stream per device by construction — a second
// register() call for the same device evicts the first rather than being
// refused, so there is no counter to compare a constant against.

// Server owns the HTTPS listener and its routes.
type Server struct {
	cfg config.Config
	st  *state.Store
	ca  *certs.CA
	// dc is the Docker read surface the eight read routes depend on.
	dc  DockerReader
	log *slog.Logger
	// streams bounds concurrent live log streams (D10), with a global
	// ceiling and a per-device cap under it (issue #80). Built here rather
	// than lazily in the handler: a nil *streamBudget would panic on first
	// use, and this way construction failure is impossible by
	// construction — NewServer always builds one.
	streams *streamBudget

	// events owns the agent's single shared Docker events subscription and
	// fans it out to every attached client (D5/D6); eventStreams enforces
	// one live event stream per paired device, newest wins (D11). Built
	// here, never lazily, for the same "construction failure is impossible
	// by construction" reason streams is.
	events       *eventHub
	eventStreams *eventRegistry

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

	// lifecycleCtx is the server's own lifetime signal, wired in as
	// http.Server's BaseContext (issue #41). Every request's r.Context() is
	// derived from it, so cancelling it cancels every in-flight request's
	// context in one step — chiefly the one long-lived handler,
	// handleStreamContainerLogs, whose stream is otherwise bounded only by
	// the client. shutdown cancels it before calling http.Server.Shutdown,
	// so a live stream unwinds immediately instead of pinning Shutdown for
	// the full shutdownGrace window. Client-initiated disconnects are
	// unaffected: they still cancel r.Context() the same way they always
	// did, independent of this context's own lifetime.
	lifecycleCtx    context.Context
	cancelLifecycle context.CancelFunc

	// afterCreateHook is a test seam, nil in production. pairDevice calls it,
	// if set, right after CreateDevice succeeds and before RedeemPairingCode
	// runs, so a test can land a context cancellation at that exact
	// interleaving point deterministically instead of racing the SQL layer
	// with a sleep. It is per-instance rather than a package-level var so
	// setting it on one test's Server can never race a different test's.
	afterCreateHook func()

	// newConnMu guards newConns and draining below (issue #117). A
	// connection that completed its TLS handshake but never sent a request
	// sits in http.StateNew for the rest of its life; http.Server.Shutdown
	// only treats such a connection as closable-idle once its state stamp
	// is more than shutdownGrace old, with one-second stamp granularity, so
	// it can pin Shutdown for the entire grace window. Tracking every
	// StateNew connection here lets shutdown close them itself instead of
	// waiting on that stamp.
	newConnMu sync.Mutex
	newConns  map[net.Conn]struct{}
	// draining is set by shutdown before it closes the tracked connections
	// above. The ConnState hook checks it so a connection that reaches
	// StateNew after draining started — in the window between shutdown
	// beginning and http.Server.Shutdown closing the listener — is closed
	// immediately too, rather than surviving to pin the grace window.
	draining bool
}

// NewServer wires the API. tlsCfg carries the server certificate, so the
// listener never re-reads it from disk. ca is retained on Server — not just
// its fingerprint — because guarded routes added in a later phase issue and
// renew device certificates from it; the status handler derives the public
// fingerprint from it on each call. ca may be nil in tests that do not
// exercise certificate issuance; handleStatus tolerates that by serving an
// empty fingerprint, and handlePair and handleRenew tolerate it by answering
// 500 through requireCA rather than panicking inside IssueDeviceCert. dc may
// likewise be nil in tests that do not exercise the Docker read routes; every
// read handler tolerates that by serving 502 instead of panicking.
func NewServer(cfg config.Config, st *state.Store, ca *certs.CA, dc DockerReader, tlsCfg *tls.Config, log *slog.Logger) *Server {
	s := &Server{
		cfg: cfg, st: st, ca: ca, dc: dc, log: log,
		streams:  newStreamBudget(maxConcurrentStreams, maxStreamsPerDevice),
		newConns: make(map[net.Conn]struct{}),
	}
	s.lifecycleCtx, s.cancelLifecycle = context.WithCancel(context.Background())
	s.events = newEventHub(dc, log)
	s.eventStreams = newEventRegistry()

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
		// BaseContext parents every request's r.Context(): cancelling
		// lifecycleCtx in shutdown cancels every in-flight request's context
		// in one step, which is how a live SSE stream (issue #41) learns to
		// stop without Shutdown having to wait out the client.
		BaseContext: func(net.Listener) context.Context { return s.lifecycleCtx },
		// ConnState tracks every connection whose current state is
		// http.StateNew (issue #117): shutdown closes them directly instead
		// of waiting on Shutdown's own idle-since-state-stamp check, which a
		// handshake-only, request-less connection can outlive for the whole
		// shutdownGrace window. See trackConnState's doc comment.
		ConnState: s.trackConnState,
	}
	return s
}

// trackConnState is the http.Server ConnState hook (issue #117). It keeps
// newConns as the current set of connections in http.StateNew — added on
// StateNew, removed the moment a connection leaves it for StateActive,
// StateIdle, StateHijacked, or StateClosed — so shutdown can close every
// still-StateNew connection itself rather than rely on Shutdown's own
// idle-since-state-stamp check, which treats a StateNew connection as
// closable-idle only once that stamp is more than shutdownGrace old.
//
// If draining is already true when a connection reaches StateNew, shutdown
// has already swept the connections it knew about; this one arrived in the
// gap between that sweep and http.Server.Shutdown closing the listener, so
// it is closed here immediately instead of being left to pin the grace
// window on its own.
//
// There is an acknowledged, accepted race: a connection can transition
// StateNew -> StateActive concurrently with shutdown closing it here. That
// is fine during shutdown — the lifecycle context is already cancelled by
// the time shutdown reaches this path, so any handler that connection's
// request might have reached is already unwinding.
func (s *Server) trackConnState(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.newConnMu.Lock()
		draining := s.draining
		if !draining {
			s.newConns[conn] = struct{}{}
		}
		s.newConnMu.Unlock()
		if draining {
			_ = conn.Close()
		}
	case http.StateActive, http.StateIdle, http.StateHijacked, http.StateClosed:
		s.newConnMu.Lock()
		delete(s.newConns, conn)
		s.newConnMu.Unlock()
	}
}

// closeNewConns marks the server as draining and closes every connection
// currently tracked as http.StateNew (issue #117). Called from shutdown
// before http.Server.Shutdown, so a handshake-only, request-less connection
// — which carries no request context for lifecycleCtx cancellation to reach
// — is closed directly instead of pinning Shutdown for the whole
// shutdownGrace window waiting on its state stamp to age out.
func (s *Server) closeNewConns() {
	s.newConnMu.Lock()
	s.draining = true
	conns := s.newConns
	s.newConns = make(map[net.Conn]struct{})
	s.newConnMu.Unlock()

	for conn := range conns {
		_ = conn.Close()
	}
}

// routes builds the mux and wraps it in the middleware chain.
//
// No catch-all "/" handler is registered: an unmatched path gets ServeMux's
// bare 404, which tells an unauthenticated scanner nothing.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// patterns is the closed set of literals registered below, the same set
	// withRoute checks a resolved match against before logging it (see
	// middleware.go). handle is the single place that both registers a
	// pattern with mux and records it here, so the two can never drift.
	patterns := make(map[string]struct{})
	handle := func(pattern string, h http.Handler) {
		mux.Handle(pattern, h)
		patterns[pattern] = struct{}{}
	}

	// The Go 1.22+ method pattern matters. Registering "/v1/status" alone would
	// also match POST, DELETE, and everything else.
	//
	// Rate-limit order (the Rate-Limiting Contract): the global unauthenticated
	// backstop runs first, then the route's own per-IP tier.
	handle("GET /v1/status",
		s.withGlobalUnauthLimit(s.withIPLimit(s.statusLimits, s.statusLimit, "status", http.HandlerFunc(s.handleStatus))))

	// Unauthenticated by design (D2): the device has no certificate yet, so
	// the pairing code itself is what authenticates this one call.
	// withPairAudit sits inside both rate-limit tiers (D7): a request either
	// one refuses must never reach it, or a throttled scanner could fill the
	// audit table (issue #44).
	handle("POST /v1/pair",
		s.withGlobalUnauthLimit(s.withIPLimit(s.pairLimits, s.pairLimit, "pair", s.withPairAudit(http.HandlerFunc(s.handlePair)))))

	// Guarded: both act on the calling device's own identity, resolved by
	// requireDevice from its client certificate — never from the request.
	// withDeviceLimit sits immediately inside requireDevice, before anything
	// else, exactly as it does for the read/logs/mutate helpers below.
	// withIdentityAudit sits inside withDeviceLimit for the same reason
	// withAudit does on the mutate routes below (D7, D15, issue #44): a
	// throttled call must never write a row, and every row must carry a real,
	// authenticated device.
	handle("POST /v1/device/renew",
		s.requireDevice(s.withDeviceLimit(s.withIdentityAudit(opRenew, http.HandlerFunc(s.handleRenew)))))
	handle("DELETE /v1/device/self",
		s.requireDevice(s.withDeviceLimit(s.withIdentityAudit(opUnpairSelf, http.HandlerFunc(s.handleUnpairSelf)))))

	// Read operations. Every one is guarded three times: requireDevice proves
	// who is calling, withDeviceLimit bounds how often, requireOp proves the
	// host's startup policy permits it.
	read := func(pattern string, h http.HandlerFunc) {
		handle(pattern, s.requireDevice(s.withDeviceLimit(s.requireOp(policy.OpRead, h))))
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
		handle(pattern, s.requireDevice(s.withDeviceLimit(s.requireOp(policy.OpLogs, h))))
	}
	logs("GET /v1/containers/{id}/logs", s.handleContainerLogs)
	logs("GET /v1/containers/{id}/logs/stream", s.handleStreamContainerLogs)

	// The container event stream. Registered through `read`, not a bespoke
	// chain: it discloses a strict subset of GET /v1/containers, so
	// policy.OpRead is its honest tier (D3), and reusing the same closure is
	// what guarantees the three guards match rather than merely resemble the
	// read routes'.
	read("GET /v1/events/stream", s.handleEventStream)

	// Mutating operations. Four guards, and the order is load-bearing (D7,
	// D15): requireDevice proves who is calling, withDeviceLimit bounds how
	// often before anything is recorded, withAudit records the attempt
	// whatever happens to it, requireOp proves the host's startup policy
	// permits it.
	mutate := func(pattern string, op policy.Operation, h http.HandlerFunc) {
		handle(pattern, s.requireDevice(s.withDeviceLimit(s.withAudit(op, s.requireOp(op, h)))))
	}
	mutate("POST /v1/containers/{id}/start", policy.OpStart, s.handleStartContainer)
	mutate("POST /v1/containers/{id}/restart", policy.OpRestart, s.handleRestartContainer)
	mutate("POST /v1/containers/{id}/stop", policy.OpStop, s.handleStopContainer)
	mutate("POST /v1/containers/{id}/kill", policy.OpKill, s.handleKillContainer)
	mutate("DELETE /v1/containers/{id}", policy.OpDelete, s.handleRemoveContainer)

	// withRoute sits outermost: it resolves the matched pattern once (or
	// unmatchedRoute for anything not in patterns) before either logger runs,
	// so withRecovery and withRequestLog both log that bounded pattern
	// instead of the attacker-controlled r.URL.Path (issue #46). It only
	// inspects the request via mux.Handler; mux itself still dispatches,
	// inside withRequestLog's next.ServeHTTP call, exactly once.
	return s.withRoute(mux, patterns, s.withRecovery(s.withRequestLog(mux)))
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

	// Cancel the shared lifecycle context first, before asking http.Server
	// to drain: it is the parent of every in-flight request's context, so
	// this is what makes a live SSE log stream (issue #41) return promptly
	// instead of pinning Shutdown below for the full shutdownGrace window.
	s.cancelLifecycle()

	// The hub's goroutine already unwinds on the cancelled lifecycle context
	// above; stop() is what makes that deterministic rather than eventual,
	// so a goroutine-leak test can assert it right after shutdown returns
	// instead of racing the hub's own teardown.
	s.events.stop()

	// Close every connection still in http.StateNew (issue #117) before
	// asking http.Server to drain. A connection that completed its TLS
	// handshake but never sent a request has no request context for
	// cancelLifecycle above to reach, and Shutdown itself only treats a
	// StateNew connection as closable-idle once its state stamp is more
	// than shutdownGrace old — with one-second stamp granularity, that can
	// pin Shutdown for the entire grace window on a connection that was
	// never going to send anything. See trackConnState's doc comment for
	// why closing it here, mid-transition, is an accepted race rather than
	// a bug.
	s.closeNewConns()

	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to drain against.
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(sctx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
