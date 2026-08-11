# Backup and Restore

This expands the backup note in [`README.md`](../README.md#backup). Read
[`docs/THREAT-MODEL.md`](THREAT-MODEL.md) first if you have not — the central
fact here is a security property, not an operational nicety: **the backup you
are about to make is itself a credential.**

## What to back up

The whole of `$DEVMON_STATE_DIR` — nothing less, nothing partial. It is a bind
mount specifically so this is possible:

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

— [`README.md:310-329`](../README.md) (`## State directory`).

`logs/` is operational output, not identity, and does not need to survive a
restore for the agent to run — but back it up anyway if you are diagnosing
something, since it is the only record of what the agent did before an
incident that predates the audit log's retention window.

The two things that matter for the agent to run at all after a restore are
`devmon.db` (the device registry and the audit log) and `certs/` (the CA and
server keypair). Back up less than the whole directory and you risk
recreating the exact partial-identity state
[`internal/certs/store.go:234-271`](../internal/certs/store.go)
(`CheckIdentityConsistency`) exists to catch and refuse to start against.

## The backup is a credential

In the README's own words, which this document does not restate differently:

> `ca.key` is stored unencrypted. There is nowhere to keep a passphrase that
> an unattended container restart could reach, so encrypting it would buy
> nothing but ceremony. The consequence is concrete and worth stating plainly:
> anyone who can read that file can mint a client certificate this agent will
> accept. It is protected by file mode `0600` and directory mode `0700` — and
> by nothing else. **It is therefore present, in the clear, in every host
> backup and every VPS snapshot of this directory.** Treat those backups as
> credentials: encrypt them at rest, and if one is exposed, delete `certs/` on
> the host and restart. The agent then creates a new CA, every paired device
> is unpaired, and the fingerprint changes.
>
> — [`README.md:331-340`](../README.md)

Concretely, that means:

- **Encrypt the backup archive**, not just the disk it eventually lands on.
  `gpg -c`, an encrypted tarball, or an encrypted destination bucket — pick
  one, but pick one.
- **Do not attach it to a support ticket, a chat message, or an issue.**
  `SECURITY.md` lists `certs/` explicitly as something never to paste into a
  report: "Anything from `certs/` — `ca.key` is the agent's entire identity"
  ([`SECURITY.md:44-49`](../SECURITY.md)).
- **A copy that outlives its purpose is a liability, not a convenience.**
  Delete old backups on the same retention discipline you would apply to any
  other credential store.
- **If a backup is ever exposed** — a misconfigured bucket, a stolen laptop
  holding one, anything — treat it exactly as the README says: delete
  `certs/` on the live host and restart. See "If `certs/` is lost" below for
  what that costs.

## Stop-then-copy for a consistent backup

The database runs in WAL mode: committed pages can sit in `devmon.db-wal`
without having been checkpointed into `devmon.db` yet
([`internal/state/store.go:106-123`](../internal/state/store.go),
`tightenPermissions`'s comment on why the sidecars matter as much as the main
file). Copying the directory while the agent is running risks a backup that
splits a transaction across files. Stop first:

```bash
docker stop devmon-agent
sudo tar czf devmon-backup.tgz -C /var/lib/devmon
docker start devmon-agent
```

— [`README.md:346-355`](../README.md). This checkpoints the write-ahead log
before the copy runs, so the archive holds a single consistent state rather
than a snapshot mid-write.

If your `$DEVMON_STATE_DIR` is not `/var/lib/devmon`, substitute it — but keep
`-C /` and the mount's absolute path, so the archive extracts back to an
absolute path rather than depending on the working directory of whoever runs
the restore.

## Restore, with the right ownership and modes

The image runs as UID `65532` (`nonroot`), the same UID `install.sh` uses when
it prepares a fresh state directory:

```bash
# NONROOT_UID='65532', NONROOT_GID='65532'
# — install.sh:30-31
```

Extract to the target path, then set ownership and mode exactly as a fresh
install would:

```bash
sudo mkdir -p /var/lib/devmon
sudo tar xzf devmon-backup.tgz -C / var/lib/devmon
sudo chown -R 65532:65532 /var/lib/devmon
sudo chmod 700 /var/lib/devmon
sudo chmod 700 /var/lib/devmon/certs /var/lib/devmon/logs
sudo chmod 600 /var/lib/devmon/devmon.db
sudo chmod 600 /var/lib/devmon/certs/ca.key /var/lib/devmon/certs/server.key
sudo chmod 644 /var/lib/devmon/certs/ca.crt /var/lib/devmon/certs/server.crt
```

The directory and file modes mirror the layout table above and
`install.sh`'s own `prepare_state_dir` step: "the image runs as UID
`$NONROOT_UID`, so the directory must be owned by it," followed by `chown` and
`chmod 700` on the state directory
([`install.sh:438-444`](../install.sh)). A wrong owner fails startup at
`MkdirAll` with "permission denied," which reads as an agent bug rather than a
restore step that was skipped — get ownership right before starting the
container, not after.

Once ownership and modes are correct, start the agent normally. It validates
the restored database is a readable, uncorrupted SQLite file at open —
"Restore by extracting to the same path with the same ownership. The agent
detects a truncated or corrupt `devmon.db` at startup and refuses to run
rather than failing obscurely at the first query."
([`README.md:357-359`](../README.md)).

## If `certs/` is lost

This is the specific case where the database survives but the certificate
authority does not — a partial restore, a `certs/` directory that failed to
copy, or a bind mount that lost only that subtree.

The agent detects it and refuses to start, loudly, rather than minting a
fresh CA that would look like a normal first run:

```go
// dbExisted && !caExists
return fmt.Errorf("%w: the state database exists but the certificate authority in %s does not; %s",
    ErrIdentityIncomplete, certsDir, reissueGuidance)
```

— [`internal/certs/store.go:264-266`](../internal/certs/store.go)
(`CheckIdentityConsistency`), where `reissueGuidance` is: "if you proceed by
clearing the state directory, every paired device must re-pair"
([`internal/certs/store.go:259`](../internal/certs/store.go)).

If you deliberately proceed — clearing the remaining state and letting the
agent start clean — here is exactly what happens:

- **A new CA is generated.** `LoadOrCreateCA` finds no existing keypair and
  creates one: [`internal/certs/ca.go:58-88`](../internal/certs/ca.go).
- **Every paired device is unpaired.** The device registry that named them is
  gone along with `devmon.db`, and even if it were not, a device's
  certificate is signed by a CA that no longer exists — it fails verification
  against the new one regardless.
- **The CA fingerprint changes.** The new CA's fingerprint is different by
  construction — it is the SHA-256 of a freshly generated certificate
  ([`internal/certs/ca.go:220-227`](../internal/certs/ca.go),
  `Fingerprint`) — and every device must compare it fresh at re-pairing time,
  exactly as at first install.
- **The agent says so.** On the transition from no-CA to CA-present, the
  agent logs the new fingerprint at WARN exactly once, which is what lets the
  operator record it: `LoadOrCreateCA`'s `created` return value exists
  precisely so "the caller (main.go) logs the fingerprint at WARN exactly
  once, on that transition, so the operator can record it."
  ([`internal/certs/ca.go:58-61`](../internal/certs/ca.go)).

There is no partial recovery path — a lost CA is not a smaller version of a
lost state directory, it is the same event. Every device re-pairs, in full,
the same way it did the first time: see `## Pairing` in
[`README.md`](../README.md).

## Related documents

- [`docs/THREAT-MODEL.md`](THREAT-MODEL.md) — why the CA key being unencrypted
  is an accepted risk rather than a defect, and what backup exposure means in
  that context.
- [`SECURITY.md`](../SECURITY.md) — what never to paste into a bug report or
  advisory, including anything from `certs/`.
- [`README.md`](../README.md) — the full state directory layout, the pairing
  flow, and the original backup note this document expands.
