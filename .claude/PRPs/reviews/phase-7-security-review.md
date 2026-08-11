# Phase 7 Security Review

**Scope**: the full agent as of `feat/hardening-and-oss-release`, reviewed
against the PRD's Technical Risks table. Written to discharge D10 of
`hardening-and-oss-release.plan.md` and the PRD's Phase 7 success signal,
"security review passes with no unmitigated high-severity finding".

**Date**: 2026-08-11
**Reviewed at**: commits `36ed472..5400700` on `feat/hardening-and-oss-release`
**Baseline**: `14b1804` (Phase 6 merge)

**Verdict: PASS.** No unmitigated high-severity finding remains. Two findings
were raised and fixed inside this phase (§3). Three risks are **accepted** with
written reasoning (§2, rows 1, 7, and 14's residual).

---

## 1. Tooling

| Tool | Version | Result |
|---|---|---|
| `gosec` | v2.28.0 | **0 issues** (46 files, 6611 lines) |
| `golangci-lint` | v2.12.2 | **0 issues** |
| `govulncheck` | latest, run 2026-08-11 | **2 findings → fixed → clean** (§3.1) |
| `go test ./internal/... -race` | Go 1.26.5 | pass, coverage **84.5%** (floor 80%) |
| `go test ./cmd/... -race` | Go 1.26.5 | pass |
| `go vet ./...`, `go vet -tags e2e ./...` | Go 1.26.5 | clean |
| `go mod tidy` | — | no-op |

### `#nosec` inventory

Eight occurrences, none added or modified by this phase
(`git diff 14b1804..HEAD -- '*.go' | grep nosec` is empty). Each justification
was re-checked against what the code does today:

| Location | Rule | Justification still holds? |
|---|---|---|
| `internal/selfid/selfid.go:85` | G304 | **Yes.** `root` is `"/"` in production and a temp dir in tests; never request-derived. |
| `internal/e2e/harness/agent.go:67,174,420` | G204, G304 | **Yes.** Test-only; arguments fixed by the function, or a path this package itself built. |
| `internal/e2e/harness/agent.go:336`, `client.go:258`, `image.go:325` | G402 | **Yes.** `InsecureSkipVerify` on the bootstrap readiness/pairing probe only, before any CA is pinned (Phase 6 D7). |
| `internal/e2e/harness/cli.go:165`, `image.go:122,180` | G204, G302 | **Yes.** Test-only, fixed argument prefixes, throwaway temp directories. |

### Key-material sweep

`grep -rn "PEM\|\.key\|pairing_code\|PairingCode" --include=*.go internal/ cmd/`
was reviewed line by line. Every hit is in `internal/certs`, `internal/state`, or
`internal/httpapi/{pair,device}.go` — the Phase 2/3 surface that legitimately
handles this material. **No log line, error string, or response field in the tree
can carry key material, a pairing code, or PEM bytes.** Phase 7 added no new
handling of any of them. The one new log line this phase adds
(`internal/dockerx/self.go:76`) carries a container ID, which
`internal/config/config.go` has already validated as 12 or 64 lowercase hex
characters.

---

## 2. The PRD risk table, row by row

Each row states the mitigation **as implemented**, with a citation, the residual
risk, and a verdict of `mitigated`, `accepted`, or `open`.

### Row 1 — Agent compromise equals host root compromise

**Mitigation as implemented.** The API is a narrow enumerated surface, not a
socket proxy: every route is registered explicitly in
`internal/httpapi/server.go:150-208`, with no passthrough. mTLS with per-device
credentials is enforced by `internal/tlsconf/tlsconf.go:18-30` plus
`internal/httpapi/middleware.go:31-87`. Every mutating attempt is audited
(`internal/httpapi/audit.go:29-70`). This review is itself the release gate the
row names.

**Residual.** Unchanged and irreducible: a flaw that gets past mTLS and the
policy gate yields Docker access, and Docker access is root. The mitigations
narrow the path; they do not change the prize.

**Verdict: `accepted`.** Documented in `docs/THREAT-MODEL.md` and stated plainly
in `SECURITY.md`. This is the project's defining trade-off.

### Row 2 — Pairing flow is the weakest link

**Mitigation.** Codes are single-use and short-lived (`internal/state/pairing.go`,
schema at `internal/state/schema.go:91-101`), and minting one requires host
console access — `device pair-code` is a host-side subcommand
(`cmd/devmon-agent/cli.go:186-200`) reachable only through `docker exec`, never
over the network. `install.sh` prints the first code to the terminal and nowhere
else. Phase 7 adds a per-IP limiter of 5/minute on `POST /v1/pair`
(`internal/httpapi/ratelimit.go`, wired at `internal/httpapi/server.go:159-160`),
which is what makes grinding a code infeasible rather than merely slow.

**Residual.** An operator who transmits a code over an insecure channel defeats
all of it.

**Verdict: `mitigated`.**

### Row 3 — Lost or stolen paired device

**Mitigation.** `device revoke` (`internal/state/devices.go:172`) takes effect on
the next request, because `requireDevice` checks device state per request rather
than caching it (`internal/httpapi/middleware.go:31-87`). Device certificates are
short-lived relative to the CA.

**Residual.** A thief with an unlocked, paired phone has whatever the policy mode
grants until the operator reaches the host. Named explicitly as a considered
adversary in `docs/THREAT-MODEL.md`.

**Verdict: `mitigated`**, with the window documented.

### Row 4 — Host-side changes and the running agent disagree

**Mitigation.** One SQLite store is the only source of truth
(`internal/state/store.go`), shared by the CLI and the server; there is no
in-memory device cache to go stale. Phase 6's contract suite asserts revocation
takes effect without a restart.

**Verdict: `mitigated`.**

### Row 5 — Live log streams break on handover or drain battery

**Mitigation.** Resumable streams and client-side backoff, with endurance testing
behind `make e2e-endurance`. Phase 7's relevant addition: the guarded limiter
keys on **device ID, not IP** (`internal/httpapi/ratelimit.go:144-177`), so a
mobile network handover does not throttle the reconnect — keying by address would
have made this row *worse*. A stream spends exactly one token at request time, so
a 30-minute stream is never throttled mid-flight.

**Residual.** Battery behaviour is a client-side property this agent cannot
control.

**Verdict: `mitigated`** on the agent side.

### Row 6 — Operator destroys state accidentally

**Mitigation.** A bind mount, not an anonymous volume, in both
`compose.example.yaml` and what `install.sh` generates — `docker compose down -v`
cannot destroy it. `internal/certs/store.go:234-271`
(`CheckIdentityConsistency`) refuses to start on a half-present identity rather
than silently minting a second one. Phase 7 adds `docs/BACKUP.md` and an
installer that **refuses a non-empty existing state directory** outright.

**Verdict: `mitigated`.**

### Row 7 — CA private key readable in host backups and VPS snapshots

**Mitigation as implemented.** `ca.key` is written 0600
(`internal/certs/ca.go:38-43`) inside a state directory `install.sh` creates 700
and owned by UID 65532. It is never logged and never leaves the host.

**Residual.** The key is **unencrypted at rest**. Anyone who reads a backup or a
VPS snapshot holds the agent's identity and can mint a device certificate.

**Verdict: `accepted`**, exactly as the PRD row states. The reasoning is now
written down in `docs/THREAT-MODEL.md` and `docs/BACKUP.md`: any unlocking secret
would have to live on the same host for the agent to start unattended, which
moves the problem rather than solving it. This phase documents the risk; per the
plan's NOT Building list, it does not revisit encryption-at-rest.

### Row 8 — CA expiry breaks every deployment on the same date

**Mitigation.** A long-lived CA with shorter-lived device certificates, and a
renewal path built in Phase 2 rather than deferred (`POST /v1/device/renew`,
`internal/httpapi/server.go:169`).

**Verdict: `mitigated`.**

### Row 9 — Persistent logs fill the host disk

**Mitigation.** Age- and size-based rotation from the first release
(`internal/logging/rotator.go`), with `DEVMON_LOG_MAX_AGE_DAYS` and
`DEVMON_LOG_MAX_TOTAL_MB` defaulting to 1 day / 64 MB
(`internal/config/config.go:51-52`). `minLogMaxTotalMB = 8`
(`internal/config/config.go:66-70`) exists because a smaller budget divides to 0
and lumberjack reads 0 as unlimited — the cap would silently disappear.

**Phase 7 note.** The rate limiter logs one `Warn` per rejection. That is bounded
by the limiter itself and lands in the size-capped operational log, not the audit
table (D7). Critically, the pre-auth tiers log **the tier name only, never the
IP** (`internal/httpapi/ratelimit.go:136`) — echoing attacker-supplied addresses
into a size-bounded log would let a scanner evict the operator's own diagnostics.

**Verdict: `mitigated`.**

### Row 10 — Host IP or hostname changes

**Mitigation.** The agent re-issues its own server certificate from the retained
CA when `DEVMON_PUBLIC_ADDR` changes, with no re-pair
(`internal/certs/store.go`). The CA fingerprint the client pins is unchanged, so
clients see nothing.

**Verdict: `mitigated`.**

### Row 11 — The unauthenticated endpoint becomes an attack or fingerprinting surface

This is the row Phase 7 exists to close: the PRD's stated mitigation names
"aggressive rate limiting", which did not exist before this phase.

**Mitigation as implemented.**

- **Strict field allowlist**: `internal/httpapi/status.go:17-31` returns
  `api_version`, `agent_version`, `policy_mode`, `server_time`, and
  `ca_fingerprint`, and nothing else — no host or container data. Phase 6's
  contract suite asserts the exact key set, so a field cannot be added by
  accident.
- **Rate limiting, new in this phase**: 30/minute per client IP on
  `GET /v1/status`, 5/minute on `POST /v1/pair`, both behind a shared global
  backstop of 50/s burst 100, because per-IP alone does not stop a distributed
  scan (`internal/httpapi/ratelimit.go:33-34`).
- **Cannot issue or renew**: `/v1/status` and `/v1/pair` are the only routes
  outside `requireDevice` (`internal/httpapi/server.go:150-208`), and renewal
  sits behind it.
- **The limits are advertised nowhere**, including in `/v1/status` — publishing
  the exact ceiling would tell a scanner how fast it may go unnoticed.

**Residual.** The endpoint remains discoverable by internet-wide scanning; that
is inherent to listening on a port, and rate limiting bounds the cost rather than
removing the fact. A VPN in front of the port is the documented hardening step.

**Verdict: `mitigated`.**

### Row 12 — A device offline past expiry cannot renew

**Mitigation.** The renewal window is a fraction of certificate lifetime, so
ordinary use always renews in time, and `/v1/status` explains the situation
rather than showing a bare failure.

**Verdict: `mitigated`.**

### Row 13 — Future email notifications leak information

**Not built.** Out of scope per the PRD's Won't list and this plan's NOT Building
list.

**Verdict: `mitigated` by non-existence.** Carries forward to whatever phase
introduces notifications, which the PRD already requires to have its own review.

### Row 14 — Internet-exposed port attracts automated scanning and abuse

**Mitigation as implemented.**

- **Rate limiting** in two tiers, new in this phase (see Row 11).
- **Non-authenticated connections are rejected cheaply**: `requireDevice` rejects
  before any handler, Engine call, or database read
  (`internal/httpapi/middleware.go:31-87`), and on the pre-auth routes the
  limiter rejects before even that.
- **VPN documented** as the recommended hardening step, in the README and
  `docs/THREAT-MODEL.md`.
- Connection establishment is bounded by the OS accept queue and
  `ReadHeaderTimeout`; body size by per-route caps; open streams by
  `maxConcurrentStreams = 8`.

**Residual.** Denial of service beyond what the limiter bounds is **not
defended** — a connection limiter belongs in front of the process, in `iptables`
or a VPN. Stated as a non-defence in `docs/THREAT-MODEL.md` rather than left as
an implied guarantee.

**Verdict: `mitigated`**, with the DoS residual **accepted** and documented.

### Row 15 — Android app and agent version drift

**Mitigation.** A versioned API contract from the first release: `api_version` in
`/v1/status` and a `/v1/` prefix on every route.

**Verdict: `mitigated`.**

---

## 3. Findings raised and their disposition

Both findings below were raised during this review and **fixed inside this
phase**, per the plan's rule that a CRITICAL or HIGH finding does not become a
Phase 8.

### 3.1 HIGH — two reachable Go standard-library vulnerabilities

`govulncheck`, run for the first time in this phase, reported:

| ID | Package | Reached from |
|---|---|---|
| GO-2026-5856 | `crypto/tls` | `httpapi.Run` → `ListenAndServeTLS`; the Engine client's `Ping` and `Read` |
| GO-2026-4970 | `os` (root escape via symlink plus trailing slash) | `certs.readFileInRoot`, `certs.writeExclusive` — the CA key's own read/write path |

Both are fixed in Go 1.26.5; `go.mod` declared 1.26.4. Because CI resolves its
toolchain with `go-version-file: go.mod`, every gate and the release build were
running on the vulnerable standard library.

**Fixed** in `f25b126`: the `go` directive is now 1.26.5, and `govulncheck`
reports **no vulnerabilities** after the bump. Severity is HIGH rather than
CRITICAL because exploiting GO-2026-4970 requires an attacker who can already
place a symlink inside the state directory — which implies host access — but the
`crypto/tls` path is reachable from the open port, and the affected `os` code is
the CA key's own I/O.

### 3.2 MEDIUM — values could break out of the generated compose file

`install.sh`'s `is_valid_san` mirrored the agent's own rule and rejected `:` and
`/`, but nothing rejected a double quote, a backtick, or a newline.
`PUBLIC_ADDR`, `STATE_DIR`, and `DEVICE_NAME` are all interpolated into a
`compose.yaml` that the script then hands to `docker compose up -d`.

An operator typing at a prompt would not produce such a value. One arriving from
an environment variable during an unattended install could: a single `"` closes
the YAML scalar and lets the caller append compose keys of their own —
`privileged: true`, or a bind mount of `/` — to a file that starts a container
already holding the Docker socket. That is a host compromise, not a malformed
config file.

**Fixed** in `5400700`: `has_unsafe_chars` is an **allowlist**
(`A-Za-z0-9._:/-`) applied to every value the script writes. Verified against
quote, backtick, `$(…)`, and embedded-newline payloads — all rejected — with
legitimate hostnames, IPv4 addresses, and absolute paths still accepted.

### 3.3 LOW — accepted without change

| Finding | Disposition |
|---|---|
| Release workflow actions pinned to major-version tags, not commit SHAs, in a job holding `packages: write` | **Accepted.** All are first-party (`actions/`) or Docker-maintained. SHA pinning is worthwhile and belongs with the signing/SBOM/provenance work this plan's NOT Building list defers to its own phase. |
| A request that hits the D9 registry-full fallback spends two `unauthGlobal` tokens, not one | **Accepted.** Fails toward *stricter* limiting, never weaker. Recorded here so a future reader does not mistake it for a bug. |
| ≥4096 distinct source IPs can saturate the pre-auth registries, forcing new callers onto the shared global bucket | **Accepted.** Inherent to D9's deliberate trade-off, and strictly better than the alternatives: refusing every new key would let an attacker lock the operator out, and ignoring the limiter when full is a trivial bypass. Falling back to the global bucket still admits a legitimate caller whenever that bucket has tokens. |

---

## 4. New attack surface introduced by this phase

The plan requires an explicit verdict on this.

### 4.1 The limiter's own memory — computed, not estimated

`rate.Limiter` (`golang.org/x/time/rate` v0.15.0) is `sync.Mutex` (8) + `Limit`
(8) + `burst int` (8) + `tokens float64` (8) + two `time.Time` (24 each) =
**80 bytes**. Map overhead is ~30–40 bytes per entry at Go's load factor; the key
is at most ~45 bytes (an IPv6 host string) or 16 bytes (a hex device ID). Worst
case ≈ **165 bytes per entry**.

`rateLimitMaxKeys = 4096` × 3 registries (status, pair, device) = 12,288 entries
× 165 B ≈ **2 MiB**, or ≈ 5 MiB doubling for allocator slack.

**Verdict: the bound holds.** 5 MiB is negligible on any host running a Docker
daemon, and it is a hard ceiling — `Registry.Allow` refuses to grow past
`maxKeys` rather than evicting blindly (`internal/ratelimit/registry.go:46-58`).

### 4.2 Does a 429 leak anything a 200 does not?

**No.** `msgRateLimited` is one message for every tier
(`internal/httpapi/ratelimit.go:22`), served through the same `writeError` that
governs every other rejection (`internal/httpapi/respond.go`). It never reveals
which check failed — per-key, global, or registry-full — nor a device ID, an IP,
or anything on `writeError`'s protected list. The only per-response variation is
the integer `Retry-After`, which differs by tier; the tier is already implied by
the path the caller chose, so it discloses nothing the caller did not know.

### 4.3 Can the eviction sweep be gamed?

**No.** `sweepIdleLocked` (`internal/ratelimit/registry.go:66-72`) runs only when
the incoming key is *absent* and the map is full. An attacker's own throttled
bucket is found on the fast path and never reaches the sweep, so it cannot be
reset. The sweep deletes only buckets refilled to full burst — already
unthrottled — and a re-created bucket for such a key starts full, which is the
state it was evicted in. Evicting the *oldest* key instead would have handed an
attacker a free reset; that inversion was avoided deliberately.

### 4.4 Is the pre-auth key client-controlled?

**No.** `clientIP` reads only `r.RemoteAddr`, which `net/http` sets from the
accepted TCP connection (`internal/httpapi/ratelimit.go:73-79`). It is the sole
caller-facing key source, and `ratelimit_test.go` asserts an
`X-Forwarded-For: 9.9.9.9` header changes nothing.

### 4.5 Is `withDeviceLimit` reachable without a device?

**No.** Every registration wraps it inside `requireDevice`
(`internal/httpapi/server.go:169,170,176,190,201`). The 500 branch is an
unreachable defensive check, kept because a limiter that degrades to no limiter
the moment it is mis-composed is worse than one that is absent.

---

## 5. Rule checklists

### `.claude/rules/ecc/common/security.md`

| Item | Status |
|---|---|
| No hardcoded secrets | **Pass** — `gosec` clean; sweep in §1 |
| All user inputs validated | **Pass** — startup config in `internal/config`; request bodies capped and parsed per route; `install.sh` validates at prompt time |
| SQL injection prevention | **Pass** — parameterized queries throughout `internal/state` |
| XSS / CSRF | **N/A** — a JSON API with no browser session and no cookies |
| AuthN / AuthZ verified | **Pass** — mTLS plus `requireDevice`, then `requireOp` against the startup policy mode |
| Rate limiting on all endpoints | **Pass** — closed by this phase; every route is behind a tier |
| Errors do not leak sensitive data | **Pass** — `writeError`'s terse-message convention, verified in §4.2 |

### Code-review OWASP-style items

Injection, broken authentication, sensitive data exposure, broken access
control, security misconfiguration, and vulnerable components were each walked.
The two with live findings — vulnerable components (§3.1) and injection (§3.2,
into a generated config rather than a query) — are fixed. Path traversal is
bounded by `os.Root` in `internal/certs`, and the container ID reaching the
Engine is regex-validated at config load.

---

## 6. Conclusion

**No unmitigated high-severity finding remains.** The PRD's Phase 7 success
signal is met.

Three risks are carried as **accepted**, each with written reasoning here and in
`docs/THREAT-MODEL.md`: the unencrypted CA key at rest (PRD row 7), the
root-equivalent blast radius of an agent compromise (row 1), and denial of
service beyond what the rate limiter bounds (row 14).

Two items are recorded as **worth their own phase** rather than as open
findings: SHA-pinning the release workflow's actions, and supply-chain
attestation (signing, SBOM, provenance) — both already on this plan's NOT
Building list.
