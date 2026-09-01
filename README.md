# devmon-agent

A small Go agent that exposes a narrow, mTLS-authenticated Docker control API.
A paired client can inspect containers, images, networks and volumes, read and
follow logs, and start, restart, stop, kill or delete containers on your host —
without SSH and without exposing the Docker socket to the internet.

- **The agent is its own certificate authority.** You mint a pairing code on
  the host, the device exchanges a CSR for a client certificate, and every
  guarded request is authenticated against it. Revocation takes effect on the
  next request.
- **What a client may do is fixed on the host.** A policy mode chosen at
  startup bounds every device, and no client can widen it.
- **It never acts on its own container**, every mutating attempt is recorded in
  an audit table that outlives the operational log, and the port is rate
  limited so it survives being scanned.

Current version: **0.6.0** ([CHANGELOG.md](CHANGELOG.md)).
License: [AGPL-3.0-only](LICENSE).

## Install

You need a Linux host with Docker and Docker Compose, and a hostname or IP the
device will reach it at.

```bash
git clone https://github.com/scnplt/devmon-agent.git
cd devmon-agent
./install.sh --public-addr vps.example.com
```

The installer resolves the Docker socket group, creates the state directory,
writes a `compose.yaml`, starts the agent, and prints the CA fingerprint and
your first pairing code. `--dry-run` shows everything it would do without
touching the host; `--help` lists every flag.

**Write the CA fingerprint down, off the host.** It is what lets you tell the
real agent apart from something else answering on that port when you pair.

Check it is up:

```bash
curl -sk https://vps.example.com:8443/v1/status
```

Upgrade with `docker compose pull && docker compose up -d`.

Installing by hand, container hardening, firewalls, VPNs, reverse proxies and
CGNAT: [docs/INSTALL.md](docs/INSTALL.md).

## Pair a device

```bash
docker exec devmon-agent devmon device pair-code --name "pixel-8"
```

Enter the code in the app. It is single-use, valid for 10 minutes by default,
and never written to a log. The app compares the agent's CA fingerprint against
the one you recorded; if they differ, something else answered.

Doing the exchange by hand with `openssl` and `curl`, certificate renewal, and
unpairing: [docs/API.md#pairing](docs/API.md#pairing).

## Operator commands

The image has no shell, so the operator CLI is a set of subcommands on the
agent binary, run through `docker exec`:

```bash
docker exec devmon-agent devmon <command>
```

| Command | What it does |
|---|---|
| `device list` | List every paired device: ID, name, paired, last seen, state |
| `device pair-code --name <name> [--ttl <minutes>]` | Mint a single-use pairing code |
| `device revoke <id>` | Withdraw a device's access, effective immediately |
| `audit list [--limit N]` | Print recent audit rows, most recent first (default 100) |
| `health` | Probe the agent's own listener; this backs the image's `HEALTHCHECK` |
| `help`, `<command> --help` | Usage screens |

Device management, the health probe, the audit log, the state directory and
backups: [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Configuration

Every setting is an environment variable, read once at startup. A client can
never change any of them. The installer writes them into `compose.yaml`; edit
the file and run `docker compose up -d` to apply a change.

| Variable | Default | Meaning |
|---|---|---|
| `DEVMON_PUBLIC_ADDR` | *(required)* | Hostname(s) or IP(s) the device dials, comma-separated. Becomes the server certificate's SANs |
| `DEVMON_POLICY_MODE` | `default` | `read-only`, `default`, or `full` — see below |
| `DEVMON_LISTEN_ADDR` | `:8443` | Listen address inside the container |
| `DEVMON_STATE_DIR` | `/var/lib/devmon` | Identity, device registry, audit log, and operational logs |
| `DEVMON_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker Engine endpoint (`unix://` or `tcp://`) |
| `DEVMON_SELF_CONTAINER` | *(auto-detected)* | The agent's own container name, so it can refuse to act on itself |
| `DEVMON_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `DEVMON_LOG_MAX_AGE_DAYS` | `1` | Operational log retention (days) |
| `DEVMON_LOG_MAX_TOTAL_MB` | `64` | Operational log size budget (MB, at least 8) |
| `DEVMON_AUDIT_MAX_AGE_DAYS` | `365` | Audit retention (days); must be at least the log retention |
| `DEVMON_AUDIT_MAX_ROWS` | `100000` | Audit row ceiling (at least 1000) |
| `DEVMON_RATE_STATUS_PER_MIN` | `30` | `GET /v1/status` limit, per client IP |
| `DEVMON_RATE_PAIR_PER_MIN` | `5` | `POST /v1/pair` limit, per client IP |
| `DEVMON_RATE_GUARDED_PER_SEC` | `20` | Authenticated-route limit, per device |
| `DEVMON_PAIR_TTL_MAX_MIN` | `10` | Ceiling for `device pair-code --ttl`, 5 to 60 minutes |

Policy modes, each a strict superset of the one above it:

| Mode | Permits |
|---|---|
| `read-only` | list, inspect, logs, event stream |
| `default` | the above, plus start, restart, stop |
| `full` | the above, plus kill, delete |

Invalid configuration is reported in full at startup with exit code 2. The
validation rules, self-exclusion, retention and rate limiting in depth:
[docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Documentation

| Document | Covers |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | Installing by hand, hardening, reaching the agent from outside, CGNAT, upgrading |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | Every variable and its validation, policy modes, self-exclusion, retention, rate limiting |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | State directory, backup, managing devices, health probe, audit log |
| [docs/API.md](docs/API.md) | Every route, failure bodies, response projections, log and event streams, pairing |
| [docs/openapi.yaml](docs/openapi.yaml) | The API contract as OpenAPI 3.1 — generate a client from this |
| [docs/BACKUP.md](docs/BACKUP.md) | Backup and restore, and why the backup is itself a credential |
| [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) | What is and is not defended, with citations into the code |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Make targets, the end-to-end suite, CI |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Branching, commits, the gate list, and what will be declined |
| [SECURITY.md](SECURITY.md) | How to report a vulnerability |

## Development

```bash
make build          # -> bin/devmon-agent
make test-race      # unit tests with -race
make cover          # coverage over ./internal/...; the floor is 90%
make lint           # gofmt + go vet + golangci-lint
make sec            # gosec
make e2e            # end-to-end suite against a real Docker Engine
```

The full target list, the e2e suite and what CI runs are in
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md); the contribution process is in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Security

This agent holds a Docker socket. Anything that can drive its API can reach
root-equivalent power over the host, so a vulnerability here is a host
compromise rather than a container-management bug. Never open a public issue
for a security problem — report it through GitHub Security Advisories as
described in [SECURITY.md](SECURITY.md).

## License

Copyright (C) 2026 Sertan Canpolat.

Licensed under the GNU Affero General Public License v3.0 only
(`AGPL-3.0-only`). The full text is in [LICENSE](LICENSE), and every source
file carries the SPDX identifier.

The AGPL's network clause is deliberate: if you run a modified version of this
agent as a service for others, they are entitled to its source.
