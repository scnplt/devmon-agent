# devmon-agent

A Go agent that exposes a narrow, mTLS-authenticated Docker control API, so an
Android client can inspect and restart containers without SSH and without
exposing the Docker socket to the internet.

**Status: Phase 2 — identity, pairing & revocation.** The agent is its own
certificate authority. An operator mints a pairing code on the host, the device
generates a keypair and exchanges a CSR for a client certificate, and every
guarded request is authenticated against that certificate. Revocation takes
effect on the next request. There is still no Docker read or write operation;
those arrive in Phases 3–5.

License: AGPL-3.0-only.

---

## Install

Two host prerequisites first. Both will otherwise look like agent bugs, and both
are consequences of the container running as `nonroot` (UID 65532):

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
  -e DEVMON_PUBLIC_ADDR=vps.example.com \
  ghcr.io/scnplt/devmon-agent:0.1.0
```

See `compose.example.yaml` for the equivalent Compose file. An automated
installer that resolves the socket GID for you is Phase 6.

Verify it is up:

```bash
curl -sk https://vps.example.com:8443/v1/status
# {"api_version":"v1","agent_version":"0.1.0","policy_mode":"default",
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
| `DEVMON_LOG_LEVEL` | enum | `info` | One of `debug`, `info`, `warn`, `error` |
| `DEVMON_LOG_MAX_AGE_DAYS` | int | `1` | ≥1 |
| `DEVMON_LOG_MAX_TOTAL_MB` | int | `64` | ≥8 |
| `DEVMON_AUDIT_MAX_AGE_DAYS` | int | `365` | ≥1, and ≥ `DEVMON_LOG_MAX_AGE_DAYS` |
| `DEVMON_AUDIT_MAX_ROWS` | int | `100000` | ≥1000 |

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

### Retention

Operational logs and the audit record have separate budgets, and the audit
record must outlive the logs — a configuration that inverts this is rejected. One
shared short budget would quietly destroy the security record to make room for
debug output.

Log growth is bounded by whichever of age or total size is hit first, so the
agent cannot be the reason a small VPS runs out of disk.

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

---

## API

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

Failure modes shared by every read route:

| Condition | Status | Body |
|---|---|---|
| No, unknown, or revoked client certificate | 401 | `{"error":"client certificate required"}` |
| Host policy forbids the operation | 403 | `{"error":"operation not permitted by host policy"}` |
| Malformed object reference | 400 | `{"error":"invalid object reference"}` |
| No such object | 404 | `{"error":"not found"}` |
| Engine unreachable, timed out, or otherwise failing | 502 | `{"error":"docker engine unavailable"}` |

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

**Command lines are returned.** Unlike env vars, the command, entrypoint, and
args are what identify a misconfigured container, and they are already visible
in the host's process table and in `docker ps`. This is a bounded, deliberate
disclosure rather than an oversight.

**Lists are capped at 500 items** and carry a `truncated` flag. A host with
thousands of images would otherwise send a multi-megabyte body to a phone on a
mobile connection. The cap is server-side and cannot be raised by a client,
which is the same rule the rest of the agent's configuration follows.

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
to, and Android has supported it since API 29.

---

## Pairing

A device generates its own keypair and sends only a certificate signing request.
The agent never sees a device private key, so there is no moment at which one is
in transit or at rest anywhere but on the device that owns it.

### 1. Mint a code on the host

```bash
docker exec devmon-agent /usr/local/bin/devmon-agent \
  device pair-code --name "pixel-8"
# Pairing code: B4HJFH3KEAZ2QMY546LLN3IDOW
# Expires:      2026-08-07T20:12:00Z
```

The code is single-use, valid for **10 minutes**, and printed to stdout only —
never to `agent.log`, which is persisted and rotated. Only its SHA-256 is
stored. Nobody, including the operator, can read it back out of the database, so
a lost code is minted again rather than recovered.

### 2. Redeem it

The Android app does this. To do it by hand:

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
subcommands on the agent binary itself:

```bash
docker exec devmon-agent /usr/local/bin/devmon-agent device <subcommand>
```

| Subcommand | Effect |
|---|---|
| `device list` | Every registered device: ID, name, paired, last seen, state |
| `device pair-code --name <name>` | Mint a single-use code, valid 10 minutes |
| `device revoke <id>` | Withdraw a device's access, effective immediately |

```bash
$ docker exec devmon-agent /usr/local/bin/devmon-agent device list
ID                NAME     PAIRED                LAST SEEN             STATE
3f9a1c74b2e05d68  pixel-8  2026-08-07T20:02:00Z  2026-08-07T21:14:33Z  active

$ docker exec devmon-agent /usr/local/bin/devmon-agent device revoke 3f9a1c74b2e05d68
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

---

## Development

```bash
make build          # -> bin/devmon-agent
make test           # unit tests
make test-race      # with -race (needs a C toolchain)
make cover          # coverage; the floor is 80% over ./internal/...
make lint           # gofmt + go vet + golangci-lint when installed
make sec            # gosec
make image          # docker build
```

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
