// SPDX-License-Identifier: AGPL-3.0-only

// Package dockerx wraps the Docker Engine client.
//
// It exposes the narrow set of read and lifecycle operations the agent's HTTP
// API needs, projected onto an explicit allowlist of DTOs (D1) so nothing
// forwards an Engine struct verbatim. Every mutating call is enforced through
// one chokepoint, resolveTarget, which refuses the agent's own container and
// any container matching the operator's DEVMON_PROTECTED_CONTAINERS list.
package dockerx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/version"
)

// pingTimeout bounds the startup reachability check. An unreachable Engine must
// surface as a specific startup error, not as a hang the operator has to guess at.
const pingTimeout = 5 * time.Second

// Client is the agent's handle on the Docker Engine.
type Client struct {
	api       *client.Client
	log       *slog.Logger
	self      selfInfo
	protected protectedSet
}

// Options configures New. Every field is startup configuration, read once and
// never changed afterward (D5): a client can never widen what the operator
// granted at process start.
type Options struct {
	// Host is DEVMON_DOCKER_HOST, the Engine endpoint to dial.
	Host string
	// SelfContainer is the operator's DEVMON_SELF_CONTAINER override, or ""
	// when unset. It is the escape hatch for the rare host where every
	// filesystem-derived candidate is wrong (D2).
	SelfContainer string
	// ProtectedContainers is the operator's DEVMON_PROTECTED_CONTAINERS
	// entries (names or IDs), or nil when unset. A container matching any
	// entry is refused by every lifecycle route and reported as protected in
	// list and inspect responses, exactly like the agent's own container.
	ProtectedContainers []string
}

// New dials the Docker Engine at opts.Host and verifies it responds.
//
// A host whose Engine is unreachable cannot serve any agent operation, so this
// is a fatal startup error rather than a degraded mode: an agent that starts and
// then fails every request is harder to diagnose than one that refuses to start.
func New(ctx context.Context, opts Options, log *slog.Logger) (*Client, error) {
	// WithHost, not client.FromEnv: the socket path comes from the agent's own
	// validated configuration and must not be redirectable by an unrelated
	// DOCKER_HOST in the process environment.
	api, err := client.New(
		client.WithHost(opts.Host),
		client.WithUserAgent("devmon-agent/"+version.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client for %s: %w", opts.Host, err)
	}

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	// Ping takes an options struct in the v29 SDK, and version negotiation moved
	// onto it from the deprecated client.WithAPIVersionNegotiation option.
	res, err := api.Ping(pctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		_ = api.Close()
		return nil, fmt.Errorf("ping docker engine at %s: %w", opts.Host, err)
	}

	log.Info("docker engine reachable",
		slog.String("api_version", res.APIVersion),
		slog.String("os_type", res.OSType),
	)

	c := &Client{api: api, log: log}
	// Resolved once here, right after Ping, so Client stays immutable for
	// its whole life (D5) — no later write for concurrent handlers to race.
	c.self = c.resolveSelf(ctx, opts.SelfContainer)
	c.protected = newProtectedSet(opts.ProtectedContainers)
	if !c.protected.empty() {
		// Names are not secrets; this is the operator's own confirmation of
		// what it configured, worth one INFO line at startup.
		log.Info("protected containers configured", slog.Any("entries", opts.ProtectedContainers))
	}
	return c, nil
}

// Close releases the Engine connection.
func (c *Client) Close() error {
	if err := c.api.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}
	return nil
}
