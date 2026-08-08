# Code Review: Phase 4 — Logs & Live Streaming

**Reviewed**: 2026-08-08
**Branch**: `feat/logs-and-live-streaming` (uncommitted working tree)
**Reviewers**: main session (Opus), `ecc:go-reviewer`, `ecc:security-reviewer`
**Decision**: **REQUEST CHANGES** — two HIGH findings, both in the live-stream keepalive path
**Resolution**: H1, H2 and L1 fixed in the same working tree before commit. See "Resolution" at the
end of this document. L2 and L3 accepted as-is.

## Summary

The phase is well built and matches its plan's decision log closely: bounds are enforced before
allocation, error mapping is structural rather than incidental, and log hygiene (D16) is proven by
a real test. Static analysis and the race suite are clean.

Both HIGH findings sit in the same narrow place — the interaction between the keepalive goroutine
and the `sseWriter`. Neither is caught by the existing tests, and each independently defeats a
decision the plan was written to protect (D7 and D12 respectively).

## Findings

### CRITICAL

None.

### HIGH

**H1 — `internal/httpapi/sse.go:132` — a keepalive before the first log line commits the response
without SSE headers and without clearing the write deadline.**

`keepalive()` writes straight to the `ResponseWriter` with no `started` check, so `startLocked()`
never runs on this path. Confirmed empirically with a throwaway probe (since deleted):

| | Expected | Actual after a keepalive with zero events |
|---|---|---|
| Status | 200 | 200 |
| `Content-Type` | `text/event-stream` | `text/plain; charset=utf-8` |
| `Cache-Control` / `X-Content-Type-Options` / `X-Accel-Buffering` | set | absent |
| Write deadline | cleared | **still the server's 30s `WriteTimeout`** |
| `Started()` | true | **false** |

This is the silent-container case — exactly what D11's keepalive exists to serve. Two consequences:

1. **The stream still dies at 30 seconds**, which is D7's entire purpose. Task 1 and D8 are correct
   and do work; this path simply never reaches `SetWriteDeadline(time.Time{})`.
2. `Started()` stays false, so a later Engine failure takes the `!sse.Started()` branch at
   `internal/httpapi/logs.go:169` and calls `writeDockerError` on an already-committed response —
   a superfluous `WriteHeader` and a JSON error body appended to what the client is parsing as a
   stream.

**Fix**: have `keepalive()` call `startLocked()` when `!s.started`, exactly as `event()` does. By
the time the first tick fires, the pre-stream inspect has already returned (bounded by the 15s
`callTimeout`), so committing there cannot steal D12's 404 window.

**Test gap**: `TestStreamKeepaliveIsRaceFree` emits lines, so `start()` has already run. Add a case
asserting `Content-Type: text/event-stream` after a keepalive with zero events.

---

**H2 — `internal/httpapi/logs.go:140-158` — the keepalive goroutine is cancelled but never joined,
so it can write to the `ResponseWriter` after the handler returns.**

```go
ctx, cancel := context.WithCancel(r.Context())
defer cancel()
...
go func() {
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            _ = sse.keepalive()
        }
    }
}()
```

`sseWriter.mu` correctly serialises the keepalive goroutine against `emit` while both are alive,
but it does not order either against the handler's return. `cancel()` closes `ctx.Done()`; it does
not block until the goroutine observes it. So the emit loop can finish, the deferred `cancel()` can
run, and `handleStreamContainerLogs` can return to `ServeHTTP` while the goroutine is still inside
`sse.keepalive()` holding `mu` mid-`Fprint`/`Flush`.

`net/http`'s contract is that a `ResponseWriter` must not be used after the handler returns. On a
keep-alive connection the server may already be serving the next request over the same connection.
This is the plan's own risk-table entry ("Keepalive goroutine races emit on the same
ResponseWriter") in the one form the mutex does not cover: goroutine vs. handler-return rather than
goroutine vs. goroutine.

**Fix**: join the goroutine. Registration order matters — `wg.Wait()` must be deferred *before*
`cancel()` so it runs *after* it:

```go
ctx, cancel := context.WithCancel(r.Context())
var wg sync.WaitGroup
defer wg.Wait()   // registered first, so it runs last
defer cancel()

wg.Add(1)
go func() {
    defer wg.Done()
    ...
}()
```

**Test gap**: `TestStreamGoroutineDoesNotLeak` polls that the goroutine *eventually* exits, not that
it has exited *before* the handler returned. A test that ticks at the return instant is what would
catch this under `-race`.

### MEDIUM

None.

### LOW

**L1 — `internal/dockerx/logs.go:112-129` — `ContainerLogs` accumulates the whole Engine response
before applying `maxHistoricalLines`.** Truncation happens at line 126, after the demux loop has
already appended everything. Not reachable today: `tailParam` clamps to `[1,2000]` before this is
called, so the Engine is asked for at most 2000 lines. But `ContainerLogs` is exported, and a future
caller passing `Tail: 0` maps to `Tail: "all"` and reintroduces unbounded accumulation — the
line-count analogue of the OOM that D9's line-length cap exists to prevent. Suggest enforcing the
cap inside the `emit` closure rather than after the read.

**L2 — `internal/httpapi/logs.go` — `handleStreamContainerLogs` is ~70 lines**, over the project's
50-line guideline. It is a single linear guard → acquire → stream → map-error sequence with no deep
nesting, so this is a note rather than a defect. Fixing H2 will add a few more lines.

**L3 — plan/prose inconsistency, no code change needed.** The plan's Task 9 bullet expects
`?tail=9999` to clamp to 2000; the API Contract table and `tailParam`'s doc comment both say
out-of-range falls back to the default. The code follows the contract table and the test asserts
that. Worth reconciling if clamping is ever preferred — it would give a client asking for 9999 the
2000 it could have had rather than 200.

## Verified clean

Spot-checked rather than taken on trust:

- **D16 log hygiene** — every new logging call passes a static op string plus `err`; no path formats
  a struct containing `LogLine.Line`. `TestAgentLogNeverCarriesLineContent` plants a secret in both
  a line and an emit error and asserts absence from the captured logger.
- **Allocation bounds** — `maxFramePayloadBytes` is checked against the raw `uint32` *before* the
  `int` conversion and before `make()`, so G115 is structurally impossible rather than merely
  unreported. `maxLogLineBytes` is enforced on the accumulating buffer, with a cross-`push` test
  proving the discard state survives frame boundaries.
- **`id:` injection** — impossible by construction: `extractTimestamp` keeps a value only if it
  round-trips through `time.Parse(RFC3339Nano)`, which no control byte satisfies.
- **Response splitting** — `event()` JSON-marshals the whole `LogLine`, so an embedded newline
  becomes the two-character escape. Asserted at byte level.
- **Error-body disclosure** — only static messages cross the wire; a test drives an Engine error
  containing `/var/run/docker.sock` through every failure path and asserts it never appears.
- **Slot release** — `defer func() { <-s.streams }()` on every path; both exhaustion tests would
  flip to 503 if a slot leaked, so they are not vacuous.
- **`statusRecorder.Unwrap()`** — does not weaken the request log. `ResponseController` never calls
  `WriteHeader`, and `sse.start()`'s `WriteHeader` still goes through `statusRecorder`.
- **Guards and precedence** — both routes sit behind `requireDevice` + `requireOp(policy.OpLogs)`;
  route precedence is tested rather than assumed.

## Validation Results

| Check | Result |
|---|---|
| `gofmt` | Pass — all 8 new files clean |
| `go vet ./...` | Pass |
| `golangci-lint run ./...` | Pass — `0 issues.` |
| `gosec ./...` | Pass — no findings |
| `go build ./...` | Pass |
| `go test ./internal/... -race` | Pass |
| Coverage | Pass — 83.5% total (floor 80%) |
| `go mod tidy` | Pass — no content diff |

Note that the race suite passing does **not** clear H2: no existing test lines a keepalive tick up
with the handler's return, so `-race` has never had the window to observe it.

## Decision

**REQUEST CHANGES.** H1 and H2 should both be fixed before this is committed. Together they are
roughly fifteen lines across `sse.go` and `logs.go`, plus two regression tests. L1 is worth taking
in the same pass; L2 and L3 can wait.

## Resolution

All three of H1, H2 and L1 were fixed in the same working tree, before anything was committed.

| Finding | Fix | Evidence |
|---|---|---|
| H1 | `keepalive()` calls `startLocked()` when `!s.started` (`sse.go:145`) | `TestSSEKeepaliveStartsResponse` failed against the unfixed code on `Started()`, `Content-Type`, the missing SSE headers, and the uncleared deadline; passes after |
| H2 | `sync.WaitGroup` with `defer wg.Wait()` registered before `defer cancel()` (`logs.go:152-155`) | `TestStreamKeepaliveGoroutineJoinedBeforeReturn` **reproduced a real `-race` data race** — write-after-handler-return on the shared recorder — on the first of 20 iterations against the unfixed code; 20/20 clean after |
| L1 | Cap enforced inside the `emit` closure (`dockerx/logs.go:124-131`) | Invariant test only. The old code and old test both used post-hoc slicing consistently, so there was no observable divergence to assert against — this documents the property rather than reproducing a failure |

Defer ordering for H2 resolves LIFO to `ticker.Stop()` → `cancel()` → `wg.Wait()`, which is correct:
the goroutine is told to stop before it is waited on.

Also changed: `deadlineAwareRecorder` gained a `deadlineCleared` field so H1's test can assert the
deadline was actually cleared rather than merely no-op'd. Test-only.

**Gates after the fix**, re-run independently of the implementing agent: build, vet, and the full
race suite clean; `go test ./internal/httpapi/ -race -count=20 -run 'TestStream|TestSSE'` passes;
coverage 83.5%, unchanged; `golangci-lint` 0 issues; `gosec` clean.

**Carried forward, not fixed:**

- **L2** (`handleStreamContainerLogs` length) — the H2 fix added a few lines, so it is now slightly
  longer. Still a single linear sequence; still a note rather than a defect.
- **L3** (plan prose vs. API contract on `?tail=9999`) — no code change; reconcile if clamping is
  ever preferred.
- **New minor note from the L1 fix**: `make([]LogLine, 0, maxHistoricalLines)` now pre-allocates
  2000 slots on every historical request, including `?tail=5` — roughly 128 KB per call regardless
  of what was asked for. Harmless at eight concurrent streams, but sizing the capacity to
  `opts.Tail` would be strictly better if this code is touched again.
