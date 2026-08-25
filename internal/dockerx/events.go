// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ContainerStates returns the current state and health of every container on
// the host, including stopped ones — the payload of the event stream's opening
// snapshot.
//
// One ContainerList call, never a per-container inspect. container.Summary
// carries Health directly, and N+1 inspects taken at N different instants
// would produce a "snapshot" that was never simultaneously true.
//
// Health arrived on the list response in Docker API v1.52 (Engine 29). On an
// older Engine Summary.Health is nil for every container and they all report
// "none"; the events themselves are unaffected.
func (c *Client) ContainerStates(ctx context.Context) ([]ContainerStateSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, classify("list container states", err)
	}

	items := make([]ContainerStateSummary, 0, len(res.Items))
	for _, s := range res.Items {
		items = append(items, toContainerStateSummary(s))
	}

	items, truncated := truncate(items)
	if truncated {
		c.log.Warn("container state snapshot truncated", slog.Int("count", len(res.Items)))
	}

	return items, nil
}

// toContainerStateSummary projects a container.Summary onto the snapshot DTO.
// s.Health is a pointer and is nil for a container with no healthcheck and on
// every Engine older than API v1.52 — both mean "none".
func toContainerStateSummary(s container.Summary) ContainerStateSummary {
	var name string
	if len(s.Names) > 0 {
		name = trimContainerName(s.Names[0])
	}

	return ContainerStateSummary{
		ID:     s.ID,
		Name:   name,
		State:  string(s.State),
		Health: healthOrNone(s.Health),
	}
}

// healthOrNone maps a *container.HealthSummary onto the four-value contract
// vocabulary. A nil summary, an empty status, or a status the Engine invents
// in a future release all collapse to "none": the field is an enum on the
// wire, and forwarding an unrecognised value would break that promise.
func healthOrNone(h *container.HealthSummary) string {
	if h == nil {
		return string(container.NoHealthcheck)
	}

	switch h.Status {
	case container.NoHealthcheck, container.Starting, container.Healthy, container.Unhealthy:
		return string(h.Status)
	default:
		return string(container.NoHealthcheck)
	}
}

// trimContainerName strips the single leading "/" the Engine puts on container
// names in list responses. The events API's own "name" attribute has no slash,
// and the snapshot and the events it is reconciled against must agree.
func trimContainerName(name string) string {
	return strings.TrimPrefix(name, "/")
}
