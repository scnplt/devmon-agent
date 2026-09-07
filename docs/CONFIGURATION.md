# Configuration

Every knob is an environment variable, read once at startup and immutable
thereafter. This is the agent's core security property: its powers are fixed by
the operator's startup configuration, never by a client. Changing any of these
requires host access and a container restart — there is deliberately no API,
file, or signal that can widen what was granted here.

## Environment variables

| Variable | Type | Default | Validation |
|---|---|---|---|
| `DEVMON_STATE_DIR` | path | `/var/lib/devmon` | Absolute path |
| `DEVMON_LISTEN_ADDR` | host:port | `:8443` | Parses as host:port; port 1–65535 |
| `DEVMON_PUBLIC_ADDR` | comma list | *(required)* | ≥1 entry; each a DNS name or IP; used as server-certificate SANs |
| `DEVMON_POLICY_MODE` | enum | `default` | One of `read-only`, `default`, `full` |
| `DEVMON_DOCKER_HOST` | URL | `unix:///var/run/docker.sock` | Scheme `unix` or `tcp` |
| `DEVMON_SELF_CONTAINER` | name or ID | *(auto-detected)* | Docker's own name grammar: `[a-zA-Z0-9][a-zA-Z0-9_.-]+` (two characters or more). Hex container IDs satisfy it too |
| `DEVMON_PROTECTED_CONTAINERS` | comma list | *(none)* | Each entry a container name or ID under the same grammar as `DEVMON_SELF_CONTAINER`; blank entries ignored; duplicates dropped |
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

## Invalid configuration and exit codes

Bad configuration is reported in full, before anything starts, with exit code 2:

```
$ docker run … -e DEVMON_POLICY_MODE=admin -e DEVMON_LOG_MAX_AGE_DAYS=x …
invalid configuration:
  DEVMON_POLICY_MODE: "admin" is not a valid policy mode (want one of: read-only, default, full)
  DEVMON_LOG_MAX_AGE_DAYS: "x" is not an integer
```

Every problem is listed at once rather than one per restart.

| Exit code | Meaning |
|---|---|
| `0` | Clean shutdown |
| `1` | Runtime failure |
| `2` | Invalid configuration |

## Policy modes

The tier is a strict superset of the one below it, and the default is the middle
one — useful out of the box, incapable of destroying anything.

| Mode | Permits |
|---|---|
| `read-only` | list, inspect, logs, event stream |
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

## The agent excludes itself

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

A malformed value is a startup configuration error (exit 2), not a warning: this
is the documented fix for the one case where lifecycle is unavailable, so a typo
must surface at start rather than when the button stays grey. A value that is
well-formed but names some *other* container protects that one instead — the
override is trusted. One that names nothing the Engine knows is discarded with a
WARN, and the agent falls back to the filesystem candidates as if it were unset.

Running the agent directly on the host rather than in a container is fine: there
is no container to protect, so lifecycle works normally and the agent says so
once at INFO.

Before 0.2.0 this variable was called `DEVMON_SELF_CONTAINER_ID`; see
[Upgrading](INSTALL.md#upgrading).

## Protecting other containers

Self-exclusion covers exactly one container: the agent's own. Everything else
the policy mode permits is fair game for every paired device. That is the wrong
boundary as soon as a host runs **more than one agent** — one per team, per
tenant, or per policy tier — because a device paired to agent A cannot touch A,
but it can stop, kill, or delete agent B, and B's devices can do the same to A.
The same gap covers any container an operator considers off-limits to remote
hands: a reverse proxy, a database, a backup job.

`DEVMON_PROTECTED_CONTAINERS` closes it. It is a comma-separated list of
container names or IDs; a container matching any entry is refused by every
lifecycle route with 403 `container is protected by host configuration`, in
every policy mode, and its rows in `GET /v1/containers` and
`GET /v1/containers/{id}` carry `"protected": true` — the same field the agent's
own row uses, so an app that already greys out the agent's controls greys these
out too without knowing why. Reads, inspect, logs, and the event stream are not
affected: protection stops a device from *changing* a container, not from
*seeing* it.

```yaml
services:
  devmon-agent-team-a:
    container_name: devmon-agent-team-a
    environment:
      DEVMON_SELF_CONTAINER: devmon-agent-team-a
      DEVMON_PROTECTED_CONTAINERS: devmon-agent-team-b,traefik
```

How an entry matches:

- An entry matches a container whose **name** equals it exactly. Names are
  case-sensitive and compared without Docker's leading `/`.
- An entry that is 12 or 64 lowercase hex characters *also* matches a container
  whose **ID** starts with (12) or equals (64) it. No other prefix length is
  honoured: a name such as `cafe` must not quietly protect every container whose
  ID happens to begin with it. An entry that is both a real name and someone
  else's short ID matches both; over-protection is the safe direction.
- **Prefer names**, for the reason given under `DEVMON_SELF_CONTAINER` above: an
  ID is stale the next time the container is recreated, a name survives.

Entries are not checked against the Engine at startup. A name that matches
nothing today is legitimate — it protects that container whenever it appears,
which is what a compose stack that recreates containers needs. The list is
logged once at INFO at startup so an operator can confirm what was protected.

When a target is both the agent itself and listed here, the self rule answers
first, with its own body; the list never weakens self-exclusion and cannot be
used to opt out of it. Like every other variable on this page the list is read
once at startup: no device can read it back, shorten it, or extend it.

A malformed entry is a startup configuration error (exit 2), reported with the
offending value, in the same way as `DEVMON_SELF_CONTAINER`. Blank entries and
exact duplicates are ignored.

## Retention

Operational logs and the audit record have separate budgets, and the audit
record must outlive the logs — a configuration that inverts this is rejected. One
shared short budget would quietly destroy the security record to make room for
debug output.

Log growth is bounded by whichever of age or total size is hit first, so the
agent cannot be the reason a small VPS runs out of disk.

The audit row ceiling is spent per device, not first-come-first-served; see
[Audit log](OPERATIONS.md#audit-log).

## Rate limiting

The listening port is rate limited in three tiers, so it survives being scanned.

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
[Reaching it from outside](INSTALL.md#reaching-it-from-outside).

Limits are startup configuration like everything else, so a client can never
raise its own ceiling, and the values are advertised nowhere — telling a scanner
exactly how fast it may go unnoticed would defeat the point. The minimum is `1`,
not `0`: there is deliberately no value that turns a limiter off. An operator who
wants a higher ceiling raises the number.

Connection establishment, body size, and open streams are bounded separately —
by the OS accept queue and `ReadHeaderTimeout`, by per-route body caps, and by a
concurrent-stream ceiling. A connection limiter belongs in front of the process,
in `iptables` or a VPN; see [`THREAT-MODEL.md`](THREAT-MODEL.md).
