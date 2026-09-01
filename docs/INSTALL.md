# Installation

This expands the Install section of [`README.md`](../README.md): what the
installer does, how to install by hand, how to verify the agent, and how to
reach it from outside the host.

## The installer

```bash
git clone https://github.com/scnplt/devmon-agent.git
cd devmon-agent
./install.sh --public-addr vps.example.com
```

`install.sh` takes a clean Linux host from nothing to a paired device. It
resolves the docker socket GID from the host, creates and chowns the state
directory, writes a `compose.yaml`, starts the agent, waits for it to answer,
and prints the CA fingerprint and your first pairing code.

- `--dry-run` prints the compose file and every command it would run without
  touching the host. That works from a workstation with no Docker daemon of
  its own, so you can read the file before running the installer on the
  server.
- `--help` lists every flag. Every prompt is also settable by flag or by an
  environment variable of the same name, and `--yes` accepts every default, so
  the script works unattended.
- The `DEVMON_INSTALL_*` variables (`DEVMON_INSTALL_PORT`,
  `DEVMON_INSTALL_DIR`, `DEVMON_INSTALL_DEVICE_NAME`) configure the installer
  only; the agent never reads them.
- The installer refuses to touch a state directory that already exists and is
  not empty. `--force` overwrites an existing `compose.yaml` and never the
  state directory.

Upgrading an existing installation is not the installer's job:

```bash
docker compose pull && docker compose up -d
```

## Installing by hand

Two host prerequisites, which `install.sh` exists to resolve for you. Both will
otherwise look like agent bugs, and both are consequences of the container
running as `nonroot` (UID 65532):

```bash
# 1. The state directory must be owned by the container's UID, or startup fails
#    at MkdirAll with "permission denied".
sudo mkdir -p /var/lib/devmon
sudo chown 65532:65532 /var/lib/devmon
sudo chmod 700 /var/lib/devmon

# 2. The container needs the host's docker group, or the startup ping fails with
#    "permission denied" on the socket. The GID varies per host.
stat -c '%g' /var/run/docker.sock
```

Then:

```bash
docker run -d --name devmon-agent \
  -v /var/lib/devmon:/var/lib/devmon \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -p 8443:8443 \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  --read-only --tmpfs /tmp \
  --pids-limit 256 \
  -e DEVMON_PUBLIC_ADDR=vps.example.com \
  ghcr.io/scnplt/devmon-agent:0.6.0
```

The four hardening flags are not needed to run the agent and none of them
changes how it behaves — they narrow what a foothold inside a container that
holds the Docker socket is worth. `no-new-privileges` is the one to keep if you
keep only one. `--read-only` covers the image's own filesystem; the state bind
mount stays writable, and `/tmp` is a tmpfs because SQLite falls back to it for
spill files.

A published port is reachable from anywhere the host is, and a host firewall
alone does not change that: Docker installs DNAT rules that are evaluated before
the chains UFW and firewalld manage, so a `ufw deny 8443` is never consulted.
Restrict it where Docker honours it — publish to one interface
(`-p 127.0.0.1:8443:8443`, behind a VPN), or write the rule into `DOCKER-USER`.

See [`compose.example.yaml`](../compose.example.yaml) for the equivalent
Compose file. It carries the knobs worth setting by hand, not all of them —
the full list is in [`CONFIGURATION.md`](CONFIGURATION.md).

## Verify it is up

```bash
curl -sk https://vps.example.com:8443/v1/status
# {"api_version":"v1","agent_version":"0.6.0","policy_mode":"default",
#  "server_time":"…Z","ca_fingerprint":"a1b2c3…"}
```

`-k` is expected: the server certificate is issued by the agent's own CA, which
no public root store knows. The client pins that CA rather than a public one.

## Record the CA fingerprint now

On first start the agent creates its CA and logs the fingerprint at WARN:

```bash
docker logs devmon-agent 2>&1 | grep -i fingerprint
```

**Write it down, off the host.** It is the same value `/v1/status` serves as
`ca_fingerprint`, and it is the one thing that lets you tell "my credential
expired" apart from "something else is answering on this port". Comparing the
fingerprint your phone shows during pairing against the one you recorded at
install is what makes the pairing exchange safe over an untrusted network. If
you only ever read it from the server you are about to trust, it proves nothing.

## Reaching it from outside

The documented deployment is **direct inbound TLS**: the device's connection
terminates in the agent process. That is not a default to be overridden — three
separate parts of the design read the TLS connection itself, so anything that
terminates TLS in front of the agent breaks them.

This rules out Cloudflare Tunnel's HTTP/HTTPS ingress, and every reverse proxy
run in HTTP mode: nginx `proxy_pass`, Caddy, Traefik, an ALB. Not as a
configuration problem — there is no setting on either side that recovers what
the termination discarded.

| What breaks | Why | What you see |
|---|---|---|
| Device authentication | The client certificate belongs to the device's TLS connection. The proxy opens its own connection to the agent, which carries no certificate. | `401 client certificate required` on every guarded route |
| CA pinning | The app pins the agent's own CA. The proxy presents its certificate, not the agent's. | The app fails during the handshake, before any HTTP response |
| Rate limiting | Both pre-authentication tiers key on the peer address, which is now the proxy's for every caller. `X-Forwarded-For` is deliberately never consulted — see [Rate limiting](CONFIGURATION.md#rate-limiting). | `GET /v1/status` and `POST /v1/pair` share one budget across all devices |

Cloudflare Access mTLS does not bridge this. It verifies the certificate at the
edge and forwards the result as a signed header; the agent authenticates from
`r.TLS.PeerCertificates` and reads no such header.

**What does work** is anything that moves bytes without terminating TLS:

- A VPN or overlay network — WireGuard, Tailscale — with the agent bound to the
  private interface and no published port. Simplest, and the agent is then not
  on the public internet at all.
- Cloudflare Zero Trust with `warp-routing`, where the tunnel carries the
  private network rather than proxying HTTP, and the device joins over WARP.
- TCP passthrough: nginx `stream`, HAProxy in `tcp` mode, or Cloudflare
  Spectrum.

Two things to get right on any of them. `DEVMON_PUBLIC_ADDR` must contain the
address the device actually dials, because it becomes the server certificate's
SAN — a private IP behind a VPN belongs there just as much as a public hostname.
And passthrough still hides the caller's address: the guarded tier is unaffected
because it keys on the device, but the status and pair tiers will see only the
forwarding host, so size those limits for the whole fleet rather than one phone.

## Behind CGNAT, with no port to forward

A home server on a carrier-grade-NAT connection has no inbound port and no
stable address. That sounds like it rules the agent out, but it does not: what
CGNAT blocks is an *incoming* connection, and the first option above never needs
one. On an overlay network both ends dial outward to meet each other, so no port
is forwarded, no dynamic-DNS record is needed, and the agent is never on the
public internet at all.

- **An overlay network — Tailscale, NetBird, ZeroTier.** Nothing else to run.
  Install it on the server and wherever the client runs, then point
  `DEVMON_PUBLIC_ADDR` at the overlay address the client dials. Do not use a
  feature that fronts the agent with its own HTTPS listener — `tailscale serve`
  and `funnel` terminate TLS, which is the case above.
- **Your own hub on a cheap VPS**, if you would rather not depend on a hosted
  coordination service: WireGuard, Headscale, or a raw TCP tunnel such as
  `ssh -R`, `frp`, or `rathole`. Both ends dial the VPS. A TCP tunnel does put
  the agent back on the public internet, so firewall it, and remember the two
  pre-authentication rate-limit tiers will see one address for every caller.
- **IPv6**, which is often overlooked: CGNAT is usually IPv4-only, and the same
  connection may carry a routable IPv6 prefix. Then no tunnel is needed, only a
  firewall rule and a DDNS AAAA record. Keep one of the options above as well,
  because the client will sometimes be on an IPv4-only network.
- **Ask the ISP.** Many will hand out a public address on request or for a small
  fee. The fewest moving parts of anything here.

`DEVMON_PUBLIC_ADDR` accepts a list, so a home server can carry both its LAN
address and its overlay name and be reachable either way. Adding an address
later is safe: the server certificate is re-issued to cover it and the CA is
untouched, so no device has to pair again.

Putting the agent behind an HTTPS proxy would mean replacing its authentication
model, not configuring it. That is a deliberate trade: request signing survives
a proxy, but a terminating proxy also reads every container name and log line in
the clear, and can alter a response the device has no way to verify. See
[`THREAT-MODEL.md`](THREAT-MODEL.md).

## Upgrading

```bash
docker compose pull && docker compose up -d
```

The state directory is a bind mount, so an upgrade never touches the agent's
identity or the device registry. Read [`CHANGELOG.md`](../CHANGELOG.md) for
anything a release asks you to change by hand.

**Upgrading from 0.1.x:** the self-identification variable was called
`DEVMON_SELF_CONTAINER_ID` before 0.2.0 and the old name is no longer read. An
installation that set it keeps working only as long as the agent detects its
own container unaided; where it cannot, lifecycle routes answer 503 with an
ERROR in the log. Rename the variable to `DEVMON_SELF_CONTAINER` — and take the
opportunity to give it the container's name rather than the ID that was there.
See [The agent excludes itself](CONFIGURATION.md#the-agent-excludes-itself).
