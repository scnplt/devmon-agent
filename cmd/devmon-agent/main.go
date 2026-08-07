// Command devmon-agent runs the DevMon Docker control agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/httpapi"
	"github.com/scnplt/devmon-agent/internal/logging"
	"github.com/scnplt/devmon-agent/internal/state"
	"github.com/scnplt/devmon-agent/internal/tlsconf"
	"github.com/scnplt/devmon-agent/internal/version"
)

// Exit codes. 2 is reserved for configuration faults so an operator (or an
// installer script) can tell "you typed something wrong" apart from "it broke".
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

// stateDirMode keeps the state directory owner-only; it holds the key material.
const stateDirMode = 0o700

// main does nothing but map run's error to an exit code. os.Exit skips deferred
// calls, so any defer placed here would silently never run — including the log
// flush.
func main() {
	if err := run(); err != nil {
		var vErr *config.ValidationError
		if errors.As(err, &vErr) {
			fmt.Fprintln(os.Stderr, vErr.Error())
			os.Exit(exitConfig)
		}
		fmt.Fprintf(os.Stderr, "devmon-agent: %v\n", err)
		os.Exit(exitFailure)
	}
	os.Exit(exitOK)
}

func run() error {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("devmon-agent %s (commit %s, built %s)\n",
			version.Version, version.Commit, version.BuildTime)
		return nil
	}

	// Configuration is read and validated before the log sink exists, so a bad
	// docker run line lands on stderr as plain readable text rather than as
	// structured slog lines an operator has to squint at.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	if err := prepareStateDir(cfg); err != nil {
		return err
	}

	sink, err := logging.NewSink(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()
	log := sink.Logger

	// Both signals: Docker sends SIGTERM, and without handling it `docker stop`
	// waits out its full timeout and then SIGKILLs, which corrupts the WAL.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, sink, log)
}

// prepareStateDir creates the bind-mounted layout. A failure here is almost
// always the host prerequisite from the README: the mount is not owned by the
// container's nonroot UID.
func prepareStateDir(cfg config.Config) error {
	for _, dir := range []string{cfg.StateDir, cfg.CertsDir(), cfg.LogsDir()} {
		if err := os.MkdirAll(dir, stateDirMode); err != nil {
			return fmt.Errorf("create state dir %s: %w", dir, err)
		}
	}
	return nil
}

// serve constructs every component in dependency order and runs the long-lived
// ones concurrently. Each construction failure is fatal and specific: an agent
// that starts and then fails every request is harder to diagnose than one that
// refuses to start.
func serve(ctx context.Context, cfg config.Config, sink *logging.Sink, log *slog.Logger) error {
	st, err := state.Open(ctx, cfg.DBPath(), log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	schemaVersion, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("state store opened",
		slog.String("path", cfg.DBPath()),
		slog.Bool("first_run", st.FirstRun),
		slog.Int("schema_version", schemaVersion),
	)

	// certs already logs the WARN on drift; re-issuance needs the Phase 2 CA.
	cert, _, err := certs.LoadOrCreateServerCert(cfg.CertsDir(), cfg.PublicAddrs, log)
	if err != nil {
		return err
	}

	dc, err := dockerx.New(ctx, cfg.DockerHost, log)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	// nil client CAs: there is no CA until Phase 2, so no client certificate can
	// be verified against anything.
	srv := httpapi.NewServer(cfg, st, tlsconf.Build(cert, nil), log)
	pruner := state.NewPruner(st, cfg.AuditMaxAge, cfg.AuditMaxRows, log)

	log.Info("agent listening",
		slog.String("addr", cfg.ListenAddr),
		slog.String("policy", cfg.PolicyMode.String()),
		slog.String("version", version.Version),
	)

	return runAll(ctx, srv.Run, sink.Run, pruner.Run)
}

// runAll runs every component until ctx is cancelled and returns the first real
// failure.
//
// The derived context is cancelled as soon as ANY component returns. Without
// that, a listener that fails to bind would leave the rotator and pruner
// ticking forever and the wait below would never finish — the agent would hang
// instead of reporting the error it already has.
//
// Deferred closes in serve then unwind in reverse construction order — HTTP
// server, Docker client, state store, log sink — because closing the state store
// while a request is still in flight is the classic shutdown bug.
func runAll(ctx context.Context, components ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(components))

	for _, component := range components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			errs <- component(ctx)
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		// Cancellation is how a clean shutdown reaches these components, not a
		// failure. Reporting it would make every SIGTERM exit non-zero.
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}
