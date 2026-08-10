# Plan: Client-Independent End-to-End Suite (DevMon Agent Phase 6)

## Summary

Turn the manual validation checklists of Phases 1-5 into **one command that runs unattended
against a real Docker Engine, with no phone and no emulator anywhere in the loop**.

Phases 4 and 5 are still marked `awaiting device validation` / `awaiting host validation` for the
same reason: the last mile of each was written as a list of things a human does with `curl` and a
handset. Those lists are the contract the agent actually promises, and today nothing executes them.
This phase makes them code.

The reframing that makes this possible: **almost none of the outstanding items need a phone. They
need a Docker Engine.** "Delete the agent's own container and watch it refuse" is a property of the
agent and the Engine; the Android app is only the thing that happened to send the request. Where an
item genuinely is client-side — an app reconnecting a stream across a Wi-Fi-to-mobile handover — the
*agent-side half* of that contract (the stream tears down on abrupt connection loss, `?since=<id>`
resumes with at most one repeated line) is tested here, and the client-side half is named explicitly
as belonging to the Android app's own suite rather than left as an unticked box in this repository.

The suite is written as the **executable definition of the API contract for future clients**, mobile
or web. That is why it asserts wire shapes — status codes and JSON keys decoded into `map[string]any`
— rather than importing the agent's own DTO structs. A suite that unmarshals into `dockerx.ContainerSummary`
cannot notice a field rename; a client written against the JSON can, and will.

## User Story

As the maintainer of an agent whose only client is a separate Android project I do not control,
I want every promise the agent makes to be checked by running the real binary against a real Engine,
so that I can change the agent, cut a release, and know the contract still holds — without borrowing
a phone, and without a future web client discovering the contract by trial and error.

## Problem → Solution

Five phases of manual checklists exist as prose. Two PRD rows say `awaiting validation`. The headline
success metric — *the agent surviving a delete attempt in the most permissive mode* — is covered by
unit tests at every layer and has never been demonstrated against a running Engine. → A build-tagged
Go suite in two groups: a **host-binary harness** that builds and runs the real `devmon-agent`, pairs
through the documented host-side command path, and drives every route over mTLS; and an
**in-container group** that builds the image and runs the agent as a container, which is the only way
to exercise self-identification through `mountinfo` and the self-exclusion guarantee. One `make e2e`.
Nothing added to the default `go test ./internal/... -race` path, and no new module dependency.

## Metadata

- **Complexity**: Large (23 files: 21 created, 4 updated; ~2000 lines of test and harness Go, **zero
  lines of production Go**)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 6 — Client-independent end-to-end suite
- **Depends on**: Phase 4 and Phase 5 (both code-complete; this phase is what closes their validation)
- **Blocks**: Phase 7 (Hardening & OSS release), which needs this regression net to check its changes against
- **Estimated Files**: 23

---

## Decisions Settled Before Planning

Each has a plausible-looking wrong answer. Settled here so implementation never re-litigates them.

| # | Decision | Choice | Why not the alternative |
|---|---|---|---|
| D1 | Harness: hand-rolled or a framework | **Hand-rolled, reusing `github.com/moby/moby/client` v0.5.1, already a direct dependency** | `testcontainers-go` and `ory/dockertest` both depend on `github.com/docker/docker` — the pre-v29 SDK this repository deliberately does not use. Taking either means two Docker SDKs in one module and a `go.sum` several hundred lines longer in a security tool whose supply chain is part of its pitch. **A build tag does not exclude a module requirement**: `//go:build e2e` keeps the code out of the binary, not the dependency out of `go.mod`. Every previous phase held "go.mod unchanged" as an acceptance criterion and this one can too. What the frameworks sell — wait strategies, a reaper sidecar, generic container startup — is worth little here: the container under test is the product itself, needing a bind mount, a curated environment, `exec` for the pairing code, and a deliberately severed TCP connection. The reaper (`ryuk`) is itself a privileged container holding the Docker socket, which is the exact posture this product exists to avoid. Cost accepted: roughly 400 lines of harness we own and can read. |
| D2 | Where the suite lives | **`internal/e2e/`**, three packages: `harness` (importable helpers), `api` (host-binary group), `incontainer` (image group) | Under `internal/`, the harness cannot be imported by another module, and `gofmt -l .`, `go build ./...`, and `go vet ./...` already sweep it with no Makefile change. Verified in this session: a directory whose files are all excluded by a build constraint is **silently skipped** by wildcard patterns (`go build ./...`, `go test ./internal/...` — exit 0, no output) but **errors on an explicit path** (`go build ./internal/e2e/api` → "build constraints exclude all Go files"). So the default gate is untouched and the coverage denominator does not move. Three packages, not one: `-run` targeting stays simple, and the in-container group can skip for its own reasons (no `docker` CLI, no Linux daemon) without dragging the host-binary group down with it. |
| D3 | Build tag | **`//go:build e2e` on every file in all three packages** | One tag, not a tag per group: two tags multiply into four states nobody tests. Group selection is by package path, which is what package paths are for. |
| D4 | What the assertions are written against | **The wire**: status codes, headers, and JSON decoded into `map[string]any` / typed local structs declared *inside the suite* | Importing `dockerx.ContainerSummary` would make the suite agree with the agent by construction — rename a JSON tag and both move together, the test stays green, and the Android app breaks in production. A field-set assertion written from the client's side is the only kind that can catch it. This is what "executable definition of the contract" means concretely, and it is why the suite deliberately duplicates a handful of struct definitions. |
| D5 | Behaviour with no Engine reachable | **Skip every test with a one-line reason naming the endpoint — unless `DEVMON_E2E_REQUIRE=1`, which turns the same condition into a hard failure. CI sets it.** | A silent pass is the failure mode that matters: a suite that quietly does nothing is worse than no suite, because the PRD row gets flipped to `complete` on its strength. A skip is visible in `go test` output and in the make target's summary; `DEVMON_E2E_REQUIRE` is what makes it impossible for CI to be the thing that skips. |
| D6 | Windows developer machine | **Both groups require a Linux Engine reached over `unix://` or `tcp://`. On Windows the documented path is WSL2 (`make e2e` from a WSL shell). Windows-native skips with exactly that sentence.** | `internal/config/config.go:242` accepts only the `unix` and `tcp` schemes, so `npipe:////./pipe/docker_engine` — Docker Desktop's default on Windows — cannot be given to the agent at all. Beyond that, bind-mount ownership semantics for a Windows host path into a `nonroot` Linux container are not something to design on without measuring, and the in-container group depends on them. WSL2 gives a real Linux path, a real unix socket, and a real `chmod`. Docker Desktop's optional `tcp://localhost:2375` also works for the host-binary group and is accepted via `DEVMON_E2E_DOCKER_HOST`, but it is not the documented path. |
| D7 | Trust bootstrap in the suite | **Pair once over `InsecureSkipVerify`, exactly as the README's `curl -k` step does, then pin `ca_certificate_pem` into a `RootCAs` pool for every subsequent request. After pairing, no client in the suite may set `InsecureSkipVerify`.** | This is the real client's sequence, and testing it any other way tests a flow no client performs. A dedicated test asserts the pinning is load-bearing: a client pinning the *wrong* CA fails the handshake. Handing the harness the CA file from the state directory instead would skip the one step where a machine-in-the-middle would show up. |
| D8 | How a pairing code is obtained | **Run the built binary's `device pair-code --name …` as a subprocess against the same `DEVMON_STATE_DIR`, while the agent is running** | It is the documented operator path, and running it against a live agent is simultaneously the test for the PRD's "the command-line path and the running agent share one state store" requirement. Reaching into SQLite from the harness would test neither. |
| D9 | How audit rows are read | **`devmon-agent audit list`**, the same subprocess path | D20 of Phase 5 keeps the audit log off the HTTPS API on purpose. A suite that opened `devmon.db` directly would be asserting against an interface no operator has, and would keep passing if the CLI broke. |
| D10 | Policy-mode coverage | **One agent instance per mode**, each with its own temp state dir, port, CA, and freshly paired device | Restarting one agent between modes couples the tests to each other and to ordering; a failure in the `full`-mode block would leave the `read-only` block running against the wrong agent. Startup is about a second and the state dir is a temp dir, so three instances cost less than the coupling does. Each instance also exercises a full pairing cycle, which is not waste — it is the PRD's "successful pairing on first attempt: 100%" metric, sampled once per test run. |
| D11 | Blast radius | **Every fixture container carries the label `com.devmon.e2e=1` and a run-scoped label; cleanup lists by label filter and never removes anything it did not create.** The suite never touches a container it did not label. | The suite runs against the developer's real Engine, with their real containers on it. A cleanup step that removed "test-looking" containers by name prefix is one typo away from deleting somebody's database. The label filter is the only safe primitive here, and it is worth stating as a rule rather than leaving to each task. |
| D12 | The agent's environment | **The harness builds the child process's environment explicitly — never `os.Environ()` plus overrides** | A developer with `DEVMON_POLICY_MODE=full` exported in their shell would silently invalidate the read-only assertions. `PATH` and `HOME` are passed through because `go build` needs them; every `DEVMON_*` variable the agent sees comes from the test case and nowhere else. |
| D13 | The suite's own knobs | `DEVMON_E2E_REQUIRE`, `DEVMON_E2E_ENDURANCE`, `DEVMON_E2E_DOCKER_HOST`, `DEVMON_E2E_KEEP` — read by the **harness**, never passed to the agent | They share the `DEVMON_` prefix for discoverability but are not part of the agent's configuration surface, and D12's curated environment guarantees the agent never sees them. A comment in the harness says so, because the prefix invites the opposite assumption. |
| D14 | Endurance and retention runs | **Compiled on every e2e run, skipped unless `DEVMON_E2E_ENDURANCE=1`**, with `make e2e-endurance` setting it plus `-timeout 45m` | A separate build tag would keep the file out of every ordinary compile, and code that compiles only in a target nobody runs weekly is code that rots. An env-gated `t.Skip` is compiled every time and prints a visible skip line naming the variable that enables it. |
| D15 | Abrupt connection loss | **A custom `DialContext` captures the `*net.TCPConn`; the test calls `SetLinger(0)` then `Close()`, which sends RST rather than FIN** | A plain `Close()` is an orderly shutdown — the agent sees EOF, which is the *graceful* path Phase 4 already verified. A phone entering a tunnel does not send FIN. `SetLinger(0)` is the standard way to get the ungraceful case from Go, and `net.TCPConn.SetLinger(sec int) error` was verified in this session. |
| D16 | The Engine-unavailable 502 path | **An in-process TCP proxy in the harness sits between the agent and the real Engine; the test closes it mid-flight** | Stopping the developer's Docker daemon to test a 502 is not something a test suite may do — it would take down everything else on the host, including the Engine the rest of the suite needs. Phases 3 and 4 used `socat` manually for exactly this reason; a Go proxy is the same idea without the external binary. The agent pings at startup, so the proxy must be listening before the agent starts. |
| D17 | `-race` on the e2e run | **Yes, on the test binary (`CGO_ENABLED=1`); the agent binary under test is still built `CGO_ENABLED=0`** | `-race` instruments only the harness process, not the child agent — but the harness has goroutines (SSE reader, proxy, output pumps) and they are exactly the kind of code that races. Building the agent with cgo enabled would diverge from the shipped artifact, which is the thing under test. |
| D18 | Test caching | **`-count=1` in every e2e target** | Results depend entirely on state outside the module. A cached PASS from before a code change is a false green, and the failure mode is silent. |
| D19 | Production code changes | **None in this phase.** A defect the suite uncovers is recorded, and fixed in its own task and its own commit, never folded into the task that found it | The value of this phase is that its output is trustworthy. A phase that fixes bugs while writing the tests that find them cannot say which of the two it got wrong. The report names every finding whether or not it was fixed here. |
| D20 | Which manual items remain manual | **Only those that need a physical device or a wall-clock window longer than a CI run.** Every one is named in the Coverage Map below, with the suite that owns it | The PRD's success signal requires this explicitly: "Anything genuinely untestable without a client device is explicitly named as belonging to the Android app's own suite rather than left as an unticked box here." An unticked box with no owner is how the previous two phases ended up `awaiting validation`. |
| D21 | CI placement | **A new `e2e` job in `.github/workflows/ci.yml`, gated on `github.base_ref == 'main'`, alongside `lint`, `image`, and `gosec`** | The `dev` PR bar is deliberately fast — `test` only. The e2e suite takes minutes and needs a Docker daemon; putting it on the `dev` path would tax every integration PR for a check that matters at release time. GitHub's `ubuntu-latest` runner ships a working Docker daemon and a `docker` CLI, so no service container is needed. |
| D22 | Image builds | **`docker build` via the CLI, one build per e2e run, in the in-container group's `TestMain`** | Assembling a tar build context in Go to call `ImageBuild` is real work with no payoff, and shelling out to `docker build` exercises the same command the `Makefile` and CI use. Everything else — create, start, inspect, exec, copy, remove — goes through the SDK, which is precise and already a dependency. The `docker` CLI's absence is a skip reason, not a failure. |

---

## Coverage Map — every outstanding manual item, and where it lands

This table is the phase's contract. Column 3 is either a test in this suite or an explicit owner
elsewhere. **No row is left blank.**

### Phase 1 — `secure-foundation-and-persistence.plan.md:1216-1229`

| Manual item | Group | Where it lands |
|---|---|---|
| Crash survival: pre-kill log lines still readable after `docker kill` + restart | in-container | `TestStateSurvivesCrashRestart` |
| Restart identity stability: server cert serial unchanged | host-binary | `TestIdentityStableAcrossRestart` (serial read from the TLS peer certificate) |
| Policy advertisement in `/v1/status` | host-binary | `TestStatusAdvertisesPolicyMode` (all three modes) |
| Config failure UX: two bad variables, both reported, exit 2 | host-binary | `TestConfigFaultsReportedTogether` |
| Missing `DEVMON_PUBLIC_ADDR` → exit 2 naming the variable | host-binary | `TestConfigFaultsReportedTogether` |
| Docker unreachable at startup → specific fatal error, no panic | host-binary | `TestStartupFailsWhenEngineUnreachable` |
| Retention bound: logs stay within `DEVMON_LOG_MAX_TOTAL_MB`, `.gz` backups exist | host-binary, endurance-gated | `TestLogRetentionBoundsDiskUse` |
| Graceful shutdown within ~1s, exit 0 | host-binary | `TestGracefulShutdown` |
| `POST /v1/status` → 405 | host-binary | `TestStatusRejectsOtherMethods` |
| No unauthenticated leakage on `/v1/containers` | host-binary | `TestUnauthenticatedRequestsLeakNothing` |

### Phase 2 — `identity-pairing-and-revocation.plan.md:908-922`

| Manual item | Group | Where it lands |
|---|---|---|
| Two devices pair with distinct codes and distinct certificates | host-binary | `TestTwoDevicesPairIndependently` |
| Both survive an agent restart | host-binary | `TestPairingsSurviveRestart` |
| Both survive an image rebuild + recreate, no re-pair | in-container | `TestPairingsSurviveImageUpgrade` |
| A device near expiry renews with no user interaction | host-binary (agent half) | `TestRenewIssuesUsableCertAndKeepsOldValid` — the *timing* of renewal is client-side; see "Named as client-side" |
| Revoked device loses access immediately, no restart | host-binary | `TestRevokedDeviceLosesAccessImmediately` |
| `DEVMON_PUBLIC_ADDR` change → cert re-issued, CA fingerprint unchanged, no re-pair | host-binary | `TestServerCertReissuedOnAddressChange` |
| `rm -rf certs/` with the DB intact → explicit identity error, not a new identity | host-binary | `TestMissingCertsDirIsLoudNotSilent` |
| Regenerated CA is distinguishable from an expired credential via `/v1/status` | host-binary | `TestWipedStateChangesCAFingerprint` (the expired-credential half is client-side) |
| No credential material in `agent.log` | both | `TestStateDirCarriesNoCredentialMaterial` (sweeps the whole state dir, not just the log) |

### Phase 3 — `read-operations.plan.md:894-907`

| Manual item | Group | Where it lands |
|---|---|---|
| `-e DB_PASSWORD=hunter2` never appears in an inspect response | host-binary | `TestContainerInspectOmitsEnvironment` |
| Image `ENV API_KEY=…` likewise | host-binary | `TestImageInspectOmitsEnvironment` |
| Volume driver `options` absent from the response | host-binary | `TestVolumeInspectOmitsDriverOptions` |
| Stopped container absent by default, present with `?all=true` as `exited` | host-binary | `TestContainerListAllParameter` |
| Failing healthcheck surfaces as `health: "unhealthy"` | host-binary | `TestContainerListReportsHealth` |
| New network and volume appear immediately (no caching) | host-binary | `TestReadsAreNotCached` |
| Docker daemon down → every read 502, agent stays up | host-binary | `TestReadsAnswer502WhenEngineIsGone` (D16's proxy) |
| Reads recover when the daemon returns, no agent restart | host-binary | same test, second phase |
| `read-only` mode → all eight read routes still 200 | host-binary | `TestReadOnlyModePermitsAllReads` |
| Revoked device → next read 401, no agent restart | host-binary | `TestRevokedDeviceLosesAccessImmediately` |
| `agent.log` request lines carry no container names or refs | host-binary | `TestRequestLogCarriesNoObjectReferences` |

### Phase 4 — `logs-and-live-streaming-report.md:178-190`

| Outstanding item | Group | Where it lands |
|---|---|---|
| **30-minute endurance stream** (PRD Phase 4 success signal) | host-binary, endurance-gated | `TestStreamEnduranceThirtyMinutes` |
| Wi-Fi ↔ mobile-data handover — **agent half** | host-binary | `TestStreamSurvivesAbruptConnectionLoss` + `TestStreamResumeRepeatsAtMostOneLine` |
| Wi-Fi ↔ mobile-data handover — **client half** (app reconnects across a real network change) | — | Named as client-side; belongs to the Android app's suite |

### Phase 5 — `lifecycle-policy-and-audit.plan.md:1086-1126`

| Manual item | Group | Where it lands |
|---|---|---|
| **Agent refuses stop/kill/delete of itself by name, short ID, and full ID in `full` mode; still running afterwards** | in-container | `TestAgentRefusesToActOnItself` — the PRD's headline metric |
| `docker ps` after the delete attempt shows the agent up with no restart | in-container | same test |
| The agent's row carries `"protected": true`; every other row `false` | in-container | `TestAgentRowIsMarkedProtected` |
| `read-only` mode → all five lifecycle routes 403, reads still 200 | host-binary | `TestPolicyMatrix` |
| Unset `DEVMON_POLICY_MODE` → start/restart/stop work, kill/delete refused | host-binary | `TestPolicyMatrix` (the `default` row, with the variable absent) |
| `audit list` shows one row per attempt with the right device, operation, target, outcome — including refusals | host-binary | `TestAuditRowPerMutatingRequest` |
| Revoked device retry → 401 and **no** new audit row | host-binary | `TestRevokedDeviceWritesNoAuditRow` |
| Engine gone → 502, audit row `engine_error`, agent stays up | host-binary | `TestLifecycleAnswers502WhenEngineIsGone` |
| `--hostname something-else` → self-identification still resolves (mountinfo, not `$HOSTNAME`) | in-container | `TestSelfIDResolvesWithOverriddenHostname` |
| Malformed `DEVMON_SELF_CONTAINER_ID` → exit 2 naming the variable | host-binary | `TestConfigFaultsReportedTogether` |
| Valid-format but unresolvable override → lifecycle 503, reads and logs 200, ERROR in `agent.log` | in-container | `TestUnresolvableSelfIDFailsClosed` |
| The whole Phase 5 curl script (restart/stop/start/idempotent start/kill 403/delete 403; then `full`: 409, stop, delete 204) | host-binary | `TestLifecycleHappyPath` + `TestDeleteRunningContainerConflicts` |
| `agent.log` never contains a pairing code, key material, or PEM | both | `TestStateDirCarriesNoCredentialMaterial` |

### Named as client-side — the Android app's own suite owns these

| Item | Why it cannot live here |
|---|---|
| Reconnecting a live stream across a Wi-Fi ↔ mobile-data handover | Requires a radio and a carrier network. The agent-side contract it depends on (abrupt teardown, `?since=` resume) is tested here; what remains is entirely app behaviour. |
| Proactive silent renewal **timing** — renewing before expiry with no user interaction | The renewal window is a client-side policy. The agent's half (`POST /v1/device/renew` issues a usable certificate and the old one keeps working until its own `not_after`) is tested here. |
| A client holding a genuinely **expired** certificate diagnosing itself from `/v1/status` | Device certificates are valid for 90 days from a compile-time constant; producing an expired one means either waiting or changing production code, and D19 forbids the latter. The other half of the same diagnosis — a regenerated CA yields a different fingerprint — **is** tested here. |
| Pairing UX, QR scanning, battery drain, app-side disabling of controls the policy forbids | UI behaviour with no server-side observable. |

### Named as out of scope for this phase entirely

| PRD metric | Why |
|---|---|
| "Host disk consumed by agent logging at defaults" over a 24h high-activity run | The bounded version of this claim is tested (`TestLogRetentionBoundsDiskUse`); a 24-hour soak is an operational measurement, not a test. |
| "Agent resource footprint on idle — TBD" | The PRD itself has no target yet. Phase 7 measures it. |
| "Time from incident notification to corrective action" | A timed human dogfooding run. |
| Rate limiting behaviour | Phase 7 builds it. The suite grows a group then. |

---

## Mandatory Reading

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `.claude/PRPs/plans/completed/lifecycle-policy-and-audit.plan.md` | 1086-1126 | The curl script and 13-item checklist this phase automates; also the plan-format contract |
| P0 | `.claude/PRPs/reports/logs-and-live-streaming-report.md` | 128-200 | What was already verified on a real host in Phase 4, and the two items that were not — do not re-litigate the verified rows |
| P0 | `cmd/devmon-agent/main.go` | 52-104, 109-116 | Startup order, the `device` / `audit` dispatch the harness drives as a subprocess, exit-code mapping (2 = configuration fault) |
| P0 | `cmd/devmon-agent/cli.go` | 59-106, 184-205, 236-287 | The exact stdout format of `device pair-code` and `audit list`, which the harness parses |
| P0 | `internal/config/config.go` | 25-51, 235-263 | Every `DEVMON_*` variable the harness sets, the defaults it must not assume, and the `unix`/`tcp`-only scheme check that decides D6 |
| P0 | `internal/httpapi/server.go` | 93-144 | The full route table — the suite's checklist of what must be exercised |
| P0 | `internal/httpapi/pair.go` | 45-56, 108-158 | The exact `/v1/pair` request and response JSON the harness builds and parses |
| P0 | `internal/certs/issue.go` | 23-38 | The CSR must be **ECDSA P-256** or issuance fails; the CSR subject is ignored |
| P1 | `internal/httpapi/sse.go` | 94-127, 141-158 | The exact SSE frame bytes (`id:`, `event:`, `data:`, blank line; `: keepalive`) the stream reader must parse |
| P1 | `internal/httpapi/logs.go` | 50-79 | `?tail=` and `?since=` semantics — `since` is RFC3339Nano and is not defaulted on a parse failure |
| P1 | `internal/httpapi/status.go` | 20-30 | The five-field allowlist the status contract test asserts exactly |
| P1 | `internal/dockerx/types.go` | 20-102 | The JSON field names the read contract tests assert — **read them, then re-declare them in the suite (D4), do not import them** |
| P1 | `internal/dockerx/engine_test.go` | 1-101 | The repository's existing style for talking to an Engine from a test; the fake-transport pattern this phase does *not* use, and why |
| P1 | `internal/logging/logging.go` | 31-36, 88-96 | Log files are `0600` owned by the container user — the reason the in-container group reads them via `CopyFromContainer` |
| P2 | `compose.example.yaml` | all | The documented in-container layout the second group must reproduce (bind mount, `group_add`, published port) |
| P2 | `Dockerfile` | all | Build args the harness passes for the upgrade rehearsal |
| P2 | `Makefile` | 24-65 | Target style and the `CGO_ENABLED` handling the new targets mirror |
| P2 | `.github/workflows/ci.yml` | 28-60, 66-80 | Job structure and the `github.base_ref == 'main'` gate the `e2e` job copies |
| P2 | `README.md` | 440-520, 591-640 | The pairing flow the harness reproduces, and the Development section that gains the e2e commands |

## External Documentation

| Topic | Source | Key takeaway |
|---|---|---|
| moby client v29 | `go doc github.com/moby/moby/client` against the pinned `v0.5.1` | Constructor is `client.New(opts...)`; every method takes an options struct; `Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})`. Verified signatures below. |
| Port types moved | `go doc github.com/moby/moby/api/types/network.PortMap` | `PortMap = map[Port][]PortBinding` where `Port` is an **opaque type**, not a string, and `PortBinding.HostIP` is a **`netip.Addr`**, not a string. The `go-connections/nat` forms memory produces do not compile. |
| Build constraints and wildcards | Measured in this session on Go 1.26.4 | A fully tag-excluded package is silently skipped by `./...` patterns (exit 0) and errors on an explicit package path. |
| `gosec` and build tags | `gosec ./...` on the probe module | gosec skipped the tag-excluded file entirely and does not scan `_test.go` files by default, so the security gate is unaffected either way. |
| Ungraceful TCP close | `go doc net.TCPConn.SetLinger` | `SetLinger(0)` then `Close()` discards unsent data and sends RST — the abrupt-loss primitive D15 needs. |
| Reading files out of a container | `go doc github.com/moby/moby/client.CopyFromContainer` | Returns `CopyFromContainerResult{Content io.ReadCloser; Stat container.PathStat}` — a **tar stream**, produced daemon-side as root, which is how the host reads `0600` files owned by UID 65532. |

### Verified SDK signatures — transcribe exactly

Every signature below was read with `go doc` in this session against the versions in `go.mod`
(`github.com/moby/moby/client v0.5.1`, `github.com/moby/moby/api v1.55.0`, Go 1.26.4).

```go
// github.com/moby/moby/client
func New(ops ...Opt) (*Client, error)
func (cli *Client) Ping(ctx context.Context, options PingOptions) (PingResult, error)

func (cli *Client) ContainerCreate(ctx context.Context, options ContainerCreateOptions) (ContainerCreateResult, error)
func (cli *Client) ContainerStart(ctx context.Context, containerID string, options ContainerStartOptions) (ContainerStartResult, error)
func (cli *Client) ContainerInspect(ctx context.Context, containerID string, options ContainerInspectOptions) (ContainerInspectResult, error)
func (cli *Client) ContainerList(ctx context.Context, options ContainerListOptions) (ContainerListResult, error)
func (cli *Client) ContainerRemove(ctx context.Context, containerID string, options ContainerRemoveOptions) (ContainerRemoveResult, error)
func (cli *Client) ContainerWait(ctx context.Context, containerID string, options ContainerWaitOptions) ContainerWaitResult // NOTE: no error return
func (cli *Client) CopyFromContainer(ctx context.Context, containerID string, options CopyFromContainerOptions) (CopyFromContainerResult, error)
func (cli *Client) ImagePull(ctx context.Context, refStr string, options ImagePullOptions) (ImagePullResponse, error)
func (cli *Client) ExecCreate(ctx context.Context, containerID string, options ExecCreateOptions) (ExecCreateResult, error)
func (cli *Client) ExecAttach(ctx context.Context, execID string, options ExecAttachOptions) (ExecAttachResult, error)
func (cli *Client) ExecInspect(ctx context.Context, execID string, options ExecInspectOptions) (ExecInspectResult, error)

type ContainerCreateOptions struct {
    Config           *container.Config
    HostConfig       *container.HostConfig
    NetworkingConfig *network.NetworkingConfig
    Platform         *ocispec.Platform
    Name             string
    Image            string // shortcut for Config.Image; set only one of the two
}
type ContainerCreateResult  struct{ ID string; Warnings []string }
type ContainerInspectResult struct{ Container container.InspectResponse; Raw json.RawMessage }
type ContainerWaitResult    struct{ Result <-chan container.WaitResponse; Error <-chan error }
type ExecCreateResult       struct{ ID string }
type ExecAttachResult       struct{ HijackedResponse }          // HijackedResponse{Conn net.Conn; Reader *bufio.Reader}
type ExecInspectResult      struct{ ID, ContainerID string; Running bool; ExitCode, PID int }
type CopyFromContainerOptions struct{ SourcePath string }
type CopyFromContainerResult  struct{ Content io.ReadCloser; Stat container.PathStat }
type ExecCreateOptions struct {
    User string; Privileged bool; TTY bool; ConsoleSize ConsoleSize
    AttachStdin, AttachStderr, AttachStdout bool
    DetachKeys string; Env []string; WorkingDir string; Cmd []string
}
type Filters map[string]map[string]bool
func (f Filters) Add(term string, values ...string) Filters

// github.com/moby/moby/api/types/network
type PortMap = map[Port][]PortBinding
type PortSet = map[Port]struct{}
type PortBinding struct {
    HostIP   netip.Addr `json:"HostIp"`   // netip.Addr, NOT string
    HostPort string
}
func MustParsePort(s string) Port           // e.g. network.MustParsePort("8443/tcp")
func ParsePort(s string) (Port, error)

// github.com/moby/moby/api/types/container — fields the harness sets
type Config     struct{ Hostname string; Env []string; Cmd []string; Image string; Labels map[string]string; ExposedPorts network.PortSet; /* … */ }
type HostConfig struct{ Binds []string; PortBindings network.PortMap; GroupAdd []string; AutoRemove bool; /* … */ }
type NetworkSettings struct{ Ports network.PortMap; /* … */ }

// crypto/x509 and net — the client side
func CreateCertificateRequest(rand io.Reader, template *CertificateRequest, priv any) ([]byte, error)
func X509KeyPair(certPEMBlock, keyPEMBlock []byte) (Certificate, error)   // crypto/tls
func (c *TCPConn) SetLinger(sec int) error                                // net
// http.Transport.DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
// — with TLSClientConfig set, this hook yields the RAW TCP conn, which is what D15 needs.
```

Two shapes that memory gets wrong and the compiler catches late:

- `ContainerWait` returns **one value**, a struct of two channels — there is no `err` to check on
  the call itself.
- `network.PortBinding.HostIP` is a `netip.Addr`. `HostIP: "127.0.0.1"` does not compile;
  `netip.MustParseAddr("127.0.0.1")` does.

---

## Patterns to Mirror

### TEST_STRUCTURE
```go
// SOURCE: internal/dockerx/client_test.go:18-48
// t.Parallel at both levels, named table cases, explicit // Arrange // Act //
// Assert, failure messages in the form: got = X, want Y.
```
Applies unchanged to the e2e suite, with one exception recorded in Task 1: tests that start an agent
process may run `t.Parallel()`, because each owns its own port and state directory; tests that
sever the shared Engine proxy may not.

### ENGINE_TEST_STYLE
```go
// SOURCE: internal/dockerx/engine_test.go:1-15
// The package comment states WHAT the fake is, WHY it was chosen over the
// obvious alternative, and what it deliberately does NOT cover.
```
Every file in `internal/e2e` opens the same way: what it proves, which manual checklist item it
replaces, and what it deliberately leaves to the client's suite.

### SUBPROCESS_ENVIRONMENT
```go
// The agent's environment is BUILT, never inherited (D12). A developer with
// DEVMON_POLICY_MODE=full exported would otherwise silently invalidate every
// read-only assertion in this file.
cmd.Env = []string{
    "PATH=" + os.Getenv("PATH"),
    "HOME=" + os.Getenv("HOME"),
    "DEVMON_STATE_DIR=" + stateDir,
    // … only what the test case asked for …
}
```

### LABELLED_FIXTURE
```go
// Every fixture carries both labels. Cleanup filters on runLabel, so a
// concurrent run on the same host cannot delete another run's containers, and
// nothing unlabelled is ever touched (D11).
const labelSuite = "com.devmon.e2e"
labels := map[string]string{labelSuite: "1", labelSuite + ".run": runID}
```

### REDACTED_FAILURE_OUTPUT
```go
// SOURCE: the repository rule in CLAUDE.md — never log key material, pairing
// codes, or PEM bytes, at any level. That rule binds test output too: a failing
// assertion that dumps a pairing response would put a device private key and a
// live pairing code into CI logs that are retained and world-readable.
func redact(body []byte) string   // strips PEM blocks and anything code-shaped
```

### CONTRACT_ASSERTION
```go
// D4: decode into a map and assert the KEY SET, not into the agent's own DTO.
// A renamed json tag must fail here, which it cannot do if the suite and the
// agent share the struct.
var got map[string]any
mustDecode(t, resp, &got)
assertExactKeys(t, got, []string{"api_version", "agent_version", "policy_mode", "server_time", "ca_fingerprint"})
```

---

## Suite Layout

The file names are part of the deliverable: someone writing a web client should be able to open
`contract_lifecycle_test.go` and read what the agent promises about lifecycle, without knowing any
Go package in this repository.

```
internal/e2e/
  harness/                    # importable, //go:build e2e on every file
    doc.go                    # what the harness is, what it refuses to do (D11, D12, D13)
    engine.go                 # Engine probe, skip-or-require (D5), labelled fixtures, cleanup
    engine_gid_linux.go       # docker socket GID lookup            //go:build e2e && linux
    engine_gid_other.go       # stub returning ("", false)          //go:build e2e && !linux
    agent.go                  # build the binary, start/stop the process, readiness, log access
    cli.go                    # `device pair-code`, `device list`, `device revoke`, `audit list`
    client.go                 # CSR + pairing + pinned mTLS client, request helpers, redaction
    stream.go                 # SSE frame reader, raw-conn capture, abrupt close (D15)
    proxy.go                  # in-process TCP proxy in front of the Engine (D16)
    image.go                  # docker build, container run/exec/copy for the in-container group

  api/                        # host-binary group — the contract, route by route
    main_test.go              # TestMain: probe, build the binary once, run, clean up
    contract_status_test.go
    contract_startup_test.go
    contract_identity_test.go
    contract_reads_test.go
    contract_logs_test.go
    contract_lifecycle_test.go
    contract_policy_test.go
    contract_audit_test.go
    contract_endurance_test.go    # env-gated (D14)

  incontainer/                # image group — what only a containerised agent can prove
    main_test.go              # TestMain: probe, docker build once, run, clean up
    contract_selfexclusion_test.go
    contract_selfid_test.go
    contract_state_test.go
```

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `internal/e2e/harness/doc.go` | CREATE | Package contract: what it does, and the three things it refuses to do |
| `internal/e2e/harness/engine.go` | CREATE | Engine probe and D5's skip-or-require; labelled fixture containers and their cleanup |
| `internal/e2e/harness/engine_gid_linux.go` | CREATE | Docker socket GID via `syscall.Stat_t` — Linux only |
| `internal/e2e/harness/engine_gid_other.go` | CREATE | Stub so the package compiles under `go vet -tags e2e` on Windows |
| `internal/e2e/harness/agent.go` | CREATE | Build the binary once, start it with a curated environment, wait for readiness, stop it, read its log |
| `internal/e2e/harness/cli.go` | CREATE | The four host-side subcommands as subprocesses, with parsed output (D8, D9) |
| `internal/e2e/harness/client.go` | CREATE | CSR generation, pairing, CA pinning, the request helper, redaction |
| `internal/e2e/harness/stream.go` | CREATE | SSE reader and the abrupt-close primitive (D15) |
| `internal/e2e/harness/proxy.go` | CREATE | The severable Engine proxy (D16) |
| `internal/e2e/harness/image.go` | CREATE | `docker build`, container run/exec/copy for the in-container group (D22) |
| `internal/e2e/api/main_test.go` | CREATE | Group `TestMain` |
| `internal/e2e/api/contract_status_test.go` | CREATE | Status allowlist, policy advertisement, 405, unauthenticated leakage |
| `internal/e2e/api/contract_startup_test.go` | CREATE | Configuration faults, unreachable Engine, graceful shutdown, missing state |
| `internal/e2e/api/contract_identity_test.go` | CREATE | Pairing, revocation, renewal, restart stability, SAN change, CA pinning |
| `internal/e2e/api/contract_reads_test.go` | CREATE | All eight read routes, secret non-leakage, 502 and recovery |
| `internal/e2e/api/contract_logs_test.go` | CREATE | Historical logs, streaming, resume, abrupt loss, slot exhaustion |
| `internal/e2e/api/contract_lifecycle_test.go` | CREATE | The five routes, idempotency, conflict, the Phase 5 curl script |
| `internal/e2e/api/contract_policy_test.go` | CREATE | Three modes × every operation |
| `internal/e2e/api/contract_audit_test.go` | CREATE | One row per mutating request, including refusals; zero for reads and 401s |
| `internal/e2e/api/contract_endurance_test.go` | CREATE | 30-minute stream and the retention budget, env-gated (D14) |
| `internal/e2e/incontainer/main_test.go` | CREATE | Group `TestMain`, image build |
| `internal/e2e/incontainer/contract_selfexclusion_test.go` | CREATE | The PRD headline metric |
| `internal/e2e/incontainer/contract_selfid_test.go` | CREATE | Hostname override, unresolvable override, fail-closed 503 |
| `internal/e2e/incontainer/contract_state_test.go` | CREATE | Crash survival, image upgrade, bind-mount persistence |
| `Makefile` | UPDATE | `e2e`, `e2e-container`, `e2e-endurance`, `e2e-lint` targets |
| `.github/workflows/ci.yml` | UPDATE | The `e2e` job, gated on `main` (D21) |
| `README.md` | UPDATE | A "End-to-end suite" section under Development |
| `.claude/PRPs/prds/devmon-agent.prd.md` | UPDATE | Phase 4, 5, and 6 status rows |

## NOT Building

- **Any production Go change** — D19. The suite observes; it does not fix. A defect it finds is
  recorded in the report and, if fixed, fixed in a separate task and a separate commit.
- **An Android emulator, adb, or any app-driving path** — the app is a separate project (PRD line 35).
  There is no client here to drive, and building a fake one would test the fake.
- **`testcontainers-go`, `dockertest`, `testify`, `gomega`, or any other new module** — D1. `go.mod`
  is unchanged at the end of this phase, and that is an acceptance criterion.
- **Rate-limiting tests** — Phase 7 builds rate limiting. The suite grows a group then.
- **Load, soak, or benchmark work beyond the 30-minute stream and the retention budget** — the PRD's
  footprint metrics are still TBD; measuring them is Phase 7's job.
- **A test for the known Phase 4 observation that a client disconnect logs at ERROR** — it is
  recorded, deliberate, and unfixed. Asserting current behaviour would make the eventual fix look
  like a regression.
- **Asserting Engine-internal shapes** — the suite asserts what the agent returns, never what Docker
  returned to the agent. A change in Engine JSON that the agent correctly absorbs must not turn the
  suite red.
- **A second Docker host, Swarm, remote TLS Engine endpoints** — one local Engine.
- **Coverage measurement over the e2e suite** — the 80% floor stays measured over `./internal/...`
  untagged. E2E tests exercise a separate process; folding them into the same number would inflate it
  while measuring nothing new about unit-testable code.
- **Retiring or trimming any existing unit test** — the e2e suite is additive. Unit tests run in
  seconds and catch different failures.

---

## Step-by-Step Tasks

Each task is one `go-implementer` invocation. Every task ends with the same two obligations:

1. **Falsifiability** — this is the e2e analogue of writing the test first. Before a task is
   complete, each new assertion must be shown capable of failing: invert the expectation (or point
   the client at the wrong agent, or skip the setup step) and record that it goes red, then restore
   it. A green e2e test that has never been red is indistinguishable from a test that asserts nothing.
2. **Gate line** — `gofmt -l .` clean for the new files, `go vet -tags e2e ./...` clean, and the
   default gate (`go test ./internal/... -race`) still passing and unchanged in duration.

### Task 1: Harness foundation — Engine probe, fixtures, agent process

- **ACTION**: Create `internal/e2e/harness/doc.go`, `engine.go`, `engine_gid_linux.go`,
  `engine_gid_other.go`, `agent.go`.
- **IMPLEMENT**:
  ```go
  // Package harness runs the real devmon-agent against a real Docker Engine.
  //
  // Three things it deliberately refuses to do:
  //   1. It never touches a container it did not create. Every fixture carries
  //      the com.devmon.e2e label plus a per-run label, and cleanup filters on
  //      both. The suite runs on a developer's own Engine, next to their own
  //      containers (D11).
  //   2. It never passes the ambient environment to the agent. The child's env
  //      is built from the test case alone, so an exported DEVMON_POLICY_MODE
  //      cannot invalidate a read-only assertion (D12).
  //   3. It never prints a pairing code, a private key, or PEM bytes — not even
  //      in a failure message. CI logs are retained and readable (CLAUDE.md).
  package harness

  // RequireEngine skips the whole test — or fails it, when DEVMON_E2E_REQUIRE=1
  // — when no Docker Engine answers. A suite that quietly does nothing is worse
  // than no suite, because a green run is what flips a PRD row (D5).
  func RequireEngine(t *testing.T) *client.Client

  // Agent is one running devmon-agent process with its own state directory,
  // port, and configuration.
  type Agent struct {
      BaseURL  string // https://127.0.0.1:<port>
      StateDir string
      // …
  }

  type AgentOptions struct {
      PolicyMode   string // "" leaves DEVMON_POLICY_MODE unset — the default-mode case
      DockerHost   string // "" uses the suite's Engine endpoint
      PublicAddr   string // defaults to 127.0.0.1
      StateDir     string // "" allocates a fresh t.TempDir
      Env          map[string]string // extra DEVMON_* for this case only
      ExpectFailure bool  // the process is expected to exit non-zero; do not wait for readiness
  }

  func BuildBinary(t *testing.T) string                       // once per package, via sync.Once
  func StartAgent(t *testing.T, opts AgentOptions) *Agent     // registers t.Cleanup
  func (a *Agent) Stop(t *testing.T) (exitCode int)           // SIGTERM, then assert a clean exit
  func (a *Agent) Kill(t *testing.T)                          // SIGKILL, for crash-survival tests
  func (a *Agent) LogText(t *testing.T) string                // reads <StateDir>/logs/agent.log
  func (a *Agent) Wait(t *testing.T) (exitCode int, stderr string) // for ExpectFailure runs
  ```
  Fixture helpers: `StartFixture(t, engine, FixtureOptions) string` returning the container ID, with
  `t.Cleanup` removing it by ID; `FixtureOptions` covers image, name suffix, command, env, labels, a
  healthcheck, and whether to leave it stopped. The default image is a pinned `busybox` tag, pulled
  via `ImagePull` + `ImagePullResponse.Wait` when absent.
- **MIRROR**: ENGINE_TEST_STYLE, SUBPROCESS_ENVIRONMENT, LABELLED_FIXTURE.
- **IMPORTS**: `context`, `os`, `os/exec`, `path/filepath`, `sync`, `syscall`, `testing`, `time`,
  `github.com/moby/moby/client`, `github.com/moby/moby/api/types/container`,
  `github.com/moby/moby/api/types/network`.
- **GOTCHA**: `//go:build e2e` on **every** file including `doc.go`. A single untagged file in the
  package puts the harness into the default build and the "go.mod unchanged, default gate untouched"
  guarantee starts eroding at the next import.
- **GOTCHA**: The socket-GID lookup needs `syscall.Stat_t`, which **does not exist on Windows**. It
  lives in `engine_gid_linux.go` behind `//go:build e2e && linux`, with a stub in
  `engine_gid_other.go` returning `("", false)`. Without the split, `go vet -tags e2e ./...` fails to
  compile on the developer's own machine — the one place it will be run first.
- **GOTCHA**: Readiness is `GET /v1/status` over TLS with `InsecureSkipVerify`, polled until it
  answers 200 or a 15s deadline expires. Do not poll the port with a TCP dial: the listener is up
  before the state store and the CA are, and a test that starts pairing at that moment fails
  intermittently in a way that will be blamed on the agent.
- **GOTCHA**: Allocate the port by listening on `127.0.0.1:0`, reading the port, and closing —
  then pass it as `DEVMON_LISTEN_ADDR`. The window between close and bind is a real race; retry the
  whole allocation once on a bind failure rather than pretending it cannot happen.
- **GOTCHA**: Build the agent with `CGO_ENABLED=0` (D17) so the binary under test matches the shipped
  one, even though the test binary itself runs with `-race` and therefore cgo.
- **GOTCHA**: `BuildBinary` must resolve the module root by walking up from the test's working
  directory to the `go.mod`, not by hardcoding `../../..`. Two packages at different depths call it.
- **GOTCHA**: `DEVMON_E2E_KEEP=1` skips fixture and state cleanup so a failure can be inspected. It
  must print the paths it kept, and it must never be the default.
- **VALIDATE**:
  ```bash
  go vet -tags e2e ./...
  go test -tags e2e ./internal/e2e/harness/... -count=1     # compiles; no tests yet
  go test ./internal/... -race                              # unchanged
  ```
- **ACCEPTANCE**: With Docker running, a throwaway test can start an agent, see `/v1/status` answer
  200, stop it with exit code 0, and leave no container and no temp directory behind. With Docker
  stopped, the same test skips with a message naming the endpoint; with `DEVMON_E2E_REQUIRE=1` set,
  it fails instead.

### Task 2: The pinned mTLS contract client and the host-side CLI

- **ACTION**: Create `internal/e2e/harness/client.go` and `internal/e2e/harness/cli.go`.
- **IMPLEMENT**:
  ```go
  // PairDevice performs the full documented pairing sequence: mint a code with
  // the host-side CLI while the agent is running (D8), generate an ECDSA P-256
  // key and CSR, POST /v1/pair over a deliberately unverified TLS connection —
  // exactly the README's `curl -k` step — then pin the returned CA for every
  // subsequent call (D7).
  //
  // The returned Device never exposes its private key as a string, and no field
  // of it is safe to print. Use Device.String(), which redacts.
  func PairDevice(t *testing.T, a *Agent, name string) *Device

  type Device struct {
      ID            string
      CAFingerprint string // SHA-256 of the pinned CA, for comparison across restarts
      // unexported: key, certificate, http.Client
  }

  // Do issues an mTLS request and returns the status, headers, and raw body.
  // It never follows a redirect and never retries: this suite asserts the exact
  // response the agent produced.
  func (d *Device) Do(t *testing.T, method, path string, body any) (status int, hdr http.Header, raw []byte)

  // JSON decodes into a map so assertions are written against the WIRE, not
  // against the agent's own structs (D4).
  func (d *Device) JSON(t *testing.T, method, path string) (status int, obj map[string]any)

  // AssertExactKeys fails when obj's key set differs from want in either
  // direction. A missing key is a broken client; an extra key is an unreviewed
  // disclosure.
  func AssertExactKeys(t *testing.T, obj map[string]any, want []string)
  ```
  `cli.go` wraps the four host-side commands:
  ```go
  func MintPairingCode(t *testing.T, a *Agent, name string) string   // parses "Pairing code: X"
  func ListDevices(t *testing.T, a *Agent) []DeviceRow
  func RevokeDevice(t *testing.T, a *Agent, id string)
  func ListAudit(t *testing.T, a *Agent, limit int) []AuditRow       // parses the tabwriter table
  ```
- **MIRROR**: REDACTED_FAILURE_OUTPUT, CONTRACT_ASSERTION.
- **IMPORTS**: `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/sha256`, `crypto/tls`,
  `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `encoding/json`, `net`, `net/http`, `os/exec`.
- **GOTCHA**: The CSR key must be **ECDSA P-256**. `internal/certs/issue.go:32-38` rejects anything
  else, and the rejection surfaces as a flat 401 `pairing failed` with no hint — which will read as a
  broken pairing flow rather than a wrong key type.
- **GOTCHA**: The CSR's subject is ignored by design (`issue.go:15-21`). Set it to something
  obviously wrong (`CN=admin`) in at least one test and assert the issued certificate's CN is the
  device ID instead. That is a security property the suite gets almost for free.
- **GOTCHA**: `InsecureSkipVerify` is permitted **only** inside `PairDevice`'s bootstrap request and
  inside `Agent` readiness polling. Anywhere else it silently removes the property the phase exists
  to check. State this in a comment at both sites, and add a test that a client pinning a *different*
  CA fails the handshake.
- **GOTCHA**: Parse `device pair-code` output by prefix (`Pairing code: `), and **never** log the
  parsed value, not even on a parse failure. On failure, report the number of lines seen — not their
  content.
- **GOTCHA**: The pairing code must be minted while the agent is running. Doing it before startup
  would pass, but would stop testing the shared-state-store requirement that D8 exists for.
- **GOTCHA**: `ListAudit` parses a `tabwriter` table whose columns are space-padded. Split on runs of
  two-or-more spaces, not on single spaces — `DETAIL` can be empty and `DEVICE` contains
  `id (name)` with a single space inside it.
- **GOTCHA**: Give each `Device`'s `http.Client` an explicit `Timeout` — except the streaming client,
  which must have none, or the 30-minute endurance run dies at the default.
- **VALIDATE**:
  ```bash
  go test -tags e2e ./internal/e2e/api/... -run TestPairingSmoke -count=1 -v
  ```
- **ACCEPTANCE**: A device pairs against a freshly started agent, the returned CA fingerprint equals
  the `ca_fingerprint` from `/v1/status`, an authenticated `GET /v1/containers` answers 200, and no
  pairing code, key, or PEM block appears anywhere in `-v` output.

### Task 3: Status and startup contract (Phase 1's checklist)

- **ACTION**: Create `internal/e2e/api/main_test.go`, `contract_status_test.go`,
  `contract_startup_test.go`.
- **IMPLEMENT**: `TestMain` probes the Engine once, builds the binary once, runs, then sweeps any
  leftover labelled fixture. Then:
  - `TestStatusAllowlist` — `/v1/status` without a client certificate returns exactly
    `api_version`, `agent_version`, `policy_mode`, `server_time`, `ca_fingerprint`, and nothing else.
  - `TestStatusAdvertisesPolicyMode` — three agents, three modes, three answers; and the unset case
    reports `default`.
  - `TestStatusRejectsOtherMethods` — `POST /v1/status` → 405.
  - `TestUnauthenticatedRequestsLeakNothing` — `/v1/containers` without a certificate; assert the
    status is 401 or 404 and the body contains no path, hostname, version, or Go type name.
  - `TestConfigFaultsReportedTogether` — start with `DEVMON_POLICY_MODE=bogus` **and**
    `DEVMON_LOG_MAX_AGE_DAYS=x` **and** a malformed `DEVMON_SELF_CONTAINER_ID`; assert exit code 2
    and all three variable names in stderr. Plus the missing-`DEVMON_PUBLIC_ADDR` case on its own.
  - `TestStartupFailsWhenEngineUnreachable` — `DEVMON_DOCKER_HOST=unix:///nonexistent/docker.sock`;
    assert a non-zero exit naming the ping failure, and no panic text in stderr.
  - `TestGracefulShutdown` — SIGTERM to a healthy agent; exits 0 within 5s (not the 10s SIGKILL
    path), and the log's final line is the shutdown message.
  - `TestMissingCertsDirIsLoudNotSilent` — pair a device, stop the agent, delete `certs/`, restart;
    assert a non-zero exit with an explicit identity error, and that the CA is **not** silently
    regenerated.
  - `TestStateDirCarriesNoCredentialMaterial` — after a full pair-and-drive cycle, walk the whole
    state directory and assert no file except `certs/` contains `PRIVATE KEY`, `BEGIN CERTIFICATE`,
    or the pairing code that was used.
- **MIRROR**: TEST_STRUCTURE, CONTRACT_ASSERTION.
- **GOTCHA**: Exit code 2 is the configuration-fault contract (`main.go:27-31`). Assert the **code**,
  not just the message: an operator's installer script branches on it.
- **GOTCHA**: The credential sweep must exclude `certs/`, which legitimately holds PEM — the claim is
  that key material does not leak *out of* there, into logs or the database.
- **GOTCHA**: `syscall.SIGTERM` compiles on Windows but cannot be delivered. The graceful-shutdown
  test is skipped by the same D6 platform guard as everything else; do not special-case it.
- **GOTCHA**: `TestMissingCertsDirIsLoudNotSilent` deletes a directory built by the agent's own UID —
  fine in the host-binary group where the agent runs as the test user, and the reason this item is
  here and not in the in-container group.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run 'TestStatus|TestConfig|TestStartup|TestGraceful|TestMissing|TestUnauth|TestStateDir' -count=1 -v`
- **ACCEPTANCE**: Every Phase 1 manual row in the Coverage Map except the retention budget passes
  unattended.

### Task 4: Identity contract (Phase 2's checklist)

- **ACTION**: Create `internal/e2e/api/contract_identity_test.go`.
- **IMPLEMENT**:
  - `TestTwoDevicesPairIndependently` — two codes, two devices, two distinct certificate serials and
    two distinct device IDs; both can call the API; `device list` shows both.
  - `TestPairingsSurviveRestart` — stop and restart the agent on the same state directory; both
    devices still authenticate, and the CA fingerprint is unchanged.
  - `TestIdentityStableAcrossRestart` — the server certificate's serial, read from the TLS peer
    certificate, is identical before and after a restart.
  - `TestServerCertReissuedOnAddressChange` — restart with an extra `DEVMON_PUBLIC_ADDR` entry;
    assert the leaf's SANs changed, the **CA fingerprint did not**, and the paired device still works
    without re-pairing.
  - `TestRevokedDeviceLosesAccessImmediately` — revoke from the host CLI while the agent runs; the
    next request is 401 with no restart in between. Assert the second device is unaffected.
  - `TestUnpairSelfWorksUnderEveryMode` — `DELETE /v1/device/self` returns 204 under `read-only`,
    `default`, and `full`, and the device is 401 afterwards.
  - `TestRenewIssuesUsableCertAndKeepsOldValid` — `POST /v1/device/renew` with a fresh CSR; the new
    certificate authenticates, **and the old one still does** until its own expiry.
  - `TestPairingCodeIsSingleUse` — redeeming the same code twice yields 401 the second time.
  - `TestUnknownAndMalformedPairingCodesAreIndistinguishable` — an unknown code and a malformed CSR
    both yield 401 with the identical body.
  - `TestCSRSubjectIsIgnored` — a CSR asking for `CN=admin` yields a certificate whose CN is the
    device ID.
  - `TestWrongCAIsRejected` — a client pinning a different CA fails the handshake (D7's proof).
  - `TestWipedStateChangesCAFingerprint` — a fresh state directory produces a different
    `ca_fingerprint`, which is the signal a client uses to tell an attack from an expiry.
- **GOTCHA**: Each test owns its own agent and state directory, so they may run `t.Parallel()`. Two
  tests sharing one agent would make revocation order-dependent — the exact bug class this suite is
  meant to catch, not to contain.
- **GOTCHA**: Read the server certificate serial from `tls.ConnectionState.PeerCertificates[0]` via
  the client's `TLSClientConfig.VerifyConnection` hook or a captured connection state — not by
  reading `certs/server.crt` off disk. The claim is about what the client sees.
- **GOTCHA**: `TestRenewIssuesUsableCertAndKeepsOldValid` is the automatable half of "renews with no
  user interaction". Say so in the test's doc comment and name the client-side half, so a future
  reader does not think the manual item is fully discharged.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run TestPair -run 'TestTwoDevices|TestPairing|TestIdentity|TestServerCert|TestRevoked|TestUnpair|TestRenew|TestCSR|TestWrong|TestWiped' -count=1 -v`
- **ACCEPTANCE**: Every Phase 2 manual row in the Coverage Map passes, except the two named as
  client-side.

### Task 5: Read contract and the Engine-unavailable path (Phase 3's checklist)

- **ACTION**: Create `internal/e2e/harness/proxy.go` and `internal/e2e/api/contract_reads_test.go`.
- **IMPLEMENT**: `proxy.go` is a plain `net.Listener` that accepts a TCP connection and copies both
  ways to the real Engine endpoint, with `Sever()` closing the listener and every live connection,
  and `Restore()` reopening on the same port.

  Then the read contract: all eight routes answer 200 with the documented key sets; a container
  started with `-e DB_PASSWORD=hunter2` never yields that string; an image with `ENV API_KEY=` likewise;
  a volume created with driver options never yields `options`; `?all=true` behaviour; `health` on a
  container with a failing healthcheck; a network and volume created mid-test appear immediately;
  invalid references yield 400 and unknown ones 404; and with the proxy severed every read yields
  **502** while the agent stays up, then recovers after `Restore()` with no agent restart.
- **GOTCHA**: The proxy must be listening **before** the agent starts — `dockerx.New` pings at
  startup and a dead endpoint is a fatal startup error, not a 502.
- **GOTCHA**: The tests that sever the proxy must **not** be `t.Parallel()` with anything sharing that
  agent. Give the 502 test its own agent and its own proxy; that is cheaper than reasoning about it.
- **GOTCHA**: Assert `truncated` is present and `false` on every list route. It is part of the
  envelope contract and a client that ignores it will silently show partial data at 500+ objects.
- **GOTCHA**: The key-set assertions must account for `omitempty` fields (`health`, `ip`,
  `public_port`, and others in `internal/dockerx/types.go`). Assert **required** keys exactly and
  optional keys as a permitted superset — and write down which is which in the test, because that
  distinction *is* the contract a client needs.
- **GOTCHA**: The healthcheck fixture needs a few seconds before Docker reports `unhealthy`. Poll
  with a deadline; do not sleep a fixed duration.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run TestRead -count=1 -v` and the named
  read tests.
- **ACCEPTANCE**: Every Phase 3 manual row passes, and the 502 test demonstrably fails when the
  proxy is left intact (falsifiability).

### Task 6: Logs, streaming, resume, and abrupt loss (Phase 4's outstanding half)

- **ACTION**: Create `internal/e2e/harness/stream.go` and `internal/e2e/api/contract_logs_test.go`.
- **IMPLEMENT**: `stream.go` parses SSE frames into `{id, event, data}` records, exposes a channel of
  them, captures the raw `*net.TCPConn` through a custom `DialContext`, and offers
  `AbruptClose()` = `SetLinger(0)` + `Close()` (D15).

  Then:
  - `TestHistoricalLogsBounded` — `?tail=20` returns 20 items, `truncated:false`, and each line
    carries exactly the documented keys.
  - `TestHistoricalLogsInvalidSince` — `?since=nonsense` → 400; `?since=` in the future → 200 with an
    empty `items`.
  - `TestStreamDeliversLinesLive` — lines written after the stream opens arrive within a deadline.
  - `TestStreamKeepaliveOnSilentContainer` — a silent container's stream stays open past the
    server's 30s `WriteTimeout` and receives keepalive frames.
  - `TestStreamSurvivesAbruptConnectionLoss` — abruptly kill the TCP connection mid-stream; assert
    the agent stays up, the stream slot is released (a new stream still succeeds), and no goroutine
    leak is visible as slot exhaustion after ten repetitions.
  - `TestStreamResumeRepeatsAtMostOneLine` — reconnect with `?since=<last id>`; assert the boundary
    line repeats **at most once** and nothing is missing. This is the agent-side half of the
    handover item.
  - `TestStreamSlotExhaustion` — the ninth concurrent stream gets 503 `too many concurrent log
    streams`, and a slot frees when one closes.
  - `TestStreamAgainstUnknownContainer` — 404 before any SSE header is committed.
- **GOTCHA**: The streaming `http.Client` must have **no** `Timeout`, and its `Transport` must not
  set `ResponseHeaderTimeout` below the keepalive interval.
- **GOTCHA**: With `TLSClientConfig` set, `Transport.DialContext` yields the **raw TCP** connection
  before the TLS handshake — verified. That is exactly the handle needed; do not reach for
  `DialTLSContext`, which would hand back a `*tls.Conn` with no `SetLinger`.
- **GOTCHA**: The SSE `id:` is the line's RFC3339Nano timestamp (`logs.go:176-178`), which is what
  goes back as `?since=`. Do not invent a cursor format.
- **GOTCHA**: "At most one repeated line" is the documented at-least-once contract, not
  exactly-once. Asserting zero repeats would fail against correct behaviour.
- **GOTCHA**: The known Phase 4 observation — an abandoned stream logs at ERROR — will appear in
  `agent.log` during these tests. Do not assert on its absence and do not fix it here (NOT Building).
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run TestStream -count=1 -v -timeout 10m`
- **ACCEPTANCE**: The abrupt-loss and resume tests pass, and `TestStreamResumeRepeatsAtMostOneLine`
  is shown to fail when the resume cursor is deliberately omitted.

### Task 7: Lifecycle and policy contract (Phase 5's curl script)

- **ACTION**: Create `internal/e2e/api/contract_lifecycle_test.go` and `contract_policy_test.go`.
- **IMPLEMENT**:
  - `TestLifecycleHappyPath` — against a `default`-mode agent and a throwaway fixture: restart 204,
    stop 204, start 204, start again 204 (idempotent), kill 403, delete 403 — the exact sequence from
    `lifecycle-policy-and-audit.plan.md:1091-1097`.
  - `TestDeleteRunningContainerConflicts` — in `full` mode: delete a running container → 409, the
    container is untouched; stop → 204; delete → 204; it is gone from `?all=true`.
  - `TestKillIsPermittedOnlyInFullMode` — 403 in `default`, 204 in `full`.
  - `TestPolicyMatrix` — three modes × the seven operations, asserting the exact status per cell, and
    that reads and logs are 200 in every mode.
  - `TestLifecycleRejectsInvalidAndUnknownRefs` — 400 and 404 respectively, in the mode that permits
    the operation.
  - `TestLifecycleAnswers502WhenEngineIsGone` — with the proxy severed, a restart yields 502 and the
    agent stays up.
  - `TestLifecycleRejectsWrongMethod` — `GET /v1/containers/x/start` → 405.
- **GOTCHA**: The `default`-mode agent must be started with `DEVMON_POLICY_MODE` **absent**, not set
  to `"default"`. The PRD metric is about the operator who configures nothing; setting the variable
  tests a different claim. Cover the explicit value separately.
- **GOTCHA**: Every fixture in these tests is created by the suite and labelled (D11). No test may
  target a container by a name the developer might also have.
- **GOTCHA**: The 409 case needs a container that is genuinely running, and `stop` returns before the
  process has finished unwinding. Poll the container's state rather than sleeping.
- **GOTCHA**: 204 means no body. Assert `Content-Length: 0` (or an empty body) as part of the
  contract — a client that tries to parse JSON from a 204 is a bug waiting for the first
  implementer who assumes every response has a body.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run 'TestLifecycle|TestDelete|TestKill|TestPolicy' -count=1 -v`
- **ACCEPTANCE**: The whole Phase 5 curl script is reproduced, cell for cell, with no human in the
  loop.

### Task 8: Audit contract

- **ACTION**: Create `internal/e2e/api/contract_audit_test.go`.
- **IMPLEMENT**:
  - `TestAuditRowPerMutatingRequest` — after a scripted sequence (restart success, kill refused by
    policy, delete of an unknown container), `audit list` holds **exactly** three rows, in order,
    with the right device ID and name, operation, target as the device supplied it, and outcomes
    `success` / `denied_policy` / `not_found`.
  - `TestAuditRecordsRefusals` — a refusal is present, which is the PRD's explicit requirement.
  - `TestReadsWriteNoAuditRows` — drive all eight read routes and both log routes; the row count is
    unchanged.
  - `TestRevokedDeviceWritesNoAuditRow` — a 401 leaves the count unchanged.
  - `TestAuditDetailCarriesNoEngineText` — no row's `DETAIL` contains a socket path, a state path, or
    an Engine message.
  - `TestAuditSurvivesAgentRestart` — rows written before a restart are still listed after it.
  - `TestAuditIsNotReachableOverTheAPI` — `GET /v1/audit` and a few plausible variants are 404. D20
    is a security boundary, and a suite that never checks it would not notice a future route.
- **GOTCHA**: `audit list` joins the device name from the live device table. After a revocation the
  row remains but the name may be gone — assert the bare ID is printed, not that the row disappeared.
- **GOTCHA**: Row identity is by order and content, not by database ID: `ListAudit` orders by `id
  DESC`, so the newest row is first. Do not assume ascending.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/api/... -run TestAudit -count=1 -v`
- **ACCEPTANCE**: One row per mutating request including refusals; zero for reads and unauthenticated
  calls; all observed through the documented host-side CLI (D9).

### Task 9: In-container harness — build, run, exec, copy

- **ACTION**: Create `internal/e2e/harness/image.go` and `internal/e2e/incontainer/main_test.go`.
- **IMPLEMENT**:
  ```go
  // BuildImage runs the documented `docker build` once per package (D22).
  // Assembling a tar build context in Go to reach ImageBuild would be real work
  // with no payoff, and shelling out exercises the same command the Makefile and
  // CI use. A missing docker CLI is a SKIP, not a failure.
  func BuildImage(t *testing.T, tag string, buildArgs map[string]string)

  // RunAgentContainer starts the agent the way compose.example.yaml documents:
  // a bind-mounted state dir, the docker socket read-only, group_add for the
  // socket's GID, and one published port.
  func RunAgentContainer(t *testing.T, e *client.Client, opts ContainerAgentOptions) *ContainerAgent

  func (c *ContainerAgent) Exec(t *testing.T, cmd ...string) (stdout string, exitCode int)
  func (c *ContainerAgent) ReadStateFile(t *testing.T, path string) []byte // via CopyFromContainer
  func (c *ContainerAgent) Restart(t *testing.T)
  func (c *ContainerAgent) KillContainer(t *testing.T)
  func (c *ContainerAgent) IsRunning(t *testing.T) (running bool, restartCount int)
  ```
  Pairing in this group reuses `harness.PairDevice`, with `MintPairingCode` routed through `Exec`
  instead of a local subprocess.
- **GOTCHA**: `network.PortBinding.HostIP` is a `netip.Addr`
  (`netip.MustParseAddr("127.0.0.1")`), and the map key is an opaque
  `network.MustParsePort("8443/tcp")`. Both differ from the `go-connections/nat` forms memory
  produces, and both are compile errors rather than silent failures — which is the good outcome.
- **GOTCHA**: Publish with host port `"0"` and read the assigned port back from
  `ContainerInspect(...).Container.NetworkSettings.Ports`. Picking a port in the test races with
  everything else on the developer's machine.
- **GOTCHA**: The state directory is a host temp dir that must be `os.Chmod(dir, 0o777)` before the
  container starts. The image runs as UID 65532 and `main.go`'s `prepareStateDir` fails with
  "permission denied" otherwise. This is a **test-only** widening of a directory that is deleted at
  cleanup; say so in a comment, because it looks exactly like the mistake the README warns operators
  against.
- **GOTCHA**: `GroupAdd` needs the numeric GID of the host's docker socket — the image is
  `distroless/static` and has no `docker` group to name. Task 1's build-tagged lookup supplies it.
- **GOTCHA**: `Exec` uses `TTY: true` so the output stream is **not** Docker-multiplexed and needs no
  frame demultiplexing. With `TTY: false` the reader must strip 8-byte headers, and the pairing code
  would come back with binary noise in it.
- **GOTCHA**: Read files out of the container with `CopyFromContainer` and an `archive/tar` reader.
  Log files are `0600` owned by UID 65532 (`internal/logging/logging.go:33`); the host test user
  usually cannot read them off the bind mount, and a test that works only for root is a test that
  fails in CI.
- **GOTCHA**: Readiness here is the same `/v1/status` poll, against `https://127.0.0.1:<published>`.
- **GOTCHA**: Skip the whole package when `docker` is not on `PATH`, when the Engine is not Linux, or
  when `runtime.GOOS == "windows"` (D6), each with its own reason string.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/incontainer/... -run TestContainerSmoke -count=1 -v`
- **ACCEPTANCE**: The image builds, the agent runs as a container the way `compose.example.yaml`
  documents, a device pairs through `docker exec`, and `GET /v1/containers` answers 200 through the
  published port.

### Task 10: The self-exclusion guarantee — the PRD's headline metric

- **ACTION**: Create `internal/e2e/incontainer/contract_selfexclusion_test.go`.
- **IMPLEMENT**:
  - `TestAgentRefusesToActOnItself` — a **`full`-mode** agent; for each of the five lifecycle routes
    and each of three reference forms (container name, 12-char short ID, 64-char full ID): expect
    **403** with the self-protection body. Fifteen cells, all of them the same answer.
  - After the fifteen attempts: the agent container is still running, its restart count is still 0,
    and the API still answers 200. That last part is the PRD metric — "Agent surviving a delete
    attempt in the most permissive mode: 100%".
  - `TestAgentRowIsMarkedProtected` — `GET /v1/containers` shows the agent's own row with
    `"protected": true` and every fixture with `"protected": false`, present in the JSON in both
    cases (no `omitempty`).
  - `TestSelfExclusionIsNotAPolicyTier` — the same refusals occur in `full` mode, which is the point:
    no configuration opts into it.
  - `TestAuditRecordsSelfRefusals` — each refusal leaves one row with outcome `denied_self`.
- **GOTCHA**: Take the short ID as the first 12 characters of the container's full ID from
  `ContainerInspect` — not from `docker ps` output parsing, and not from `$HOSTNAME`.
- **GOTCHA**: Assert the agent is still running by asking the **Engine**, not by asking the agent.
  An agent that answered 403 and then died would pass the naive version of this test.
- **GOTCHA**: This test must be shown red at least once. The most convincing falsification is to run
  the same fifteen cells against a *fixture* container instead of the agent and confirm they are 204
  — proving the 403s come from the self-exclusion rule and not from a broken client.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/incontainer/... -run 'TestAgent|TestSelfExclusion|TestAuditRecordsSelf' -count=1 -v`
- **ACCEPTANCE**: Fifteen refusals, an agent still serving, and `protected: true` on exactly one row.
  This is the single most important task in the phase.

### Task 11: Self-identification variants and state survival

- **ACTION**: Create `internal/e2e/incontainer/contract_selfid_test.go` and `contract_state_test.go`.
- **IMPLEMENT**:
  - `TestSelfIDResolvesWithOverriddenHostname` — run with `Config.Hostname` set to something that is
    not the container ID; self-exclusion still fires, proving the `mountinfo` path rather than
    `$HOSTNAME`. Assert the resolved ID appears in the startup log.
  - `TestUnresolvableSelfIDFailsClosed` — `DEVMON_SELF_CONTAINER_ID` set to a well-formed 64-hex ID
    that does not exist; all five lifecycle routes answer **503**, reads and logs answer 200, and
    `agent.log` carries an ERROR naming `DEVMON_SELF_CONTAINER_ID`.
  - `TestExplicitSelfIDOverrideIsHonoured` — the override set to the agent's real ID resolves
    immediately, and self-exclusion behaves identically to the auto-detected case.
  - `TestStateSurvivesCrashRestart` — pair a device, drive traffic, `docker kill` the container,
    start it again; the pre-kill log lines are still present (read via `CopyFromContainer`), new
    lines append below them, and the device does **not** re-pair.
  - `TestPairingsSurviveImageUpgrade` — rebuild the image with a different `VERSION` build arg,
    recreate the container against the **same bind mount**, and assert both paired devices still
    authenticate, the CA fingerprint is unchanged, and `/v1/status` reports the new version. This is
    the PRD's "Pairings surviving a restart and an image upgrade: 100%".
  - `TestStateBindMountIsTheOnlyDurableState` — remove the container without the bind mount and start
    a fresh one on a fresh directory; the CA fingerprint differs, which is the loud signal the design
    depends on.
- **GOTCHA**: `docker kill` sends SIGKILL. That is the point — the crash-survival claim is about an
  agent that had no chance to flush. Do not soften it to `docker stop`.
- **GOTCHA**: The upgrade rehearsal must **recreate** the container, not restart it. A restart shares
  the container's writable layer and would prove nothing about the mount.
- **GOTCHA**: The unresolvable-override case is the one where the agent starts *successfully* and
  degrades. Assert the process is up and reads work — an agent that refused to start would also
  produce 503s to a careless test.
- **VALIDATE**: `go test -tags e2e ./internal/e2e/incontainer/... -run 'TestSelfID|TestUnresolvable|TestExplicit|TestState|TestPairingsSurvive' -count=1 -v -timeout 15m`
- **ACCEPTANCE**: Every remaining in-container row of the Coverage Map passes.

### Task 12: The long-running group — endurance and retention

- **ACTION**: Create `internal/e2e/api/contract_endurance_test.go`.
- **IMPLEMENT**:
  ```go
  // These two tests are compiled on every e2e run and skipped unless
  // DEVMON_E2E_ENDURANCE=1 (D14). A separate build tag would keep them out of
  // every ordinary compile, and code that compiles only in a target nobody runs
  // weekly is code that rots. `make e2e-endurance` sets the variable and the
  // longer -timeout.
  ```
  - `TestStreamEnduranceThirtyMinutes` — a fixture logging one line per second; a single stream held
    for 30 minutes; assert every line arrives in order, no gaps, no reconnect, the connection is
    still open at the end, and the agent's restart count is still 0. This is the PRD's Phase 4
    success signal.
  - `TestLogRetentionBoundsDiskUse` — an agent with `DEVMON_LOG_MAX_TOTAL_MB=8` and
    `DEVMON_LOG_LEVEL=debug`; drive traffic until rotation occurs; assert the logs directory stays at
    or below roughly its budget and that `.gz` backups exist. This is the PRD's Phase 1 retention
    success signal.
- **GOTCHA**: Track the expected line sequence by content (a counter printed by the fixture), not by
  timing. A 30-minute assertion built on wall-clock arithmetic will be flaky forever.
- **GOTCHA**: The skip message must name `DEVMON_E2E_ENDURANCE` and `make e2e-endurance`. A skip
  nobody knows how to un-skip is a deleted test with extra steps.
- **GOTCHA**: "Roughly its budget" is deliberate: `internal/logging` divides the total across
  `backups+1` files, so the on-disk total lands near, not exactly at, the number. Assert a bound with
  headroom and state why in the comment — an exact-equality assertion here would fail on a boundary
  that is correct.
- **VALIDATE**:
  ```bash
  DEVMON_E2E_ENDURANCE=1 go test -tags e2e ./internal/e2e/api/... -run TestStreamEndurance -count=1 -v -timeout 45m
  ```
- **ACCEPTANCE**: The 30-minute run completes green once, and the PRD's Phase 4 row can finally move
  off `awaiting device validation`.

### Task 13: Make targets, CI job, docs, and the verification sweep

- **ACTION**: Update `Makefile`, `.github/workflows/ci.yml`, `README.md`, and the PRD phase table.
- **IMPLEMENT**:
  ```makefile
  # The e2e suite needs a real Docker Engine and is excluded from every default
  # target by the `e2e` build tag. -count=1 defeats the test cache: results
  # depend on state outside the module, so a cached PASS is a false green (D18).
  E2E_PKGS := ./internal/e2e/...

  e2e:
  	CGO_ENABLED=1 go test -tags e2e $(E2E_PKGS) -race -count=1 -timeout 15m

  e2e-container:
  	CGO_ENABLED=1 go test -tags e2e ./internal/e2e/incontainer/... -race -count=1 -timeout 15m

  e2e-endurance:
  	DEVMON_E2E_ENDURANCE=1 CGO_ENABLED=1 go test -tags e2e ./internal/e2e/api/... -race -count=1 -timeout 45m

  e2e-lint:
  	go vet -tags e2e ./...
  	@command -v golangci-lint >/dev/null 2>&1 \
  		&& golangci-lint run --build-tags e2e $(E2E_PKGS) \
  		|| echo "golangci-lint not installed — go vet -tags e2e was the only lint run"
  ```
  CI gains an `e2e` job with `if: github.base_ref == 'main'`, `env: DEVMON_E2E_REQUIRE: 1`, running
  `make e2e`. README gains an "End-to-end suite" subsection under Development: what it is, the two
  groups, the four commands, the `DEVMON_E2E_*` knobs with a sentence stating they are harness knobs
  and **not** agent configuration, and the WSL2 note for Windows. The PRD's Phase 4 and Phase 5 rows
  move to `complete` once the suite is green, and Phase 6's row gains its plan link.
- **GOTCHA**: `make lint`'s `gofmt -l .` already covers the e2e files — `gofmt` ignores build tags.
  `golangci-lint` and `go vet` do not, which is why `e2e-lint` exists. Do not add `--build-tags e2e`
  to the default `lint` target: it would pull the e2e packages into every ordinary lint run and slow
  the fast path for no benefit on a `dev` PR.
- **GOTCHA**: `gosec ./...` is unaffected — verified in this session that it skips tag-excluded files
  and does not scan `_test.go` by default. State this in the report rather than leaving a reviewer to
  wonder whether the security gate silently lost coverage.
- **GOTCHA**: The coverage floor stays measured over `./internal/...` **untagged**. Do not add
  `-tags e2e` to `make cover`: it would add packages with no non-test code and move a number that is
  supposed to mean one specific thing.
- **GOTCHA**: The PRD rows may only be flipped after a green run is actually observed and recorded in
  the report, with the Engine version it ran against. Flipping them on the strength of the code
  existing is precisely the failure this phase was created to end.
- **VALIDATE**: the full sweep below.
- **ACCEPTANCE**: `make e2e` is green on a real Engine; `make lint`, `make cover`, and
  `go test ./internal/... -race` are unchanged in behaviour and duration; `go.mod` and `go.sum` are
  byte-identical to the start of the phase.

---

## Testing Strategy

This phase's tests *are* its deliverable, so the usual unit-test table is replaced by the contract
matrix the suite must cover. Every row is a cell a future client depends on.

### Contract matrix — status and identity

| Case | Expected |
|---|---|
| `GET /v1/status`, no client certificate | 200, exactly five keys |
| `POST /v1/status` | 405 |
| `GET /v1/containers`, no client certificate | 401 or 404, terse body, no path or version |
| Client pinning the wrong CA | TLS handshake failure, no HTTP status |
| Pairing code redeemed twice | 201 then 401 |
| Unknown code vs malformed CSR | identical 401 body |
| CSR with `CN=admin` | certificate CN is the device ID |
| Two devices | distinct IDs, distinct serials, both work |
| Agent restart | same CA fingerprint, same server cert serial, no re-pair |
| `DEVMON_PUBLIC_ADDR` changed | new SANs, same CA fingerprint, no re-pair |
| Fresh state directory | different CA fingerprint |
| `certs/` deleted, DB intact | non-zero exit, explicit identity error, no new CA |
| `DELETE /v1/device/self` | 204 in every mode, then 401 |
| `POST /v1/device/renew` | 200, new cert works, **old cert still works** |
| Host-side revoke while running | next request 401, no restart |

### Contract matrix — reads and logs

| Case | Expected |
|---|---|
| Eight read routes, `read-only` mode | 200 each, documented key sets |
| Container with `DB_PASSWORD=hunter2` | string absent from the whole response |
| Image with `ENV API_KEY=` | same |
| Volume with driver options | `options` absent |
| Stopped container | absent by default, present with `?all=true` as `exited` |
| Failing healthcheck | `health: "unhealthy"` |
| Network/volume created mid-test | visible on the next call |
| Invalid ref (`%2e%2e`) / unknown ref | 400 / 404 |
| Engine severed | 502 on every read, agent up; recovers on restore |
| `?tail=20` | 20 items, `truncated:false` |
| `?since=nonsense` / future `?since=` | 400 / 200 with empty items |
| Live stream, silent container | open past 30s, keepalive frames |
| Abrupt TCP loss ×10 | agent up, slots released, new stream 200 |
| Resume with `?since=<last id>` | at most one repeated line, nothing missing |
| Ninth concurrent stream | 503 `too many concurrent log streams` |

### Contract matrix — lifecycle, policy, audit

| Case | `read-only` | `default` (unset) | `full` |
|---|---|---|---|
| `POST …/start`, `…/restart`, `…/stop` | 403 | 204 | 204 |
| `POST …/kill` | 403 | 403 | 204 |
| `DELETE /v1/containers/{id}` | 403 | 403 | 204 |
| Reads and logs | 200 | 200 | 200 |
| Start an already-running container | — | 204 | 204 |
| Delete a running container | — | — | 409, container untouched |
| Any lifecycle route on **the agent itself** | 403 | 403 | **403 (self-protected)** |
| Audit rows produced | 1 per attempt | 1 per attempt | 1 per attempt |

### Edge cases checklist
- [ ] Suite skips visibly, never silently passes, when no Engine answers — and fails under `DEVMON_E2E_REQUIRE=1`
- [ ] Suite removes every container it created and nothing else, including after a failed test
- [ ] `DEVMON_E2E_KEEP=1` preserves fixtures and prints their names
- [ ] Two concurrent suite runs on one host do not delete each other's fixtures
- [ ] No pairing code, private key, or PEM block appears in `-v` output
- [ ] The default `go test ./internal/... -race` neither compiles nor runs any e2e file
- [ ] `go build ./...`, `go vet ./...`, `gofmt -l .`, `gosec ./...` behave exactly as before
- [ ] `go.mod` and `go.sum` are unchanged
- [ ] Every new test has been observed failing at least once (falsifiability)
- [ ] Windows-native skips with the WSL2 sentence rather than failing confusingly

---

## Validation Commands

### Static analysis
```bash
gofmt -l .
go vet ./...
go vet -tags e2e ./...
```
EXPECT: all silent. (`gofmt -l .` reports the whole tree on a CRLF checkout — compare against the
pre-change output rather than expecting silence in that case.)

### Lint
```bash
make lint
make e2e-lint
```
EXPECT: clean.

### Security scan
```bash
gosec ./...
```
EXPECT: no findings, and no change in the file count scanned versus before this phase — the e2e files
are both tag-excluded and `_test.go`, so the scanner never sees them.

### Default gate — must be unaffected
```bash
go test ./internal/... -race
make cover
```
EXPECT: identical package list, identical pass/fail, coverage total still ≥ 80%.

### The e2e suite
```bash
make e2e                 # both groups, ~5-10 min against a local Engine
make e2e-container       # the in-container group alone
make e2e-endurance       # the 30-minute stream and the retention budget
```
EXPECT: green. With Docker stopped: every test skips with a reason naming the endpoint. With Docker
stopped and `DEVMON_E2E_REQUIRE=1`: hard failure.

### Module hygiene
```bash
go mod tidy
git diff --stat go.mod go.sum
```
EXPECT: no change. This phase adds no dependency, and that is the point of D1.

---

## Manual Validation

Almost nothing is left. What remains is what the suite cannot see about itself.

- [ ] Run `make e2e` on a clean Linux host and record the Engine version in the report
- [ ] Run `make e2e` from WSL2 on the Windows development machine; confirm both groups run, not skip
- [ ] Run `make e2e` on Windows-native; confirm the skip message names WSL2 and does not read as a failure
- [ ] Run `make e2e` twice concurrently on one host; confirm neither run's fixtures are disturbed
- [ ] With Docker stopped, confirm the suite skips and the make target's exit code is 0; with
      `DEVMON_E2E_REQUIRE=1`, confirm it fails
- [ ] Inspect a full `-v` run's output for any pairing code, `BEGIN` PEM block, or private key
- [ ] Confirm `docker ps -a` after a full run shows no leftover `com.devmon.e2e` container
- [ ] Run `make e2e-endurance` once and record the result — this is the PRD's Phase 4 success signal
- [ ] Confirm the CI `e2e` job runs on a PR into `main` and is **skipped, not queued**, on a PR into `dev`

## Acceptance Criteria
- [ ] All 13 tasks complete
- [ ] Every row of the Coverage Map is either an executing test or explicitly owned by the Android
      app's suite — no unticked, unowned boxes remain in this repository
- [ ] The PRD's Phase 5 headline metric — the agent surviving a delete attempt in the most permissive
      mode, by name, short ID, and full ID, across all five lifecycle routes — passes unattended
- [ ] The PRD's Phase 4 success signal — a 30-minute stream without loss — passes once and is recorded
- [ ] The suite asserts wire shapes, not the agent's own structs (D4), and a deliberate JSON tag
      rename is shown to break it
- [ ] `go.mod` and `go.sum` unchanged; no test framework or container library added
- [ ] The default gate (`go test ./internal/... -race`, `make cover`, `make lint`, `gosec ./...`)
      is unchanged in behaviour and in duration
- [ ] No production Go file changed in this phase (D19); any defect found is recorded in the report
- [ ] `make e2e` skips visibly without an Engine and fails hard under `DEVMON_E2E_REQUIRE=1`
- [ ] Nothing in test output can carry a pairing code, key material, or PEM bytes
- [ ] PRD Phase 4 and Phase 5 rows moved to `complete`; Phase 6 marked complete with its plan and
      report links

## Completion Checklist
- [ ] Every e2e file carries `//go:build e2e` — including `doc.go`
- [ ] Every fixture is labelled and every cleanup filters on the label (D11)
- [ ] Every agent subprocess gets a built environment, never an inherited one (D12)
- [ ] `InsecureSkipVerify` appears only in the pairing bootstrap and readiness polling, each with a
      comment saying why
- [ ] Tests are table-driven where the shape allows, `t.Parallel` where the resource ownership allows,
      AAA-commented
- [ ] Each new test has been observed red once, and the report says how
- [ ] README's Development section documents the suite, its two groups, and its four commands
- [ ] The report names the Engine version, the platform, and the wall-clock duration of a full run

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| The suite deletes a developer's real container | L | **CRITICAL** | D11's dual label and label-filtered cleanup; no name-prefix matching anywhere; a dedicated checklist item that a concurrent run is undisturbed |
| A test leaks a pairing code or a device private key into CI logs | M | **HIGH** | The redaction helper, the prohibition on printing parsed CLI output, and a manual sweep of a full `-v` run |
| The suite passes without doing anything (Engine absent, everything skipped) | **H** | **HIGH** | D5's `DEVMON_E2E_REQUIRE` in CI; the skip reason is printed; a checklist item verifies both branches |
| A green e2e run that has never been red — assertions that cannot fail | M | HIGH | The per-task falsifiability obligation, and the report records how each was falsified |
| Flakiness turns the suite into a check people re-run until it passes | **H** | HIGH | Poll with deadlines, never sleep fixed durations; per-test agents and state directories; content-based sequence assertions in the endurance test; `-count=1` so a flake is never masked by the cache |
| The e2e dependency creeps into `go.mod` via a "small" helper library | M | MEDIUM | D1's reasoning is written down; module hygiene is an acceptance criterion; the CI `test` job would surface a `go.mod` change on the `dev` path immediately |
| Code written from the pre-v29 Docker SDK, or from `go-connections/nat` port types | **H** | MEDIUM | Verified signatures transcribed above; `network.PortBinding.HostIP` as `netip.Addr` and the opaque `network.Port` are the two that differ most from memory, and both are compile errors |
| `syscall.Stat_t` breaks the build on the Windows development machine | **H** | MEDIUM | The `_linux` / `_other` file split in Task 1, validated by `go vet -tags e2e ./...` on Windows |
| Bind-mount permissions defeat the in-container group | M | MEDIUM | `os.Chmod(0o777)` on a temp state dir, documented as test-only; file reads go through `CopyFromContainer` so `0600`-owned-by-65532 files are never a problem |
| The suite finds a real defect and the phase turns into a bug-fix phase | M | MEDIUM | D19: findings are recorded and fixed in their own tasks and commits. This is a success mode, not a failure — it is what the phase is for |
| CI minutes and PR latency grow | M | LOW | D21 keeps the job on the `main` path only; the `dev` bar is unchanged |
| The 30-minute endurance run never actually gets run | M | MEDIUM | It is compiled every run, its skip names the command that enables it, it has a make target, and running it once is an acceptance criterion and a manual checklist item |

## Notes

- **This phase writes no production code, and that constraint is doing work.** Every previous phase
  ended with a checklist a human was supposed to run and did not. The way to break that pattern is
  not to write better checklists — it is to make the checklist executable and then never again ship
  a phase whose validation depends on somebody remembering. If the suite finds bugs, that is the
  phase succeeding, and each fix earns its own task and its own commit (D19).

- **"It needs an Engine, not a phone" is the insight the whole phase rests on.** Fourteen of the
  sixteen outstanding items were filed as manual because the person who would have run them was
  imagined holding a handset. Only two of them actually are about the handset, and for both of those
  the agent-side half is testable here. The Coverage Map is the audit of that claim, and it is the
  part of this document worth reviewing hardest.

- **D4 is the difference between a regression suite and a contract.** Asserting against
  `map[string]any` is more tedious than unmarshalling into the agent's own DTOs, and it is the only
  version that fails when a JSON tag changes. The Android app and any future web client see the
  wire; so must the suite. Anyone tempted to "simplify" it by importing `dockerx` should read this
  paragraph first.

- **The in-container group exists for exactly one reason.** Self-identification runs through
  `/proc/self/mountinfo`, which only says anything inside a container, and self-exclusion is
  meaningless without it. Everything else in that group — crash survival, image upgrade, bind-mount
  persistence — is there because it was already paying the cost of a running container. If the group
  ever grows a test that would run equally well against the host binary, it belongs in the other one.

- **What this phase leaves for Phase 7**: rate limiting and its tests, the security review against
  the PRD risk table, the automated installer, the threat-model documentation, and the AGPL-3.0
  release. Phase 7 now inherits a regression net it can change the agent against, which is why the
  PRD makes it depend on this one.
