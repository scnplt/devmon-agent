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

	"github.com/scnplt/devmon-agent/internal/config"
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
)

// Server owns the HTTPS listener and its routes.
type Server struct {
	cfg  config.Config
	st   *state.Store
	log  *slog.Logger
	http *http.Server
}

// NewServer wires the API. tlsCfg carries the server certificate, so the
// listener never re-reads it from disk.
func NewServer(cfg config.Config, st *state.Store, tlsCfg *tls.Config, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, log: log}

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
