// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// callTimeout bounds every Engine call. The HTTP server's WriteTimeout is 30s;
// a wedged Engine must become a 502 the agent controls rather than a response
// that dies mid-body on the client.
const callTimeout = 15 * time.Second

// ListContainers returns every container on the host, or only running ones
// when all is false, capped at maxListItems.
func (c *Client) ListContainers(ctx context.Context, all bool) (ListResult[ContainerSummary], error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: all})
	if err != nil {
		return ListResult[ContainerSummary]{}, classify("list containers", err)
	}

	items := make([]ContainerSummary, 0, len(res.Items))
	for _, s := range res.Items {
		items = append(items, toContainerSummary(s, c.self.id))
	}

	items, truncated := truncate(items)

	return ListResult[ContainerSummary]{Items: items, Truncated: truncated}, nil
}

// InspectContainer returns the full projection of a single container. ref is
// validated before any Engine call reaches out over the network.
func (c *Client) InspectContainer(ctx context.Context, ref string) (ContainerDetail, error) {
	if err := ValidateRef(ref); err != nil {
		return ContainerDetail{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// Size: false — calculating the container's filesystem size walks the
	// filesystem and is not needed for this projection.
	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerDetail{}, classify("inspect container", err)
	}

	return toContainerDetail(res.Container, c.self.id), nil
}

// toContainerSummary projects a container.Summary onto the allowlisted DTO.
// s.Health and s.NetworkSettings are nil for ordinary containers (no
// healthcheck, no attached network) and must be guarded. selfID is the
// agent's own resolved container ID (D18); "" means self-resolution never
// succeeded, so nothing can ever be marked protected.
func toContainerSummary(s container.Summary, selfID string) ContainerSummary {
	var health string
	if s.Health != nil {
		health = string(s.Health.Status)
	}

	ports := make([]Port, 0, len(s.Ports))
	for _, p := range s.Ports {
		ports = append(ports, toPort(p))
	}

	names := make([]string, 0, len(s.Names))
	names = append(names, s.Names...)

	return ContainerSummary{
		ID:        s.ID,
		Names:     names,
		Image:     s.Image,
		ImageID:   s.ImageID,
		Command:   s.Command,
		CreatedAt: unixToRFC3339(s.Created),
		State:     string(s.State),
		Status:    s.Status,
		Health:    health,
		Labels:    defaultLabels(s.Labels),
		Ports:     ports,
		Protected: s.ID == selfID && selfID != "",
	}
}

// toContainerDetail projects a container.InspectResponse onto the allowlisted
// DTO. State, Config, and HostConfig are all nil in ordinary real-world cases
// (a container mid-creation, an image with no config, a restore of an old
// record) and must be guarded individually. selfID is the agent's own
// resolved container ID (D18); "" means self-resolution never succeeded, so
// nothing can ever be marked protected.
func toContainerDetail(r container.InspectResponse, selfID string) ContainerDetail {
	d := ContainerDetail{
		ID:           r.ID,
		Name:         r.Name,
		ImageID:      r.Image,
		CreatedAt:    r.Created,
		Command:      r.Path,
		Platform:     r.Platform,
		RestartCount: r.RestartCount,
		Mounts:       toMounts(r.Mounts),
		Protected:    r.ID == selfID && selfID != "",
	}

	args := make([]string, 0, len(r.Args))
	args = append(args, r.Args...)
	d.Args = args

	if r.State != nil {
		d.State = string(r.State.Status)
		d.Running = r.State.Running
		d.Paused = r.State.Paused
		d.Restarting = r.State.Restarting
		d.ExitCode = r.State.ExitCode
		d.StartedAt = zeroTimeToEmpty(r.State.StartedAt)
		d.FinishedAt = zeroTimeToEmpty(r.State.FinishedAt)
		if r.State.Health != nil {
			d.Health = string(r.State.Health.Status)
		}
	}

	if r.Config != nil {
		d.Image = r.Config.Image
		d.Labels = r.Config.Labels
		d.WorkingDir = r.Config.WorkingDir
		d.User = r.Config.User
		entrypoint := make([]string, 0, len(r.Config.Entrypoint))
		entrypoint = append(entrypoint, r.Config.Entrypoint...)
		d.Entrypoint = entrypoint
	}
	d.Labels = defaultLabels(d.Labels)

	if r.HostConfig != nil {
		d.RestartPolicy = string(r.HostConfig.RestartPolicy.Name)
	}

	if r.NetworkSettings != nil {
		d.Networks = toEndpointSummaries(r.NetworkSettings.Networks)
		d.Ports = toPortsFromPortMap(r.NetworkSettings.Ports)
	}
	if d.Networks == nil {
		d.Networks = make([]EndpointSummary, 0)
	}
	if d.Ports == nil {
		d.Ports = make([]Port, 0)
	}

	return d
}

// zeroTimeToEmpty returns "" for a container timestamp that Docker reports as
// the zero RFC3339Nano value ("0001-01-01T00:00:00Z"), which means "never
// started" or "never finished" rather than an actual timestamp. A prefix
// check is used instead of a fixed-length slice so short strings (which a
// misbehaving Engine, proxy, or fake can return) never panic.
func zeroTimeToEmpty(ts string) string {
	if ts == "" || strings.HasPrefix(ts, "0001") {
		return ""
	}
	return ts
}

// toPort projects a container.PortSummary onto the allowlisted DTO. IP is
// netip.Addr, not a string, and is the zero value (invalid) for ports that
// are exposed but not published to the host.
func toPort(p container.PortSummary) Port {
	var ip string
	if p.IP.IsValid() {
		ip = p.IP.String()
	}
	return Port{
		IP:          ip,
		PrivatePort: p.PrivatePort,
		PublicPort:  p.PublicPort,
		Protocol:    p.Type,
	}
}

// toMounts projects a container's mount points onto the allowlisted DTO.
func toMounts(mounts []container.MountPoint) []Mount {
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, Mount{
			Type:        string(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			ReadWrite:   m.RW,
		})
	}
	return out
}

// toPortsFromPortMap projects an inspected container's port bindings onto the
// allowlisted DTO. A container port with no host binding still has an entry
// in the map, keyed by its exposed port, with a nil/empty binding slice.
func toPortsFromPortMap(ports network.PortMap) []Port {
	out := make([]Port, 0, len(ports))
	for port, bindings := range ports {
		if len(bindings) == 0 {
			out = append(out, Port{
				PrivatePort: port.Num(),
				Protocol:    string(port.Proto()),
			})
			continue
		}
		for _, b := range bindings {
			var ip string
			if b.HostIP.IsValid() {
				ip = b.HostIP.String()
			}
			var publicPort uint16
			if n, err := strconv.ParseUint(b.HostPort, 10, 16); err == nil {
				publicPort = uint16(n)
			}
			out = append(out, Port{
				IP:          ip,
				PrivatePort: port.Num(),
				PublicPort:  publicPort,
				Protocol:    string(port.Proto()),
			})
		}
	}
	return out
}

// toEndpointSummaries projects a container's per-network attachments onto the
// allowlisted DTO. Map values can be nil even when the map itself is not.
func toEndpointSummaries(networks map[string]*network.EndpointSettings) []EndpointSummary {
	out := make([]EndpointSummary, 0, len(networks))
	for name, ep := range networks {
		if ep == nil {
			continue
		}
		var ipAddress, gateway string
		if ep.IPAddress.IsValid() {
			ipAddress = ep.IPAddress.String()
		}
		if ep.Gateway.IsValid() {
			gateway = ep.Gateway.String()
		}
		aliases := make([]string, 0, len(ep.Aliases))
		aliases = append(aliases, ep.Aliases...)
		out = append(out, EndpointSummary{
			NetworkName: name,
			NetworkID:   ep.NetworkID,
			IPAddress:   ipAddress,
			Gateway:     gateway,
			MACAddress:  ep.MacAddress.String(),
			Aliases:     aliases,
		})
	}
	return out
}
