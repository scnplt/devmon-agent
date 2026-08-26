# Changelog

All notable changes to this project are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versions here are the **image tags** — `ghcr.io/scnplt/devmon-agent:X.Y.Z`. The
git tag carries a `v` prefix (`vX.Y.Z`); the image tag does not.

An `Internal` section appears where there is one. It is not user-facing and
changes nothing about the agent's behaviour, but this is an open repository and
a contributor reading the history should not have to reconstruct why the build
moved.

## [Unreleased]

### Fixed

- **`GET /v1/containers/{id}` now reports `image` and `image_id` consistently
  with the container list.** The detail response previously returned the
  Engine's digest-prefixed image ID in the `image` field. `image` now carries
  the image reference the container was created from (as in the list route),
  and a new `image_id` field carries the digest-prefixed ID. Clients reading
  the digest out of `image` should switch to `image_id`. ([#120])

## [0.4.0] - 2026-08-26

A feature release. The agent gains `GET /v1/events/stream`, a live feed of
container health and lifecycle transitions, so a paired client can show a
notification the moment a container turns unhealthy or dies instead of polling
`GET /v1/containers` on a timer.

The configuration surface is unchanged: every `DEVMON_*` variable means in
0.4.0 exactly what it meant in 0.3.0, and no variable was added or removed.
The new route is additive — no existing route changed its behaviour.

### Added

- **`GET /v1/events/stream` — a Server-Sent Events stream of container health
  and lifecycle transitions.** The stream always opens with one
  `event: snapshot` frame carrying `{id, name, state, health}` for every
  container, then forwards `event: health` frames as Engine events arrive —
  exactly `health_status: healthy`, `health_status: unhealthy`, `die`, `start`,
  `stop` and `oom`. There is deliberately no replay (`?since=`,
  `Last-Event-ID`): the snapshot is the single recovery path after any gap. A
  `: heartbeat` comment every 25 seconds keeps intermediaries from reaping an
  idle connection. The route sits behind the same guard chain as every other
  read route, one Docker `/events` subscription is fanned out to all clients
  through bounded per-client buffers, and each device holds at most one stream
  — a newer connection supersedes the older one. The Engine's raw event action
  and attributes can carry healthcheck output and container labels, so a closed
  action allowlist and a `name`-only attribute read keep them off the wire and
  out of the log. Event streams consume none of the log-stream budget.
  ([#116](https://github.com/scnplt/devmon-agent/pull/116))

### Changed

- **`modernc.org/sqlite` bumped from 1.56.0 to 1.57.0** — the release's only
  runtime dependency change.
  ([#112](https://github.com/scnplt/devmon-agent/pull/112))

### Internal

- The builder image moved from `golang:1.26-alpine` to `golang:1.27-alpine`
  and the `distroless/static-debian12` digest pin was refreshed; both stay
  digest-pinned under `GOTOOLCHAIN=local`.
  ([#110](https://github.com/scnplt/devmon-agent/pull/110),
  [#111](https://github.com/scnplt/devmon-agent/pull/111))
- `actions/deploy-pages` and `actions/upload-pages-artifact` moved to v5 in the
  Pages workflow.
  ([#113](https://github.com/scnplt/devmon-agent/pull/113),
  [#114](https://github.com/scnplt/devmon-agent/pull/114))
- The git workflow rules now require labels on every issue and PR and an issue
  link on every PR, and code comments cite sibling code by name rather than
  line number so the references survive edits.
  ([#109](https://github.com/scnplt/devmon-agent/pull/109),
  [#115](https://github.com/scnplt/devmon-agent/pull/115))

## [0.3.0] - 2026-08-20

A hardening release. Two of the agent's shared budgets — the live log stream
slots and the audit table — were host-wide, so a single paired device could
exhaust either one and lock every other device out of it. Both are now bounded
per device under an unchanged global ceiling. The image also gained a working
`HEALTHCHECK`, which a distroless base made impossible until the agent could
probe itself.

The configuration surface is unchanged: every `DEVMON_*` variable means in
0.3.0 exactly what it meant in 0.2.0, and no variable was added or removed.

### Added

- **`devmon-agent health`, and the image `HEALTHCHECK` it makes possible.** The
  image is `distroless/static:nonroot` — no shell, no curl — so the only thing
  in it that can make an HTTPS request is the agent's own binary. The
  subcommand performs one `GET /v1/status` against the agent's own listener on
  loopback and exits 0 or 1. `restart: unless-stopped` reacts to the process
  exiting; it does not react to a listener that is up and no longer answering,
  and `docker ps` previously showed no health state at all for an agent that
  reports `health` for every other container on the host.
  ([#89](https://github.com/scnplt/devmon-agent/pull/89))

- **Audit rows for pairing, certificate renewal and self-revocation.** Identity
  events are the highest-value entries an audit trail can hold, yet only the
  five container-lifecycle routes went through `withAudit`; identity events
  were recoverable only from the operational log, whose retention budget is
  deliberately shorter than the audit table's. Pair rows carry the newly
  created device ID on success and an empty device ID on failure. Details never
  contain the submitted code or CSR.
  ([#69](https://github.com/scnplt/devmon-agent/pull/69))

### Changed

- **Live log streams are budgeted per device — three each — under the unchanged
  host ceiling of eight.** The host-wide budget meant one device holding slots
  answered 503 to every other device without any authentication failure or
  policy violation. The two refusals are now distinct: a device at its own cap
  gets `too many concurrent log streams for this device`, a genuinely full host
  gets the existing `too many concurrent log streams` and a WARN naming the
  holders. This is the release's one caller-visible behaviour change — a client
  that legitimately held four or more concurrent streams from a single device
  now sees a 503 where 0.2.0 served the stream.
  ([#93](https://github.com/scnplt/devmon-agent/pull/93))

- **The audit table is trimmed fair-share per device before the row-count
  backstop runs.** `PruneAudit` bounded the table's size but not whose rows were
  evicted. With the defaults a paired device may issue 20 admitted mutations per
  second, filling the 100,000-row budget in roughly 83 minutes; the oldest-first
  DELETE then removed every other device's history along with it. `maxRows` is
  now divided evenly across the device buckets still present and each bucket
  trimmed to its own newest rows. The row-count pass stays last and unchanged,
  so the hard disk-budget guarantee is untouched.
  ([#87](https://github.com/scnplt/devmon-agent/pull/87))

- **The image and both compose files are hardened.** Base images are digest
  pinned, so two builds of one commit are the same build, and `GOTOOLCHAIN=local`
  turns a base image that drifts below the `go.mod` requirement into a loud
  error rather than a silent mid-build toolchain download. `/var/lib/devmon` is
  pre-created and owned by `65532`. `compose.example.yaml` and the file
  `install.sh` generates both now set `no-new-privileges`, `cap_drop: [ALL]`,
  `read_only: true` with a tmpfs for `/tmp`, and `pids_limit: 256`. This
  container holds the Docker socket, so each setting narrows what a foothold
  inside it is worth; none changes how the agent behaves.
  ([#88](https://github.com/scnplt/devmon-agent/pull/88))

- **`install.sh` handles its arguments defensively and `--dry-run` is inert.** A
  trailing value-flag no longer aborts with dash's raw `shift: can't shift that
  many`, a declined re-run no longer leaves the state directory created and
  chowned, and `--dry-run` no longer requires a responding Docker daemon — the
  compose file can be previewed from a workstation, which is most of what the
  flag is for.
  ([#90](https://github.com/scnplt/devmon-agent/pull/90))

### Fixed

- **Retention was silently unenforced on two paths.** `PrunePairingCodes` was
  written and tested but never called from production code, so every minted
  pairing code was retained forever. Neither the pruner nor the log rotator ran
  a pass at startup — their first ticks land after 6h and 24h — so an agent
  restarted more often than that never enforced `DEVMON_AUDIT_MAX_AGE_DAYS`,
  `DEVMON_AUDIT_MAX_ROWS` or `DEVMON_LOG_MAX_AGE_DAYS` at all. Both now make one
  pass before entering their ticker loop. Only counts are logged, never codes.
  ([#67](https://github.com/scnplt/devmon-agent/pull/67))

- **A graceful shutdown no longer exits 1 while a log stream is open.**
  `http.Server.Shutdown` waits for in-flight requests but never cancels their
  contexts, so a live SSE stream pinned it for the full grace window: the
  process exited 1 on an ordinary `docker stop`, and the deferred Docker and
  state closes raced the still-running stream goroutine. A server-lifetime
  context is now wired in as `BaseContext` and cancelled first.
  ([#68](https://github.com/scnplt/devmon-agent/pull/68))

- **An aborted pairing request no longer leaves a permanent pending device
  row.** The rollback that deletes the half-created row ran on the request
  context, so a client that dropped the connection at the right moment made the
  cleanup fail on the same dead context — repeatable without authentication, at
  the pair-tier rate limit, and visible as an active device in `device list`.
  The rollback now runs on a detached context with its own timeout.
  ([#66](https://github.com/scnplt/devmon-agent/pull/66))

- **A container detail response no longer 500s on a short timestamp.**
  `zeroTimeToEmpty` sliced `ts[:4]` unguarded, so an Engine — or a proxy in
  front of one — returning e.g. `"0"` for `State.StartedAt` panicked into a 500.
  ([#65](https://github.com/scnplt/devmon-agent/pull/65))

- **A broken `DEVMON_PUBLIC_ADDR` is reported even when another variable is also
  wrong.** The missing-address problem was suppressed whenever the loader had
  already recorded a fault for any earlier field, so the operator learned about
  one variable, fixed it, restarted, and only then learned about the next — the
  one-fault-per-restart experience aggregated validation exists to prevent.
  ([#79](https://github.com/scnplt/devmon-agent/pull/79))

- **The identity routes answer 500 instead of panicking when no CA is
  configured**, and request logs record the matched route pattern rather than
  `r.URL.Path`, which is attacker-controlled up to the header budget.
  ([#86](https://github.com/scnplt/devmon-agent/pull/86))

- **`ErrNotModified` maps to 204 rather than a 502 and a false `engine_error`
  audit row.** The moby SDK returns nil for 304 today, so this is unreachable
  in practice; the half-wired state meant any change in SDK behaviour would have
  reported an Engine outage for "start an already-running container".
  ([#70](https://github.com/scnplt/devmon-agent/pull/70))

### Internal

- The OpenAPI lint gate documented since 0.1.0 now actually runs in CI, and
  fails on warnings rather than only errors.
  ([#82](https://github.com/scnplt/devmon-agent/pull/82),
  [#83](https://github.com/scnplt/devmon-agent/pull/83))
- The release workflow no longer pushes `:latest` unconditionally, and every
  third-party action is pinned by SHA.
  ([#84](https://github.com/scnplt/devmon-agent/pull/84))
- The release bar runs on pushes to `main`, not only on PRs into it, and CI
  verifies the container health projection against a real Engine 29 service.
  ([#71](https://github.com/scnplt/devmon-agent/pull/71),
  [#94](https://github.com/scnplt/devmon-agent/pull/94))
- `make lint` and `make e2e-lint` fail on `gofmt` and `golangci-lint` findings
  instead of reporting them and exiting 0, and a `.dockerignore` keeps runtime
  state out of the build context.
  ([#61](https://github.com/scnplt/devmon-agent/pull/61),
  [#62](https://github.com/scnplt/devmon-agent/pull/62))
- The coverage floor moved from 80% to 90%.
  ([#38](https://github.com/scnplt/devmon-agent/pull/38))
- Three flaky or sleep-synchronised tests were pinned to observable state: the
  shutdown-error contract, the sleep-based synchronisation across the suite, and
  the stream goroutine-leak assertion, which counted the whole process's
  goroutines under `t.Parallel()`.
  ([#73](https://github.com/scnplt/devmon-agent/pull/73),
  [#91](https://github.com/scnplt/devmon-agent/pull/91),
  [#96](https://github.com/scnplt/devmon-agent/pull/96))
- The dead self-signing certificate path is gone, drifted documentation
  citations are re-anchored, the `SECURITY.md` supported-versions table is
  drift-proof, and the local planning documents are no longer tracked.
  ([#63](https://github.com/scnplt/devmon-agent/pull/63),
  [#75](https://github.com/scnplt/devmon-agent/pull/75),
  [#76](https://github.com/scnplt/devmon-agent/pull/76),
  [#85](https://github.com/scnplt/devmon-agent/pull/85),
  [#86](https://github.com/scnplt/devmon-agent/pull/86))

## [0.2.0] - 2026-08-12

The agent can now identify its own container by **name**, which is the first
form of self-identification that survives a `docker compose up -d`. The knob
that carries it was renamed in the process, and the old name is not read — see
the breaking change below before upgrading.

### Changed

- **BREAKING: `DEVMON_SELF_CONTAINER_ID` is now `DEVMON_SELF_CONTAINER`, and it
  accepts a container name.** The old variable name is no longer read and no
  longer produces an error; an installation that still sets it is running as
  though nothing were set at all. That is only fatal where automatic detection
  fails — the exact case the variable existed for — and it fails the way it
  always has: an ERROR at startup and 503 on the five lifecycle routes, with
  reads, logs, pairing and status unaffected.

  The rename is what the widened value forced. The variable took 12- or
  64-character lowercase hex and nothing else, and an ID is the one form that
  cannot be set correctly: adding the variable to a compose file changes the
  container's spec, so the next `docker compose up -d` recreates the container
  and mints a new ID, and the value just written is stale before it is first
  read. Copying the new ID and repeating never converges. A name survives
  recreation and is set once. Keeping `_ID` on a field whose right answer is a
  name would have documented the trap rather than removed it.

  To upgrade: rename the variable, and give it the container's name.

  ```yaml
  services:
    devmon-agent:
      container_name: devmon-agent
      environment:
        DEVMON_SELF_CONTAINER: devmon-agent   # was DEVMON_SELF_CONTAINER_ID: 3f2a91c4e5b8
  ```

  A value that names nothing the Engine recognises is now discarded with a WARN
  and the agent falls back to its filesystem candidates, rather than being
  carried forward as a certainty. A malformed value is still a startup error
  (exit 2). ([#30](https://github.com/scnplt/devmon-agent/pull/30))

- **`install.sh` and `compose.example.yaml` pin `container_name` and set
  `DEVMON_SELF_CONTAINER`.** Self-identification was previously left to
  detection on a fresh install, which is correct on most hosts and silently
  wrong on the rest. Naming it makes a new installation deterministic instead
  of merely likely.

### Added

- Documentation for reaching the agent from a host behind CGNAT — an overlay
  network, a self-hosted hub on a VPS, IPv6, or asking the ISP. A
  carrier-grade-NAT connection has no inbound port, which reads as
  disqualifying, but the recommended answer never needs one: on an overlay both
  ends dial outward. The section also states which shortcut does not work —
  `tailscale serve` and `funnel` terminate TLS, and terminating TLS is the one
  thing the agent's authentication cannot survive.
  ([#31](https://github.com/scnplt/devmon-agent/pull/31),
  [#34](https://github.com/scnplt/devmon-agent/pull/34))

### Internal

- Two flaky log-rotator tests fixed. One removed its temp directory while the
  rotator goroutine was still writing under it; the other left compression on,
  so a rotated file was rewritten asynchronously after the assertion had already
  read the directory. Both failed only under CI timing, which is the kind of
  failure that teaches a team to re-run the job instead of reading it.
  ([#32](https://github.com/scnplt/devmon-agent/pull/32),
  [#33](https://github.com/scnplt/devmon-agent/pull/33))

## [0.1.2] - 2026-08-11

The agent's behaviour is unchanged from 0.1.1 — nothing in `cmd/` or
`internal/` moved. This release exists so the documentation and the published
image carry the same version, and so the new API description ships alongside
the agent it describes rather than trailing it.

### Added

- **`docs/openapi.yaml` — an OpenAPI 3.1 description of the whole `/v1`
  surface.** The client ships independently of the agent, so the contract
  between them needed to exist as something a machine can read: it generates
  client types, and it diffs between releases, which is how a breaking change
  gets noticed before a user finds it. Until now the only definition was README
  prose and the end-to-end contract tests.

  It also writes down the sharp edge nothing had documented: object references
  are validated against `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$` before reaching the
  Engine, and the pattern excludes `:`. An image ID read from a list response is
  digest-prefixed (`sha256:…`) and must have that prefix stripped before
  `GET /v1/images/{id}` will accept it; a tagged reference is a 400, not a
  lookup.

### Changed

- The client is described platform-neutrally throughout the documentation. The
  agent has never had an Android-specific line in it, and saying "the Android
  app" implied a constraint the API does not have.
  ([#24](https://github.com/scnplt/devmon-agent/pull/24))

### Fixed

- The restore command in `docs/BACKUP.md` was wrong — a `tar` invocation that
  could not have worked as written. Backup documentation is read exactly once,
  by someone whose state directory is already gone, which is the worst possible
  moment to find a typo.
  ([#23](https://github.com/scnplt/devmon-agent/pull/23))

### Internal

- **`govulncheck` runs on the release bar.** `gosec` and `govulncheck` answer
  different questions: one looks for faults in the code written here, the other
  for known vulnerabilities in the dependencies and the Go toolchain underneath
  it. Nothing in CI asked the second. Its single manual run, during the Phase 7
  security review, reported GO-2026-5856 and produced the Go 1.26.5 bump — a
  finding that arrived only because someone remembered to run it by hand.
  `make vuln` is the local counterpart.
- **Dependabot version updates** for `gomod`, `github-actions`, and `docker`.
  Alerts were already on; version updates are configuration and exist only where
  they are committed, so nothing was watching any of the three. The `docker`
  ecosystem is the one no Go gate can cover: `govulncheck` reads the module
  graph, not the base-image layers underneath it.
- Documentation records why an HTTPS-terminating proxy cannot front the agent.
  Three separate parts of the design read the TLS connection itself, so this is
  not a configuration problem with a setting that recovers it.
  ([#22](https://github.com/scnplt/devmon-agent/pull/22))

## [0.1.1] - 2026-08-11

### Fixed

- Abandoning a log stream no longer writes an `ERROR` line. Closing a log view
  is the most ordinary thing a client does, and every disconnect was logging, at
  the agent's highest severity, that it could not deliver a terminal error frame
  to a client that was no longer there. On a phone this became the dominant line
  in a size-capped, rotated log — burning the operator's log budget and burying
  the failures they would actually want to find. A genuine Engine fault with the
  client still connected is unchanged: the frame is still sent, and a failure to
  send it is still `ERROR`.
  ([#9](https://github.com/scnplt/devmon-agent/issues/9))

### Changed

- The `make e2e`, `make e2e-container` and `make e2e-endurance` targets now run
  with `-v`. `go test` prints a skip and its reason only under `-v`, so a
  package whose tests all skipped previously reported a bare `ok` —
  indistinguishable from one that ran and passed them. This suite skips for real
  reasons, and each has to be readable rather than inferred.
  (part of [#15](https://github.com/scnplt/devmon-agent/issues/15))

### Internal

- Every GitHub Actions action moved to a major that declares the Node 24
  runtime, ahead of GitHub's removal of Node 20. All seven were affected:
  `checkout` v4→v7, `setup-go` v5→v7, `golangci-lint-action` v8→v9,
  `login-action` v3→v4, `setup-qemu-action` v3→v4, `setup-buildx-action` v3→v4,
  `build-push-action` v6→v7.
  ([#16](https://github.com/scnplt/devmon-agent/issues/16))
- The end-to-end suite now passes on a native Linux Docker Engine, not only on
  the WSL2 Engine the phase checklists used. Its first CI run exposed three
  assumptions: files created inside the container under the bind mount are owned
  by UID 65532 and unreachable from the host test user, and the `health` field
  of the container list needs Engine 29+ / API v1.52, which GitHub's runners do
  not yet speak.

## [0.1.0] - 2026-08-11

First public release — the full surface.

### Added

- **Identity.** The agent is its own certificate authority. An operator mints a
  short-lived, single-use pairing code on the host; the device generates a
  keypair and exchanges a CSR for a client certificate. Certificates renew
  silently over the authenticated channel. Revocation takes effect on the next
  request, with no restart.
- **Reads.** List and inspect containers, images, networks and volumes, as
  allowlisted projections rather than passthroughs of the Engine's payload — a
  volume's driver options and a container's environment never leave the host.
- **Logs.** Bounded historical retrieval plus a live SSE stream that survives an
  abrupt connection loss and resumes without duplicating lines.
- **Lifecycle.** `start`, `restart`, `stop`, `kill`, `delete`, bounded by a
  policy mode fixed at startup (`read-only`, `default`, `full`) that a client can
  never widen. The agent permanently excludes its own container from every
  mutating route, in every mode.
- **Audit.** Every mutating attempt recorded and attributed to the calling
  device, including the ones policy refused. The audit table outlives the
  operational log.
- **Rate limiting.** Two pre-authentication tiers on the listening port, plus a
  per-device tier behind the handshake and a global backstop.
- **Installer.** `install.sh` takes a clean host to a printed pairing code:
  resolving the docker socket GID, creating and chowning the state directory,
  writing a `compose.yaml`, and waiting for the agent to answer.
- **Multi-arch image.** `linux/amd64` and `linux/arm64`.

[#120]: https://github.com/scnplt/devmon-agent/issues/120

[Unreleased]: https://github.com/scnplt/devmon-agent/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/scnplt/devmon-agent/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/scnplt/devmon-agent/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/scnplt/devmon-agent/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/scnplt/devmon-agent/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/scnplt/devmon-agent/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/scnplt/devmon-agent/releases/tag/v0.1.0
