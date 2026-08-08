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

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
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
	http    *http.Server
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
	mux.HandleFunc("GET /v1/status", s.handleStatus)

	// Unauthenticated by design (D2): the device has no certificate yet, so
	// the pairing code itself is what authenticates this one call.
	mux.HandleFunc("POST /v1/pair", s.handlePair)

	// Guarded: both act on the calling device's own identity, resolved by
	// requireDevice from its client certificate — never from the request.
	mux.Handle("POST /v1/device/renew", s.requireDevice(http.HandlerFunc(s.handleRenew)))
	mux.Handle("DELETE /v1/device/self", s.requireDevice(http.HandlerFunc(s.handleUnpairSelf)))

	// Read operations. Every one is guarded twice: requireDevice proves who is
	// calling, requireOp proves the host's startup policy permits it.
	read := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpRead, h)))
	}
	read("GET /v1/containers", s.handleListContainers)
	read("GET /v1/containers/{id}", s.handleInspectContainer)
	read("GET /v1/images", s.handleListImages)
	read("GET /v1/images/{id}", s.handleInspectImage)
	read("GET /v1/networks", s.handleListNetworks)
	read("GET /v1/networks/{id}", s.handleInspectNetwork)
	read("GET /v1/volumes", s.handleListVolumes)
	read("GET /v1/volumes/{name}", s.handleInspectVolume)

	// Log routes. Same double guard as the read routes, with policy.OpLogs —
	// which, like OpRead, every mode permits (see internal/policy/mode.go).
	logs := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpLogs, h)))
	}
	logs("GET /v1/containers/{id}/logs", s.handleContainerLogs)
	logs("GET /v1/containers/{id}/logs/stream", s.handleStreamContainerLogs)

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
