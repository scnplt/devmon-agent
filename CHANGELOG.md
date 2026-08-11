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
- **Rate limiting.** Two tiers on the listening port.
- **Installer.** `install.sh` takes a clean host to a printed pairing code:
  resolving the docker socket GID, creating and chowning the state directory,
  writing a `compose.yaml`, and waiting for the agent to answer.
- **Multi-arch image.** `linux/amd64` and `linux/arm64`.

[0.1.2]: https://github.com/scnplt/devmon-agent/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/scnplt/devmon-agent/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/scnplt/devmon-agent/releases/tag/v0.1.0
