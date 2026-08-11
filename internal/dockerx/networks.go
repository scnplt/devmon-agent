// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ListNetworks returns every network on the host, capped at maxListItems.
func (c *Client) ListNetworks(ctx context.Context) (ListResult[NetworkSummary], error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return ListResult[NetworkSummary]{}, classify("list networks", err)
	}

	items := make([]NetworkSummary, 0, len(res.Items))
	for _, s := range res.Items {
		items = append(items, toNetworkSummary(s.Network))
	}

	items, truncated := truncate(items)

	return ListResult[NetworkSummary]{Items: items, Truncated: truncated}, nil
}

// InspectNetwork returns the full projection of a single network, including
// its attached containers. ref is validated before any Engine call reaches
// out over the network.
func (c *Client) InspectNetwork(ctx context.Context, ref string) (NetworkDetail, error) {
	if err := ValidateRef(ref); err != nil {
		return NetworkDetail{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.NetworkInspect(ctx, ref, client.NetworkInspectOptions{})
	if err != nil {
		return NetworkDetail{}, classify("inspect network", err)
	}

	return toNetworkDetail(res.Network), nil
}

// toNetworkSummary projects a network.Network onto the allowlisted DTO.
// network.Summary and network.Inspect both embed network.Network, so this one
// mapper serves list, and detail (via toNetworkDetail below).
//
// Created's declared type is network.Network.Created (documented as
// timeext.Time in the upstream swagger-generated file). The import in that
// file is `timeext "time"` — an alias for the standard library "time"
// package, not a distinct named type — so Created is a plain time.Time and
// needs no conversion.
func toNetworkSummary(n network.Network) NetworkSummary {
	return NetworkSummary{
		ID:         n.ID,
		Name:       n.Name,
		Driver:     n.Driver,
		Scope:      n.Scope,
		CreatedAt:  n.Created.UTC().Format(time.RFC3339),
		Internal:   n.Internal,
		EnableIPv6: n.EnableIPv6,
		Labels:     defaultLabels(n.Labels),
	}
}

// toNetworkDetail projects a network.Inspect onto the allowlisted DTO,
// including its attached containers.
func toNetworkDetail(n network.Inspect) NetworkDetail {
	containers := make([]NetworkEndpoint, 0, len(n.Containers))
	for id, ep := range n.Containers {
		containers = append(containers, toNetworkEndpoint(id, ep))
	}

	return NetworkDetail{
		NetworkSummary: toNetworkSummary(n.Network),
		Containers:     containers,
	}
}

// toNetworkEndpoint projects a single container's attachment to an inspected
// network onto the allowlisted DTO. IPv4Address and IPv6Address are
// netip.Prefix, not netip.Addr, and must be guarded with IsValid() the same
// way.
func toNetworkEndpoint(containerID string, ep network.EndpointResource) NetworkEndpoint {
	var ipv4, ipv6 string
	if ep.IPv4Address.IsValid() {
		ipv4 = ep.IPv4Address.String()
	}
	if ep.IPv6Address.IsValid() {
		ipv6 = ep.IPv6Address.String()
	}

	return NetworkEndpoint{
		ContainerID: containerID,
		Name:        ep.Name,
		IPv4Address: ipv4,
		IPv6Address: ipv6,
	}
}
