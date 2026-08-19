# Implementation Report: Read Operations (Phase 3)

> **Historical record.** Figures, thresholds, and release status below describe the
> repo as it stood when this phase shipped, and are deliberately not updated in
> place. Most visibly, the coverage floor was 80% at the time and is 90% today.

## Summary

Eight mTLS-guarded `GET` routes give a paired device full read visibility into the host's
containers, images, networks, and volumes. A new read-only surface on `internal/dockerx`
projects Docker Engine responses through explicit allowlisted DTOs rather than forwarding
raw Engine JSON, so no response can carry environment variables or volume driver options
at any nesting depth.

## Assessment vs Reality

| Metric | Predicted (Plan) | Actual |
|---|---|---|
| Complexity | Medium–Large (18 files, 10 tasks, ~900 lines production Go) | Matched; 1,072 lines production Go across 9 new files |
| Confidence | Plan carried verified SDK signatures | Two plan facts were wrong (see Deviations); both caught at compile time |
| Files Changed | 18 (12 created, 6 updated) | 24 (17 created, 7 updated) |
| Coverage | ≥ 80% on `./internal/...` | 85.4% (needed one unplanned test file to get there) |

## Tasks Completed

| # | Task | Status | Notes |
|---|---|---|---|
| 1 | DTO types and list envelope | Complete | All DTOs verbatim from the plan's allowlist |
| 2 | Error classification | Complete | `errdefs` promoted from indirect to direct |
| 3 | Object reference validation | Complete | Digest refs (`sha256:…`) rejected, asserted explicitly |
| 4 | Container reads | Complete | Deviated — added `toPortsFromPortMap` |
| 5 | Image, network, volume reads | Complete | Deviated — two plan facts corrected |
| 6 | `requireOp` policy gate | Complete | No behavioural change today, by design (D10) |
| 7 | `DockerReader` interfaces + 8 handlers | Complete | Deviated — `Server.dc` field added early to compile |
| 8 | Wire routes and dependency | Complete | Deviated — `pair_test.go` also needed the new argument |
| 9 | Handler and DTO tests | Complete | Split across two packages; two leak tests already existed |
| 10 | Docs | Complete | README API table + "Why responses are projections" |
| — | Coverage remediation | Complete | **Unplanned.** See Issues Encountered |

## Validation Results

| Level | Status | Notes |
|---|---|---|
| Static analysis (`gofmt`, `go vet`) | Pass | `go vet ./...` clean; new files gofmt-clean |
| Lint (`golangci-lint`) | Pass | v2.12.2, default linter set, `0 issues` repo-wide |
| Security scan (`gosec`) | Pass | `gosec ./...` → **0 issues**, 34 files, 4,468 lines |
| Unit tests | Pass | `go test ./internal/... -race` all green |
| Coverage | Pass | 85.4% on `./internal/...`, floor is 80% |
| Build | Pass | `CGO_ENABLED=0` static binary, 18.9 MB |
| Integration / E2E | Pass | Full manual checklist run against Docker 29.6.1 / API 1.55 — see below |
| Edge cases | Pass (unit level) | Nil-pointer, truncation, empty-list, traversal all covered |

Per-package coverage: certs 79.1, config 90.9, dockerx 95.2, httpapi 84.8, logging 84.6,
policy 100, state 80.2, tlsconf 100.

**On the two security-scan numbers.** `gosec ./...` — the command in the Makefile and this plan —
reports **0 issues**, because gosec skips `_test.go` files unless `-tests` is passed. Running
`gosec -tests ./...` surfaces 5 findings, and `golangci-lint --enable gosec` surfaces 4 of the
same set (it applies its own severity filtering). Every one of them is in a Phase 1–2 test file
this phase never touched:

| File | Rule | Note |
|---|---|---|
| `internal/certs/ca_test.go:346,367` | G306 | Fixture writes a stub file with `0o644` into a temp dir |
| `internal/certs/store_test.go:141,163` | G306 | Same |
| `internal/state/pairing_test.go:126` | G115 | `string(rune('a'+n))` int→rune conversion in a test loop |

Production code is clean under both tools. The test-file findings are left alone deliberately —
fixing them here would widen this PR beyond the eight read routes — but they are worth a small
separate cleanup so that `gosec -tests` is also clean.

## Files Changed

| File | Action | Lines |
|---|---|---|
| `internal/dockerx/types.go` | CREATED | +164 |
| `internal/dockerx/errors.go` | CREATED | +31 |
| `internal/dockerx/ref.go` | CREATED | +29 |
| `internal/dockerx/containers.go` | CREATED | +270 |
| `internal/dockerx/images.go` | CREATED | +104 |
| `internal/dockerx/networks.go` | CREATED | +113 |
| `internal/dockerx/volumes.go` | CREATED | +84 |
| `internal/httpapi/reads.go` | CREATED | +240 |
| `internal/httpapi/policygate.go` | CREATED | +37 |
| `internal/dockerx/types_test.go` | CREATED | +310 |
| `internal/dockerx/errors_test.go` | CREATED | +83 |
| `internal/dockerx/ref_test.go` | CREATED | +52 |
| `internal/dockerx/containers_test.go` | CREATED | +346 |
| `internal/dockerx/objects_test.go` | CREATED | +412 |
| `internal/dockerx/engine_test.go` | CREATED | +656 |
| `internal/httpapi/reads_test.go` | CREATED | +740 |
| `internal/httpapi/policygate_test.go` | CREATED | +69 |
| `internal/httpapi/server.go` | UPDATED | +30 / -1 |
| `cmd/devmon-agent/main.go` | UPDATED | +1 / -1 |
| `internal/httpapi/server_test.go` | UPDATED | +1 / -1 |
| `internal/httpapi/status_test.go` | UPDATED | +7 / -1 |
| `internal/httpapi/pair_test.go` | UPDATED | +1 / -1 |
| `go.mod` | UPDATED | +2 / -2 |
| `README.md` | UPDATED | +46 |

## Deviations from Plan

1. **`timeext.Time` conversion was a non-issue.** The plan warned that
   `network.Network.Created` is `timeext.Time`, not `time.Time`, and prescribed a fallback.
   In reality the upstream swagger-generated file imports the standard library as
   `timeext "time"` — it is an import alias, not a distinct type. `Created` *is* a
   `time.Time`. Used `n.Created.UTC().Format(time.RFC3339)` directly and documented the
   finding in the mapper so nobody reintroduces a spurious conversion.

2. **`image.InspectResponse.OS` is spelled `Os`.** Mapped `ImageDetail.OS = r.Os`.

3. **Added `toPortsFromPortMap` (Task 4).** `ContainerDetail.Ports` is in the DTO allowlist
   but the plan named no helper to populate it from `InspectResponse.NetworkSettings.Ports`
   (`network.PortMap`, a different shape from `Summary.Ports`). Added with the same nil
   guarding as the rest of the file.

4. **`network.EndpointResource.IPv4Address/IPv6Address` are `netip.Prefix`, not `netip.Addr`.**
   Same `.IsValid()` → `.String()` guard; values include the prefix length
   (`"172.19.0.2/16"`), matching Docker's own documented example for that field.

5. **`Server.dc` field landed in Task 7 rather than Task 8**, because `reads.go` does not
   compile without it. Task 8 wired the constructor and routes as planned.

6. **`internal/httpapi/pair_test.go` also needed the new `NewServer` argument.** The plan
   enumerated four helpers; a fifth existed. One-line mechanical fix.

7. **`TestImageDetailNeverCarriesEnv` and `TestVolumeSummaryNeverCarriesOptions` were
   already written in Task 5's `objects_test.go`**, both already asserting on marshalled
   JSON. Not duplicated in `types_test.go`; a comment there points at them.

## Issues Encountered

**Coverage came in below the floor at 78.0%.** After Task 9, `internal/dockerx` sat at
58.6% because all eight Engine-calling wrappers were at 0% — every test covered only the
pure mappers, so the truncation path, the `classify` error path, and the
validate-ref-before-any-Engine-call ordering were never exercised through a real call.
That is a genuine test gap, not just an unflattering number.

Fixed by adding `internal/dockerx/engine_test.go`, which drives the wrappers against a
fake Engine using an in-process `http.RoundTripper` injected via the SDK's exported
`client.WithHTTPClient`. No live daemon and no listening socket. It proves, among other
things, that an invalid reference produces `ErrInvalidRef` with **zero** requests reaching
the Engine. `internal/dockerx` went 58.6% → 94.1%; the `./internal/...` total went
78.0% → 85.4%.

**`gofmt -l` flags the entire working tree.** The repo has `core.autocrlf=true`, so every
checked-out file has CRLF and `gofmt` reports all of them. This is pre-existing and
repo-wide, not introduced here; the committed blobs are LF and a Linux CI checkout is
clean. Each new file was verified individually.

**`golangci-lint` and `gosec` are not installed on this machine**, so two gates in the
plan's validation section could not be run. `go vet ./...` is clean. These should run in
CI or after installing the tools before merge.

## Tests Written

| Test File | Tests | Coverage |
|---|---|---|
| `internal/dockerx/types_test.go` | 8 field-count guards + 2 leak guards | DTO allowlist cannot widen silently |
| `internal/dockerx/errors_test.go` | 1 table (3 cases) | `classify` not-found vs passthrough |
| `internal/dockerx/ref_test.go` | 1 table (11 cases) | Traversal, digest, length boundaries |
| `internal/dockerx/containers_test.go` | 9 | Container mappers, all nil-pointer branches |
| `internal/dockerx/objects_test.go` | ~12 | Image/network/volume mappers, leak guards |
| `internal/dockerx/engine_test.go` | 7 tables across all 8 methods | Wrappers, truncation, 404/500, ref-first |
| `internal/httpapi/reads_test.go` | 11 tables across all 8 routes | 200/400/401/404/405/502, `?all=`, nil reader |
| `internal/httpapi/policygate_test.go` | 1 table (6 cases) | 3 modes × 2 operations |

The highest-value tests are the ones asserting on **marshalled JSON** that `hunter2` and
`env` never appear in a `ContainerDetail` or `ImageDetail`, and that `options` never
appears in a `VolumeSummary`. A field-level assertion would miss a future embedded struct.

## Acceptance Criteria

- [x] All 10 tasks complete
- [x] Eight routes registered, each behind `requireDevice` **and** `requireOp(policy.OpRead)`, in that order
- [x] No response DTO carries env vars or volume driver options, proven by marshalled-JSON tests
- [x] `ErrNotFound` → 404, `ErrInvalidRef` → 400, all other Engine failures → 502; no 500 from a read route
- [x] Lists capped at 500 with an honest `truncated` flag; empty list marshals as `[]`
- [x] `gofmt`, `go vet`, `golangci-lint`, `gosec` all clean for this phase's code
- [x] `./internal/...` coverage 85.4% ≥ 80%
- [x] **A paired client retrieves accurate data on a host with real workloads** — verified against
      Docker 29.6.1 / API 1.55. The PRD's Phase 3 success signal is met.

## End-to-End Validation

Environment: agent running in its own container against a real daemon, a genuinely paired device
(EC keypair → CSR → `/v1/pair` → 201), and fixtures built to carry real secrets. The Engine was
reached through a socat proxy so it could be made unreachable *while the agent stayed running* —
stopping the host daemon would have killed the agent container too and proved nothing.

| Check | Result |
|---|---|
| Container with `-e DB_PASSWORD=hunter2`, inspected | 698-byte body, no `hunter2`, no `DB_PASSWORD`, no `env` key |
| Image with `ENV API_KEY=supersecretkey123`, inspected | no `supersecretkey123`, no `API_KEY`, no `env`, no `config` |
| Volume with `type=tmpfs,o=size=1m,uid=1000` | no `options` key, none of the option values |
| All eight routes, `default` mode | 200 |
| All eight routes, `read-only` mode | 200 (confirms D10) |
| Stopped container | absent from `/v1/containers`, present in `?all=true` as `exited` / `Exited (1)` |
| Engine unreachable | four list routes 502 `docker engine unavailable`; `/v1/status` still 200; agent up, 0 restarts |
| Engine restored | reads recover with no agent restart |
| Unknown object | 404 |
| `%2e%2e`, `%2e%2e%2finfo`, `a%2fb`, `foo$bar`, `-leading`, `sha256:ab` | 400 `invalid object reference`, none reached the Engine |
| `POST /v1/containers` | 405 |
| No client certificate | 401 `client certificate required` |
| Device revoked | every read route 401 immediately, no agent restart |

Two things worth recording:

- **`curl` normalizes `%2e%2e` client-side**, which made the first traversal probe look like a
  404 from the agent. With `--path-as-is` the request reaches the handler and returns the
  documented 400. The contract holds; the initial reading was a client artifact.
- **Confirmed review finding M4 with evidence.** The Debug request log does carry reference
  values (`path=/v1/containers/cb6831df…`). It carries no secrets: zero hits for env values,
  private key material, or pairing codes across the whole run. The "never log secrets" rule
  holds; the plan checklist's "no reference values" claim does not.

## Next Steps

- [x] ~~Run `golangci-lint` and `gosec`~~ — done, both clean
- [ ] Separate cleanup commit for the five `gosec -tests` findings in Phase 1–2 test files
- [ ] Work the plan's Manual Validation checklist against a real host — in particular:
      start a container with `-e DB_PASSWORD=hunter2` and confirm the string appears
      nowhere in the API response; stop the daemon and confirm 502 with the agent still up
- [ ] Code review via `/ecc:code-review` (`go-reviewer` + `security-reviewer`)
- [ ] Create PR into `dev` via `/ecc:prp-pr`
