# Implementation Report: Logs & Live Streaming (Phase 4)

## Summary

A paired device can now read a container's output two ways: a bounded historical fetch that
returns JSON, and a live Server-Sent Events stream that resumes from a timestamp cursor after a
network handover. Both demultiplex Docker's stdout/stderr framing server-side and hand the client
structured per-line records rather than a raw byte stream.

All 11 plan tasks landed. Every automated gate passes. The manual and end-to-end sweeps, which
need a real Docker host and a real device on mobile data, have **not** been run — see
"Outstanding validation" below. That is why the PRD row reads `awaiting validation` rather than
`complete`.

## Assessment vs Reality

| Metric | Predicted (Plan) | Actual |
|---|---|---|
| Complexity | Large — 16 files, 11 tasks, ~750 lines of production Go plus tests | Matched |
| Files changed | 16 (8 created, 8 updated) | 17 (8 created, 9 updated) |
| Tasks | 11 | 11, all complete |
| `go.mod` change | none | none — confirmed by `go mod tidy` producing no content diff |

The extra updated file is `internal/dockerx/types_test.go`, which gained the `LogLine` field-count
guard the plan called for without naming the file.

## Tasks Completed

| # | Task | Status | Notes |
|---|---|---|---|
| 1 | `statusRecorder.Unwrap()` | Complete | Test failed with `feature not supported` before the fix and passes after, as the plan intended |
| 2 | `LogLine` and `ErrInvalidSince` | Complete | `classify` deliberately left untouched |
| 3 | Frame parsing and line splitting | Complete | Deviated — extracted `lineState`, `emitLine`, `extractTimestamp` helpers |
| 4 | `LogOptions` and TTY detection | Complete | Deviated — extracted `containerHasTTY` so the nil-`Config` case is testable |
| 5 | Historical and streaming reads | Complete | Deviated — extracted `demuxNonTTY` / `demuxTTY` for the same reason |
| 6 | The SSE writer | Complete | Mutex deferred to Task 7 by agreement, with the decision recorded |
| 7 | `LogReader` and the two handlers | Complete | Deviated — `keepaliveInterval` became a package `var`; also absorbed the `streams` field so `logs.go` could compile |
| 8 | Wire the routes and the semaphore | Complete | `NewServer`'s signature unchanged (D14), so no test helper was touched |
| 9 | Handler tests | Complete | One plan-internal contradiction surfaced — see Deviations |
| 10 | `dockerx` log tests | Complete | Package coverage 80.0% → 82.7% |
| 11 | Docs and the gate sweep | Complete | README API rows, failure modes, and a "Live log streaming" section |

## Validation Results

| Level | Status | Notes |
|---|---|---|
| `gofmt` | Pass | All eight new files clean. Pre-existing files report dirty tree-wide because of CRLF line endings on this checkout — a known condition, unchanged by this phase |
| `go vet ./...` | Pass | No output |
| `golangci-lint run ./...` | Pass | `0 issues.` |
| `gosec ./...` | Pass | No findings. G115 on the frame-size `uint32` is guarded by the `maxFramePayloadBytes` bound check that precedes the conversion |
| `go build ./...` | Pass | |
| `go test ./internal/... -race` | Pass | All packages green |
| Coverage | Pass | Total **83.5%**, floor is 80%. `dockerx` 82.7%, `httpapi` 86.3% |
| `go mod tidy` | Pass | No content diff — this phase added no dependency |
| Integration / manual | **Not run** | Requires a real Docker host; see below |

## Files Changed

| File | Action |
|---|---|
| `internal/dockerx/framing.go` | CREATED |
| `internal/dockerx/framing_test.go` | CREATED |
| `internal/dockerx/logs.go` | CREATED |
| `internal/dockerx/logs_test.go` | CREATED |
| `internal/httpapi/sse.go` | CREATED |
| `internal/httpapi/sse_test.go` | CREATED |
| `internal/httpapi/logs.go` | CREATED |
| `internal/httpapi/logs_test.go` | CREATED |
| `internal/dockerx/types.go` | UPDATED — `LogLine` |
| `internal/dockerx/types_test.go` | UPDATED — `LogLine` field-count guard |
| `internal/dockerx/errors.go` | UPDATED — `ErrInvalidSince` |
| `internal/dockerx/errors_test.go` | UPDATED — sentinel coverage |
| `internal/httpapi/middleware.go` | UPDATED — `statusRecorder.Unwrap()` |
| `internal/httpapi/reads.go` | UPDATED — `DockerReader` embeds `LogReader` |
| `internal/httpapi/reads_test.go` | UPDATED — `fakeDocker` gained two nil-safe methods |
| `internal/httpapi/server.go` | UPDATED — `maxConcurrentStreams`, `streams` channel, two routes |
| `internal/httpapi/server_test.go` | UPDATED — flushability and route-precedence tests |
| `README.md` | UPDATED — API rows, failure modes, "Live log streaming" |

## Deviations from Plan

1. **Pure-logic helpers extracted for testability** (Tasks 3, 4, 5). `Client.api` is a concrete
   `*client.Client` with no mock seam, matching every other method in the package. Rather than
   introduce a mocking framework, the demultiplexing and nil-guard logic was pulled into
   unexported helpers — `demuxNonTTY`, `demuxTTY`, `containerHasTTY`, `extractTimestamp` — and
   tested directly. This mirrors the existing `toContainerDetail` / `toContainerSummary` pattern in
   `containers.go`. No exported surface beyond what the plan specifies.

2. **`keepaliveInterval` is a package `var`, not a `const`** (Task 7). The plan required the
   interval to be shortenable so the race test does not wait 20 real seconds, and offered a choice
   between a package variable and threading it through `newSSEWriter`. The variable was chosen: it
   needs no signature change, and the test restores it with `t.Cleanup`.

3. **Task 7 absorbed the `streams` field and `maxConcurrentStreams` constant** that the plan listed
   under Task 8. Without them `logs.go` does not compile, so splitting them exactly as written
   would have left a non-building intermediate state. Task 8 still owns the route registration and
   the precedence test.

4. **`?tail=9999` falls back to the default, it does not clamp to 2000.** The plan contradicts
   itself here: the API Contract table says "out of range or unparsable → the default", while Task
   9's bullet expects the fake to observe `2000` for `?tail=9999`. The implementation and its doc
   comment follow the contract table, and the test asserts that behaviour. Worth a decision in a
   future phase if clamping is preferred — it would give a client asking for 9999 the 2000 it could
   have had, rather than 200.

5. **A `deadlineAwareRecorder` test helper was added** to `server_test.go`. `httptest.ResponseRecorder`
   implements `Flush` but not `SetWriteDeadline`, so a bare recorder cannot prove `Unwrap` forwards
   both. The helper is test-only; no production code exists for its sake.

## Issues Encountered

- **`go mod tidy` reported `go.mod` as modified with an empty diff.** The rewrite changed only line
  endings, not content. `git checkout -- go.mod` restored it; the "no dependency added" gate holds.
- **`go tool cover -func=coverage.out` needs its flag quoted in PowerShell**, or the shell splits
  the argument and writes a file named `coverage`. Not a code issue, but it will bite the next
  person running the gates on Windows.

## Tests Written

| Test file | Coverage area |
|---|---|
| `internal/dockerx/framing_test.go` | Frame parsing, partial header/payload, clean vs unexpected EOF, oversized length, line splitting across frames, CRLF, the 8 KiB cap, timestamp extraction |
| `internal/dockerx/logs_test.go` | Engine option mapping, `ErrInvalidSince`, nil `Config`, emit-error propagation, TTY passthrough, stdout/stderr interleaving, silent container, historical truncation and never-nil `Items` |
| `internal/httpapi/sse_test.go` | Exact frame bytes, keepalive bytes, lazy start, no raw newline in `data:`, unflushable writer |
| `internal/httpapi/logs_test.go` | Both routes end to end: status mapping, tail bounds, lazy-header 404, terminal error frame, slot exhaustion and release, keepalive race under `-race`, goroutine leak, field counts, log hygiene, error-body leakage |
| `internal/httpapi/server_test.go` | `statusRecorder` flushability, route precedence between `/{id}`, `/{id}/logs`, `/{id}/logs/stream` |

## Outstanding validation

None of these can be executed from this environment — they need a real Docker host and, for two of
them, a real phone on mobile data. They are the plan's own Manual Validation checklist and the
remaining Phase 4 acceptance criteria:

- [ ] The 30-second test: a stream still delivering at 60s and at 5 minutes (proves D7/D8 on a real server)
- [ ] The 30-minute endurance run across a network handover — the PRD's Phase 4 success signal
- [ ] TTY container: no stray header bytes
- [ ] Interleaving: a known alternating stdout/stderr order preserved
- [ ] Oversized line cut at 8 KiB with no agent memory spike
- [ ] Silent container: keepalive comments visible under `curl -N`
- [ ] Abandoned stream: goroutine and connection counts return to baseline
- [ ] Container exit mid-stream: clean end, no error frame
- [ ] Engine death mid-stream: terminal `event: error`, agent stays up
- [ ] `DEVMON_POLICY_MODE=read-only`: both routes still 200
- [ ] Revocation mid-stream: reconnect refused 401 without a restart
- [ ] Log hygiene: a secret printed by a container never appears in `agent.log`

The last one is already enforced by `TestAgentLogNeverCarriesLineContent`, but the on-host check is
worth running against a real logger and a real rotation file.

## Observations for a future phase

- **A client disconnect logs at `ERROR`.** When the client's socket goes away, `emit` fails, the
  stream unwinds correctly — and then the terminal `event: error` frame also fails to write, which
  is logged via `s.log.Error`. Every normal "user closed the log view" therefore leaves an ERROR
  line in `agent.log`. Correct behaviour, noisy telemetry; worth downgrading or suppressing when
  the write failure is a broken pipe. Out of this plan's scope, so it was left alone.
- `maxConcurrentStreams = 8` remains the plan's own flagged guess. Phase 6's hardening pass is the
  right place to measure it.

## Review

Reviewed before commit by the main session plus `ecc:go-reviewer` and `ecc:security-reviewer` —
see [logs-and-live-streaming-review.md](../reviews/logs-and-live-streaming-review.md).

Two HIGH findings, both in the keepalive path, and both fixed in the same working tree before
anything was committed:

- **H1** — a keepalive firing before the first log line committed the response with no SSE headers
  and, critically, without clearing the write deadline, so a silent container's stream still died at
  the server's 30s `WriteTimeout`. That is D7 defeated on the one path D11 exists to serve.
- **H2** — the keepalive goroutine was cancelled but never joined, so it could write to the
  `ResponseWriter` after the handler returned. Its regression test reproduced a genuine `-race` data
  race against the unfixed code.

One LOW (unbounded historical accumulation in an exported method) was fixed in the same pass. Both
HIGH findings passed every gate before they were found — the race suite included — which is worth
remembering: `-race` only reports windows a test actually opens.

## Next Steps

- [ ] Run the on-host validation sweep above, then flip the PRD Phase 4 row to `complete`
- [ ] PR into `dev` from `feat/logs-and-live-streaming`
