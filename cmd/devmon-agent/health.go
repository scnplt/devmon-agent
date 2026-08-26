// SPDX-License-Identifier: AGPL-3.0-only

// `health` is the process that runs inside the distroless/static image's
// HEALTHCHECK, 2880 times a day. The image has no shell and no curl, so a
// HEALTHCHECK is only implementable if the agent's own binary can probe
// itself — this subcommand exists for exactly that reason and no other.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
)

// healthClientTimeout bounds the whole probe request. It must sit comfortably
// under the Dockerfile's HEALTHCHECK --timeout=5s so a slow-but-alive listener
// is reported unhealthy by this client, with a readable reason, rather than by
// Docker killing the process with no explanation at all.
const healthClientTimeout = 3 * time.Second

// healthStatusPath is the one route this probes, chosen because it is the only
// unauthenticated GET the agent serves. The listener's ClientAuth is
// VerifyClientCertIfGiven (internal/tlsconf), so a client presenting no
// certificate completes the handshake and gets a 200 — which is what makes
// this route reachable from a process that must never touch certs/.
const healthStatusPath = "/v1/status"

// loopbackHost is dialed whenever the configured listener is bound to every
// interface. Docker's HEALTHCHECK runs inside the same network namespace as
// the agent, so loopback is always a valid path to a listener bound to
// 0.0.0.0 or [::].
const loopbackHost = "127.0.0.1"

// runHealthCommand implements `devmon-agent health`. It performs one HTTPS GET
// against the agent's own listener and returns nil for healthy or an error for
// unhealthy — main() maps a non-nil error to exit code 1, which is what
// Docker's HEALTHCHECK reads as "unhealthy".
//
// It takes no subcommand and no flags, matching how `device revoke` rejects
// the wrong argument count: a HEALTHCHECK line is fixed at image build time
// and a stray argument here would otherwise fail silently 2880 times a day
// instead of being caught once, at review time.
//
// Like `device` and `audit` (see cli.go), this MUST NOT build a log sink,
// prepare the state directory, or load certificates: it runs on a 30-second
// interval forever, so any cost paid here is paid 2880 times a day, and a
// second process touching certs/ could race the running agent's own startup
// exactly as cli.go's comment on runDeviceCommand explains.
func runHealthCommand(ctx context.Context, cfg config.Config, args []string) error {
	if helpRequested(args) {
		printHealthUsage(os.Stdout)
		return nil
	}
	if len(args) != 0 {
		return fmt.Errorf("health: unexpected argument %q (health takes no arguments)", args[0])
	}

	addr, err := healthTargetAddr(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}

	return probeStatus(ctx, addr)
}

// healthTargetAddr derives the address to dial from the agent's own
// DEVMON_LISTEN_ADDR. An empty host, "0.0.0.0", or "::" means the listener is
// bound to every interface, and loopback is the correct way in from inside
// the container that owns it. Any other host means an operator bound the
// listener to one specific address on purpose, and that address is dialed
// as configured rather than second-guessed.
func healthTargetAddr(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse listen address %q: %w", listenAddr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = loopbackHost
	}
	return net.JoinHostPort(host, port), nil
}

// probeStatus performs the single HTTPS GET and maps its outcome to healthy
// (nil) or unhealthy (error), printing a one-line reason so `docker inspect`'s
// health log stays readable.
func probeStatus(ctx context.Context, addr string) error {
	client := &http.Client{
		Timeout: healthClientTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// #nosec G402 -- The server certificate is issued by the
				// agent's own CA for DEVMON_PUBLIC_ADDR's SANs, which do not
				// include the loopback address this probe dials. This check
				// measures liveness of the listener from inside the container
				// that owns it, not the identity of the peer — verifying the
				// chain here would require a second process reading certs/,
				// which the CLI deliberately never does (see cli.go's comment
				// on runDeviceCommand racing the agent's own startup).
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS13,
			},
			// A fresh, single-use transport: the process exits after one
			// request, so a connection pool buys nothing, and leaving the
			// connection open would hold an idle server-side connection slot
			// 2880 times a day for no benefit.
			DisableKeepAlives: true,
		},
	}

	url := "https://" + addr + healthStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("healthy: GET /v1/status returned 200")
		return nil
	case http.StatusTooManyRequests:
		// A 429 proves the listener is accepting connections, running the
		// full middleware chain, and answering — exactly and only what this
		// probe measures. An operator who lowers DEVMON_RATE_STATUS_PER_MIN
		// far enough must not thereby make their container permanently
		// unhealthy, so a rate-limited response counts as healthy too.
		fmt.Println("healthy: GET /v1/status returned 429 (rate-limited, listener is answering)")
		return nil
	default:
		return fmt.Errorf("unhealthy: GET /v1/status returned %d", resp.StatusCode)
	}
}
