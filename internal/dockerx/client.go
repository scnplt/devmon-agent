// Package dockerx wraps the Docker Engine client.
//
// Phase 1 exposes only New and Close. No container, image, network, or volume
// operation belongs here yet — reads are Phase 3 and lifecycle control is
// Phase 5. Keeping the surface this narrow is what makes the policy tier
// enforceable later: every capability arrives with the phase that audits it.
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
	api *client.Client
	log *slog.Logger
}

// New dials the Docker Engine at host and verifies it responds.
//
// A host whose Engine is unreachable cannot serve any agent operation, so this
// is a fatal startup error rather than a degraded mode: an agent that starts and
// then fails every request is harder to diagnose than one that refuses to start.
func New(ctx context.Context, host string, log *slog.Logger) (*Client, error) {
	// WithHost, not client.FromEnv: the socket path comes from the agent's own
	// validated configuration and must not be redirectable by an unrelated
	// DOCKER_HOST in the process environment.
	api, err := client.New(
		client.WithHost(host),
		client.WithUserAgent("devmon-agent/"+version.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client for %s: %w", host, err)
	}

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	// Ping takes an options struct in the v29 SDK, and version negotiation moved
	// onto it from the deprecated client.WithAPIVersionNegotiation option.
	res, err := api.Ping(pctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		_ = api.Close()
		return nil, fmt.Errorf("ping docker engine at %s: %w", host, err)
	}

	log.Info("docker engine reachable",
		slog.String("api_version", res.APIVersion),
		slog.String("os_type", res.OSType),
	)
	return &Client{api: api, log: log}, nil
}

// Close releases the Engine connection.
func (c *Client) Close() error {
	if err := c.api.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}
	return nil
}
