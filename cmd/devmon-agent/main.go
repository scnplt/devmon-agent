// SPDX-License-Identifier: AGPL-3.0-only

// Command devmon-agent runs the DevMon Docker control agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

// Exit codes. 2 is reserved for configuration faults and CLI usage mistakes
// so an operator (or an installer script) can tell "you typed something
// wrong" apart from "it broke".
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

// stateDirMode keeps the state directory owner-only; it holds the key material.
const stateDirMode = 0o700

// usageError is returned for a CLI mistake that a config.ValidationError
// cannot represent — an unknown command or a bad global flag. main() maps it
// to exitConfig, the same code as a configuration fault, since both mean
// "you typed something wrong" rather than "the agent broke".
type usageError struct {
	msg string
}

func (e *usageError) Error() string { return e.msg }

// unknownCommandHint is appended to the unknown-command error so it is
// visible on stderr even though the full root usage screen is not — an
// unknown command is a mistake, not a help request, and only requested help
// goes to stdout.
const unknownCommandHint = "Run 'devmon help' for usage."

// main does nothing but map run's error to an exit code. os.Exit skips deferred
// calls, so any defer placed here would silently never run — including the log
// flush.
func main() {
	if err := run(os.Args, os.Getenv, os.Stdout, os.Stderr); err != nil {
		var vErr *config.ValidationError
		if errors.As(err, &vErr) {
			fmt.Fprintln(os.Stderr, vErr.Error())
			os.Exit(exitConfig)
		}
		var uErr *usageError
		if errors.As(err, &uErr) {
			fmt.Fprintln(os.Stderr, uErr.Error())
			os.Exit(exitConfig)
		}
		fmt.Fprintf(os.Stderr, "devmon-agent: %v\n", err)
		os.Exit(exitFailure)
	}
	os.Exit(exitOK)
}

// run holds every path through the binary: the operator CLI (device, audit,
// health, help) and the daemon path (container ENTRYPOINT). argv, getenv,
// stdout, and stderr are injected so tests can exercise it without touching
// the process's real os.Args, environment, or standard streams.
//
// Every help path below returns before config.Load is reached — a `--help`
// invocation must not require DEVMON_PUBLIC_ADDR or any other env var, and
// must never open the SQLite state store.
func run(argv []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("devmon-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printRootUsage(stderr) }
	showVersion := fs.Bool("version", false, "print version information and exit")

	if err := fs.Parse(argv[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printRootUsage(stdout)
			return nil
		}
		return &usageError{msg: err.Error()}
	}

	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "devmon-agent %s (commit %s, built %s)\n",
			version.Version, version.Commit, version.BuildTime)
		return nil
	}

	cmd := fs.Arg(0)
	rest := fs.Args()[min(1, fs.NArg()):]

	switch {
	case cmd == "help":
		printRootUsage(stdout)
		return nil
	case cmd == "" && isDevmonAlias(argv[0]):
		printRootUsage(stdout)
		return nil
	case cmd == "":
		// No subcommand under the devmon-agent name: this is the container
		// ENTRYPOINT starting the daemon, unchanged.
	case (cmd == "device" || cmd == "audit" || cmd == "health") && helpRequested(rest):
		printCommandUsage(stdout, cmd)
		return nil
	case cmd == "device" || cmd == "audit" || cmd == "health":
		// Handled below, after config.Load.
	default:
		return &usageError{msg: fmt.Sprintf("unknown command %q\n%s", cmd, unknownCommandHint)}
	}

	// Configuration is read and validated before the log sink exists, so a bad
	// docker run line lands on stderr as plain readable text rather than as
	// structured slog lines an operator has to squint at.
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}

	// The `device` CLI is a host-side management path (D8): it opens the same
	// SQLite file the running agent has open and must never build a log sink
	// or touch certs/ — see cli.go for why. It is dispatched before the
	// server path so it never triggers state-dir prep, log-sink construction,
	// or certificate loading meant only for the long-running agent.
	if cmd == "device" {
		return runDeviceCommand(context.Background(), cfg, rest)
	}

	// The `audit list` CLI (D19) is the audit trail's only reader — it is
	// deliberately not exposed over the HTTPS API (D20). It is dispatched here
	// for the same reason as `device`: it must never trigger state-dir prep,
	// log-sink construction, or certificate loading meant only for the
	// long-running agent.
	if cmd == "audit" {
		return runAuditCommand(context.Background(), cfg, rest)
	}

	// `health` (issue #56) backs the Dockerfile's HEALTHCHECK. It is
	// dispatched here for the same reason as `device` and `audit`: it must
	// never trigger state-dir prep, log-sink construction, or certificate
	// loading meant only for the long-running agent — and doubly so here,
	// since Docker invokes it every 30 seconds for the life of the container.
	if cmd == "health" {
		return runHealthCommand(context.Background(), cfg, rest)
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

// printCommandUsage prints the per-command usage screen for one of
// "device", "audit", or "health". It is only ever called with those three
// values, guarded by the switch in run.
func printCommandUsage(w io.Writer, cmd string) {
	switch cmd {
	case "device":
		printDeviceUsage(w)
	case "audit":
		printAuditUsage(w)
	case "health":
		printHealthUsage(w)
	}
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

	dc, err := dockerx.New(ctx, dockerx.Options{
		Host:                cfg.DockerHost,
		SelfContainer:       cfg.SelfContainer,
		ProtectedContainers: cfg.ProtectedContainers,
	}, log)
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
