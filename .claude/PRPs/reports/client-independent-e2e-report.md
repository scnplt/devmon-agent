# Implementation Report: Client-Independent End-to-End Suite (Phase 6)

## Summary

The manual validation checklists of Phases 1-5 are now code. `internal/e2e/` holds a
hand-rolled harness and 73 contract tests in two groups: a **host-binary group** that
builds the real `devmon-agent`, starts it with a curated environment, pairs through the
documented `device pair-code` path, and drives every route over pinned mTLS; and an
**in-container group** that builds the image and runs the agent as a container, which is
the only way to exercise self-identification through `/proc/self/mountinfo` and the
self-exclusion guarantee.

Every assertion is written against the wire — status codes, headers, and JSON decoded into
`map[string]any` or into structs declared inside the suite — never against the agent's own
DTOs. That is the difference between a regression suite and a contract: a renamed JSON tag
must fail here, and it cannot if the suite and the agent share the struct.

No production Go file was changed (D19), no module dependency was added (D1), and the
default gate is untouched: every file carries `//go:build e2e`, so `go build ./...`,
`go vet ./...`, `go test ./internal/... -race` and `make cover` neither compile nor run a
line of it.

## Assessment vs Reality

| Metric | Predicted (Plan) | Actual |
|---|---|---|
| Complexity | Large, ~2000 lines of test and harness Go | 7,109 lines across 27 files |
| Files | 23 created, 4 updated | 27 created, 4 updated |
| Tasks | 13 | 13, all complete |
| Commits | one per task | 13 |
| Production Go changed | none | none |
| `go.mod` / `go.sum` | unchanged | unchanged |
| Tests | — | 76 (61 host-binary, 12 in-container, 3 harness unit tests) |

The line estimate was low by roughly 3.5x. The gap is almost entirely in the contract
tests themselves rather than the harness: writing key-set assertions against
`map[string]any`, with required and optional keys separated and the distinction spelled
out, costs several times what unmarshalling into `dockerx.ContainerSummary` would have —
which is D4's cost, paid deliberately. `contract_reads_test.go` alone is 788 lines for
eight routes.

Four files beyond the plan's 23: `harness/proxy_test.go`, `harness/stream_test.go` (pure
unit tests over the SSE frame parser and the proxy's own bookkeeping — the parts of the
harness that can be wrong without an Engine, and that would otherwise be trusted blindly),
`harness/rebind.go` (the port-allocation retry the plan's Task 1 gotcha called for, split
out rather than inlined), and this report.

## Tasks Completed

| # | Task | Commit | Notes |
|---|---|---|---|
| 1 | Harness foundation — Engine probe, fixtures, agent process | `448476a` | `_linux`/`_other` split for the socket-GID lookup |
| 2 | Pinned mTLS contract client and host-side CLI | `b4ee77f` | `InsecureSkipVerify` confined to pairing bootstrap and readiness |
| 3 | Status and startup contract | `61aa7b2` | |
| 4 | Identity contract | `d872793` | |
| 5 | Read contract and the Engine-unavailable path | `0dcd68b` | Includes the severable in-process proxy |
| 6 | Logs, streaming, resume, abrupt loss | `1fd3c37` | `SetLinger(0)` for the ungraceful teardown |
| 7 | Lifecycle and policy contract | `f54fe38` | |
| 8 | In-container harness — build, run, exec, copy | `843118a` | |
| 9 | Self-exclusion guarantee | `2985b74` | The PRD's headline metric |
| 10 | Self-identification variants and state survival | `a69c034` | |
| 11 | Audit contract | `b4f3b95` | |
| 12 | Endurance and retention | `ebba469` | Env-gated on `DEVMON_E2E_ENDURANCE` |
| 13 | Make targets, CI job, docs | `813482f` | |

Task numbering in the commit sequence differs from the plan's: the audit contract (plan
Task 8) landed after the in-container group, because the in-container harness was the
long pole and the audit tests reuse nothing from it.

## Validation Results

| Level | Status | Notes |
|---|---|---|
| `gofmt -l` on new files | Pass | Whole-tree listing is CRLF checkout noise, as in prior phases |
| `go vet ./...` | Pass | |
| `go vet -tags e2e ./...` | Pass | Including on Windows, which is what the `_linux`/`_other` split is for |
| `go test ./internal/... -race` | Pass | All 10 packages, unchanged package list and duration |
| `gosec ./...` | Pass | Unchanged — the e2e files are both tag-excluded and `_test.go`, so the scanner never sees them |
| Module hygiene | Pass | `go.mod` and `go.sum` byte-identical to the start of the phase |
| **`make e2e` against a real Engine** | **Not run** | See Outstanding |

## Outstanding — the suite has not yet been observed green

**No part of this suite has been executed against a Docker Engine.** The development
machine is Windows with Docker Desktop's Linux engine stopped, and D6 puts the documented
path inside WSL2. What has been proved is that everything compiles under `-tags e2e`, that
the pure-function parts (SSE frame parsing, proxy bookkeeping, the skip gates) behave, and
that the default gate is unaffected.

This is why **the PRD's Phase 4 and Phase 5 rows were not flipped to `complete`**. The
plan is explicit that they may move only after a green run is actually observed and
recorded here with the Engine version it ran against, and that flipping them on the
strength of the code existing is precisely the failure this phase was created to end.
Phase 6's own row reads `awaiting a green run on a real Engine` for the same reason.

Falsifiability is in the same position. Each test's doc comment records how it should be
falsified — the specific inversion, wrong target, or omitted setup step that must turn it
red — but those inversions have not been performed. The per-task obligation is therefore
**discharged in design and outstanding in execution**, and a green first run is not
sufficient to close it: a green test that has never been red is indistinguishable from a
test that asserts nothing.

### What a first run must do, in order

1. `make e2e` from a WSL2 shell with the Linux engine running. Record the Engine version,
   the platform, and the wall-clock duration in this report.
2. Work through the failures. A first run of 76 tests written without execution will not be
   green, and the interesting question is which failures are the suite's and which are the
   agent's. **A defect the suite uncovers is recorded here and fixed in its own task and
   its own commit** (D19) — never folded into a fix of the test that found it.
3. Perform each test's recorded falsification once, and note in this report how each was
   falsified.
4. `make e2e-endurance` once — the 30-minute stream is the PRD's Phase 4 success signal.
5. Only then flip the PRD's Phase 4, Phase 5, and Phase 6 rows.

### The rest of the manual checklist

- [ ] `make e2e` on a clean Linux host, Engine version recorded
- [ ] `make e2e` from WSL2; confirm both groups run rather than skip
- [ ] `make e2e` Windows-native; confirm the skip names WSL2 and does not read as a failure
- [ ] Two concurrent runs on one host; confirm neither disturbs the other's fixtures
- [ ] Docker stopped: suite skips, target exits 0; with `DEVMON_E2E_REQUIRE=1`, it fails
- [ ] A full `-v` run inspected for any pairing code, PEM block, or private key
- [ ] `docker ps -a` after a full run shows no leftover `com.devmon.e2e` container
- [ ] The CI `e2e` job runs on a PR into `main` and is skipped, not queued, on a PR into `dev`

## Coverage of the Plan's Contract

Every row of the plan's Coverage Map is an executing test or an explicitly named owner
elsewhere; no unticked, unowned box remains in this repository. The four items named as
belonging to the Android app's own suite — stream reconnection across a real network
handover, renewal *timing*, a genuinely expired client certificate, and pairing UX — are
recorded in the plan with the reason each cannot live here, and in each case the
agent-side half of the contract is tested.

Coverage measurement deliberately does not move: the 80% floor stays measured over
`./internal/...` untagged. E2E tests exercise a separate process, and folding them into the
same number would inflate it while measuring nothing new about unit-testable code.

## Next Steps

Phase 7 (Hardening & OSS release) depends on this phase for a regression net it can change
the agent against — rate limiting, the security review against the PRD risk table, the
automated installer, the threat-model docs, and the AGPL-3.0 release. It should not start
until the suite has run green once, because a regression net nobody has seen catch
anything is not yet a net.
