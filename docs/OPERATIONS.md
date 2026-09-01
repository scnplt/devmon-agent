# Operations

Day-to-day operation of a running agent: what lives in the state directory,
how to back it up, how to manage paired devices, and how to read the health
probe and the audit log.

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
> accept. It is protected by file mode `0600` and directory mode `0700` — and
> by nothing else. **It is therefore present, in the clear, in every host backup
> and every VPS snapshot of this directory.** Treat those backups as
> credentials: encrypt them at rest, and if one is exposed, delete `certs/` on
> the host and restart. The agent then creates a new CA, every paired device is
> unpaired, and the fingerprint changes.

The audit trail lives in `devmon.db`, not in `logs/`. That is what the SQLite
choice bought: the host-side CLI can read it while the agent writes, and
retention is an indexed `DELETE` rather than file surgery.

## Backup

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
key. [`BACKUP.md`](BACKUP.md) covers restore, ownership, and what happens if
`certs/` is lost.

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

Revocation is enforced on the next request the device makes, and a live log or
event stream the device already had open ends within one keepalive tick (at
most about 25 seconds). See [Authentication](API.md#authentication).

`last seen` is written at most once a minute. It is an operator aid, not an
audit record, and paying a write on every request to sharpen it would be a poor
trade.

Reading this list while the agent runs is safe and expected — that is what WAL
mode and the busy timeout were configured for.

The pairing flow itself — minting a code, redeeming it by hand, and renewal —
is in [Pairing](API.md#pairing).

## Health

The image carries a `HEALTHCHECK`, so `docker ps` reports a health state
alongside the running state:

```bash
$ docker ps --filter name=devmon-agent --format '{{.Names}} {{.Status}}'
devmon-agent    Up 4 hours (healthy)
```

It runs `devmon-agent health`, a subcommand on the same binary — for the same
reason as the device and audit commands, there is no shell and no curl in the
image to run a probe with. The subcommand makes one HTTPS `GET /v1/status`
against the agent's own listener on loopback and exits 0 or 1.
`restart: unless-stopped` alone only reacts to the process exiting; this also
catches a listener that is up but no longer answering.

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

## Audit log

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

`--limit` defaults to 100 rows, most recent first.

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
[`THREAT-MODEL.md`](THREAT-MODEL.md) states in full.
