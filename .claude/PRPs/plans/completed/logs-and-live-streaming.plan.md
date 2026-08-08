# Plan: Logs & Live Streaming (DevMon Agent Phase 4)

## Summary

Give a paired device two ways to read a container's logs: a bounded historical fetch that returns
JSON, and a live **Server-Sent Events** stream that survives a mobile network handover by resuming
from a timestamp cursor. Both demultiplex Docker's stdout/stderr framing server-side and hand the
client structured, per-line records rather than a raw byte stream.

This phase is where the agent first holds a connection open for minutes rather than milliseconds.
Nearly all of its difficulty is in that fact: the server's 30s `WriteTimeout`, the response-wrapping
middleware, and Docker's binary frame format are each individually capable of turning a working
stream into one that dies, buffers forever, or emits garbage.

## User Story

As an operator diagnosing a failing container from my phone,
I want to read its recent log output and then watch new lines arrive live,
so that I can see what it is doing wrong without opening a shell on the host.

## Problem → Solution

A paired device can enumerate and inspect every Docker object on the host but cannot read a single
line of output from any of them — the one thing an operator actually looks at during an incident.
→ A paired device fetches the last N lines on demand, and opens a live stream that keeps delivering
new lines across a 30-minute session and reconnects without gaps after the network drops.

## Metadata

- **Complexity**: Large (16 files, 11 tasks; ~750 lines of production Go plus tests)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 4 — Logs & live streaming
- **Depends on**: Phase 2 (complete). Phase 3 (complete) is not a dependency but has already landed,
  so this plan builds on its `dockerx` and `httpapi` structure rather than beside it.
- **Estimated Files**: 16 changed (8 created, 8 updated)

---

## Decisions Settled Before Planning

Each has a plausible-looking wrong answer. Settled here so implementation never re-litigates them.

| # | Decision | Choice | Why not the alternative |
|---|---|---|---|
| D1 | Live transport | **Server-Sent Events** (`text/event-stream`) on the existing mTLS listener | The PRD's architecture note says "long-lived bidirectional channel", but nothing flows client→server once the request is made — logs are strictly one-directional. WebSocket buys bidirectionality nobody uses and costs a new module dependency on an internet-facing port, a framing and authentication story that sits *beside* the HTTP middleware instead of inside it, and a resume protocol you design yourself. SSE reuses `requireDevice`, `requireOp`, and `writeError` unchanged, and has resumption in its own spec. Chunked NDJSON is SSE with the event IDs, keepalive, and reconnect semantics deleted and then reinvented. |
| D2 | Two routes, not one `?follow=` flag | `GET …/logs` (JSON) and `GET …/logs/stream` (SSE) are **separate routes** | One route whose response *type* changes with a query parameter means every client, proxy, and test has to branch on a flag to know what it is reading. Separate paths also make it structurally impossible for the bounded historical route to accidentally become an unbounded stream after a parameter-parsing bug. |
| D3 | Line shape | `{"ts":…,"stream":"stdout"\|"stderr","line":…}` — timestamp **extracted** into its own field | Requires `Timestamps: true` on every Engine call, then splitting Docker's `<RFC3339Nano> <text>` prefix off server-side. Leaving the prefix embedded in `line` would make every client parse a Docker-specific wire format to get the resume cursor it needs — pushing agent-side knowledge onto the Android app, which is exactly what the projection rule (Phase 3 D1) exists to prevent. |
| D4 | Demultiplexing | **Hand-rolled 8-byte frame reader** for non-TTY containers; raw passthrough for TTY containers | `stdcopy.StdCopy(outW, errW, src)` is a pump into two `io.Writer`s. Using it means two `io.Pipe`s and two goroutines, and — the real problem — **it destroys the interleaving between stdout and stderr**, which is the ordering an operator reads a log for. The frame format is 8 bytes of header and a length; parsing it inline is ~30 lines, keeps one ordered sequence, and needs no goroutine. |
| D5 | TTY detection | **Inspect the container first**, read `Config.Tty`, then choose the reader | There is no way to tell from the stream itself. Running the frame reader over a TTY stream treats the first 8 bytes of real output as a header and emits corruption; skipping it on a non-TTY stream emits the binary headers as text. The extra `ContainerInspect` doubles as the ref validation and the 404 path, so it is not a wasted call. |
| D6 | Resume | `?since=<RFC3339Nano>`, mapped onto Docker's `Since`; the SSE `id:` field carries each line's timestamp | Delivery is **at-least-once**, not exactly-once: Docker's `since` filter is inclusive at its granularity boundary, so a resume can repeat the last line or two. Say so in the README and let the client dedupe on `(ts, line)`. Claiming exactly-once here would be a lie that only shows up as duplicated log lines on a user's screen. |
| D7 | The 30s `WriteTimeout` | Cleared per-request via `http.NewResponseController(w).SetWriteDeadline(time.Time{})` | The server sets `WriteTimeout: 30s` globally (`server.go:28`). Without clearing it the stream dies at exactly 30 seconds, every time — which reads as "works in testing, breaks in the 30-minute endurance run the PRD's success signal requires". Lowering the global timeout is not an option: it is a Slowloris defence for every other route. |
| D8 | `statusRecorder` must gain `Unwrap()` | `func (r *statusRecorder) Unwrap() http.ResponseWriter` added to `middleware.go` | **Verified empirically, not assumed.** `withRequestLog` wraps every response in `statusRecorder`, which embeds the `http.ResponseWriter` *interface* — so `Flush` is not promoted, and there is no `Unwrap`. `http.ResponseController` therefore returns `feature not supported` for **both** `Flush()` and `SetWriteDeadline()`. Without this three-line fix the stream both buffers forever and dies at 30s, and neither failure names the middleware as the cause. |
| D9 | Line length cap | `maxLogLineBytes = 8 KiB`; longer lines are cut and marked `"truncated":true` | A container printing a 500 MB single line (a stack dump, a base64 payload, a minified bundle) would otherwise be accumulated whole in agent memory before any line boundary is reached. On a 512 MB VPS that is the agent OOM-killing itself while reading logs. Marking rather than silently cutting keeps the response honest. |
| D10 | Concurrent stream cap | `maxConcurrentStreams = 8`, non-blocking acquire, 503 when full | Each stream holds a goroutine, an Engine HTTP connection, and a socket for its whole life. Unbounded, a single compromised or buggy phone exhausts the host's file descriptors — the agent causing the outage it exists to prevent. A constant, not an env var: the PRD's own reasoning is that every additional startup setting is surface the operator must understand at install time. |
| D11 | Keepalive | SSE comment frame (`: keepalive\n\n`) every 20s | A container that logs nothing for five minutes is normal; a connection that sends nothing for five minutes gets dropped by mobile-carrier NAT and by any intervening proxy. The keepalive write is also the **only** way the agent learns the client vanished — the write fails, and the stream unwinds. Without it an abandoned stream leaks a goroutine and an Engine connection until the TCP stack gives up. |
| D12 | Errors before vs. after the first byte | SSE headers are written **lazily, on the first emitted line** | Once `200 OK` and the headers are committed, the status code can never be corrected — a container that turns out not to exist would be reported as a successful empty stream. Deferring the header write until there is something to write keeps `writeDockerError`'s 400/404/502 mapping intact for every pre-stream failure. After the stream has started, a failure becomes a terminal `event: error` frame. |
| D13 | Per-call timeout | The 15s `callTimeout` applies to the historical route and to the pre-stream inspect — **never to the stream itself** | Reusing `callTimeout` on the streaming call kills every stream at 15 seconds. The stream's lifetime is bounded by the request context and nothing else. |
| D14 | `DockerReader` gains `LogReader` | `LogReader` is its own named interface, **embedded** into `DockerReader` | The Phase 3 plan's note ("Phase 4 should compose its own `LogReader`") was written for the case where both phases landed concurrently. Phase 3 has landed. Embedding keeps `NewServer`'s signature — and therefore all four test helpers — untouched, while `LogReader` stays independently referenceable. A fifth constructor parameter would churn every helper again for no gain. |
| D15 | Audit logging | **None.** Reads are not mutating operations | Consistent with Phase 3 and with the PRD, which scopes the audit log to mutating calls. A 30-minute stream would otherwise produce either one meaningless entry or thousands. |
| D16 | Container log content in `agent.log` | **Never, at any level** | Log lines are, by design, whatever the container printed — including the secrets Phase 3 went to such lengths to keep out of responses. The difference is that here the operator asked for them. That makes it more important, not less, that they pass through the agent without ever being written to its own persistent log. |
| D17 | Persisting container logs | Not in scope, in any form | The PRD excludes it explicitly in "What We're NOT Building". Nothing in this phase writes to the state directory. |

---

## UX Design

### Before

```
┌──────────────────────────────────────────────────────────────┐
│  Phone (paired)                                               │
│    GET /v1/containers/a1b2c3     ──►  200 {state:"exited",    │
│                                          exit_code: 1, …}     │
│    GET /v1/containers/a1b2c3/logs ──►  404  (no such route)   │
│                                                               │
│  The operator can see the container died, and cannot see why. │
└──────────────────────────────────────────────────────────────┘
```

### After

```
┌────────────────────────────────────────────────────────────────────────┐
│  Phone (paired)                                                         │
│                                                                         │
│  ── Historical ──                                                       │
│  GET /v1/containers/a1b2c3/logs?tail=200                                │
│    ◄── 200 application/json                                             │
│        {"items":[                                                       │
│           {"ts":"2026-08-08T10:02:11.441Z","stream":"stdout",           │
│            "line":"listening on :8080"},                                │
│           {"ts":"2026-08-08T10:02:14.882Z","stream":"stderr",           │
│            "line":"panic: nil map write"}],                             │
│         "truncated":false}                                              │
│                                                                         │
│  ── Live ──                                                             │
│  GET /v1/containers/a1b2c3/logs/stream?tail=50                          │
│    ◄── 200 text/event-stream                                            │
│                                                                         │
│        id: 2026-08-08T10:02:11.441Z                                     │
│        event: log                                                       │
│        data: {"ts":"…","stream":"stdout","line":"listening on :8080"}   │
│                                                                         │
│        : keepalive                          ← every 20s when idle       │
│                                                                         │
│        id: 2026-08-08T10:02:14.882Z                                     │
│        event: log                                                       │
│        data: {"ts":"…","stream":"stderr","line":"panic: nil map write"} │
│                                                                         │
│  ── Handover: connection drops, app reconnects from its last id ──      │
│  GET /v1/containers/a1b2c3/logs/stream?since=2026-08-08T10:02:14.882Z   │
│    ◄── 200 text/event-stream   (resumes; may repeat the boundary line)  │
│                                                                         │
│  ── Failures ──                                                         │
│  GET /v1/containers/nope/logs        ◄── 404 {"error":"not found"}      │
│  GET /v1/containers/%2e%2e/logs      ◄── 400 {"error":"invalid …"}      │
│  GET …/logs/stream  (9th stream)     ◄── 503 {"error":"too many …"}     │
│  GET …/logs?since=yesterday          ◄── 400 {"error":"invalid since …"}│
└────────────────────────────────────────────────────────────────────────┘
```

### Interaction Changes

| Touchpoint | Before | After | Notes |
|---|---|---|---|
| Container detail screen | State, exit code, mounts — no output | A "Logs" affordance showing the last 200 lines | The diagnostic payload the whole incident flow was missing |
| Live view | Does not exist | A follow mode that keeps scrolling as lines arrive | PRD's hardest transport requirement |
| Network handover | n/a | App reconnects with `?since=<last id>`; may see one repeated line | At-least-once, documented (D6) |
| Silent container | n/a | Connection stays open; keepalive comments are invisible to the user | Without D11 the app would show a dead stream as an error |
| Ninth concurrent stream | n/a | 503 with a distinct message | The app can say "close another log view" rather than "the agent is broken" |
| stderr vs stdout | n/a | Per-line `stream` field the app can colour | Requires D4's ordered demux; `stdcopy.StdCopy` would lose the interleaving |

---

## Mandatory Reading

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `internal/httpapi/middleware.go` | 106–142 | `withRequestLog` and `statusRecorder` — D8's `Unwrap()` goes here, and without it nothing else in this phase works |
| P0 | `internal/httpapi/server.go` | 19–36, 79–110 | The `writeTimeout` constant D7 must defeat; `routes()` and the `read` helper the two new routes mirror |
| P0 | `internal/httpapi/reads.go` | 13–55, 79–117 | `DockerReader` (D14 embeds into it), `writeDockerError`, `requireDocker`, `listAllParam` — all reused verbatim |
| P0 | `internal/dockerx/containers.go` | 1–57 | `callTimeout`, `ValidateRef`-first ordering, `classify` — the exact call shape both new dockerx methods follow |
| P0 | `internal/dockerx/errors.go` | all (31) | `ErrNotFound`, `ErrInvalidRef`, `classify` — this phase adds one sentinel beside them |
| P1 | `internal/httpapi/respond.go` | 9–47 | `writeJSON`'s header set (the SSE writer sets the same three, plus its own) and `writeError` |
| P1 | `internal/policy/mode.go` | 28–65 | `OpLogs` is already registered at `ModeReadOnly` — this phase adds no policy code, it only cites it |
| P1 | `internal/httpapi/reads_test.go` | all | `fakeDocker` gains two methods (D14); `testServerWithDocker` is the helper the new tests reuse |
| P1 | `internal/httpapi/status_test.go` | 25–72 | `testLogger`, the three server helpers, `peerCertWithSerial` — unchanged by this phase, and used by its tests |
| P2 | `internal/dockerx/types.go` | all | Where `LogLine` joins the DTO allowlist, and the `ListResult[T]` envelope the historical route reuses |
| P2 | `README.md` | 192–240 | The API and failure-mode tables this phase extends |
| P2 | `Makefile` | 28–58 | The exact gate commands |

## External Documentation

| Topic | Source | Key Takeaway |
|---|---|---|
| SSE wire format | WHATWG HTML §9.2 (server-sent events) | Fields are `id:`, `event:`, `data:`, and a bare `:` comment. Every frame ends with a **blank line**. A `data:` value must not contain a raw newline — JSON-encode the payload, which also escapes it. |
| `ContainerLogs` v29 signature | `go doc github.com/moby/moby/client.Client.ContainerLogs` | `(ctx, containerID string, options ContainerLogsOptions) (ContainerLogsResult, error)`. `ContainerLogsResult` is an **interface** — `interface { io.ReadCloser }` — not a struct with an `.Items` field like every other v29 result type. |
| Docker log framing | `github.com/moby/moby/api/pkg/stdcopy` package doc | Non-TTY streams are `[8]byte{TYPE,0,0,0,SIZE1..SIZE4}` + payload, size big-endian `uint32`. TTY streams are raw with **no** framing. |
| `http.ResponseController` | Go stdlib (1.20+) | `Flush()` and `SetWriteDeadline()` traverse wrappers via an `Unwrap() http.ResponseWriter` method. A wrapper without one yields `http.ErrNotSupported`. |

### Verified SDK signatures — transcribe exactly

```go
// github.com/moby/moby/client (v29 API), module github.com/moby/moby/client v0.5.1
func (cli *Client) ContainerLogs(ctx context.Context, containerID string, options ContainerLogsOptions) (ContainerLogsResult, error)

type ContainerLogsResult interface{ io.ReadCloser }   // an INTERFACE, not a struct

type ContainerLogsOptions struct {
    ShowStdout bool
    ShowStderr bool
    Since      string   // Unix timestamp or RFC3339
    Until      string
    Timestamps bool
    Follow     bool
    Tail       string   // a STRING: "all", or a decimal count
    Details    bool
}

// github.com/moby/moby/api/pkg/stdcopy (module github.com/moby/moby/api v1.55.0 — already a direct dep)
func StdCopy(destOut, destErr io.Writer, multiplexedSource io.Reader) (written int64, _ error)
const ( Stdin StdType = 0; Stdout StdType = 1; Stderr StdType = 2 )

// github.com/moby/moby/api/types/container
container.Config.Tty  // bool — the TTY flag D5 reads; Config is a *Config and CAN BE NIL
```

### Gotchas that will not compile or will silently misbehave if guessed

```go
res, err := c.api.ContainerLogs(...)   // res is an io.ReadCloser directly.
                                       // res.Items does NOT exist. defer res.Close().
opts.Tail = strconv.Itoa(n)            // Tail is a string. Passing an int does not compile.
opts.ShowStdout, opts.ShowStderr = true, true
                                       // BOTH default to false. Leaving them unset returns
                                       // an EMPTY stream with a nil error — a "works, shows
                                       // nothing" bug with no error to trace.
opts.Timestamps = true                 // Always. D3's ts field and D6's resume cursor both
                                       // come from this prefix; without it there is no cursor.
```

---

## Patterns to Mirror

### NAMING_CONVENTION
```go
// SOURCE: internal/httpapi/reads.go:56-77
// Grouped const block; the doc comment on the block states WHY the values exist,
// then one comment per constant. Message constants are msg<Thing><Outcome>.
const (
	// msgInvalidRef is served when a path-supplied object reference fails
	// dockerx.ValidateRef. The request never reached the Engine.
	msgInvalidRef = "invalid object reference"

	// msgNotFound is served when the Engine confirms the object does not
	// exist. Safe to state plainly here: the caller is already an
	// authenticated device (requireDevice ran first) …
	msgNotFound = "not found"
)
```

### ERROR_HANDLING
```go
// SOURCE: internal/httpapi/reads.go:83-93
// Sentinel checks first via errors.Is, then log-with-context + terse client body.
func (s *Server) writeDockerError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, dockerx.ErrInvalidRef):
		s.writeError(w, http.StatusBadRequest, msgInvalidRef)
	case errors.Is(err, dockerx.ErrNotFound):
		s.writeError(w, http.StatusNotFound, msgNotFound)
	default:
		s.log.Error(op, slog.Any("err", err))
		s.writeError(w, http.StatusBadGateway, msgEngineUnavailable)
	}
}
```

```go
// SOURCE: internal/dockerx/errors.go:23-31
// Engine errors classified once, wrapped with %w and the operation name.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, err)
}
```

### SERVICE_PATTERN
```go
// SOURCE: internal/dockerx/containers.go:41-57
// Validate the ref BEFORE any Engine call, derive a timeout context, classify the error.
func (c *Client) InspectContainer(ctx context.Context, ref string) (ContainerDetail, error) {
	if err := ValidateRef(ref); err != nil {
		return ContainerDetail{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerDetail{}, classify("inspect container", err)
	}

	return toContainerDetail(res.Container), nil
}
```

### HTTP_HANDLER_PATTERN
```go
// SOURCE: internal/httpapi/reads.go:135-147
// Handler = guard, resolve inputs, call the reader, map the error, write. No logic in the body.
func (s *Server) handleInspectContainer(w http.ResponseWriter, r *http.Request) {
	if !s.requireDocker(w) {
		return
	}

	resp, err := s.dc.InspectContainer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeDockerError(w, "inspect container", err)
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}
```

### ROUTE_REGISTRATION
```go
// SOURCE: internal/httpapi/server.go:95-107
// Method-prefixed patterns; a local closure applies the shared guard chain so the
// per-route lines stay one line each and cannot drift apart.
read := func(pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpRead, h)))
}
read("GET /v1/containers", s.handleListContainers)
read("GET /v1/containers/{id}", s.handleInspectContainer)
```

### RESPONSE_HEADER_SET
```go
// SOURCE: internal/httpapi/respond.go:17-25
// Every response sets these three. The SSE writer sets the same three with a
// different Content-Type, plus its own streaming-specific headers.
w.Header().Set("Content-Type", "application/json")
w.Header().Set("Cache-Control", "no-store")
w.Header().Set("X-Content-Type-Options", "nosniff")
w.WriteHeader(code)
```

### DEFENSIVE_SLICE_INIT
```go
// SOURCE: internal/dockerx/containers.go:68-74
// Never a nil slice: nil marshals to null, and the app would have to handle
// both null and [].
ports := make([]Port, 0, len(s.Ports))
for _, p := range s.Ports {
	ports = append(ports, toPort(p))
}
```

### TEST_STRUCTURE
```go
// SOURCE: internal/dockerx/client_test.go:18-48
// t.Parallel at both levels, a table of named cases, explicit // Arrange // Act // Assert,
// failure messages in the form: got = X, want Y.
func TestNewUnreachableEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
	}{
		{name: "missing unix socket", host: "unix:///nonexistent/docker.sock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			c, err := New(context.Background(), tt.host, testLogger())

			// Assert
			if err == nil {
				_ = c.Close()
				t.Fatalf("New(%q) error = nil, want an unreachable-engine failure", tt.host)
			}
		})
	}
}
```

### FIELD_ALLOWLIST_GUARD
```go
// SOURCE: internal/httpapi/status_test.go:74-105
// Assert the EXACT key count, not just presence, so a later phase cannot widen
// a payload silently.
if len(body) != 5 {
	t.Fatalf("status payload has %d keys (%v), want exactly 5", len(body), keysOf(body))
}
```

---

## API Contract (after this phase)

| Route | Auth | Policy op | Success | Body |
|---|---|---|---|---|
| `GET /v1/containers/{id}/logs?tail=<n>&since=<rfc3339>` | client cert | `logs` | 200 `application/json` | `{"items":[LogLine],"truncated":bool}` |
| `GET /v1/containers/{id}/logs/stream?tail=<n>&since=<rfc3339>` | client cert | `logs` | 200 `text/event-stream` | SSE frames, `event: log`, `id:` = line timestamp |

Query parameters, both routes:

| Parameter | Default | Bounds | Notes |
|---|---|---|---|
| `tail` | 200 (historical), 100 (stream) | 1…2000; out of range or unparsable → the default | A typo must not fail a diagnostic request mid-incident (mirrors `listAllParam`) |
| `since` | absent | must parse as RFC3339 / RFC3339Nano | **Unparsable → 400.** Unlike `tail`, this value reaches the Engine's request URL, so it is a boundary input, not a preference |

Failure modes:

| Condition | Status | Body |
|---|---|---|
| No / unknown / revoked client certificate | 401 | `{"error":"client certificate required"}` |
| Policy mode forbids `logs` | 403 | `{"error":"operation not permitted by host policy"}` |
| Reference fails `ValidateRef` | 400 | `{"error":"invalid object reference"}` |
| `since` does not parse | 400 | `{"error":"invalid since timestamp"}` |
| Container does not exist | 404 | `{"error":"not found"}` |
| Engine unreachable / timed out / other Engine fault | 502 | `{"error":"docker engine unavailable"}` |
| Stream slots exhausted (stream route only) | 503 | `{"error":"too many concurrent log streams"}` |
| Wrong method on a known path | 405 | ServeMux default |
| Engine fails **after** the stream opened | 200 (already sent) | terminal SSE `event: error`, `data: {"error":"docker engine unavailable"}`, then close |

### DTO field allowlist

```go
// internal/dockerx/types.go — joins the existing allowlist

// LogLine is one demultiplexed line of container output. Unlike every other DTO
// in this package, its payload is not a projection of Engine metadata — Line is
// whatever the container printed, which is precisely what the operator asked
// for. That makes it the one field in the codebase that may legitimately carry
// a secret, and the reason D16 forbids writing it to the agent's own log.
type LogLine struct {
	Timestamp string `json:"ts"`                  // RFC3339Nano; "" if the Engine emitted no parsable prefix
	Stream    string `json:"stream"`              // "stdout" or "stderr"
	Line      string `json:"line"`
	Truncated bool   `json:"truncated,omitempty"` // set when the line exceeded maxLogLineBytes
}
```

`Truncated` is the only `omitempty` field, so an ordinary line marshals to exactly three keys and a
truncated one to four. The field-count guard in Task 10 asserts both.

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `internal/dockerx/types.go` | UPDATE | Add `LogLine` to the DTO allowlist |
| `internal/dockerx/framing.go` | CREATE | The stdcopy frame reader and the per-stream line splitter (D4) |
| `internal/dockerx/framing_test.go` | CREATE | Frame parsing, split lines, oversized lines, truncated frames |
| `internal/dockerx/logs.go` | CREATE | `LogOptions`, `ContainerLogs`, `StreamContainerLogs`, TTY detection |
| `internal/dockerx/logs_test.go` | CREATE | Option mapping, timestamp extraction, emit-error propagation |
| `internal/dockerx/errors.go` | UPDATE | Add `ErrInvalidSince` beside the existing sentinels |
| `internal/dockerx/errors_test.go` | UPDATE | Cover the new sentinel |
| `internal/httpapi/sse.go` | CREATE | The SSE writer: lazy headers (D12), deadline clearing (D7), flush, keepalive |
| `internal/httpapi/sse_test.go` | CREATE | Wire format, lazy start, keepalive, unflushable-writer failure |
| `internal/httpapi/logs.go` | CREATE | `LogReader`, the two handlers, `tailParam`/`sinceParam`, the stream semaphore |
| `internal/httpapi/logs_test.go` | CREATE | Both routes against a fake `LogReader` |
| `internal/httpapi/reads.go` | UPDATE | `DockerReader` embeds `LogReader` (D14) |
| `internal/httpapi/reads_test.go` | UPDATE | `fakeDocker` gains the two `LogReader` methods |
| `internal/httpapi/middleware.go` | UPDATE | `statusRecorder.Unwrap()` (D8) |
| `internal/httpapi/server.go` | UPDATE | Stream semaphore field, its init in `NewServer`, two routes |
| `internal/httpapi/server_test.go` | UPDATE | A regression test that `statusRecorder` stays flushable |
| `README.md` | UPDATE | Two API rows, the new failure modes, an SSE and resume section |

No `go.mod` change: `github.com/moby/moby/api` is already a direct dependency, and
`api/pkg/stdcopy` is consulted for its documented frame layout rather than imported (D4).

## NOT Building

- **WebSocket, or any bidirectional channel** — D1. Nothing flows client→server.
- **`Last-Event-ID` header handling** — the app resumes with the explicit `?since=` query parameter.
  Supporting both would be two resume mechanisms that can disagree. `id:` is still emitted, because
  it is where the client reads its cursor from.
- **Server-side log search, grep, or level filtering** — the client filters what it has. Passing a
  pattern through to the Engine widens the enumerated surface with no PRD requirement behind it.
- **Persisting or caching container logs** — PRD "What We're NOT Building", explicitly.
- **Logs for anything but containers** — there is no such Engine concept for images, networks, or volumes.
- **`?until=`** — a bounded historical window is `tail` plus `since`. `until` is a reporting feature
  for a product that keeps history; this one does not.
- **`?stdout=`/`?stderr=` selection** — both are always requested and each line is labelled. Letting
  the client suppress one server-side saves nothing and loses the interleaving D4 exists to preserve.
- **Rate limiting the stream routes** — Phase 6. D10's concurrency cap is a resource bound, not a
  rate limit, and is in scope because a stream's cost is measured in held connections rather than in
  requests per second.
- **Audit entries for log reads** — D15.
- **Adjusting `maxConcurrentStreams` or `maxLogLineBytes` from the environment** — D10.
- **The agent's own logs over the API** — the agent's `agent.log` is read via host access. Serving it
  over the same port that serves container logs would put the audit trail's neighbour on a client path.

---

## Step-by-Step Tasks

### Task 1: `statusRecorder.Unwrap()` — the prerequisite
- **ACTION**: Update `internal/httpapi/middleware.go`; add a regression test to `server_test.go`.
- **IMPLEMENT**:
  ```go
  // Unwrap exposes the wrapped ResponseWriter to http.ResponseController.
  //
  // Without this, ResponseController cannot find the underlying writer's Flush
  // or SetWriteDeadline and returns http.ErrNotSupported for both — because
  // statusRecorder embeds the ResponseWriter *interface*, whose method set is
  // only Header/Write/WriteHeader. The SSE stream in logs.go depends on both:
  // one to deliver each line, the other to escape the server's 30s WriteTimeout.
  func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
  ```
- **MIRROR**: The existing doc-comment voice in `middleware.go` — every comment names the failure it prevents.
- **IMPORTS**: none new.
- **GOTCHA**: This is verified behaviour, not a precaution. A probe against Go's own
  `ResponseController` confirms `Flush()` and `SetWriteDeadline()` both return
  `feature not supported` through a wrapper with no `Unwrap`, and return `nil` with one.
  **Do this task first.** Every stream test written before it will fail for a reason that points
  at the SSE code rather than at the middleware.
- **GOTCHA**: `statusRecorder` must stay a pointer receiver throughout — `withRequestLog` already
  passes `&statusRecorder{…}`.
- **VALIDATE**: A test that wraps `httptest.NewRecorder()` in a `statusRecorder`, calls
  `http.NewResponseController(rec).Flush()`, and asserts a nil error. It fails before the edit and
  passes after — that is the point of writing it.

### Task 2: `LogLine` and the `ErrInvalidSince` sentinel
- **ACTION**: Update `internal/dockerx/types.go`, `errors.go`, and `errors_test.go`.
- **IMPLEMENT**: `LogLine` exactly as specified in "DTO field allowlist", with its doc comment. In
  `errors.go`:
  ```go
  // ErrInvalidSince is returned when a caller-supplied resume cursor does not
  // parse as a timestamp. Like ErrInvalidRef it never reaches the Engine: the
  // value is interpolated into the Engine's request URL, so it is validated at
  // the agent's boundary rather than trusted to an upstream parser.
  var ErrInvalidSince = errors.New("invalid since timestamp")
  ```
- **MIRROR**: The sentinel style already in `errors.go`; NAMING_CONVENTION for the doc comments.
- **IMPORTS**: none new.
- **GOTCHA**: Do **not** extend `classify` to produce `ErrInvalidSince`. `classify` maps *Engine*
  errors; this one is raised before any Engine call, exactly like `ErrInvalidRef`.
- **VALIDATE**: `go build ./internal/dockerx/`; `go test ./internal/dockerx/ -race -run TestClassify`.

### Task 3: Frame parsing and line splitting
- **ACTION**: Create `internal/dockerx/framing.go` and `framing_test.go`.
- **IMPLEMENT**:
  ```go
  // Framing constants. Docker's non-TTY log stream is a sequence of
  // [8]byte{STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4} headers followed
  // by SIZE bytes of payload, size big-endian.
  const (
      frameHeaderLen = 8
      frameTypeIdx   = 0
      frameSizeOff   = 4

      streamStdout = "stdout"
      streamStderr = "stderr"

      // maxLogLineBytes bounds a single line. A container emitting one
      // enormous line — a stack dump, a base64 blob, a minified bundle — would
      // otherwise be accumulated whole in agent memory before any newline
      // arrived, which is the agent OOM-killing itself while reading logs.
      maxLogLineBytes = 8 << 10

      // maxFramePayloadBytes bounds one Engine frame. The size field is
      // attacker-influenced only via container output, but a corrupt or
      // truncated stream can present a 4 GiB length that make() would honour.
      maxFramePayloadBytes = 1 << 20
  )

  // readFrame reads one multiplexed frame. It returns io.EOF cleanly at a frame
  // boundary and io.ErrUnexpectedEOF for a stream cut mid-frame.
  func readFrame(r io.Reader) (stream string, payload []byte, err error)

  // lineSplitter accumulates payload bytes per stream and emits whole lines.
  // Docker's json-file driver usually frames one line at a time, but that is
  // not guaranteed by any driver contract: a frame may hold several lines or
  // half of one, so partial lines must be buffered across frames.
  type lineSplitter struct{ /* per-stream buffers */ }

  func (s *lineSplitter) push(stream string, payload []byte, emit func(LogLine) error) error
  func (s *lineSplitter) flush(emit func(LogLine) error) error
  ```
  `push` splits on `'\n'`, trims a trailing `'\r'`, extracts the timestamp prefix, and calls `emit`
  per complete line. `flush` emits any buffered remainder when the stream ends — a container's final
  line frequently has no trailing newline, and dropping it loses the panic message.
- **MIRROR**: DEFENSIVE_SLICE_INIT; ERROR_HANDLING (`fmt.Errorf` with `%w`).
- **IMPORTS**: `bytes`, `encoding/binary`, `fmt`, `io`, `strings`, `time`.
- **GOTCHA**: Use `io.ReadFull` for both the header and the payload, never a bare `Read` — a single
  `Read` on a network stream routinely returns a partial buffer, and treating that as a whole frame
  desynchronises the parser permanently: every subsequent line is garbage.
- **GOTCHA**: `io.ReadFull` returns `io.EOF` when it reads **zero** bytes and `io.ErrUnexpectedEOF`
  when it reads some but not all. Only the first is a clean end of stream. Conflating them turns
  every normal stream close into a logged error.
- **GOTCHA**: Timestamp extraction is `strings.Cut(line, " ")` on the RFC3339Nano prefix. If the
  prefix does not parse with `time.Parse(time.RFC3339Nano, …)`, keep the **whole original line** and
  leave `Timestamp` empty. Never drop a line because its timestamp looked odd — the malformed line
  is often the interesting one.
- **GOTCHA**: Enforce `maxLogLineBytes` on the *accumulating buffer*, not after a newline arrives.
  Checking afterwards means the unbounded growth has already happened.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run 'TestFrame|TestLineSplitter'`. Table cases:
  a two-frame stdout/stderr interleave, a frame carrying three lines, a line split across two frames,
  a 20 KiB line (→ cut at 8 KiB, `Truncated: true`), a header cut after 3 bytes
  (→ `io.ErrUnexpectedEOF`), a clean EOF at a boundary (→ `io.EOF`), a final line with no newline
  (→ emitted by `flush`).

### Task 4: `LogOptions` and TTY detection
- **ACTION**: Create `internal/dockerx/logs.go` (first half).
- **IMPLEMENT**:
  ```go
  // LogOptions is the validated, agent-side view of a log request. Both fields
  // are bounded by the caller in httpapi before they arrive here; this struct
  // re-validates Since because it is what reaches the Engine's request URL.
  type LogOptions struct {
      Tail  int    // lines to seed with; <= 0 means "all"
      Since string // RFC3339Nano cursor; "" means from the beginning
  }

  // engineOptions maps LogOptions onto the SDK's option struct.
  //
  // ShowStdout and ShowStderr both default to false, and an unset pair yields
  // an empty stream with a nil error — a silent no-output bug with nothing in
  // it to trace. Timestamps is likewise mandatory rather than optional: the
  // per-line ts field and the client's resume cursor are both derived from it.
  func (o LogOptions) engineOptions(follow bool) (client.ContainerLogsOptions, error)

  // containerUsesTTY reports whether ref was created with a TTY. A TTY stream
  // carries no multiplexing headers at all, so the answer selects between two
  // incompatible readers; there is no way to detect it from the stream itself.
  func (c *Client) containerUsesTTY(ctx context.Context, ref string) (bool, error)
  ```
  `engineOptions` returns `ErrInvalidSince` when `Since` is non-empty and fails
  `time.Parse(time.RFC3339Nano, …)`; sets `Tail: strconv.Itoa(o.Tail)` when `Tail > 0` and `"all"`
  otherwise. `containerUsesTTY` calls `ValidateRef`, derives a `callTimeout` context, calls
  `ContainerInspect`, `classify`s the error, and reads `res.Container.Config.Tty`.
- **MIRROR**: SERVICE_PATTERN — validate first, timeout context, `classify`.
- **IMPORTS**: `context`, `strconv`, `time`, `github.com/moby/moby/client`.
- **GOTCHA**: `Config` is a `*container.Config` and **can be nil** (Phase 3 documents this and its
  mappers guard it). A nil `Config` means "assume no TTY" — the common case — not a panic.
- **GOTCHA**: `Tail` is a **string** in `ContainerLogsOptions`. `Tail: o.Tail` does not compile.
- **GOTCHA**: `containerUsesTTY` is where 404 comes from for both routes. It must run before any
  streaming call so a missing container is a clean 404 rather than a stream that opens and
  immediately errors (D12).
- **VALIDATE**: `go build ./internal/dockerx/`; a table test over `engineOptions` asserting
  `ShowStdout && ShowStderr && Timestamps` are all true, `Tail` mapping, and `ErrInvalidSince` on
  `"yesterday"`, `"1700000000"`, and `""`+non-empty-garbage.

### Task 5: Historical and streaming reads
- **ACTION**: Complete `internal/dockerx/logs.go`; create `logs_test.go`.
- **IMPLEMENT**:
  ```go
  // maxHistoricalLines caps the bounded fetch. Mirrors maxListItems' reasoning:
  // a phone on a mobile connection must not receive a body the size of a log
  // file, and the cap is server-side because startup configuration — not the
  // client — is this agent's security boundary.
  const maxHistoricalLines = 2000

  // ContainerLogs returns a bounded slice of recent lines.
  func (c *Client) ContainerLogs(ctx context.Context, ref string, opts LogOptions) (ListResult[LogLine], error)

  // StreamContainerLogs follows ref's output, calling emit once per line until
  // the container stops, ctx is cancelled, or emit returns an error.
  //
  // emit is a callback rather than a returned channel deliberately: the caller
  // needs to flush after every line and to abort the stream the instant a
  // client write fails. A channel would need a producer goroutine, a second
  // error channel, and close semantics careful enough to survive a client that
  // vanishes mid-line — three places to leak a goroutine instead of none.
  func (c *Client) StreamContainerLogs(ctx context.Context, ref string, opts LogOptions, emit func(LogLine) error) error
  ```
  Both call `containerUsesTTY` first, then `engineOptions`, then `ContainerLogs` on the SDK.
  Non-TTY drives `readFrame` + `lineSplitter`; TTY reads the body directly through the same
  `lineSplitter` with a fixed `streamStdout` label.
- **MIRROR**: `ListContainers`' truncate-after-mapping shape; SERVICE_PATTERN.
- **IMPORTS**: add `io`, `errors`.
- **GOTCHA**: `ContainerLogsResult` is an **interface** (`interface{ io.ReadCloser }`), unlike every
  other v29 result type. There is no `.Items`. `defer func() { _ = res.Close() }()` — leaking it
  holds an Engine connection for the process's life.
- **GOTCHA**: **Do not apply `callTimeout` to the streaming call.** It bounds the pre-stream inspect
  and the historical fetch only. Wrapping the follow call in a 15s context kills every stream at 15
  seconds — and the PRD's success signal is a 30-*minute* stream.
- **GOTCHA**: Propagate `emit`'s error out of `StreamContainerLogs` unchanged and stop reading. That
  error is "the client went away", and treating it as an Engine fault produces a 502 log line for
  every user who closes a log view.
- **GOTCHA**: Treat `io.EOF` from the frame reader as a clean end, never an error. A container that
  exits ends its own log stream; that is normal, and returning an error makes every stopped container
  look like a failure.
- **GOTCHA**: `ContainerLogs` (historical) must call `splitter.flush(...)` after the read loop, or a
  container whose last line lacks a trailing newline silently loses it.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run TestLog`; `dockerx` package coverage ≥ 80%.
  Drive the framing and splitting logic through in-memory readers — a live daemon is deliberately not
  required (`client_test.go`).

### Task 6: The SSE writer
- **ACTION**: Create `internal/httpapi/sse.go` and `sse_test.go`.
- **IMPLEMENT**:
  ```go
  // SSE framing and liveness constants.
  const (
      // keepaliveInterval bounds how long the stream may be silent. A container
      // that logs nothing for minutes is ordinary; a TCP connection that sends
      // nothing for minutes is dropped by mobile-carrier NAT and by any proxy
      // in between. The keepalive write is also the only way this agent learns
      // the client is gone — the write fails, and the stream unwinds. Without
      // it an abandoned stream leaks a goroutine and an Engine connection.
      keepaliveInterval = 20 * time.Second

      sseContentType = "text/event-stream"
      sseEventLog    = "log"
      sseEventError  = "error"
  )

  // sseWriter frames one SSE response. Headers are written lazily, on the first
  // event: once 200 and the headers are committed the status can never be
  // corrected, so deferring the commit keeps the 400/404/502 mapping available
  // for every failure that happens before there is anything to send.
  type sseWriter struct {
      w       http.ResponseWriter
      rc      *http.ResponseController
      started bool
  }

  func newSSEWriter(w http.ResponseWriter) *sseWriter
  func (s *sseWriter) start() error                     // headers + clear deadline + flush
  func (s *sseWriter) event(id, name string, data any) error
  func (s *sseWriter) keepalive() error                 // ": keepalive\n\n"
  func (s *sseWriter) Started() bool
  ```
  `start()` sets `Content-Type: text/event-stream`, plus `Cache-Control: no-store`,
  `X-Content-Type-Options: nosniff`, `Connection: keep-alive`, and `X-Accel-Buffering: no`; calls
  `s.rc.SetWriteDeadline(time.Time{})`; writes `200`; flushes. `event()` calls `start()` when not
  yet started, JSON-encodes `data`, writes `id:`/`event:`/`data:` lines and the terminating blank
  line, then flushes.
- **MIRROR**: RESPONSE_HEADER_SET — the same three headers `writeJSON` sets, with a different type.
- **IMPORTS**: `encoding/json`, `fmt`, `net/http`, `time`.
- **GOTCHA**: **Every frame ends with a blank line.** A frame without one is buffered by the client's
  SSE parser until the *next* frame arrives — which looks exactly like "the stream is one message
  behind" and is nearly impossible to diagnose from the app side.
- **GOTCHA**: `data:` must never contain a raw newline. JSON-encoding the payload escapes them, which
  is a second reason the payload is a JSON object rather than the bare line text.
- **GOTCHA**: `json.Encoder.Encode` appends its own `'\n'`. Use `json.Marshal` and write the bytes,
  or the frame gains a stray line break in the middle.
- **GOTCHA**: `SetWriteDeadline(time.Time{})` clears the deadline; `SetWriteDeadline(time.Now())`
  expires it immediately. The zero `time.Time` is the correct argument and the two are one character
  apart in intent.
- **GOTCHA**: A `SetWriteDeadline` failure is fatal to the stream — surface it from `start()` rather
  than ignoring it. Silently continuing produces a stream that dies at 30s with no explanation, which
  is precisely the D7/D8 failure this plan exists to prevent.
- **VALIDATE**: `go test ./internal/httpapi/ -race -run TestSSE`. Assert the exact bytes for one
  event; assert `Started()` is false before the first event and true after; assert `start()` returns
  an error when given a writer whose `ResponseController` cannot flush.

### Task 7: `LogReader` and the two handlers
- **ACTION**: Create `internal/httpapi/logs.go`; update `internal/httpapi/reads.go`.
- **IMPLEMENT**:
  ```go
  // LogReader is the container-log surface, named separately from the four read
  // interfaces because a log is a stream rather than a projection: one of its
  // two methods never returns until the caller stops it.
  type LogReader interface {
      ContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error)
      StreamContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions, emit func(dockerx.LogLine) error) error
  }
  ```
  In `reads.go`, add `LogReader` to `DockerReader`'s embedded set. The existing
  `var _ DockerReader = (*dockerx.Client)(nil)` then proves the concrete client satisfies it too, in
  the package that owns the contract.

  Constants and parameter parsing in `logs.go`:
  ```go
  const (
      defaultHistoricalTail = 200
      defaultStreamTail     = 100

      // msgTooManyStreams is served when every stream slot is in use. Unlike the
      // other rejections this one is specific and actionable: the caller is an
      // authenticated device and the fix is on its side — close a log view.
      msgTooManyStreams = "too many concurrent log streams"

      // msgInvalidSince is served for an unparsable resume cursor. Unlike tail,
      // since is not defaulted on a parse failure: it reaches the Engine's
      // request URL, which makes it a boundary input rather than a preference.
      msgInvalidSince = "invalid since timestamp"
  )

  func tailParam(r *http.Request, def int) int          // out of range or unparsable -> def
  func sinceParam(r *http.Request) (string, error)      // unparsable -> ErrInvalidSince

  func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request)
  func (s *Server) handleStreamContainerLogs(w http.ResponseWriter, r *http.Request)
  ```
  `handleStreamContainerLogs`: `requireDocker` → `sinceParam` → acquire a stream slot
  (non-blocking, 503 on failure, `defer` release) → build the `sseWriter` → start a
  `time.Ticker` at `keepaliveInterval` in a goroutine writing keepalives → call
  `StreamContainerLogs` with an `emit` that writes one `event: log` frame → on return, if
  `!sse.Started()` map the error with `writeDockerError`, otherwise emit a terminal
  `event: error` frame and stop.
- **MIRROR**: HTTP_HANDLER_PATTERN; ERROR_HANDLING; NAMING_CONVENTION for the `msg*` block.
- **IMPORTS**: `context`, `errors`, `log/slog`, `net/http`, `strconv`, `time`,
  `github.com/scnplt/devmon-agent/internal/dockerx`.
- **GOTCHA**: The keepalive goroutine and `emit` both write to the same `http.ResponseWriter`, which
  is **not safe for concurrent use**. Guard the `sseWriter` with a `sync.Mutex` held across each
  whole frame. `-race` will catch this, but only if a test actually runs a keepalive concurrently
  with a line — write that test.
- **GOTCHA**: The keepalive goroutine must exit when the handler returns, or every stream leaks one
  goroutine and a ticker for the process's life. Derive a `context.WithCancel` from
  `r.Context()`, `defer cancel()`, and select on `ctx.Done()` in the ticker loop. `defer ticker.Stop()`
  as well — `Stop` alone does not close the channel, and the goroutine still needs its exit signal.
- **GOTCHA**: Release the stream slot with `defer`, on **every** path including the 400 and 404 ones.
  A slot leaked on an error path is permanent: after eight bad requests the route answers 503 forever
  and only a restart clears it.
- **GOTCHA**: `emit` returning an error must abort the stream, not be logged and swallowed. It means
  the client's socket is gone.
- **GOTCHA**: Never log the `LogLine.Line` content — D16. Log the container ref and the error, never
  the payload, at any level.
- **VALIDATE**: `go build ./...`.

### Task 8: Wire the routes and the semaphore
- **ACTION**: Update `internal/httpapi/server.go`.
- **IMPLEMENT**:
  ```go
  // maxConcurrentStreams bounds simultaneous live log streams. Each holds a
  // goroutine, an Engine connection, and a socket for its entire life, so an
  // unbounded count is a file-descriptor exhaustion the agent inflicts on the
  // host it exists to protect. A constant rather than an env var: the PRD's
  // rule is that every extra startup setting is surface the operator has to
  // understand at install time, and eight concurrent log views on one phone is
  // already beyond any real use.
  const maxConcurrentStreams = 8
  ```
  `Server` gains `streams chan struct{}`, initialised in `NewServer` as
  `make(chan struct{}, maxConcurrentStreams)`. In `routes()`, beside the `read` closure:
  ```go
  // Log routes. Same double guard as the read routes, with policy.OpLogs —
  // which, like OpRead, every mode permits (see internal/policy/mode.go).
  logs := func(pattern string, h http.HandlerFunc) {
      mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpLogs, h)))
  }
  logs("GET /v1/containers/{id}/logs", s.handleContainerLogs)
  logs("GET /v1/containers/{id}/logs/stream", s.handleStreamContainerLogs)
  ```
- **MIRROR**: ROUTE_REGISTRATION.
- **GOTCHA**: `NewServer`'s signature does **not** change (D14), so no test helper needs touching.
  Resist adding a parameter — four helpers across three files depend on the current arity.
- **GOTCHA**: Initialise `streams` in `NewServer`, not lazily in the handler. A nil channel blocks
  forever on send and *never* succeeds on a non-blocking select — every stream would answer 503.
- **GOTCHA**: `ServeMux` matches the more specific pattern, so `/v1/containers/{id}/logs/stream` does
  not collide with `/v1/containers/{id}` or with `/v1/containers/{id}/logs`. Add a route-precedence
  test rather than trusting it — the three patterns are one path segment apart.
- **VALIDATE**: `go build ./... && go test ./internal/... -race` — every pre-existing test still passes.

### Task 9: Handler tests
- **ACTION**: Create `internal/httpapi/logs_test.go`; update `internal/httpapi/reads_test.go`.
- **IMPLEMENT**:
  - Extend `fakeDocker` with `containerLogsFn` and `streamContainerLogsFn` fields plus the two
    methods, so it satisfies the widened `DockerReader`. Existing tests that leave them nil keep
    working — give each a nil-safe default.
  - Historical route: 200 with the expected body, 404 on `ErrNotFound`, 502 on a generic error,
    400 on `ErrInvalidRef`, 400 on `?since=yesterday`, 401 with no client certificate, 405 on `POST`.
  - Stream route: a fake whose `StreamContainerLogs` emits three lines then returns nil — assert the
    exact SSE bytes, the `id:` values, and `Content-Type: text/event-stream`.
  - `TestStreamErrorBeforeFirstLine`: fake returns `ErrNotFound` without emitting → **404 JSON**, not
    a 200 stream. This is D12's guarantee and the easiest thing in the phase to get wrong.
  - `TestStreamErrorAfterFirstLine`: fake emits one line then returns a generic error → 200, one
    `event: log`, then a terminal `event: error`.
  - `TestStreamSlotExhaustion`: hold `maxConcurrentStreams` streams open, assert the next is 503, then
    release one and assert the following request succeeds — proving the slot is returned.
  - `TestStreamSlotReleasedOnError`: issue `maxConcurrentStreams + 1` requests that all fail with 404,
    then assert a valid stream still succeeds. Catches the leaked-slot bug directly.
  - `TestStreamKeepaliveIsRaceFree`: run a fake that emits lines while a keepalive tick fires, under
    `-race`. Inject the interval rather than waiting 20 real seconds — make `keepaliveInterval` a
    package variable the test can shorten, or thread it through `newSSEWriter`.
  - `TestLogLineFieldCount`: three keys for an ordinary line, four for a truncated one.
  - `TestAgentLogNeverCarriesLineContent`: emit a line containing `"hunter2"` through a handler whose
    logger writes to a buffer; assert the buffer never contains it. D16's enforcement.
- **MIRROR**: TEST_STRUCTURE; FIELD_ALLOWLIST_GUARD.
- **GOTCHA**: Guarded routes need `req.TLS` set and a matching device in a real store — reuse
  `testServerWithStore` + `peerCertWithSerial` rather than inventing a second mechanism.
- **GOTCHA**: `httptest.ResponseRecorder` implements `Flush`, so `ResponseController` works against
  it **once Task 1 lands**. If SSE tests fail with `feature not supported`, Task 1 is missing —
  do not work around it in the SSE code.
- **GOTCHA**: Streaming tests must not wait on wall-clock time. Drive the fake's emissions and the
  handler's return explicitly, and give every test a context deadline so a hang fails fast rather
  than blocking the suite for ten minutes.
- **VALIDATE**: `make cover` — `./internal/...` total ≥ 80%.

### Task 10: `dockerx` log tests
- **ACTION**: Complete `internal/dockerx/logs_test.go` and `framing_test.go`.
- **IMPLEMENT**: The full edge-case table below, driven through `bytes.Reader` and `io.Pipe` fixtures
  so no daemon is required. Include `TestStreamPropagatesEmitError` (emit returns an error → the
  reader stops and the error surfaces unchanged) and `TestTTYStreamHasNoFraming` (a TTY-mode read of
  raw text yields lines labelled `stdout`, with none of the header bytes appearing in the output).
- **MIRROR**: TEST_STRUCTURE.
- **GOTCHA**: Build the multiplexed fixtures with an explicit helper that writes the 8-byte header —
  hand-typed hex in a test literal is where an off-by-one in the size offset hides, and it would make
  the test agree with a wrong parser.
- **VALIDATE**: `go test ./internal/dockerx/ -race -cover` — package coverage ≥ 80%.

### Task 11: Docs and the manual sweep
- **ACTION**: Update `README.md`.
- **IMPLEMENT**: Two rows in the API table; the 503 and `invalid since` rows in the failure-mode
  table. Then a **Live log streaming** subsection covering: SSE rather than WebSocket and why (D1);
  the `LogLine` shape; the `?since=` resume contract and its **at-least-once** honesty (D6); the
  8 KiB line cap and the `truncated` flag (D9); the 8-stream concurrent limit and its 503 (D10);
  and a plain statement that **container logs are never persisted by the agent** — the PRD's
  exclusion, restated where an operator worried about disk will actually look.
- **MIRROR**: The README's voice — every statement carries its reason, as in the existing
  "Why responses are projections" section.
- **GOTCHA**: English only, per CLAUDE.md.
- **GOTCHA**: The projections section says environment variables are never returned. Add one sentence
  distinguishing that from log content, which *is* returned in full: the operator explicitly asked
  for it, and it is the point of the feature. Leaving that unsaid makes the two sections look
  contradictory to a security reviewer in Phase 6.
- **VALIDATE**: The full gate sweep plus the manual checklist below.

---

## Testing Strategy

### Unit Tests

| Test | Input | Expected Output | Edge Case? |
|---|---|---|---|
| `TestStatusRecorderIsFlushable` | `statusRecorder` over a recorder | `ResponseController.Flush()` returns nil | **yes** |
| `TestReadFrameStdoutStderr` | two frames, types 1 and 2 | `"stdout"`, `"stderr"` payloads in order | no |
| `TestReadFramePartialHeader` | 3 bytes then EOF | `io.ErrUnexpectedEOF` | **yes** |
| `TestReadFramePartialPayload` | header says 100, 40 bytes follow | `io.ErrUnexpectedEOF` | **yes** |
| `TestReadFrameCleanEOF` | EOF at a frame boundary | `io.EOF` | no |
| `TestReadFrameOversizedLength` | header claims 4 GiB | error, no giant allocation | **yes** |
| `TestLineSplitterMultiLineFrame` | one frame, three `\n`-separated lines | three `LogLine`s | no |
| `TestLineSplitterSplitAcrossFrames` | one line split over two frames | one `LogLine` | **yes** |
| `TestLineSplitterFinalLineNoNewline` | payload with no trailing `\n`, then `flush` | the line is emitted | **yes** |
| `TestLineSplitterOversizedLine` | a 20 KiB line | cut at 8 KiB, `Truncated: true` | **yes** |
| `TestLineSplitterCRLF` | `"text\r\n"` | `line == "text"` | yes |
| `TestTimestampExtraction` | `"2026-08-08T10:02:11.441Z hello"` | `ts` set, `line == "hello"` | no |
| `TestTimestampUnparsable` | `"notatimestamp hello world"` | `ts == ""`, whole line preserved | **yes** |
| `TestEngineOptionsAlwaysBothStreams` | any `LogOptions` | `ShowStdout && ShowStderr && Timestamps` | **yes** |
| `TestEngineOptionsTail` | `Tail: 50` / `Tail: 0` | `"50"` / `"all"` | no |
| `TestEngineOptionsInvalidSince` | `"yesterday"`, `"1700000000"` | `ErrInvalidSince` | **yes** |
| `TestContainerUsesTTYNilConfig` | `InspectResponse{Config: nil}` | `false`, no panic | **yes** |
| `TestStreamPropagatesEmitError` | `emit` returns an error on line 2 | that error returned; reading stops | **yes** |
| `TestTTYStreamHasNoFraming` | raw text, TTY mode | lines labelled `stdout`, no header bytes | **yes** |
| `TestSSEFrameBytes` | one event | exact `id:`/`event:`/`data:` + blank line | no |
| `TestSSELazyStart` | no events emitted | no header written; `Started()` false | **yes** |
| `TestSSEDataHasNoRawNewline` | a line containing `\n` | JSON-escaped, one `data:` line | **yes** |
| `TestSSEUnflushableWriter` | writer with no `Unwrap`/`Flush` | `start()` returns an error | **yes** |
| `TestHistoricalLogsOK` | fake returns 2 lines | 200, JSON body, `truncated:false` | no |
| `TestHistoricalTailBounds` | `?tail=0`, `9999`, `abc`, absent | fake sees `200,2000,200,200` | **yes** |
| `TestLogsInvalidSince` | `?since=yesterday` | 400, `msgInvalidSince` | **yes** |
| `TestStreamErrorBeforeFirstLine` | fake returns `ErrNotFound`, emits nothing | **404 JSON**, not a 200 stream | **yes** |
| `TestStreamErrorAfterFirstLine` | one line, then a generic error | 200, `event: log`, then `event: error` | **yes** |
| `TestStreamSlotExhaustion` | 8 held + 1 more | 503, `msgTooManyStreams` | **yes** |
| `TestStreamSlotReleasedOnError` | 9 failing requests, then a valid one | the valid one succeeds | **yes** |
| `TestStreamKeepaliveIsRaceFree` | keepalive + emit concurrently, `-race` | no race reported | **yes** |
| `TestStreamGoroutineDoesNotLeak` | open and close 20 streams | goroutine count returns to baseline | **yes** |
| `TestLogRoutesRequireDevice` × 2 | no `req.TLS` | 401, `msgClientCertRequired` | yes |
| `TestLogRoutesRejectOtherMethods` | `POST` on both | 405 | yes |
| `TestLogRoutePrecedence` | `/logs` vs `/logs/stream` vs `/{id}` | each reaches its own handler | **yes** |
| `TestLogLineFieldCount` | ordinary / truncated line | 3 keys / 4 keys | guard |
| `TestAgentLogNeverCarriesLineContent` | a line containing `"hunter2"` | agent log buffer has no `hunter2` | **yes** |
| `TestNilDockerReaderOnLogRoutes` | `s.dc == nil` | 502, no panic | **yes** |
| `TestLogErrorBodiesLeakNothing` | every failure path | no `StateDir`, `docker.sock`, `devmon.db` | **yes** |

### Edge Cases Checklist
- [ ] Container that has produced no output at all → `{"items":[],"truncated":false}` / an open, silent stream
- [ ] Container with a TTY (`docker run -t`) → no header bytes in the output
- [ ] Container without a TTY → stdout and stderr correctly labelled and **interleaved in order**
- [ ] A line larger than 8 KiB → cut and marked
- [ ] A line split across two Engine frames → reassembled
- [ ] Final line with no trailing newline → still emitted
- [ ] Container exits while the stream is open → clean end, no error frame
- [ ] Client disconnects mid-stream → handler returns, goroutine and slot released
- [ ] Engine socket removed mid-stream → terminal `event: error`, agent stays up
- [ ] `?since=` in the future → empty stream, not an error
- [ ] `?since=` at the exact boundary → at-least-once, may repeat one line (documented)
- [ ] Ninth concurrent stream → 503
- [ ] `read-only` policy mode → both routes still 200 (`OpLogs` is `ModeReadOnly`)
- [ ] Revoked device → 401 on both routes, mid-stream reconnect refused
- [ ] Stream open across the 30s `WriteTimeout` boundary → still delivering at 60s

---

## Validation Commands

### Static Analysis
```bash
gofmt -l .
go vet ./...
```
EXPECT: both print nothing. (`gofmt -l .` reports the whole tree on this checkout because of CRLF
line endings — compare against the pre-change output rather than expecting silence.)

### Lint
```bash
make lint
```
EXPECT: clean.

### Security Scan
```bash
gosec ./...
```
EXPECT: no findings. Watch for G104 (unhandled errors) around the `Close`, `Flush`, and
`SetWriteDeadline` calls, and for G115 (integer conversion) on the frame size `uint32`.

### Unit Tests
```bash
go test ./internal/dockerx/ ./internal/httpapi/ -race -v
```
EXPECT: all pass. `-race` is not optional here — the keepalive goroutine writing beside `emit` is the
one concurrency bug this phase can introduce, and it is invisible without it.

### Full Test Suite with Coverage
```bash
make cover
```
EXPECT: all packages pass; the total line is ≥ 80%.

### Build Verification
```bash
make build
CGO_ENABLED=0 go build ./...
make image
```
EXPECT: static binary; image builds.

### Module Hygiene
```bash
go mod tidy
git diff go.mod
```
EXPECT: **no change.** This phase adds no dependency; a diff here means something was imported that
should not have been.

### End-to-End Validation (real host with real workloads)
```bash
CURL="curl --cacert ca.pem --cert device.pem --key device.key"
CID=$(docker ps -q | head -1)

# Historical
$CURL "https://$HOST:8443/v1/containers/$CID/logs?tail=20" | jq '.items | length'
$CURL "https://$HOST:8443/v1/containers/$CID/logs?tail=5"  | jq -r '.items[] | "\(.stream) \(.line)"'

# Live — leave running, then restart the container in another shell
$CURL -N "https://$HOST:8443/v1/containers/$CID/logs/stream?tail=5"

# Resume from a cursor
$CURL "https://$HOST:8443/v1/containers/$CID/logs/stream?since=2026-08-08T10:02:14.882Z"

# Failures
$CURL -o /dev/null -w '%{http_code}\n' "https://$HOST:8443/v1/containers/nope/logs"          # 404
$CURL -o /dev/null -w '%{http_code}\n' "https://$HOST:8443/v1/containers/$CID/logs?since=x"  # 400
curl -o /dev/null -w '%{http_code}\n' --cacert ca.pem \
     "https://$HOST:8443/v1/containers/$CID/logs"                                            # 401

# Concurrency cap: open 8 streams, then try a 9th
for i in $(seq 8); do $CURL -N "https://$HOST:8443/v1/containers/$CID/logs/stream" & done
sleep 2
$CURL -o /dev/null -w '%{http_code}\n' -N "https://$HOST:8443/v1/containers/$CID/logs/stream"  # 503
kill %1 %2 %3 %4 %5 %6 %7 %8
```

### Manual Validation
- [ ] **The 30s test.** Open a stream against a container logging once a minute; confirm it is still
      delivering at 60s and at 5 minutes. Failure here means Task 1 or D7 was skipped.
- [ ] **The 30-minute endurance run** (PRD success signal). Keep a stream open 30+ minutes on a real
      device over mobile data; confirm no gaps and no disconnect.
- [ ] **Handover.** Mid-stream, switch the device from Wi-Fi to mobile data. Confirm the app
      reconnects with `?since=<last id>` and that the log resumes with at most one repeated line.
- [ ] **TTY.** `docker run -t` a container; confirm no stray bytes at the start of any line.
- [ ] **Interleaving.** Run a container writing to both stdout and stderr in a known alternating
      order; confirm the response preserves it.
- [ ] **Oversized line.** `head -c 20000 /dev/urandom | base64` inside a container; confirm the line
      is cut at 8 KiB and marked `truncated:true`, and that the agent's memory does not spike.
- [ ] **Silent container.** Stream a container producing nothing for 5 minutes; confirm the
      connection stays open (keepalive comments visible with `curl -N`).
- [ ] **Abandoned stream.** Open a stream, kill the client (`Ctrl-C`), wait 30s; confirm the agent
      logs the stream ending and that a `docker exec` count of the agent's goroutines/connections
      returns to baseline.
- [ ] **Container exit.** Stream a container, then `docker stop` it; confirm a clean end rather than
      an error frame.
- [ ] **Engine death mid-stream.** Stop the Docker daemon while a stream is open; confirm a terminal
      `event: error` and that the agent container stays up.
- [ ] **Read-only mode.** Start with `DEVMON_POLICY_MODE=read-only`; confirm both routes still 200.
- [ ] **Revocation.** Revoke the device from the host mid-stream; confirm the reconnect is refused
      401 without restarting the agent.
- [ ] **Log hygiene.** Print a recognisable secret from a container, read it through both routes, then
      grep `agent.log` for it — it must not appear. D16.

---

## Acceptance Criteria
- [ ] All 11 tasks complete
- [ ] Two routes registered, each behind `requireDevice` **and** `requireOp(policy.OpLogs)`
- [ ] `statusRecorder` implements `Unwrap`, proven by a test that fails without it
- [ ] A stream survives well past the server's 30s `WriteTimeout`, proven on a real host
- [ ] Non-TTY output is demultiplexed with stdout/stderr labelled and **ordering preserved**;
      TTY output carries no header bytes
- [ ] An error before the first line yields a JSON 400/404/502; an error after it yields a terminal
      SSE `event: error` on an already-200 response
- [ ] Stream slots are released on every path, proven by a test that exhausts them via failures
- [ ] No container log content is ever written to the agent's own log, at any level
- [ ] `gofmt`, `go vet`, `golangci-lint`, `gosec` all clean; `go mod tidy` produces no diff
- [ ] `./internal/...` coverage ≥ 80%
- [ ] A live stream runs 30+ minutes across a network handover without losing data (PRD Phase 4
      success signal)

## Completion Checklist
- [ ] Code follows the patterns captured above
- [ ] Errors wrapped with `%w` and named context
- [ ] Logging uses `slog` typed attrs; failures logged with context, returned terse
- [ ] Tests are table-driven, `t.Parallel`, AAA-commented, and never wait on wall-clock time
- [ ] No hardcoded values — every bound is a named constant with a comment saying why
- [ ] README API table, failure modes, and the streaming section updated
- [ ] `go.mod` unchanged
- [ ] PRD Phase 4 row moved to `complete` with links to this plan and its report
- [ ] No scope beyond the two log routes

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| The stream dies at exactly 30s because the write deadline was never cleared | **H** | HIGH | D7 + D8; Task 1 lands first; a manual 60s check is the first item on the sweep |
| `ResponseController` silently fails because `statusRecorder` has no `Unwrap` | **H** | HIGH | Verified empirically, not assumed; Task 1's test fails before the fix and passes after |
| Keepalive goroutine races `emit` on the same `ResponseWriter` | **H** | HIGH | A mutex across whole frames; a `-race` test that runs both concurrently |
| A goroutine or stream slot leaks per abandoned stream, degrading the agent over days | M | HIGH | `defer cancel()`, `defer ticker.Stop()`, `defer` slot release on every path; a dedicated leak test and an exhaust-via-failures test |
| `stdcopy.StdCopy` used instead of the frame reader, destroying stdout/stderr interleaving | M | MEDIUM | D4 states the reason; a test asserting a known alternating order |
| TTY containers emit header bytes as text, or non-TTY output is parsed as raw | M | MEDIUM | D5's pre-inspect; both paths covered by tests and by the manual sweep |
| `ShowStdout`/`ShowStderr` left false → an empty stream with a nil error and nothing to trace | M | MEDIUM | Called out in the SDK gotchas; `TestEngineOptionsAlwaysBothStreams` guards it |
| An enormous single line exhausts agent memory on a small VPS | M | HIGH | D9's cap enforced on the accumulating buffer, not after the newline |
| A 200 is committed before a 404 is known, so a missing container reads as an empty stream | M | MEDIUM | D12's lazy headers; `TestStreamErrorBeforeFirstLine` |
| Resume duplicates or, worse, skips lines at the boundary | M | MEDIUM | At-least-once stated in the README and in the plan; the client dedupes on `(ts, line)` |
| Container log content reaches `agent.log`, persisting secrets the operator only wanted in transit | L | HIGH | D16; `TestAgentLogNeverCarriesLineContent` |
| `ContainerLogsResult` written as a struct with `.Items`, by analogy with every other v29 result | **H** | LOW | Called out twice in the SDK section — it is an interface |
| Route precedence between `/{id}`, `/{id}/logs`, and `/{id}/logs/stream` | L | MEDIUM | `TestLogRoutePrecedence` rather than trusting `ServeMux` |

## Notes

- **Task 1 is not a preamble, it is the phase's load-bearing fix.** `statusRecorder` is three lines
  short of being compatible with every streaming API in `net/http`, and the failure mode is
  `feature not supported` from `ResponseController` — an error whose text points nowhere near
  `withRequestLog`. It was verified against the real stdlib rather than reasoned about, because the
  reasoning ("it embeds `http.ResponseWriter`, so surely `Flush` is promoted") is wrong in a way that
  sounds right: embedding an *interface* promotes only that interface's three methods.
- **SSE was chosen over WebSocket for what it costs, not what it does.** The PRD called for a
  "bidirectional channel", but no message ever flows client→server on this feature. WebSocket's real
  price is a new module on an internet-facing port plus an authentication and framing story that
  lives beside the HTTP middleware instead of inside it — meaning `requireDevice`, `requireOp`, and
  every future rate limit would each need a second implementation. If Phase 6 or later adds `exec`,
  that genuinely is bidirectional and can bring its own transport; nothing here blocks it.
- **The frame reader is hand-rolled for one specific reason.** `stdcopy.StdCopy` pumps into two
  separate writers, which loses the relative ordering of stdout and stderr. For a log viewer that
  ordering *is* the information — "the error appeared right after that request" is the whole
  diagnostic. Thirty lines of `io.ReadFull` and a `binary.BigEndian.Uint32` preserve it and remove
  two goroutines and two pipes.
- **`maxConcurrentStreams` is the one number here likely to be wrong.** Eight is a guess anchored on
  "nobody watches nine log views on a phone", not a measurement. It is a constant precisely so that
  changing it is a code change with a code review, and Phase 6's hardening pass is the right place to
  revisit it against a real host.
- **Delivery is at-least-once and the README says so.** The alternative — the agent tracking per-device
  cursors so it can guarantee exactly-once — means server-side state per stream, which is a durable
  store this phase has no business adding. A repeated line after a handover is a cosmetic flaw; a
  dropped one is a diagnostic failure, and this design trades the right way round.
- Deferred to Phase 5, recorded here so it is not lost: once the agent can identify its own container,
  streaming its *own* logs over the API is worth a deliberate decision. It is currently reachable —
  the agent's container is just another container — and it would let a device read the agent's
  operational log without host access, which is a different authority level than the PRD grants.
- Deferred to Phase 6: rate limiting the log routes. The concurrency cap bounds resources held, not
  requests made, so a client can still open and close streams in a tight loop. That belongs with the
  rest of the rate limiting rather than as a second mechanism here.
