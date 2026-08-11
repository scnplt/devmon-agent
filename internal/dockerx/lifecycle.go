// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"time"

	"github.com/moby/moby/client"
)

const (
	// lifecycleTimeout bounds every lifecycle Engine call. Larger than
	// callTimeout because a stop carries stopGraceSeconds of waiting plus
	// process teardown plus the pre-flight inspect, and smaller than the HTTP
	// server's 30s WriteTimeout so a slow stop becomes a 502 the agent
	// controls rather than a response that dies mid-body (D13).
	lifecycleTimeout = 20 * time.Second

	// stopGraceSeconds is how long the Engine waits for a container to exit
	// on its own before SIGKILL. Passed explicitly rather than left nil: nil
	// means "the container's configured timeout", which an operator can set
	// to -1 (wait forever) and hang the request (D12).
	stopGraceSeconds = 10
)

// resolveTarget validates ref, resolves it to a full container ID, and
// enforces the agent's permanent self-exclusion. Every lifecycle method goes
// through it — that is what makes "the agent can never act on itself" a
// property of this layer rather than of five route registrations (D1).
func (c *Client) resolveTarget(ctx context.Context, ref string) (string, error) {
	if err := ValidateRef(ref); err != nil {
		return "", err
	}
	if c.self.containerized && c.self.id == "" {
		return "", ErrSelfUnknown
	}
	detail, err := c.InspectContainer(ctx, ref)
	if err != nil {
		return "", err
	}
	if detail.ID == c.self.id && c.self.id != "" {
		return "", ErrSelfProtected
	}
	return detail.ID, nil
}

// StartContainer starts a stopped container. Starting an already-running
// container answers 304 from the Engine, which this SDK version treats as a
// successful status and returns with a nil error — so D9's "already in the
// requested state is success" holds here with no special case.
func (c *Client) StartContainer(ctx context.Context, ref string) error {
	id, err := c.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()

	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return classify("start container", err)
	}
	return nil
}

// RestartContainer stops and starts a container, waiting up to
// stopGraceSeconds for it to exit on its own before SIGKILL.
func (c *Client) RestartContainer(ctx context.Context, ref string) error {
	id, err := c.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()

	grace := stopGraceSeconds
	if _, err := c.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &grace}); err != nil {
		return classify("restart container", err)
	}
	return nil
}

// StopContainer stops a running container, waiting up to stopGraceSeconds
// for it to exit on its own before SIGKILL. Stopping an already-stopped
// container answers 304, which returns nil for the same reason described on
// StartContainer (D9).
func (c *Client) StopContainer(ctx context.Context, ref string) error {
	id, err := c.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()

	grace := stopGraceSeconds
	if _, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &grace}); err != nil {
		return classify("stop container", err)
	}
	return nil
}

// KillContainer sends SIGKILL to a running container. The signal is fixed,
// not caller-selectable: the kill button means "stop this now", not a
// config-reload idiom (D11).
func (c *Client) KillContainer(ctx context.Context, ref string) error {
	id, err := c.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()

	if _, err := c.api.ContainerKill(ctx, id, client.ContainerKillOptions{}); err != nil {
		return classify("kill container", err)
	}
	return nil
}

// RemoveContainer deletes a stopped container. A running container yields
// ErrConflict rather than being force-stopped and removed in one step: stop,
// then delete, is two operations, two audit rows, two deliberate taps (D10).
func (c *Client) RemoveContainer(ctx context.Context, ref string) error {
	id, err := c.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()

	if _, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{}); err != nil {
		return classify("remove container", err)
	}
	return nil
}
