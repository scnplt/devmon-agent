# Implementation Report: Secure Foundation & Persistence (Phase 1)

## Summary

The DevMon agent now runs as a container. It parses and validates its entire
startup configuration with aggregated errors, opens a versioned SQLite state
store on a host bind mount, generates and persists a self-signed EC server
certificate, connects to the Docker Engine v29 SDK, terminates TLS 1.3 on one
port, serves `GET /v1/status` with exactly four fields and no client certificate,
writes rotating persistent logs bounded by both age and size, and shuts down
gracefully on SIGTERM.

All three PRD Phase 1 success signals were verified against a running container,
not only in unit tests.

## Assessment vs Reality

| Metric | Predicted (Plan) | Actual |
|---|---|---|
| Complexity | Large (~22 files, ~1400 lines incl. tests) | Large — 24 files, ~1800 lines incl. tests |
| Confidence | High (plan carried verified upstream signatures) | Confirmed — the v29 SDK and lumberjack gotchas were exactly as documented |
| Files Changed | 22 created, 1 updated | 24 created, 3 updated |

## Tasks Completed

| # | Task | Status | Notes |
|---|---|---|---|
| 1 | Module scaffold and build tooling | Complete | Pre-existing from an earlier session; Makefile amended (see Deviations) |
| 2 | Policy modes | Complete | Pre-existing from an earlier session; verified against the plan |
| 3 | Configuration parsing and validation | Complete | |
| 4 | Persistent logging with bounded growth | Complete | |
| 5 | SQLite state store | Complete | |
| 6 | Server certificate — generate, persist, reload | Complete | Deviated — writes scoped under `os.Root` (see Deviations) |
| 7 | TLS configuration builder | Complete | |
| 8 | Docker Engine connectivity | Complete | |
| 9 | HTTP server, middleware, status endpoint | Complete | Deviated — `shutdownGrace` lowered (see Deviations) |
| 10 | Wire it together in `main` | Complete | Deviated — `runAll` cancels on first return (see Deviations) |
| 11 | Container image and compose example | Complete | Image builds; `-version` smoke test passes |
| 12 | Test sweep, lint, security scan | Complete | |

## Validation Results

| Level | Status | Notes |
|---|---|---|
| Static analysis | Pass | `gofmt -l .`, `goimports -l .`, `go vet ./...` all silent |
| Lint | Pass | `golangci-lint run ./...` — 0 issues |
| Security scan | Pass | `gosec ./...` — 0 issues (one G304 found and genuinely fixed, not suppressed) |
| Unit tests | Pass | 8 packages, 84.8% coverage over `./internal/...` (floor 80%) |
| Build | Pass | `go build ./...`; `CGO_ENABLED=0` static binary; `docker build` |
| Integration / E2E | Pass | Full container run against a live Docker socket — see below |
| Edge cases | Pass | Every item on the plan's checklist exercised |

### Coverage by package

| Package | Coverage |
|---|---|
| `policy` | 100.0% |
| `tlsconf` | 100.0% |
| `httpapi` | 93.8% |
| `config` | 90.9% |
| `logging` | 84.6% |
| `certs` | 79.5% |
| `state` | 79.0% |
| `dockerx` | 57.1% — the success path needs a live daemon, which the plan puts in the manual checklist |
| **total** | **84.8%** |

### End-to-end results (live container, real Docker socket)

| Check | Result |
|---|---|
| Startup log sequence | Matches the plan's UX diagram line for line |
| `GET /v1/status` | `{"api_version":"v1","agent_version":"dev","policy_mode":"read-only","server_time":"…Z"}` — exactly 4 fields |
| TLS | `Protocol: TLSv1.3`; verify code 18 (self-signed, expected until Phase 2) |
| `POST /v1/status` | 405, not 200 — the method-aware pattern works |
| `GET /v1/containers` | 404 with no path, hostname, or version detail |
| **Crash survival** (success signal) | `docker kill` → restart: pre-kill lines still present, new lines appended below |
| **Identity stability** | Certificate serial `96F7B39…` identical across restart; `first_run=false`; schema still v1 |
| Bad configuration | All 3 problems listed at once, exit code 2, before any state was touched |
| Docker unreachable | Specific fatal `ping docker engine at unix:///var/run/docker.sock: …`, no panic |
| Graceful shutdown | `docker stop` returned in 0s (not the 10s SIGKILL timeout), exit code 0, `"shutting down http server"` logged |
| Database | `integrity_check=ok`, `journal_mode=wal`, `version=1`, tables `audit devices schema_meta` |
| File modes (Linux) | `devmon.db` 600, `certs/` 700, `server.key` 600, `server.crt` 644, `logs/` 700, `agent.log` 600 |

## Files Changed

| File | Action | Lines |
|---|---|---|
| `internal/config/config.go` | CREATED | +336 |
| `internal/config/config_test.go` | CREATED | +330 |
| `internal/logging/logging.go` | CREATED | +98 |
| `internal/logging/rotator.go` | CREATED | +63 |
| `internal/logging/logging_test.go` | CREATED | +237 |
| `internal/state/store.go` | CREATED | +220 |
| `internal/state/schema.go` | CREATED | +48 |
| `internal/state/pruner.go` | CREATED | +67 |
| `internal/state/store_test.go` | CREATED | +346 |
| `internal/certs/selfsigned.go` | CREATED | +111 |
| `internal/certs/store.go` | CREATED | +175 |
| `internal/certs/certs_test.go` | CREATED | +246 |
| `internal/tlsconf/tlsconf.go` | CREATED | +49 |
| `internal/tlsconf/tlsconf_test.go` | CREATED | +99 |
| `internal/dockerx/client.go` | CREATED | +72 |
| `internal/dockerx/client_test.go` | CREATED | +59 |
| `internal/httpapi/server.go` | CREATED | +114 |
| `internal/httpapi/status.go` | CREATED | +42 |
| `internal/httpapi/middleware.go` | CREATED | +96 |
| `internal/httpapi/respond.go` | CREATED | +41 |
| `internal/httpapi/status_test.go` | CREATED | +261 |
| `internal/httpapi/server_test.go` | CREATED | +80 |
| `cmd/devmon-agent/main.go` | CREATED | +185 |
| `Dockerfile` | CREATED | +38 |
| `compose.example.yaml` | CREATED | +59 |
| `README.md` | CREATED | +212 |
| `Makefile` | UPDATED | +5 / -2 |
| `go.mod`, `go.sum` | UPDATED | dependencies promoted from indirect |
| `.claude/PRPs/prds/devmon-agent.prd.md` | UPDATED | Phase 1 → complete |

`internal/version/version.go`, `internal/policy/mode.go`, `internal/policy/mode_test.go`,
`go.mod`, `Makefile`, and `.gitignore` already existed from an earlier session
(Tasks 1–2); each was read and verified against the plan before continuing.

## Deviations from Plan

1. **`Makefile`: `test-race` and `cover` now set `CGO_ENABLED=1`.**
   *Why:* the file exports `CGO_ENABLED=0` globally, which is correct for the
   shipped binary but makes `-race` fail outright — the race detector is
   implemented in cgo. As written, `make test-race` could never have run. The
   shipped binary is still built with cgo disabled.

2. **`internal/certs`: file writes are scoped under `os.Root` rather than plain
   `os.OpenFile`.**
   *Why:* `gosec` flagged G304 on the original form. Rather than suppress it with
   `#nosec`, the write path now opens a root on the certs directory, which
   genuinely prevents a path escaping it — including via a symlink planted in the
   bind mount, which is reachable by anyone with host access to the state
   directory. `gosec` is clean with no suppressions anywhere in the repo.

3. **`internal/httpapi`: `shutdownGrace` is 5s, not the planned 15s.**
   *Why:* Docker's default stop timeout is 10s. A 15s drain would guarantee that
   every `docker stop` hit SIGKILL — corrupting the WAL, which is exactly the
   outcome the plan's SIGTERM handling exists to prevent. Verified: `docker stop`
   now returns in under a second with exit code 0.

4. **`cmd/devmon-agent`: `runAll` cancels a derived context as soon as any
   component returns.**
   *Why:* the plan's "wait for all three" shape deadlocks when one component
   fails early — a listener that cannot bind would leave the rotator and pruner
   ticking forever while `wg.Wait()` never returned, so the agent would hang
   instead of reporting the error it already had. Found during Task 10 review,
   before it could reach the E2E run.

5. **Two tests skip on Windows** (`state.TestOpenDBFileIsOwnerOnly`,
   `certs.TestServerKeyIsOwnerOnly`).
   *Why:* Windows has no POSIX permission bits — `os.Chmod` there only toggles
   the read-only flag, so the mode always reads back `0666`. The guarantee is
   real and was verified on the target platform instead: the E2E run confirms
   `devmon.db` 600 and `server.key` 600 inside the Linux container.

6. **`config.isAbsPath` accepts a leading `/` even on Windows.**
   *Why:* the agent's paths are container paths. `filepath.IsAbs("/var/lib/devmon")`
   is false on Windows, so the default configuration would have failed validation
   on any developer machine while being correct in the image.

## Issues Encountered

- **`-race` was initially unavailable on this machine** (no C toolchain on PATH;
  `gcc` not found), so the first pass of tests ran without the detector.
  **Resolved:** MinGW-w64 (WinLibs, GCC 16.1.0, `x86_64-w64-mingw32`, UCRT) was
  installed and the full suite re-run with `CGO_ENABLED=1 go test -race`. All
  packages pass and **no data races were reported**, including
  `state.TestConcurrentStores` — two `Store` handles writing simultaneously — which
  was the test most in need of the detector. Coverage under `-race` is 84.5%,
  identical to the non-race run. `./cmd/...` passes under `-race` as well.

  Note for anyone reproducing this on Windows: the classic mingw.org "MinGW
  Installation Manager" toolchain does **not** work. It targets `i686` only, cgo
  needs a 64-bit compiler for `windows/amd64`, and dropping to `GOARCH=386` is not
  an escape hatch because the race detector does not support `windows/386` at all.
  A MinGW-w64 distribution (WinLibs, MSYS2 `mingw-w64-ucrt-x86_64-gcc`, or
  w64devkit) is required.
- **`gosec` G304** in `internal/certs`. Fixed genuinely rather than suppressed;
  see Deviation 2.
- **Git Bash mangles container paths** on Windows (`/var/run/docker.sock` became
  `C:\Program Files\Git\var\...`). Worked around with `MSYS_NO_PATHCONV=1` for
  the E2E run. This affects only local manual testing, not the agent.
- **The state directory itself is `0755` when Docker creates the volume**, not
  the documented `0700`. `MkdirAll` does not re-chmod an existing directory, and
  the agent deliberately does not restyle a mount point the operator owns. The
  README now instructs `chmod 700` alongside the existing `chown` prerequisite.
  Every file the agent creates inside it is correctly `0600`.

## Tests Written

| Test File | Tests | Coverage |
|---|---|---|
| `internal/config/config_test.go` | 7 funcs / 36 cases | Defaults, every field override, 19 rejections, 3-problem aggregation, zero-Config-on-failure, derived paths |
| `internal/logging/logging_test.go` | 6 funcs / 10 cases | Crash survival across reopen, per-file budget arithmetic, lumberjack settings, rotation, ticker, context cancel, uncreatable dir |
| `internal/state/store_test.go` | 11 funcs | First run, reopen, corruption, schema-too-new, non-numeric version, file mode, prune by age+count, empty-table prune, two-handle concurrency, pruner lifecycle |
| `internal/certs/certs_test.go` | 7 funcs / 10 cases | Generation shape and validity window, PKCS#8 form, empty SANs, idempotent serial, SAN drift, key mode, half keypair, corrupt key, SAN splitting |
| `internal/tlsconf/tlsconf_test.go` | 2 funcs / 3 cases | ClientAuth mode selection (G3), TLS 1.3 floor, no CipherSuites, ALPN |
| `internal/dockerx/client_test.go` | 2 funcs / 3 cases | Unreachable socket and port, malformed host, error names the host |
| `internal/httpapi/status_test.go` | 8 funcs / 15 cases | **Exactly 4 keys**, policy advertisement, headers, UTC RFC3339 time, 405 on other methods, no leakage on 404, `requireDevice` 401, panic containment, timeouts |
| `internal/httpapi/server_test.go` | 2 funcs | Graceful shutdown returns nil, listen failure is reported |

The `handleStatus` exact-key-count assertion is the guard the plan asks for:
Phase 2 must consciously change 4 to 5 when `ca_fingerprint` lands.

## Nothing from § NOT Building was implemented

Verified: `dockerx` exposes only `New`/`Close`; there is no CA, pairing, device
write, or revocation; no `ca_fingerprint` field; no log retrieval or WebSocket
dependency; no audit writing (table and pruner only); no rate limiting; no
installer; no certificate re-issuance on drift (WARN only); no metrics or
profiling endpoints.

All four Phase 2 seams are in place: `tlsconf.Build` takes `clientCAs` (Phase 1
passes `nil`), `requireDevice` exists and rejects everything, `state.Store.FirstRun`
is exposed, and the `devices` table ships at schema v1 so Phase 2 adds no
migration.

## Post-Implementation Review

`/ecc:go-review` ran the go-reviewer and security-reviewer agents plus
`staticcheck` and `govulncheck`. No CRITICAL issues. Verdict: no security
finding above MEDIUM; the design boundary ("a client can never widen what the
operator granted") was walked end to end and confirmed intact.

Fixed in response:

| Severity | Finding | Fix |
|---|---|---|
| HIGH | `writeJSON` logged encode failures via `slog.Default()`, bypassing the file-backed sink so the line never reached `agent.log` — flagged independently by both reviewers | `writeJSON`/`writeError` are now methods on `*Server` and use the injected logger |
| HIGH | `runAll` — the most concurrency-sensitive function in the repo — had no test at all | Added `cmd/devmon-agent/main_test.go`: 7 funcs / 11 cases covering clean shutdown, failure in each position, sibling cancellation, real-error-vs-cancellation precedence, and the degenerate empty case; every case asserts `runAll` *returns* within 5s |
| MEDIUM | `os.Chmod` tightened only `devmon.db`, not the `-wal`/`-shm` sidecars, which from Phase 2 can hold device records not yet checkpointed into the main file | `tightenPermissions` now covers all three, tolerating `fs.ErrNotExist` |
| MEDIUM | A failed cleanup of the orphaned key after a failed certificate write discarded its error | Now logged as a `WARN`, preserving the original error as the return value |
| LOW | No `X-Content-Type-Options` on the one endpoint every scanner can reach | `nosniff` added in `writeJSON` |

Reviewed and deliberately not changed:

- **`version.Version`/`Commit`/`BuildTime` as package-level vars** (MEDIUM,
  go-reviewer). This is the standard `-ldflags -X` idiom; the linker writes them
  before `main` runs and nothing mutates them after. Restructuring to satisfy a
  literal reading of the no-globals rule would obscure a well-understood pattern.
- **`prepareStateDir` and `logging.NewSink` both creating `logs/`** (MEDIUM,
  go-reviewer). `NewSink` must work standalone — the logging tests rely on it —
  and `MkdirAll` is idempotent.
- **`VerifyClientCertIfGiven` letting unauthenticated TCP clients reach the HTTP
  layer** (LOW, security-reviewer). Accepted and already documented: it is
  forced by the single-port requirement, and `ReadHeaderTimeout`,
  `MaxHeaderBytes`, and `withRecovery` cover the realistic failure modes. Worth
  re-confirming when Phase 6 rate limiting lands.
- **One finding was incorrect.** go-reviewer reported (LOW) that
  `dockerx/client_test.go` calls `Close()` on a nil `*Client` in the error path.
  It does not — the call sits inside the `err == nil` branch, where `c` is valid,
  and is correct cleanup for the unexpected-success case. Verified before
  dismissing.

Post-fix gates: `gofmt`, `goimports`, `go vet`, `staticcheck`, `golangci-lint`,
`gosec` all clean; coverage 84.5% over `./internal/...`; image rebuilt and the
full E2E sequence re-run green (status payload, `nosniff` header present, clean
shutdown exit 0, `devmon.db` at 0600).

### Dependency advisory

`govulncheck` reports two Go **1.26.4 standard library** vulnerabilities, both
fixed in 1.26.5:

- **GO-2026-5856** — Encrypted Client Hello privacy leak in `crypto/tls`,
  reachable from `httpapi.Run` and `dockerx.New`.
- **GO-2026-4970** — root escape via symlink plus trailing slash in `os`,
  reachable from `certs.writeExclusive` — i.e. it affects the `os.Root`
  hardening added for gosec G304.

The Dockerfile's `golang:1.26-alpine` already resolves to 1.26.5, so the
**shipped image is not affected** (verified: the rebuilt image compiled under
go1.26.5). Only a binary built with the local 1.26.4 toolchain is. Recommend
upgrading the local toolchain and pinning the builder to a patch version so a
future base-image regression cannot silently reintroduce this.

## Next Steps

- [x] **Run `make test-race`** — done. Executed locally after installing MinGW-w64;
      all packages pass with no data races, and the `cover` target reports 84.5%.
      Both Makefile targets are now verified end to end, not just believed correct.
- [ ] Upgrade the local Go toolchain to ≥1.26.5 and consider pinning the
      Dockerfile builder to an exact patch version.
- [ ] Measure the retention bound under sustained load (`DEVMON_LOG_MAX_TOTAL_MB=8`,
      `DEVMON_LOG_LEVEL=debug`) and record the numbers; the PRD's open question
      about default log budgets is answerable only with that measurement.
- [ ] Code review via `/ecc:code-review` or `/ecc:go-review`
- [x] Commit and open a PR — PR #2, `feat/secure-foundation-and-persistence` -> `dev`
- [ ] `/ecc:prp-plan` for Phase 2 — identity, pairing & revocation
