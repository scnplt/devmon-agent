# API

The machine-readable form of everything below is
[`openapi.yaml`](openapi.yaml) — OpenAPI 3.1, covering every route, payload,
and failure body. Generate a client from it rather than hand-writing one, and
diff it between releases to see what changed. This document keeps the
reasoning; the spec keeps the shapes.

## Routes

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

## Lifecycle semantics

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

## Log parameters

Both log routes accept `?tail=<n>` and `?since=<rfc3339>`. `tail` is bounded to
1…2000 and falls back to its default — 200 historical, 100 for the stream — when
it is absent, unparsable, or out of range, because a typo must not fail a
diagnostic request in the middle of an incident. `since` is the opposite: it is
interpolated into the Engine's own request URL, so an unparsable value is a 400
rather than a silently ignored preference.

## Failure modes

Shared by every read route:

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

Over a rate limit, every route answers 429 with a `Retry-After` header; see
[Rate limiting](CONFIGURATION.md#rate-limiting).

## Why responses are projections

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

## Live log streaming

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

## Container health event stream

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

## Authentication

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
rejected. Live SSE streams are each one long-lived request, so they re-check
revocation on every keepalive/heartbeat tick as well: a stream the device
already had open ends within one tick interval (at most ~25 seconds) of the
revoke, with a terminal `event: error` frame carrying `device revoked`.

TLS 1.3 is the floor. Both peers are ours, so there is nothing to negotiate down
to, and every current client TLS stack supports it.

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
stdout only — never to `agent.log`, which is persisted and rotated. Only its
SHA-256 is stored. Nobody, including the operator, can read it back out of the
database, so a lost code is minted again rather than recovered.

`--ttl <minutes>` picks a different lifetime, from 5 minutes up to the
`DEVMON_PAIR_TTL_MAX_MIN` ceiling the operator fixed at startup (itself capped
at 60). A value outside that range is a hard error — never silently clamped —
and when the flag is omitted the default is 10 minutes or the ceiling,
whichever is lower. A longer-lived code is a longer exposure window if it
leaks unused, which is why the ceiling is startup configuration: nothing
reachable from a client can raise it.

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

### Unpairing

A device removes itself with `DELETE /v1/device/self`. The operator removes a
device from the host with `device revoke <id>`; see
[Managing devices](OPERATIONS.md#managing-devices).
