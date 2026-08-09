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

	// The `device` CLI is a host-side management path (D8): it opens the same
	// SQLite file the running agent has open and must never build a log sink
	// or touch certs/ — see cli.go for why. It is dispatched before the
	// server path so it never triggers state-dir prep, log-sink construction,
	// or certificate loading meant only for the long-running agent.
	if flag.Arg(0) == "device" {
		return runDeviceCommand(context.Background(), cfg, flag.Args()[1:])
	}

	// The `audit list` CLI (D19) is the audit trail's only reader — it is
	// deliberately not exposed over the HTTPS API (D20). It is dispatched here
	// for the same reason as `device`: it must never trigger state-dir prep,
	// log-sink construction, or certificate loading meant only for the
	// long-running agent.
	if flag.Arg(0) == "audit" {
		return runAuditCommand(context.Background(), cfg, flag.Args()[1:])
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

	// The identity consistency check MUST run before LoadOrCreateCA: once the CA
	// is created, the partial-restore signature this check exists to catch can
	// never fire again (D9).
	if err := certs.CheckIdentityConsistency(cfg.CertsDir(), !st.FirstRun); err != nil {
		return err
	}

	ca, caCreated, err := certs.LoadOrCreateCA(cfg.CertsDir(), log)
	if err != nil {
		return err
	}
	if caCreated {
		log.Warn("generated a new certificate authority; record this fingerprint, it is the pairing anchor every device pins",
			slog.String("fingerprint", ca.Fingerprint()),
		)
	}

	// certs already logs the WARN on SAN drift and re-issues the leaf from ca
	// automatically; no device has to re-pair because clients pin the CA, not
	// this leaf.
	cert, err := certs.LoadOrCreateServerCert(cfg.CertsDir(), cfg.PublicAddrs, ca, log)
	if err != nil {
		return err
	}

	dc, err := dockerx.New(ctx, cfg.DockerHost, cfg.SelfContainerID, log)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	// Client CAs come from the agent's own certificate authority: a device's
	// client certificate is verified against ca.Pool() at the handshake.
	tlsCfg := tlsconf.Build(cert, ca.Pool())
	srv := httpapi.NewServer(cfg, st, ca, dc, tlsCfg, log)
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
