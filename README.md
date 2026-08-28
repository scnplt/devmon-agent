# devmon-agent

A Go agent that exposes a narrow, mTLS-authenticated Docker control API, so a
paired client can inspect and restart containers without SSH and without
exposing the Docker socket to the internet.

**Status: 0.5.0 — an operator CLI via `docker exec devmon`.** The agent is its own certificate
authority. An operator mints a pairing code on the host, the device generates a
keypair and exchanges a CSR for a client certificate, and every guarded request
is authenticated against that certificate. Revocation takes effect on the next
request.

A paired client can list and inspect containers, images, networks and volumes;
read historical logs and follow a live stream; and start, restart, stop, kill
and delete containers — as far as the host's startup policy mode permits, and
never against the agent's own container. Every mutating attempt is recorded in
an audit table that outlives the operational log. The listening port is rate
limited in two pre-authentication tiers, plus a per-device tier behind the
handshake and a global backstop, and an executable contract suite runs the real
binary against a real Docker Engine.

License: [AGPL-3.0-only](LICENSE). Changes by version: [CHANGELOG.md](CHANGELOG.md).

---

## Install

```bash
git clone https://github.com/scnplt/devmon-agent.git
cd devmon-agent
./install.sh --public-addr vps.example.com
```

That resolves the docker socket GID from the host, creates and chowns the state
directory, writes a `compose.yaml`, starts the agent, waits for it to answer,
and prints the CA fingerprint and your first pairing code. Pass `--dry-run` to
see the compose file and every command it would run without touching the host —
that works from a workstation with no Docker daemon of its own, so you can read
the file before running the installer on the server — and `--help` for the full
flag list. Every prompt is also settable by flag or environment variable, so it
works unattended.

The installer refuses to touch a state directory that already exists and is not
empty. Upgrading an existing installation is `docker compose pull && docker
compose up -d`.

### Installing by hand

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
  ghcr.io/scnplt/devmon-agent:0.5.0
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

See `compose.example.yaml` for the equivalent Compose file. It carries the knobs
worth setting by hand, not all of them — the full list is the environment table
below.

Verify it is up:

```bash
curl -sk https://vps.example.com:8443/v1/status
# {"api_version":"v1","agent_version":"0.5.0","policy_mode":"default",
#  "server_time":"…Z","ca_fingerprint":"a1b2c3…"}
```

`-k` is expected: the server certificate is issued by the agent's own CA, which
no public root store knows. The client pins that CA rather than a public one.

### Record the CA fingerprint now

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

### Reaching it from outside

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
| Rate limiting | Both pre-authentication tiers key on the peer address, which is now the proxy's for every caller. `X-Forwarded-For` is deliberately never consulted — see [Rate limiting](#rate-limiting). | `GET /v1/status` and `POST /v1/pair` share one budget across all devices |

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

### Behind CGNAT, with no port to forward

A home server on a carrier-grade-NAT connection has no inbound port and no
stable address. That sounds like it rules the agent out, but it does not: what
CGNAT blocks is an *incoming* connection, and the first option above never needs
one. On an overlay network both ends dial outward to meet each other, so no port
is forwarded, no dynamic-DNS record is needed, and the agent is never on the
public internet at all.

- **An overlay network — Tailscale, NetBird, ZeroTier.** Nothing else to run.
  Install it on the server and wherever the client runs, then point
  `DEVMON_PUBLIC_ADDR` at the overlay address the client dials. Do not use a feature that fronts the
  agent with its own HTTPS listener — `tailscale serve` and `funnel` terminate
  TLS, which is the case above.
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
[docs/THREAT-MODEL.md](docs/THREAT-MODEL.md).

---

## Configuration

Every knob is an environment variable, read once at startup and immutable
thereafter. This is the agent's core security property: its powers are fixed by
the operator's startup configuration, never by a client. Changing any of these
requires host access and a container restart — there is deliberately no API,
file, or signal that can widen what was granted here.

| Variable | Type | Default | Validation |
|---|---|---|---|
| `DEVMON_STATE_DIR` | path | `/var/lib/devmon` | Absolute path |
| `DEVMON_LISTEN_ADDR` | host:port | `:8443` | Parses as host:port; port 1–65535 |
| `DEVMON_PUBLIC_ADDR` | comma list | *(required)* | ≥1 entry; each a DNS name or IP; used as server-certificate SANs |
| `DEVMON_POLICY_MODE` | enum | `default` | One of `read-only`, `default`, `full` |
| `DEVMON_DOCKER_HOST` | URL | `unix:///var/run/docker.sock` | Scheme `unix` or `tcp` |
| `DEVMON_SELF_CONTAINER` | name or ID | *(auto-detected)* | Docker's own name grammar: `[a-zA-Z0-9][a-zA-Z0-9_.-]+` (two characters or more). Hex container IDs satisfy it too |
| `DEVMON_LOG_LEVEL` | enum | `info` | One of `debug`, `info`, `warn`, `error` |
| `DEVMON_LOG_MAX_AGE_DAYS` | int | `1` | ≥1 |
| `DEVMON_LOG_MAX_TOTAL_MB` | int | `64` | ≥8 |
| `DEVMON_AUDIT_MAX_AGE_DAYS` | int | `365` | ≥1, and ≥ `DEVMON_LOG_MAX_AGE_DAYS` |
| `DEVMON_AUDIT_MAX_ROWS` | int | `100000` | ≥1000 |
| `DEVMON_RATE_STATUS_PER_MIN` | int | `30` | ≥1 |
| `DEVMON_RATE_PAIR_PER_MIN` | int | `5` | ≥1 |
| `DEVMON_RATE_GUARDED_PER_SEC` | int | `20` | ≥1 |
| `DEVMON_PAIR_TTL_MAX_MIN` | int | `10` | 5–60; ceiling in minutes for `device pair-code --ttl` |

`DEVMON_PUBLIC_ADDR` has no default on purpose: a server certificate with no
subject alternative name matches nothing, and the failure would otherwise
surface weeks later as an opaque TLS error on the operator's phone rather than
at startup.

Bad configuration is reported in full, before anything starts, with exit code 2:

```
$ docker run … -e DEVMON_POLICY_MODE=admin -e DEVMON_LOG_MAX_AGE_DAYS=x …
invalid configuration:
  DEVMON_POLICY_MODE: "admin" is not a valid policy mode (want one of: read-only, default, full)
  DEVMON_LOG_MAX_AGE_DAYS: "x" is not an integer
```

Every problem is listed at once rather than one per restart. Exit codes: `0`
clean shutdown, `1` runtime failure, `2` invalid configuration.

### Policy modes

The tier is a strict superset of the one below it, and the default is the middle
one — useful out of the box, incapable of destroying anything.

| Mode | Permits |
|---|---|
| `read-only` | list, inspect, logs |
| `default` | the above, plus start, restart, stop |
| `full` | the above, plus kill, delete |

The mode is fixed at startup and read once. **No client can widen it** — there
is no API call that changes the tier, by design: a compromised phone must not be
able to grant itself more than the operator granted. Changing the mode means
changing `DEVMON_POLICY_MODE` on the host and restarting the agent.

Enforcement is server-side. The app reads `policy_mode` from `/v1/status` and
greys out what the host forbids, but that is a courtesy to the user, not the
boundary — a request for a forbidden operation is refused with a 403 whatever
the client believes, and the refusal is recorded in the audit log.

### The agent excludes itself

The agent will not start, restart, stop, kill, or delete **its own container**,
in any policy mode. This is a fixed rule rather than a setting: an agent that
deletes itself destroys the operator's only remote access, and no configuration
may opt into that.

The rule is enforced on the container ID the Engine resolves, not on the text
the device sent, so naming the agent by container name, by short ID, or by full
ID is refused identically. Its row in `GET /v1/containers` carries
`"protected": true`; every other row carries `"protected": false`, so the app
can grey out those controls and explain why.

To identify itself, the agent checks `DEVMON_SELF_CONTAINER` first, then looks
for a container ID in `/proc/self/mountinfo` and `/proc/self/cgroup`, then falls
back to `$HOSTNAME`. Each candidate is confirmed against the Engine before it is
accepted, because no single source is reliable — cgroup v2 with a private
namespace reports nothing useful, and `$HOSTNAME` stops being the container ID
the moment anyone passes `--hostname`. The resolved ID is logged once at
startup, so an operator can confirm it protected the right thing.

If the agent is containerised but **no** candidate is confirmed, it logs an
ERROR naming `DEVMON_SELF_CONTAINER` and answers **503 on the five lifecycle
routes only**. Reads, logs, pairing, and status keep working. The fix is to name
the container and tell the agent that name:

```yaml
services:
  devmon-agent:
    container_name: devmon-agent
    environment:
      DEVMON_SELF_CONTAINER: devmon-agent
```

**Give it a name, not an ID.** The variable accepts either — the Engine resolves
both — but an ID copied out of `docker ps` is worthless here. Adding a variable
to a compose file changes the container's spec, so the next `docker compose up
-d` recreates the container and mints a new ID, and the value that was just
written is stale before it is ever read. Copying the new ID and repeating never
converges. A name survives recreation, so it is set once. `install.sh` and
`compose.example.yaml` both pin `container_name` and set this variable for
exactly this reason.

**Upgrading from 0.1.x:** this variable was called `DEVMON_SELF_CONTAINER_ID`
before 0.2.0 and the old name is no longer read. An installation that set it
keeps working only as long as the agent detects its own container unaided; where
it cannot, lifecycle goes back to answering 503 with the ERROR above. Rename the
variable — and take the opportunity to give it the container's name rather than
the ID that was there.

A malformed value is a startup configuration error (exit 2), not a warning: this
is the documented fix for the one case where lifecycle is unavailable, so a typo
must surface at start rather than when the button stays grey. A value that is
well-formed but names some *other* container protects that one instead — the
override is trusted. One that names nothing the Engine knows is discarded with a
WARN, and the agent falls back to the filesystem candidates as if it were unset.

Running the agent directly on the host rather than in a container is fine: there
is no container to protect, so lifecycle works normally and the agent says so
once at INFO.

### Retention

Operational logs and the audit record have separate budgets, and the audit
record must outlive the logs — a configuration that inverts this is rejected. One
shared short budget would quietly destroy the security record to make room for
debug output.

Log growth is bounded by whichever of age or total size is hit first, so the
agent cannot be the reason a small VPS runs out of disk.

### Rate limiting

The listening port is rate limited in two tiers, so it survives being scanned.

| Tier | Applies to | Keyed by | Default |
|---|---|---|---|
| Status | `GET /v1/status` | client IP | 30 / minute |
| Pair | `POST /v1/pair` | client IP | 5 / minute |
| Guarded | every route behind a client certificate | **device ID** | 20 / second |

The two pre-authentication tiers share a global backstop, because per-IP limits
alone do not stop a distributed scan. Over the limit, the agent answers:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 2

{"error":"rate limit exceeded"}
```

`Retry-After` is always an integer number of seconds. Which tier a caller hit is
operator information: it goes to the log, never the response.

**The authenticated tier keys on the device, not the IP.** A phone roams across
mobile IPs mid-incident, and keying that tier by address would throttle exactly
the network handover this product exists to work through. A device ID is the
stronger identifier anyway — it is proven by a client certificate rather than
asserted by a packet header. For the same reason `X-Forwarded-For` is never
consulted: the documented deployment is direct inbound, and honouring a
client-supplied forwarding header would let any caller mint a fresh limiter key
per request, which is worse than no limiter because it looks like protection.
What that costs you behind a forwarding host is
[Reaching it from outside](#reaching-it-from-outside).

Limits are startup configuration like everything else, so a client can never
raise its own ceiling, and the values are advertised nowhere — telling a scanner
exactly how fast it may go unnoticed would defeat the point. The minimum is `1`,
not `0`: there is deliberately no value that turns a limiter off. An operator who
wants a higher ceiling raises the number.

Connection establishment, body size, and open streams are bounded separately —
by the OS accept queue and `ReadHeaderTimeout`, by per-route body caps, and by a
concurrent-stream ceiling. A connection limiter belongs in front of the process,
in `iptables` or a VPN; see [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md).

---

## State directory

```
$DEVMON_STATE_DIR/                 bind mount — not a named volume        0700
├── devmon.db                      SQLite, WAL mode                       0600
├── devmon.db-wal                  created by SQLite
├── devmon.db-shm                  created by SQLite
├── certs/                                                                0700
│   ├── ca.crt                     the agent's CA, valid 10 years         0644
│   ├── ca.key                     CA private key — UNENCRYPTED           0600
│   ├── server.crt                 issued by the CA above                 0644
│   └── server.key                 EC P-256 private key                   0600
└── logs/                                                                 0700
    ├── agent.log                  current                                0600
    └── agent-….log.gz             rotated and compressed                 0600
```

A **bind mount, not a named volume**, deliberately: the operator can see, back
up, and restore it, and `docker compose down -v` cannot destroy it. This
directory holds the agent's identity — losing it unpairs every device.

> **`ca.key` is stored unencrypted.** There is nowhere to keep a passphrase that
> an unattended container restart could reach, so encrypting it would buy
> nothing but ceremony. The consequence is concrete and worth stating plainly:
> anyone who can read that file can mint a client certificate this agent will
> accept. It is protected by file mode `0600` and directory mode `0700` — and by
> nothing else. **It is therefore present, in the clear, in every host backup
> and every VPS snapshot of this directory.** Treat those backups as
> credentials: encrypt them at rest, and if one is exposed, delete `certs/` on
> the host and restart. The agent then creates a new CA, every paired device is
> unpaired, and the fingerprint changes.

The audit trail lives in `devmon.db`, not in `logs/`. That is what the SQLite
choice bought: the host-side CLI can read it while the agent writes, and
retention is an indexed `DELETE` rather than file surgery.

### Backup

Stop the agent first, so the write-ahead log is checkpointed and the copy is
consistent:

```bash
docker stop devmon-agent
sudo tar czf devmon-backup.tgz -C / var/lib/devmon
docker start devmon-agent
```

Restore by extracting to the same path with the same ownership. The agent
detects a truncated or corrupt `devmon.db` at startup and refuses to run rather
than failing obscurely at the first query.

**The backup is itself a credential.** It contains `ca.key`, so whoever holds it
can mint a device certificate for your agent. Store it as you would a private
key. [docs/BACKUP.md](docs/BACKUP.md) covers restore, ownership, and what
happens if `certs/` is lost.

---

## API

The machine-readable form of everything below is
[docs/openapi.yaml](docs/openapi.yaml) — OpenAPI 3.1, covering every route,
payload, and failure body. Generate a client from it rather than hand-writing
one, and diff it between releases to see what changed. This section keeps the
reasoning; the spec keeps the shapes.

| Route | Auth | Purpose |
|---|---|---|
| `GET /v1/status` | none | `api_version`, `agent_version`, `policy_mode`, `server_time`, `ca_fingerprint` |
| `POST /v1/pair` | pairing code | Exchange a CSR for a client certificate |
| `POST /v1/device/renew` | client cert | Exchange a CSR for a fresh certificate |
| `DELETE /v1/device/self` | client cert | Unpair this device |
| `GET /v1/containers?all=<bool>` | client cert | List containers; `all=true` includes stopped ones |
| `GET /v1/containers/{id}` | client cert | Inspect one container |
| `GET /v1/images` | client cert | List images |
| `GET /v1/images/{id}` | client cert | Inspect one image |
| `GET /v1/networks` | client cert | List networks |
| `GET /v1/networks/{id}` | client cert | Inspect one network |
| `GET /v1/volumes` | client cert | List volumes |
| `GET /v1/volumes/{name}` | client cert | Inspect one volume |
| `GET /v1/containers/{id}/logs` | client cert | Recent log lines as JSON |
| `GET /v1/containers/{id}/logs/stream` | client cert | Live log stream (Server-Sent Events) |
| `GET /v1/events/stream` | client cert | Live container health and lifecycle events (Server-Sent Events) |
| `POST /v1/containers/{id}/start` | client cert | Start a stopped container — needs `default` |
| `POST /v1/containers/{id}/restart` | client cert | Restart a container — needs `default` |
| `POST /v1/containers/{id}/stop` | client cert | Stop a running container — needs `default` |
| `POST /v1/containers/{id}/kill` | client cert | SIGKILL a running container — needs `full` |
| `DELETE /v1/containers/{id}` | client cert | Delete a stopped container — needs `full` |

The five mutating routes answer **204 with no body**. The Engine's lifecycle
calls return before the container has finished changing state — a restart
returns before it is healthy, a stop while the process is still unwinding — so
any state sent back would be a snapshot that is already stale. Re-fetch
`GET /v1/containers/{id}` instead; it is one cheap request and always true.

Starting a container that is already running, or stopping one that is already
stopped, is a **204**, not an error. The goal is already met, and reporting a
failure would invite a retry that cannot help.

Delete never force-stops. A running container is a 409, so removing one is stop
then delete: two deliberate operations leaving two audit rows, rather than one
tap that kills and destroys in a single step. Kill is always SIGKILL — there is
no `?signal=`, because the kill button means "stop this now".

Both log routes accept `?tail=<n>` and `?since=<rfc3339>`. `tail` is bounded to
1…2000 and falls back to its default — 200 historical, 100 for the stream — when
it is absent, unparsable, or out of range, because a typo must not fail a
diagnostic request in the middle of an incident. `since` is the opposite: it is
interpolated into the Engine's own request URL, so an unparsable value is a 400
rather than a silently ignored preference.

Failure modes shared by every read route:

| Condition | Status | Body |
|---|---|---|
| No, unknown, or revoked client certificate | 401 | `{"error":"client certificate required"}` |
| Host policy forbids the operation | 403 | `{"error":"operation not permitted by host policy"}` |
| Malformed object reference | 400 | `{"error":"invalid object reference"}` |
| Unparsable `?since=` timestamp | 400 | `{"error":"invalid since timestamp"}` |
| No such object | 404 | `{"error":"not found"}` |
| Engine unreachable, timed out, or otherwise failing | 502 | `{"error":"docker engine unavailable"}` |
| Calling device already holds its own stream cap (stream route only) | 503 | `{"error":"too many concurrent log streams for this device"}` |
| Every stream slot on the host is in use (stream route only) | 503 | `{"error":"too many concurrent log streams"}` |

The mutating routes add three of their own:

| Condition | Status | Body |
|---|---|---|
| The target is the agent's own container | 403 | `{"error":"the agent cannot act on itself"}` |
| Delete of a running container | 409 | `{"error":"container is running"}` |
| The agent is containerised but cannot identify itself | 503 | `{"error":"agent cannot identify its own container"}` |

403 is deliberately distinguishable from 401 and 502 so the app can say "your
host forbids this" rather than "something broke", and 409 lets it offer "stop it
first" instead of a bare failure.

502 rather than 500 is deliberate: the Engine is an upstream dependency, so its
failures are gateway failures. That keeps 500 meaning "the agent itself broke",
which is a different page for whoever is on call.

### Why responses are projections

Read responses are not the Docker Engine's JSON. Every field is copied into an
explicit allowlist type before it is serialised, so what the API returns is a
decision rather than a consequence of whatever the Engine emits this release.

**Environment variables are never returned, at any level.** A container's
`Config.Env` and an image's baked-in env hold the database passwords and API
keys the operator passed in, and this channel was never designed to carry them.
Redacting values would still disclose which secrets exist and what they are
called, so the fields simply do not exist in the response types. The same
reasoning removes a volume's driver `Options`, which routinely carry NFS and
CIFS credentials.

**Container log content is returned in full**, and that is not a contradiction
of the rule above. Env vars are stripped because nobody asked for them: they
ride along inside an inspect response the operator wanted for other reasons. A
log line is the thing the operator explicitly requested, and a log viewer that
redacted the output would have no purpose. The agent passes those lines through
without inspecting them, and — see below — without ever writing them to its own
log.

**Command lines are returned.** Unlike env vars, the command, entrypoint, and
args are what identify a misconfigured container, and they are already visible
in the host's process table and in `docker ps`. This is a bounded, deliberate
disclosure rather than an oversight.

**Engine event attributes are never forwarded.** The event stream maps no
attribute of a Docker event, because an event's attributes are the container's
label set — the same rule that removes env vars, applied to a different field.
Only the container's name is read out of them, by exact key.

**Lists are capped at 500 items** and carry a `truncated` flag. A host with
thousands of images would otherwise send a multi-megabyte body to a phone on a
mobile connection. The cap is server-side and cannot be raised by a client,
which is the same rule the rest of the agent's configuration follows.

### Live log streaming

`GET /v1/containers/{id}/logs/stream` is Server-Sent Events, not a WebSocket.
Nothing ever flows client to server on this feature — logs are strictly
one-directional — so a bidirectional transport would buy nothing and cost a new
module on an internet-facing port plus a framing and authentication story that
sits beside the HTTP middleware instead of inside it. SSE reuses the same
certificate and policy guards as every other route, and resumption is part of
its own specification rather than something invented here.

Each frame carries one line:

```
id: 2026-08-08T10:02:14.882Z
event: log
data: {"ts":"2026-08-08T10:02:14.882Z","stream":"stderr","line":"panic: nil map write"}
```

The timestamp is extracted into its own field rather than left as the prefix
Docker emits, so a client never has to parse a Docker-specific wire format to
find its resume cursor. `stream` is `stdout` or `stderr`, and the two are
interleaved in the order the container wrote them — that ordering is most of
what a log is worth during an incident, so the agent demultiplexes Docker's
frames itself rather than pumping them into two separate sinks.

**Resuming is at-least-once.** After a network handover, a client reconnects
with `?since=<the last id it saw>`. Docker's `since` filter is inclusive at its
granularity boundary, so a resume can repeat the last line or two; clients
should dedupe on `(ts, line)`. Guaranteeing exactly-once would mean the agent
keeping a durable per-device cursor, which is server-side state this feature has
no business adding. A repeated line is cosmetic; a dropped one is a diagnostic
failure, and the trade goes the right way round.

**A silent stream is still a live stream.** The agent writes an SSE keepalive
comment every 20 seconds. A container that logs nothing for five minutes is
ordinary; a TCP connection that sends nothing for five minutes is dropped by
mobile-carrier NAT and by any proxy in between. The keepalive is also how the
agent notices a client that vanished without closing — the write fails, and the
stream unwinds instead of leaking a connection.

**Single lines are capped at 8 KiB** and marked `"truncated":true` when cut. A
container printing one enormous line — a stack dump, a base64 payload, a
minified bundle — would otherwise be accumulated whole in agent memory before
any line boundary arrived, which is the agent OOM-killing itself while reading
logs.

**Eight live streams per host, three per device**, after which the route answers
503 — `too many concurrent log streams for this device` when the caller is at its
own cap, `too many concurrent log streams` when the host's total is gone. Each
stream holds a goroutine, an Engine connection, and a socket for its entire life,
so an unbounded count is file-descriptor exhaustion the agent inflicts on the host
it exists to protect. The per-device cap under that total is what stops one paired
device — a phone with a stack of tabs, or a client that leaks streams on
backgrounding — from denying live logs to every other paired device; the host-wide
refusal is logged with the devices holding the slots, so the operator can tell the
two apart. Both limits are constants rather than settings, on the same reasoning as
the rest of the configuration: every additional knob is surface the operator has to
understand at install time.

If the container turns out not to exist, or the Engine is unreachable, the
failure arrives as an ordinary 404 or 502 with a JSON body — the response
headers are not written until there is a first line to send, precisely so the
status code stays correctable. Once the stream has started, a later failure can
only be a terminal `event: error` frame on a response that already said 200.

**Container logs are never persisted by the agent.** They are not written to
`devmon.db`, not written to `logs/agent.log`, and not cached anywhere in
`/var/lib/devmon`. Log lines are by design whatever the container printed,
including the secrets the projection rules above work to keep out of
responses — the difference is only that here the operator asked for them. That
makes it more important, not less, that they pass through in transit and leave
nothing behind.

### Container health event stream

`GET /v1/events/stream` tells a client the moment a container's health flips or
a container dies, starts, stops, or is OOM-killed — without polling. It is SSE
on the same listener, behind the same certificate, rate-limit, and policy guards
as every read route, and it is permitted in every policy mode because it
discloses a strict subset of what `GET /v1/containers` already returns.

**The snapshot replaces replay.** The first frame is always one
`event: snapshot` carrying `{id, name, state, health}` for every container on
the host. There is deliberately no `?since=` and no `Last-Event-ID` resume: a
replay window would mean the agent buffering events per absent device — exactly
the durable per-client state the log stream refused to add — and would still be
wrong at its edges. A snapshot is one Engine call and is always true. The
ordering is guaranteed the strong way round: the agent subscribes to the
Engine's event feed first and takes the snapshot only once the subscription is
live, so an event firing in between lands in the stream rather than in a gap.

After the snapshot, one `event: health` frame per forwarded event. Exactly six
Engine events are forwarded — `health_status: healthy`,
`health_status: unhealthy`, `die`, `start`, `stop`, `oom` — through a closed
allowlist. That allowlist is a security control, not a filter: Docker's raw
health action string can carry the healthcheck's own output, and an event's
attributes are the container's labels, so neither ever reaches the wire or the
agent's own log.

**One Engine subscription, fanned out.** However many devices are connected,
the agent holds exactly one `/events` connection to the Engine, started when
the first client attaches and closed when the last one leaves. An event stream
therefore consumes none of the log-stream budget — holding a health view open
never costs the device a log view.

**Every gap becomes a disconnect.** If the Engine feed dies the agent does not
silently reconnect — a resumed feed would have a hole in it and no way to say
what fell in. Every client gets a terminal `event: error` /
`docker engine unavailable` and reconnects into a fresh snapshot. A client that
cannot keep up is dropped the same way (`event stream fell behind`) rather than
having events silently skipped: a dropped client re-snapshots and is correct
again, a client missing one event is quietly wrong forever.

**One stream per device, newest wins.** A second connection from the same
device closes the older one with `event stream superseded` — in practice the
second connection is a client that reconnected before its old socket was
reaped, and the newer socket is the one the device is actually holding. That is
the one terminal error a client must not retry; the other two are retryable
with backoff.

A `: heartbeat` comment is written every 25 seconds, for the same two reasons
the log stream's keepalive exists. `id` is the join key across routes: `name`
on this stream strips the leading `/` the Engine puts on list names, because
the Engine's list and event APIs disagree on the form and one stream must be
self-consistent. Snapshot `health` needs Engine 29+ (API v1.52) — on an older
Engine every container snapshots as `none` while the events themselves still
arrive.

`/v1/status` is the only endpoint served without a client certificate. Its fields
are a strict allowlist — it may inform, never issue — and it carries no host,
container, or credential data. Advertising the policy mode lets a client tell
"the agent refuses to restart containers" apart from "the agent is broken"
without needing a credential. The CA fingerprint is deliberately public: it is
the value a client pins against.

`/v1/pair` is reachable without a client certificate because a device that has
none is exactly what it exists to serve. The pairing code is the credential for
that one request.

Every authenticated request costs one indexed lookup against the device
registry — there is no in-memory cache of paired devices. That is what makes
revocation immediate rather than eventually-consistent: `device revoke` is a
single `UPDATE`, and the next request the revoked device makes is already
rejected.

TLS 1.3 is the floor. Both peers are ours, so there is nothing to negotiate down
to, and every current client TLS stack supports it.

---

## Pairing

A device generates its own keypair and sends only a certificate signing request.
The agent never sees a device private key, so there is no moment at which one is
in transit or at rest anywhere but on the device that owns it.

### 1. Mint a code on the host

```bash
docker exec devmon-agent devmon device pair-code --name "pixel-8"
# Pairing code: B4HJFH3KEAZ2QMY546LLN3IDOW
# Expires:      2026-08-07T20:12:00Z
```

The code is single-use, valid for **10 minutes** by default, and printed to
stdout only —
never to `agent.log`, which is persisted and rotated. Only its SHA-256 is
stored.

`--ttl <minutes>` picks a different lifetime, from 5 minutes up to the
`DEVMON_PAIR_TTL_MAX_MIN` ceiling the operator fixed at startup (itself capped
at 60). A value outside that range is a hard error — never silently clamped —
and when the flag is omitted the default is 10 minutes or the ceiling,
whichever is lower. A longer-lived code is a longer exposure window if it
leaks unused, which is why the ceiling is startup configuration: nothing
reachable from a client can raise it. Nobody, including the operator, can read it back out of the database, so
a lost code is minted again rather than recovered.

### 2. Redeem it

The client app does this. To do it by hand:

```bash
openssl ecparam -name prime256v1 -genkey -noout -out device.key
openssl req -new -key device.key -subj "/CN=ignored" -out device.csr

curl -sk https://vps.example.com:8443/v1/pair \
  -H 'Content-Type: application/json' \
  -d "$(jq -Rn --arg c "B4HJFH3KEAZ2QMY546LLN3IDOW" \
        --arg r "$(cat device.csr)" \
        '{pairing_code:$c, csr_pem:$r}')" | tee paired.json
```

The response carries the issued certificate, the CA to pin, and the expiry:

```json
{
  "device_id": "3f9a1c74b2e05d68",
  "certificate_pem": "-----BEGIN CERTIFICATE-----\n…",
  "ca_certificate_pem": "-----BEGIN CERTIFICATE-----\n…",
  "not_after": "2026-11-05T20:02:00Z"
}
```

The CSR's subject is ignored on purpose. The device name comes from the code the
operator minted, so a device cannot name itself — the subject line is the one
field in the request an attacker fully controls.

Check `ca_certificate_pem` against the fingerprint you recorded at install:

```bash
jq -r .ca_certificate_pem paired.json > ca.crt
openssl x509 -in ca.crt -noout -fingerprint -sha256
```

If it does not match, stop: something else answered.

### 3. Verify the credential works

```bash
jq -r .certificate_pem paired.json > device.crt
curl -s --cacert ca.crt --cert device.crt --key device.key \
  https://vps.example.com:8443/v1/status
```

`--cacert` replaces the earlier `-k`: from here the connection is verified in
both directions.

### Renewal

Client certificates are valid for **90 days**. The app renews silently against
`POST /v1/device/renew` with a fresh CSR, well before expiry. The old
certificate keeps working until its own `not_after` — renewal never invalidates
the credential the device is currently holding, so a renewal that fails halfway
cannot lock a device out.

---

## Managing devices

These run against the same state directory the agent is using. The image is
`distroless/static:nonroot` — no shell, no second binary — so the host CLI is
subcommands on the agent binary itself. `devmon` is a symlink to
`/usr/local/bin/devmon-agent`, so the full path and the `devmon-agent` name
work just as well:

```bash
docker exec devmon-agent devmon device <subcommand>
```

`docker exec devmon-agent devmon` on its own (or `devmon help`, or
`devmon <command> --help`) prints a usage screen listing every command.

| Subcommand | Effect |
|---|---|
| `device list` | Every registered device: ID, name, paired, last seen, state |
| `device pair-code --name <name> [--ttl <minutes>]` | Mint a single-use code; valid 10 minutes by default, `--ttl` picks 5 up to `DEVMON_PAIR_TTL_MAX_MIN` |
| `device revoke <id>` | Withdraw a device's access, effective immediately |

```bash
$ docker exec devmon-agent devmon device list
ID                NAME     PAIRED                LAST SEEN             STATE
3f9a1c74b2e05d68  pixel-8  2026-08-07T20:02:00Z  2026-08-07T21:14:33Z  active

$ docker exec devmon-agent devmon device revoke 3f9a1c74b2e05d68
revoked 3f9a1c74b2e05d68 (pixel-8)
```

Revocation marks the **device**, not a certificate, so it covers every
certificate that device holds — including one issued seconds earlier by a
renewal. The revoked row is kept rather than deleted: a device that was removed
and a device that never existed should not look the same to whoever reads this
later.

`last seen` is written at most once a minute. It is an operator aid, not an
audit record, and paying a write on every request to sharpen it would be a poor
trade.

Reading this list while the agent runs is safe and expected — that is what WAL
mode and the busy timeout were configured for.

### Health

The image carries a `HEALTHCHECK`, so `docker ps` reports a health state
alongside the running state:

```bash
$ docker ps --filter name=devmon-agent --format '{{.Names}} {{.Status}}'
devmon-agent    Up 4 hours (healthy)
```

It runs `devmon-agent health`, a third subcommand on the same binary — for the
same reason as the two above, there is no shell and no curl in the image to
run a probe with. The subcommand makes one HTTPS `GET /v1/status` against the
agent's own listener on loopback and exits 0 or 1. `restart: unless-stopped`
alone only reacts to the process exiting; this also catches a listener that is
up but no longer answering.

```bash
$ docker exec devmon-agent devmon health
healthy: GET /v1/status returned 200

$ docker inspect -f '{{.State.Health.Status}}' devmon-agent
healthy
```

A rate-limited answer (429) counts as healthy: it proves the listener is
accepting connections and running the middleware chain, which is all this probe
claims to measure. Lowering `DEVMON_RATE_STATUS_PER_MIN` therefore cannot make
a working container report unhealthy. TLS verification is skipped, deliberately
— the server certificate is issued for `DEVMON_PUBLIC_ADDR`'s SANs, which do
not include loopback, and a process probing the listener from inside the
container that owns it is not the place to re-establish who that listener is.
It never reads `certs/`, for the same reason `device` and `audit` do not.

One consequence worth knowing: the probe is a real request, so it writes a
request log line every 30 seconds — about 2900 a day. Raise
`DEVMON_LOG_MAX_TOTAL_MB` if that crowds out what you are actually reading.

### The audit log

Every mutating request writes exactly **one** row — successes, refusals by
policy, refusals by self-exclusion, and failures alike. Reads are not recorded:
a row per list refresh would drown the record the log exists for and then push
the destructive-operation history out under retention.

```bash
$ docker exec devmon-agent devmon audit list --limit 5
WHEN                  DEVICE            OPERATION  TARGET  OUTCOME        DETAIL
2026-08-08T21:06:40Z  3f9a1c… (pixel-8) delete     devmon  denied_self
2026-08-08T21:05:02Z  3f9a1c… (pixel-8) kill       api     denied_policy
2026-08-08T21:04:11Z  3f9a1c… (pixel-8) restart    api     success        9c2e…
```

Each row carries the calling device, the operation, the target **as the device
supplied it**, and the outcome. Recording only the resolved ID would erase the
case where a device named a container that does not exist — precisely the
pattern that separates a fat-fingered operator from something scanning for
targets. `detail` holds the resolved container ID or a short fixed reason, never
an Engine message, which could name a socket path or a host mount.

An unauthenticated request writes **no** row. Attribution is the point, and
letting an anonymous caller write would hand a scanner a way to flood the record.

The log lives in the same SQLite file as the device registry, bounded by
`DEVMON_AUDIT_MAX_AGE_DAYS` and `DEVMON_AUDIT_MAX_ROWS`. It **is not reachable
over the API**, in any policy mode — it is the one artifact whose value survives
a compromised device, and a phone that can read it can see what it would need to
cover up. Host access is the authority here, exactly as it is for revocation.

The row ceiling is spent **per device**, not first-come-first-served: pruning
divides the budget evenly across the devices present in the table and trims each
to its own share, so one device generating traffic at the rate limit cannot push
another device's history out of the record. A device that overruns its own share
still loses its own oldest rows — the table is finite on purpose — which
`docs/THREAT-MODEL.md` states in full.

---

## Development

```bash
make build          # -> bin/devmon-agent
make test           # unit tests
make test-race      # with -race (needs a C toolchain)
make cover          # prints total coverage over ./internal/...; CI enforces the 90% floor
make lint           # gofmt + go vet + golangci-lint when installed
make sec            # gosec
make image          # docker build
```

### End-to-end suite

`internal/e2e/` runs the real agent against a real Docker Engine, with no phone
and no emulator anywhere in the loop. It is the executable definition of the API
contract: assertions are written against the wire — status codes, headers, and
JSON decoded into `map[string]any` — never against the agent's own structs, so a
renamed JSON tag fails the suite instead of silently breaking a client.

Two groups:

- **`internal/e2e/api`** — builds the real binary, starts it as a host process,
  pairs through the documented `devmon-agent device pair-code` path, and drives
  every route over pinned mTLS.
- **`internal/e2e/incontainer`** — builds the image and runs the agent as a
  container, which is the only way to exercise self-identification through
  `/proc/self/mountinfo` and the self-exclusion guarantee.

```bash
make e2e             # both groups, ~5-10 min against a local Engine
make e2e-container   # the in-container group alone
make e2e-endurance   # the 30-minute stream and the retention budget
make e2e-lint        # go vet -tags e2e, plus golangci-lint when installed
make e2e-clean       # remove containers a crashed run left behind
```

Every container the suite creates is removed by the cleanup that created it,
addressed by ID — so two runs on one host never disturb each other. `e2e-clean`
exists for the case where a run died hard enough to skip its own cleanups, and
is deliberately manual: it matches on the shared `com.devmon.e2e` label, which
cannot distinguish a dead run's leftovers from a live run's containers. Run it
only when no e2e run is in flight.

Every file carries `//go:build e2e`, so nothing here compiles into `make build`,
`make test`, `make lint`, or `make cover`, and the suite adds no module
dependency — `go.mod` is unchanged by it.

Four environment variables tune the harness. **They are not agent
configuration**: the harness reads them and never passes them to the agent,
whose own environment is built explicitly from each test case.

| Variable | Effect |
|---|---|
| `DEVMON_E2E_REQUIRE=1` | An unreachable Engine becomes a hard failure instead of a skip. CI sets it. |
| `DEVMON_E2E_ENDURANCE=1` | Runs the 30-minute stream and the retention test, which otherwise skip. |
| `DEVMON_E2E_DOCKER_HOST` | Engine endpoint for the suite, when it is not the default socket. |
| `DEVMON_E2E_KEEP=1` | Keeps fixture containers and state directories after a failure, and prints their paths. |

Without an Engine, every test **skips** with a reason naming the endpoint —
visibly, in `go test` output — rather than passing quietly.

Every `make e2e*` target runs with `-v`, and that is load-bearing rather than
cosmetic: `go test` prints a skip and its reason **only** under `-v`. Without
it, a package whose tests all skipped reports a bare `ok`, indistinguishable
from one that ran and passed them — a green run would silently imply coverage
it does not have. The output is long; the alternative is a log that cannot tell
you which of the reasons below fired.

`TestContainerListReportsHealth` additionally needs Engine 29+ / API v1.52: the
`ContainerSummary.Health` field the list projection reads was only added at
that version, so on an older Engine (even a reachable, healthy one) the test
skips with that reason instead of failing, and `DEVMON_E2E_REQUIRE=1` does not
override that skip — it is a genuine Engine capability floor, not a missing
Engine.

**On Windows, run these from a WSL2 shell.** The agent accepts only `unix://`
and `tcp://` Docker endpoints, so Docker Desktop's default
`npipe:////./pipe/docker_engine` cannot be given to it at all, and the
in-container group depends on Linux bind-mount ownership semantics. A
Windows-native run skips with exactly that explanation.

### Continuous integration

`.github/workflows/ci.yml` runs the same gates on GitHub Actions, scaled to the
branch a pull request targets:

| Job | Runs on | What it does |
|---|---|---|
| `test` | every PR, and pushes to `main` | `go build ./...`, race tests over the whole module, and the 90% coverage floor over `./internal/...` |
| `lint` | PRs into `main`, and pushes to `main` | `gofmt`, `go vet`, `golangci-lint` |
| `image` | PRs into `main`, and pushes to `main` | `docker build` of the release image |
| `gosec` | PRs into `main`, and pushes to `main` | `gosec ./...` |
| `govulncheck` | PRs into `main`, and pushes to `main` | `govulncheck ./...` — known vulnerabilities in the dependencies and the Go toolchain, which `gosec` does not look for |
| `shellcheck` | PRs into `main`, and pushes to `main` | `shellcheck -s sh install.sh` |
| `e2e` | PRs into `main`, and pushes to `main` | `make e2e` against the runner's Docker Engine, with `DEVMON_E2E_REQUIRE=1`, plus `make e2e-lint` |

`dev` is the integration branch, so a PR into it gets fast feedback from `test`
alone; the full release bar applies on the way into `main`. The six
`main`-only jobs are gated on `github.base_ref == 'main'`, which GitHub
populates for `pull_request` events only, **or** `github.ref ==
'refs/heads/main'`, which covers the push of the merge commit. Neither is true
on a `dev` PR, so they are skipped there — not queued. The second half of the
gate is what makes `main`'s HEAD carry its own release-bar result: `main`'s
ruleset is not strict, so the merge commit can differ from the tree the PR
tested, and without it that commit would only ever have been checked by
`test`.

Toolchain versions come from `go.mod`, and the linter and scanner versions are
pinned in the workflow's `env` block — bump them there, deliberately, rather
than tracking `latest`. Note that `-race` needs `CGO_ENABLED=1` even though the
shipped binary is built with `CGO_ENABLED=0`.

Two things that will bite anyone writing code here from memory:

- **The Docker SDK is `github.com/moby/moby/client`**, not
  `github.com/docker/docker/client`. The SDK split at Engine v29 and the old
  module is deprecated. The constructor is `client.New(opts...)`, every method
  takes an options struct, and `Ping` takes **two** arguments. The pre-v29 forms
  will not compile.
- **Build with `CGO_ENABLED=0`.** `modernc.org/sqlite` is pure Go, which keeps
  the binary static so it runs on `distroless/static`. Its `database/sql` driver
  name is `"sqlite"`, not `"sqlite3"`.

Never log key material, pairing codes, or PEM bytes, at any level.

Contributing guidelines, the branching model, and the full gate list are in
[CONTRIBUTING.md](CONTRIBUTING.md).

---

## Security

This agent holds a Docker socket. Anything that can drive its API can reach
root-equivalent power over the host, so a vulnerability here is a host
compromise rather than a container-management bug.

- **Reporting**: [SECURITY.md](SECURITY.md). Never open a public issue for a
  security problem — use GitHub Security Advisories.
- **What is and is not defended**: [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md),
  including the risks that are accepted rather than fixed.
- **Backup and restore**: [docs/BACKUP.md](docs/BACKUP.md).

---

## License

Copyright (C) 2026 Sertan Canpolat.

Licensed under the GNU Affero General Public License v3.0 only
(`AGPL-3.0-only`). The full text is in [LICENSE](LICENSE), and every source
file carries the SPDX identifier.

The AGPL's network clause is deliberate: if you run a modified version of this
agent as a service for others, they are entitled to its source.
