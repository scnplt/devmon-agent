# Implementation Report: Lifecycle, Policy & Audit (Phase 5)

## Summary

A paired device can now act. Five lifecycle routes — start, restart, stop, kill, delete —
are live, each gated by the host's startup policy mode, each blocked from touching the
agent's own container, and each recorded in the audit trail with the calling device's
identity. The `audit` table created in Phase 1 holds rows for the first time, and
`devmon-agent audit list` reads them from the host.

The three pieces shipped together because they are one story: the lifecycle calls are the
capability, the policy tier is what stops a compromised phone exceeding what the operator
granted, and the audit row is the record that a destructive operation reachable from a
phone actually happened.

## Assessment vs Reality

| Metric | Predicted (Plan) | Actual |
|---|---|---|
| Complexity | Large (~1100 lines production Go plus tests) | As predicted |
| Files changed | 26 (12 created, 14 updated) | 27 (12 created, 15 updated) |
| Tasks | 12 | 12, all complete |
| Commits | one per task | 13 (12 tasks + 1 gate fix) |
| `internal/...` coverage | ≥ 80% | 84.0% |
| `go.mod` | unchanged | unchanged |

The extra updated file is `internal/httpapi/logs.go`, unavoidable collateral of the
`writeDockerError` signature change the plan itself flagged (Task 8's first GOTCHA); the
plan listed the eight read handlers but not the two log call sites.

## Tasks Completed

| # | Task | Status | Notes |
|---|---|---|---|
| 1 | Self-identification candidates (`internal/selfid`) | Complete | 100% coverage |
| 2 | Verify the self ID against the Engine | Complete | |
| 3 | Lifecycle sentinels and error classification | Complete | |
| 4 | The five lifecycle calls with the self-exclusion chokepoint | Complete | Deviated — see below |
| 5 | `protected` on the container DTOs | Complete | Field counts bumped 11→12, 24→25 |
| 6 | The audit repository | Complete | No migration; schema version stays 2 |
| 7 | The `withAudit` middleware | Complete | |
| 8 | The five handlers and `ContainerController` | Complete | Deviated — see below |
| 9 | Route registration | Complete | |
| 10 | Wire self-identification through configuration and startup | Complete | |
| 11 | Host-side `audit list` | Complete | |
| 12 | Docs and the manual sweep | Complete | Automated sweep done; manual sweep outstanding |

## Validation Results

| Level | Status | Notes |
|---|---|---|
| `gofmt` | Pass | New files clean; whole-tree listing is CRLF checkout noise, verified with `gofmt -d` |
| `go vet ./...` | Pass | |
| `golangci-lint run ./...` | Pass | 0 issues, after fixing one QF1001 |
| `gosec ./...` | Pass | 0 issues, after justifying one G304 |
| Unit tests (`-race`) | Pass | All 10 packages |
| Coverage | Pass | 84.0% total, floor is 80% |
| Build | Pass | `go build ./...` and `CGO_ENABLED=0 go build ./...` |
| Module hygiene | Pass | `go mod tidy` leaves `go.mod` and `go.sum` unchanged |
| End-to-end on a real host | **Not run** | Requires a VPS with real containers |

Per-package coverage: `selfid` 100%, `policy` 100%, `tlsconf` 100%, `config` 91.4%,
`httpapi` 88.1%, `logging` 84.6%, `dockerx` 81.3%, `state` 80.8%, `certs` 79.1%.

## Deviations from Plan

**1. `ErrNotModified` is unreachable from the lifecycle calls, so no handler branch was
written for it.**

*What*: The plan (Task 8, D9) specified a handler branch treating `dockerx.ErrNotModified`
as success with an "already in requested state" audit detail. That branch does not exist.

*Why*: `github.com/moby/moby/client@v0.5.1/request.go:225` — `checkResponseErr` returns
nil for every status in `[200, 400)`, which includes the Engine's 304. `ContainerStart` on
a running container therefore returns a nil error, not a classified one, and the success
path already answers 204. D9's requirement ("already in the requested state is success")
holds without a special case. Verified by reading the SDK source, not assumed.

The `ErrNotModified` sentinel and `classify`'s `IsNotModified` branch were still added per
Task 3 — they are correct and cost nothing, and would start working if the SDK's error
handling changed. The two `lifecycle.go` doc comments that claimed a 304 "surfaces as
ErrNotModified" were corrected to describe what actually happens.

**2. `internal/httpapi/logs.go` was updated.** Not in the plan's file list; forced by the
`writeDockerError(w, r, op, err)` signature change. Mechanical, no behaviour change.

**3. Two gate findings fixed after the task sweep**, in a separate commit: a staticcheck
QF1001 (De Morgan) in `audit_test.go`, and a gosec G304 in `selfid.go`. The G304 carries a
`#nosec` with rationale — the path is assembled from a caller-supplied root plus fixed
literal segments and never from request data. This is the repository's first `nosec`.

## Issues Encountered

**The self-identification problem was the phase's real difficulty, as the plan predicted.**
No single source is reliable, so the implementation is a candidate chain with Engine
verification: `DEVMON_SELF_CONTAINER_ID`, then `/proc/self/mountinfo`, then
`/proc/self/cgroup`, then `$HOSTNAME`. The full 64-char ID from the Engine's inspect
response is what gets stored — never the candidate string, which is what makes a 12-hex
`$HOSTNAME` candidate still compare equal to a resolved target.

**`resolveTarget` is the load-bearing code.** All five lifecycle methods reach the Engine
only through it, and it enforces the self check on the resolved ID rather than on the
caller's reference. That is what makes refusal identical whether a device names the agent
by container name, short ID, or full ID.

## Tests Written

| Test File | Area covered |
|---|---|
| `internal/selfid/selfid_test.go` | Detection from each source, cgroup v2 empty case, `$HOSTNAME` rejection, override precedence, de-duplication |
| `internal/dockerx/self_test.go` | First-confirmed-wins, full-ID storage, none-confirmed still yields a usable client |
| `internal/dockerx/lifecycle_test.go` | Self-exclusion by name / short ID / full ID across all five methods, fail-closed before any Engine call, conflict mapping, explicit stop timeout |
| `internal/dockerx/errors_test.go` | Not-modified and conflict classification |
| `internal/dockerx/types_test.go`, `containers_test.go` | `protected` true/false, field-count guards at 12 and 25 |
| `internal/state/audit_test.go` | Round-trip, nullable columns, `id DESC` ordering, limit, interaction with `PruneAudit` |
| `internal/httpapi/audit_test.go` | Exactly one row per request including refusals, zero rows unauthenticated, store failure does not fail the response |
| `internal/httpapi/lifecycle_test.go` | Per-route status matrix, 3 modes × 5 operations, 401 without a device, 405 on wrong method, nil Docker reader |
| `cmd/devmon-agent/cli_test.go` | `audit list` dispatch, formatting, device-name join, deleted-device fallback, `--limit` |

## Outstanding

The plan's **End-to-End Validation** and **Manual Validation** checklists require a real
host with real containers and have not been run. The headline item among them is the PRD's
success metric: *the agent surviving a delete attempt in the most permissive mode*. That is
covered by unit tests at every layer, but not yet demonstrated on hardware.

## Next Steps

- [ ] Run the end-to-end and manual checklists from the plan on a real host
- [ ] Move the PRD Phase 5 row to `complete` once that passes
- [ ] Phase 6: rate limiting on the mutating routes, security review against the risk
      table, and the automated installer — which now has one more decision to surface,
      since the policy mode actually changes what a phone can do
