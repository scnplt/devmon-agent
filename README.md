# devmon-agent

A Go agent that exposes a narrow, mTLS-authenticated Docker control API, so an
Android client can inspect and restart containers without SSH and without
exposing the Docker socket to the internet.

**Status: Phase 1 — secure foundation & persistence.** The agent starts, validates
its configuration, opens durable state, terminates TLS, and serves one
unauthenticated informational endpoint. There is no pairing, no authentication,
and no Docker read or write operation yet; those arrive in Phases 2–5.

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
# {"api_version":"v1","agent_version":"0.1.0","policy_mode":"default","server_time":"…Z"}
```

The certificate is self-signed in Phase 1, so `-k` is expected. Phase 2 issues it
from an agent-held CA that the Android client pins during pairing.

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
│   ├── server.crt                 self-signed in Phase 1                 0644
│   └── server.key                 EC P-256 private key                   0600
└── logs/                                                                 0700
    ├── agent.log                  current                                0600
    └── agent-….log.gz             rotated and compressed                 0600
```

A **bind mount, not a named volume**, deliberately: the operator can see, back
up, and restore it, and `docker compose down -v` cannot destroy it. From Phase 2
onward this directory holds the agent's identity — losing it unpairs every
device.

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

| Route | Auth | Response |
|---|---|---|
| `GET /v1/status` | none | `api_version`, `agent_version`, `policy_mode`, `server_time` |

`/v1/status` is the only endpoint served without a client certificate. Its fields
are a strict allowlist — it may inform, never issue — and it carries no host,
container, or credential data. Advertising the policy mode lets a client tell
"the agent refuses to restart containers" apart from "the agent is broken"
without needing a credential.

TLS 1.3 is the floor. Both peers are ours, so there is nothing to negotiate down
to, and Android has supported it since API 29.

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
