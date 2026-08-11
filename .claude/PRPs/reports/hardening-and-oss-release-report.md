# Implementation Report: Hardening & OSS Release (Phase 7)

**Plan**: [hardening-and-oss-release.plan.md](../plans/completed/hardening-and-oss-release.plan.md)
**Security review**: [phase-7-security-review.md](../reviews/phase-7-security-review.md)
**Branch**: `feat/hardening-and-oss-release`
**Baseline**: `14b1804` (Phase 6 merge) → `854251a`
**Date**: 2026-08-10 – 2026-08-11

---

## Summary

Phase 7 closed the last Must-have capability the PRD still listed as missing —
rate limiting on the listening port, and especially on the unauthenticated
endpoint — and discharged the release obligations around it: a written security
review against the PRD's own risk table, a one-command installer, a published
threat model and backup procedure, AGPL-3.0-only licensing across every source
file, and a tag-driven release workflow.

Two production defects were fixed. One carried over from Phase 6 (a silently
discarded `DEVMON_SELF_CONTAINER_ID`). The other was found by this phase's own
security review: `govulncheck`, run here for the first time, showed the module
pinned to a Go standard library carrying two vulnerabilities reachable from this
code — one of them on the CA key's own I/O path.

**The code is complete and every gate that can run on this machine is green. The
phase is not fully proven**: everything requiring a real Docker Engine, a clean
Linux host, or a GHCR push is deferred and listed explicitly under
[Outstanding](#outstanding). Most importantly, **`v0.1.0` has not been
published**, so the image `install.sh` pulls does not exist yet.

---

## Assessment vs Reality

| Metric | Predicted (plan) | Actual |
|---|---|---|
| Complexity | Large | Large — as scoped |
| Tasks | 12 | 12, plus 2 unplanned fix commits and 1 unplanned chore |
| Files created | 14 | 15 |
| Files updated | 12 | 116 (111 of them a one-line SPDX header) |
| New dependencies | 1 (`golang.org/x/time`) | 1 — exactly as planned; `go mod tidy` is a no-op |
| Commits | 12 (one per task) | 15 |
| Coverage | ≥80% | 84.5% |

The file count diverged only because the SPDX sweep touches every `.go` file by
design. The three extra commits are the Go toolchain security fix, the
`install.sh` input-validation fix (both under Defects), and a `.gitattributes`
chore explained under Deviations.

---

## Tasks Completed

| # | Task | Commit | Status |
|---|---|---|---|
| — | Plan and PRD status | `36ed472` | Complete |
| 1 | `internal/ratelimit` keyed limiter registry | `c938bce` | Complete |
| 2 | Three `DEVMON_RATE_*` variables | `767279f` | Complete |
| 3 | Limiter middleware and route wiring | `dc97ce0` | Complete |
| 4 | Finding 1 — warn on a discarded self-ID override | `020a936` | Complete |
| 5 | Rate-limiting contract tests | `ba406f2` | **Written, not run** — see Outstanding |
| 6 | `install.sh` | `d36a377`, `7c6916c` | Complete; **manual run deferred** |
| 7 | Security review | `854251a`, fixes in `f25b126` and `5400700` | Complete |
| 8 | Threat model and backup documentation | `f9782e4` | Complete |
| 9 | AGPL-3.0 licensing and OSS furniture | `4733db5` | Complete |
| 10 | Release workflow and shellcheck gate | `eff757b` | Complete; **never triggered** |
| 11 | README, compose example, PRD status | `f4f0aa7` | Complete |
| 12 | Verification sweep and report | this document | Partial — see Outstanding |

---

## Validation Results

| Gate | Result | Notes |
|---|---|---|
| `go build ./...` | **Pass** | |
| `go vet ./...` | **Pass** | |
| `go vet -tags e2e ./...` | **Pass** | Confirms the SPDX sweep did not render a build constraint inert |
| `golangci-lint run ./...` | **Pass** | 0 issues |
| `gosec ./...` | **Pass** | 0 issues |
| `govulncheck ./...` | **Pass** | After the Go 1.26.5 bump; 2 findings before it |
| `go test ./internal/... -race` | **Pass** | |
| `go test ./cmd/... -race` | **Pass** | |
| Coverage over `./internal/...` | **84.5%** | Floor is 80% |
| `go mod tidy` | **No-op** | `golang.org/x/time v0.15.0` is the only added requirement, direct |
| `grep -rLn "SPDX-License-Identifier" --include=*.go .` | **Empty** | All 111 files carry it |
| Internal documentation links | **All resolve** | Checked programmatically across `README.md` and `docs/` |
| Workflow and issue-template YAML | **All parse** | `ci.yml`, `release.yml`, both templates |
| `sh -n install.sh` | **Pass** | |
| `shellcheck -s sh install.sh` | **Not run** | Not installed on this machine; the CI job is in place |
| `make e2e` | **Not run** | Needs a Linux Engine via WSL2 |
| `make e2e-endurance` | **Not run** | Same |

**Environment proved against**: Go 1.26.5, Windows 11 (win32), Docker Engine
29.6.1 reachable but not used for the suite from this shell. `make` is
unavailable here, so every target was run as its underlying command.

---

## Defects Found and Fixed

### D1 — Two reachable Go standard-library vulnerabilities (HIGH)

`govulncheck`, run for the first time in this phase, reported **GO-2026-5856**
(`crypto/tls`, reached from `httpapi.Run`'s `ListenAndServeTLS` and the Engine
client's `Ping`/`Read`) and **GO-2026-4970** (`os` root escape via symlink,
reached from `certs.readFileInRoot` and `certs.writeExclusive` — the CA key's own
read/write path).

`go.mod` declared `go 1.26.4`; both are fixed in 1.26.5. Because CI resolves its
toolchain from `go-version-file: go.mod`, every gate *and the release build* were
running on the vulnerable standard library.

**Fixed in `f25b126`.** `govulncheck` reports no vulnerabilities after the bump.

This is the clearest argument for the plan's decision to add `govulncheck` here:
it found a real, reachable vulnerability that four clean `gosec` runs across four
phases could not, because it is not a property of this code but of what this code
is compiled against.

### D2 — `install.sh` values could break out of the generated compose file (MEDIUM)

Raised by the security review. `is_valid_san` mirrored the agent's own rule and
rejected `:` and `/`, but nothing rejected a double quote, a backtick, or a
newline — and `PUBLIC_ADDR`, `STATE_DIR`, and `DEVICE_NAME` are all interpolated
into a `compose.yaml` the script hands to `docker compose up -d`.

An operator at a prompt would not produce such a value; one arriving from an
environment variable in an unattended install could. A single `"` closes the YAML
scalar and lets the caller append `privileged: true`, or a bind mount of `/`, to
a file that starts a container already holding the Docker socket.

**Fixed in `5400700`** with an allowlist (`A-Za-z0-9._:/-`) applied to every value
the script writes, verified against quote, backtick, `$(…)`, and embedded-newline
payloads.

### D3 — Phase 6 Finding 1, carried over (LOW, as predicted)

An unrecognised `DEVMON_SELF_CONTAINER_ID` was discarded with no log line at all.
Security posture was sound — self-exclusion still armed via detection — but an
operator who pinned the wrong ID got no signal.

**Fixed in `020a936`.** `confirmSelf` now emits one `Warn` naming the discarded
ID, including in the not-found case that was silent. The Phase 6 e2e test that
had been loosened to assert the *absence* of that warning is tightened to assert
its presence.

---

## Deviations from Plan

| # | What | Why |
|---|---|---|
| 1 | Added `.gitattributes` (`7c6916c`), not in the plan | `install.sh` is only ever run on Linux, and this repo's Windows checkout converts to CRLF — producing `bad interpreter: /bin/sh^M`. Shipping an installer without pinning it to LF would have been a live defect. Existing files were deliberately **not** renormalized: that would rewrite every file and swamp the phase diff. |
| 2 | `withIPLimit` takes an extra `limit rate.Limit` parameter | The plan's signature gave no way to compute `Retry-After` per tier without exposing the registry's configured rate (a change to `internal/ratelimit`, outside Task 3's scope) or a tier-name lookup. The parameter is the smaller change. |
| 3 | `internal/httpapi` mirrors `internal/config`'s rate defaults as its own constants | Task 3's gotcha requires `NewServer` to floor each rate "at its package default"; `internal/config`'s defaults are unexported. The duplication is noted in a comment at both sites. |
| 4 | Tasks 7 and 8 drafted by subagents, reviewed by the main session | `CLAUDE.md` routes docs to the main session. Context exhaustion would have ended the phase mid-flight, so the research-heavy drafting was delegated and the output verified — citations spot-checked against the cited lines, every link resolved programmatically. Recorded because it departs from the documented routing. |
| 5 | Task 12's falsification steps not performed | All three require a running Engine. See Outstanding. |

---

## Outstanding

**Nothing below is optional for a real release.** Each item is deferred solely
because it cannot run from this machine.

### Requires WSL2 and a Linux Docker Engine

- [ ] `make e2e` — the whole suite, including the five new rate-limit contract
      cases. They are compile-verified (`go vet -tags e2e ./...`,
      `go build -tags e2e`) but have **never executed**. The plan flags
      `waitReady`'s status polling as a likely source of flakiness here; the
      tests loop to the first 429 rather than counting requests, but that
      mitigation is itself untested.
- [ ] `make e2e-endurance` — the limiter must not throttle a 30-minute stream.
- [ ] Falsification 1: raise `DEVMON_RATE_STATUS_PER_MIN` and confirm the
      rate-limit test goes **red** rather than passing because no 429 arrives.
- [ ] Falsification 2: remove `withDeviceLimit` from the `mutate` helper only and
      confirm the per-device case goes red.
- [ ] Sweep full `-v` e2e output for PEM blocks and pairing-code-shaped strings.

### Requires a clean Linux host

- [ ] `install.sh` end to end → a paired device with no hand-written `docker run`
- [ ] `install.sh --dry-run` prints the compose file and executes nothing
- [ ] Falsification 3: the installer refuses a non-empty existing state directory
- [ ] A wrong `DEVMON_SELF_CONTAINER_ID` produces the Warn, with self-exclusion
      still arming
- [ ] `/v1/status` throttles and recovers on a real host, honouring `Retry-After`
- [ ] Two paired devices: throttling one leaves the other served

### Requires a tag push

- [ ] **`v0.1.0` is not published.** `install.sh`, `compose.example.yaml`, and the
      README all name `ghcr.io/scnplt/devmon-agent:0.1.0`, and that image does not
      exist. Until the tag is pushed, the documented install path fails at
      `docker compose up -d`. This is the single most important remaining item.
- [ ] `docker buildx imagetools inspect` shows amd64 and arm64
- [ ] A fresh `docker compose pull` of the published tag starts and answers
      `/v1/status`

### Tooling

- [ ] `shellcheck -s sh install.sh` — not installed locally; `sh -n` and a careful
      read were the substitutes. The CI job runs it on the first PR into `main`.

---

## Acceptance Criteria

| Criterion | Status |
|---|---|
| All 12 tasks complete, one commit each | **Met** (15 commits: 12 tasks + 2 fixes + 1 chore) |
| Rate limiting enforced in the specified tiers and order | **Met** |
| Three `DEVMON_RATE_*` variables validated, aggregating with other faults | **Met** |
| `golang.org/x/time v0.15.0` the only added dependency; tidy a no-op | **Met** |
| Phase 6 Finding 1 closed and its e2e test tightened | **Met** (run deferred) |
| Security review written, every risk row given a verdict, no open high finding | **Met** |
| `install.sh` takes a clean host to a paired device; shellcheck clean | **Unproven** — never run on a host; shellcheck deferred to CI |
| `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, threat model, backup guide exist | **Met** |
| Every `.go` file carries the SPDX identifier | **Met** — 111/111 |
| `v0.1.0` published to GHCR for amd64 and arm64 | **Not met** — workflow in place, tag never pushed |
| All 76 pre-existing e2e tests green, plus the new cases | **Unproven** — suite not run |
| Coverage ≥80% | **Met** — 84.5% |
| README claims no phase state that is not true | **Met** |
| PRD Phase 7 row → complete | **Met** |

---

## Next Steps

1. From WSL2: `make e2e`, then `make e2e-endurance`, then the three falsification
   steps. Fix anything the rate-limit contract tests surface — they are the
   least-proven code in this phase.
2. On a throwaway VM: `install.sh --dry-run`, then a real install, then confirm it
   refuses a second run against the same state directory.
3. Open the PR into `dev`; the release bar (`lint`, `image`, `gosec`,
   `shellcheck`, `e2e`) runs on the PR into `main`.
4. After that merges, tag `v0.1.0` and confirm the published manifest carries both
   architectures.
5. Only then is the README's install path true.
