# Plan: Lifecycle, Policy & Audit (DevMon Agent Phase 5)

## Summary

Give a paired device the ability to **act**: start, restart, stop, kill, and delete a container —
each gated server-side by the host's startup policy mode, each blocked from touching the agent's own
container, and each recorded in the audit trail with the calling device's identity.

Three things land together because they are one story. The lifecycle calls are the capability. The
policy tier is what stops a compromised phone from exceeding what the operator granted. The audit
row is the record that a destructive operation reachable from a phone actually happened, and who did
it. Shipping any one without the others produces either an unaccountable destructive API or dead
plumbing.

The hardest problem in the phase is not any of the five Engine calls. It is **the agent knowing
which container it is**, because the PRD makes self-exclusion a fixed rule rather than a setting: an
agent that deletes itself destroys the operator's only remote access, and no configuration flag may
opt into that.

## User Story

As an operator whose service is down and who only has a phone,
I want to restart, stop, or remove the broken container,
so that I can fix the incident now — while the host's configuration still decides how far I am
allowed to go, and every action I take leaves a record.

## Problem → Solution

A paired device can see everything and change nothing: it lists, inspects, and reads logs, and the
`audit` table created in Phase 1 has never held a row. → A paired device can perform the full
container lifecycle set within the tier the operator fixed at startup; the agent refuses every
operation the mode forbids and every operation targeting itself, and writes exactly one audit row
per mutating request — successes, policy refusals, and failures alike.

## Metadata

- **Complexity**: Large (26 files changed: 12 created, 14 updated; ~1100 lines of production Go plus tests)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 5 — Lifecycle, policy & audit
- **Depends on**: Phase 3 (complete). Independent of Phase 4, which is awaiting device validation.
- **Estimated Files**: 26

---

## Decisions Settled Before Planning

Each has a plausible-looking wrong answer. Settled here so implementation never re-litigates them.

| # | Decision | Choice | Why not the alternative |
|---|---|---|---|
| D1 | Where self-exclusion is enforced | **Inside `internal/dockerx`**, on every lifecycle method, keyed on the resolved full container ID | Enforcing it in a handler makes it a property of five route registrations — a sixth route, or a refactor, silently loses it. `dockerx` is the only package that can reach the Engine, so putting the rule there makes "the agent can never act on itself" a structural property of the layer rather than a convention five call sites have to remember. The PRD calls this a fixed rule, not a policy knob; fixed rules belong at the chokepoint. |
| D2 | How the agent identifies itself | A **candidate chain**, every candidate verified by `ContainerInspect`: (1) `DEVMON_SELF_CONTAINER_ID` if set, (2) a 64-hex segment in `/proc/self/mountinfo`, (3) a 64-hex segment in `/proc/self/cgroup`, (4) `$HOSTNAME` as a 12-hex short ID. First one the Engine confirms wins; its **full 64-char ID** is what is stored. | No single source is reliable. `/proc/self/cgroup` reports a bare `0::/` under cgroup v2 with a private cgroup namespace — the default on modern Docker. `$HOSTNAME` is the short ID only until someone passes `--hostname` or a compose `hostname:`. `mountinfo` works because Docker bind-mounts `/etc/hostname`, `/etc/hosts`, and `/etc/resolv.conf` from `/var/lib/docker/containers/<id>/`, but it breaks under rootless and some storage drivers. A chain with Engine verification at the end turns four unreliable heuristics into one reliable answer, and the env override is the escape hatch when all four are wrong. |
| D3 | Behaviour when the agent is containerised but cannot identify itself | Start normally, log at **ERROR** naming `DEVMON_SELF_CONTAINER_ID` as the fix, and answer **503** on all five lifecycle routes. Reads, logs, pairing, and status are unaffected. | The PRD's success metric is "Agent surviving a delete attempt in the most permissive mode: 100%". If the rule cannot be enforced, the operations it protects must not be available — that is the only reading of a *fixed* rule that holds. Refusing to start would be worse: it takes away the read and log surface too, over a failure that has a documented one-line fix. Serving lifecycle anyway would trade the phase's headline guarantee for convenience. |
| D4 | Behaviour when the agent is **not** containerised | Lifecycle routes work normally; log once at INFO. | Detected by the absence of `/.dockerenv`, which Docker creates in every container regardless of base image (including `distroless/static`). A process running directly on the host has no container to protect, so there is nothing to fail closed about — and a developer running `go run` against a local socket must not be blocked by a guard that has no subject. |
| D5 | Where the self ID is resolved | In **`dockerx.New`**, right after the existing `Ping`, so `Client` stays immutable for its whole life | A `SetSelfID` after construction is a write to a struct that concurrent handlers read; even if the only write happens before `ListenAndServeTLS`, it is a data race the race detector is entitled to flag once a test constructs a client and a server in the same goroutine tree. Resolving in the constructor removes the question. `New` already makes an Engine call, so this costs one more round trip at startup and none afterwards. |
| D6 | Detection code location | A separate `internal/selfid` package returning **unverified candidates**; `dockerx` does the verifying | `selfid` then has no Docker dependency and is testable against a temp directory of fake `/proc` files — the only way to get coverage on four filesystem-shaped heuristics without a container. Verification needs the Engine, so it belongs with the Engine client. |
| D7 | Docker surface for lifecycle | **`ContainerController` interface declared in `httpapi`, embedded into the existing `DockerReader`** | Exactly what Phase 4 did with `LogReader` (D14 of that plan): `NewServer`'s signature stays untouched and the five existing test helpers need no edit, while the interface remains independently referenceable. A second constructor parameter would churn `server_test.go`, `status_test.go`, `reads_test.go`, and `logs_test.go` for no gain. |
| D8 | Response body on success | **204 No Content** for all five | Every Engine lifecycle call is asynchronous at the edges: `restart` returns before the container is healthy, `stop` returns while the process is still unwinding. Any state we returned would be a snapshot that is already stale — the same reasoning that made Phase 3 refuse to cache Engine responses. The app re-fetches `GET /v1/containers/{id}`, which is one cheap request and always true. |
| D9 | Idempotency | Engine "not modified" (start on a running container, stop on a stopped one) is **success, 204** | HTTP 304 from the Engine means "you asked for a state it is already in". Surfacing that as an error would make the app show a failure for an operation whose goal is already met, and would invite the user to retry an action that cannot help. `cerrdefs.IsNotModified` is the supported check. |
| D10 | Delete semantics | `Force: false`, `RemoveVolumes: false`, `RemoveLinks: false`. A running container yields **409 Conflict**. | `Force` makes delete a kill-then-delete, collapsing two audit-distinguishable acts into one and removing the only natural confirmation step a destructive operation has. `RemoveVolumes` destroys data the agent has no mandate over — the PRD explicitly keeps provisioning and data destruction out of scope. Stop, then delete: two operations, two audit rows, two deliberate taps. |
| D11 | Kill signal | Fixed **SIGKILL** (the Engine default); no `?signal=` parameter | A signal parameter widens the enumerated surface with no requirement behind it, and `kill --signal=HUP` is a config-reload idiom that has no place in an incident-response tool whose kill button means "stop this now". The narrow surface *is* the product. |
| D12 | Stop/restart grace period | Fixed **10s**, passed explicitly rather than left nil | `nil` means "use the container's configured timeout", which an operator can set to `-1` (wait forever) — that would hang the request until the server's 30s `WriteTimeout` killed it mid-response. An explicit 10s matches Docker's own default and fits inside `lifecycleTimeout`. |
| D13 | Per-call Engine timeout for lifecycle | **20s**, separate from the 15s `callTimeout` used by reads | 10s of grace plus process teardown plus the pre-flight inspect does not reliably fit in 15s, and the server's `WriteTimeout` is 30s. 20s leaves 10s of headroom for the response write, so a slow stop becomes a 502 the agent controls rather than a connection the phone watches die. |
| D14 | Audit write placement | A **`withAudit` middleware** that seeds one entry per mutating request and writes it on the way out, refined by inner layers | Calling `AppendAudit` from inside five handlers means five chances to forget, and the policy-refusal path never reaches a handler at all — so "audit every refused call" would be structurally unreachable. One middleware that always writes exactly one row makes completeness a property of the chain. |
| D15 | Middleware order for mutating routes | `requireDevice( withAudit( requireOp( handler ) ) )` | `withAudit` must be **inside** `requireDevice` so every row carries a real device ID — an unauthenticated caller is unattributable, and letting one write audit rows hands a scanner a way to flood the operator's security record. It must be **outside** `requireOp` so policy refusals land in the log, which the PRD requires explicitly. |
| D16 | Audit write failure | Log at ERROR; **never** fail the response | The row is written after the act. Returning 500 for a successful restart because the audit insert failed would tell the app the container was not restarted, when it was — a lie is worse than a gap. The gap is loud in `agent.log`, which is the operator's other durable record. |
| D17 | Reads are not audited | Only mutating routes carry `withAudit` | The PRD scopes the audit log to mutating operations. Auditing reads would put a row on every list refresh and drown the record the log exists for, then trigger retention pruning that deletes the destructive-operation history to make room. |
| D18 | `protected` in listings | `ContainerSummary` and `ContainerDetail` gain a `protected` bool, set in the **`dockerx` mappers** from the resolved self ID | Deferred here from Phase 3 D13 with the enforcement that consumes it. Setting it in the mapper — the same place the exclusion is enforced — means the flag and the rule can never disagree. Bumps two `*FieldCount` tests, which is the intended friction. |
| D19 | Host-side audit reading | A **`devmon-agent audit list`** subcommand alongside `device list` | `internal/state/schema.go` already states the reason the audit trail lives in SQLite rather than in `logs/`: "so the host-side CLI can read it while the agent writes". This is that CLI. Without it the phase ships a table nothing can read. |
| D20 | No audit route on the API | The audit log is **not** exposed over HTTPS | It is not in the PRD's enumerated operation set, and it is the one artifact whose value survives a compromised device. A phone that can read the audit log can see what it needs to cover up; host access is the correct authority, exactly as it is for revocation. |
| D21 | What a target is in an audit row | The reference **as the device supplied it**, plus the resolved full ID in `detail` when the pre-flight inspect succeeded | The honest record of a request is what was requested. Recording only the resolved ID would erase the case where a device named a container that does not exist — which is precisely the pattern that distinguishes a fat-fingered operator from something scanning for targets. |

---

## UX Design

### Before

```
┌──────────────────────────────────────────────────────────────┐
│  Phone (paired, policy mode "default")                        │
│    GET  /v1/containers          ──►  200  [ … ]               │
│    GET  /v1/containers/{id}     ──►  200  { … }               │
│    GET  /v1/containers/{id}/logs──►  200  { … }               │
│    POST /v1/containers/{id}/restart ─►  404  (no such route)  │
│                                                               │
│  The operator can see the broken container and nothing else.  │
│  The audit table has never held a row.                        │
└──────────────────────────────────────────────────────────────┘
```

### After

```
┌────────────────────────────────────────────────────────────────────────┐
│  Phone (paired, policy mode "default")                                  │
│    POST   /v1/containers/api/restart   ◄── 204                          │
│    POST   /v1/containers/api/stop      ◄── 204                          │
│    POST   /v1/containers/api/start     ◄── 204  (already running → 204) │
│    POST   /v1/containers/api/kill      ◄── 403 {"error":"operation not  │
│                                                  permitted by host      │
│                                                  policy"}               │
│    DELETE /v1/containers/api           ◄── 403  (same — needs "full")    │
│                                                                         │
│  Policy mode "full":                                                    │
│    POST   /v1/containers/api/kill      ◄── 204                          │
│    DELETE /v1/containers/api           ◄── 204                          │
│    DELETE /v1/containers/running-one   ◄── 409 {"error":"container is   │
│                                                  running"}              │
│    DELETE /v1/containers/devmon-agent  ◄── 403 {"error":"the agent      │
│                                                  cannot act on itself"} │
│                                                                         │
│    GET /v1/containers                                                   │
│      ◄── 200 {"items":[ … {"names":["/devmon-agent"],                   │
│                            "protected":true}, … ]}                      │
└────────────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────────────┐
│  Host                                                                   │
│  $ docker exec devmon-agent /usr/local/bin/devmon-agent audit list      │
│  WHEN                  DEVICE            OPERATION  TARGET   OUTCOME    │
│  2026-08-08T21:04:11Z  a1b2… (Pixel 9)   restart    api      success    │
│  2026-08-08T21:05:02Z  a1b2… (Pixel 9)   kill       api      denied_policy│
│  2026-08-08T21:06:40Z  a1b2… (Pixel 9)   delete     devmon…  denied_self │
└────────────────────────────────────────────────────────────────────────┘
```

### Interaction Changes

| Touchpoint | Before | After | Notes |
|---|---|---|---|
| Container detail screen | Read-only | Start / restart / stop / kill / delete buttons | The app disables what `policy_mode` from `/v1/status` forbids |
| The agent's own row in the list | Indistinguishable | `protected: true` | The app greys out its controls and can explain why (D18) |
| Forbidden operation | n/a | 403 with a specific body | Distinguishable from 401 and from 502, so the app says "your host forbids this" rather than "something broke" |
| Delete a running container | n/a | 409 | The app can offer "stop it first" instead of a bare failure |
| Operator on the host | Can list devices | Can also read who did what, when | `devmon-agent audit list` (D19) |
| Agent that cannot identify itself | n/a | 503 on lifecycle only; reads and logs unaffected | ERROR line in `agent.log` names the env var that fixes it (D3) |

---

## Mandatory Reading

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `internal/httpapi/server.go` | 47–132 | `Server` struct and `routes()`; five routes are added and the mutating helper lives here |
| P0 | `internal/httpapi/policygate.go` | all (37) | `requireOp` — the gate the new routes reuse unchanged, and the composition-order comment that D15 extends |
| P0 | `internal/httpapi/reads.go` | 13–120 | `DockerReader` and its sub-interfaces (D7 embeds into this), `writeDockerError` (extended), `requireDocker` |
| P0 | `internal/dockerx/containers.go` | 1–57 | `callTimeout`, the `ValidateRef`-then-call shape every lifecycle method mirrors, and `toContainerSummary`/`toContainerDetail` which gain `protected` |
| P0 | `internal/dockerx/errors.go` | all (37) | `classify` and the sentinel style; three sentinels are added here |
| P0 | `internal/dockerx/client.go` | 20–71 | `Client` struct and `New` — self-resolution lands here (D5) |
| P0 | `internal/policy/mode.go` | 28–65 | `OpStart`/`OpRestart`/`OpStop`/`OpKill`/`OpDelete` already exist in `minMode`; this phase is the first to use them |
| P0 | `internal/state/schema.go` | 43–56 | The `audit` table's exact columns — created in Phase 1, written for the first time here |
| P1 | `internal/state/devices.go` | 41–58, 209–239 | Repository method shape, `rowScanner`, and the nullable-Unix-column scan pattern `audit.go` mirrors |
| P1 | `internal/state/store.go` | 253–287 | `PruneAudit` — already enforces retention on the rows this phase starts writing; no change needed, but it must not be broken |
| P1 | `internal/httpapi/middleware.go` | 20–85 | `deviceCtxKey`/`DeviceFrom` — the exact pattern `withAudit`'s context entry copies |
| P1 | `internal/httpapi/device.go` | 43–77, 123–146 | The canonical guarded-handler shape and the 204-with-no-body precedent (D8) |
| P1 | `cmd/devmon-agent/cli.go` | 22–99, 101–139 | Subcommand dispatch, `openDeviceStore`, and the tabwriter table `audit list` mirrors |
| P1 | `cmd/devmon-agent/main.go` | 52–95, 113–174 | `device` dispatch (line 74) gains a sibling; `dockerx.New` at line 155 gains an argument |
| P1 | `internal/config/config.go` | 24–35, 71–99, 155–160 | Env key constants, `Config` fields, and `raw()` — one variable is added |
| P2 | `internal/httpapi/reads_test.go` | all | `fakeDocker` — it must grow the five new methods or nothing in `httpapi` compiles |
| P2 | `internal/dockerx/types_test.go` | all | The `*FieldCount` guards that D18 deliberately breaks |
| P2 | `internal/httpapi/status_test.go` | 25–72 | `testServerWithStore`, `peerCertWithSerial` — every audit test needs a real store and a real device |
| P2 | `README.md` | 117–139, 193–234 | Policy-mode and API sections this phase extends |
| P2 | `Makefile` | 28–58 | The exact gate commands |

## External Documentation

| Topic | Source | Key Takeaway |
|---|---|---|
| moby client v29 lifecycle methods | `go doc github.com/moby/moby/client` (module `github.com/moby/moby/client v0.5.1`) | Every method takes an options struct and returns an **empty** result struct plus an error. Verified signatures below — do not write these from memory. |
| Engine idempotency | Docker Engine API | `POST /containers/{id}/start` returns **304** when already running; `POST /containers/{id}/stop` returns 304 when already stopped. Surfaces through the SDK as an error satisfying `cerrdefs.IsNotModified`. |
| Engine delete conflict | Docker Engine API | `DELETE /containers/{id}` on a running container returns **409**, surfacing as `cerrdefs.IsConflict`. |
| Container ID from inside a container | `/proc/self/mountinfo`, `/proc/self/cgroup`, `/.dockerenv` | Docker bind-mounts `/etc/hostname`, `/etc/hosts`, `/etc/resolv.conf` from `/var/lib/docker/containers/<64-hex>/`, so those paths appear in `mountinfo`. `cgroup` carries the ID only under cgroup v1 or `--cgroupns=host`. `/.dockerenv` exists in every Docker container regardless of base image. |
| errdefs predicates | `github.com/containerd/errdefs` (already a direct dependency) | `IsNotFound`, `IsConflict`, `IsNotModified` are all available; no new module is needed. |

### Verified SDK signatures — transcribe exactly

```go
// github.com/moby/moby/client (v29 API)
func (cli *Client) ContainerStart(ctx context.Context, containerID string, options ContainerStartOptions) (ContainerStartResult, error)
func (cli *Client) ContainerRestart(ctx context.Context, containerID string, options ContainerRestartOptions) (ContainerRestartResult, error)
func (cli *Client) ContainerStop(ctx context.Context, containerID string, options ContainerStopOptions) (ContainerStopResult, error)
func (cli *Client) ContainerKill(ctx context.Context, containerID string, options ContainerKillOptions) (ContainerKillResult, error)
func (cli *Client) ContainerRemove(ctx context.Context, containerID string, options ContainerRemoveOptions) (ContainerRemoveResult, error)

type ContainerStartOptions   struct { CheckpointID string; CheckpointDir string }
type ContainerRestartOptions struct { Signal string; Timeout *int }   // Timeout is *int, SECONDS
type ContainerStopOptions    struct { Signal string; Timeout *int }   // Timeout is *int, SECONDS
type ContainerKillOptions    struct { Signal string }                 // "" means SIGKILL
type ContainerRemoveOptions  struct { RemoveVolumes bool; RemoveLinks bool; Force bool }

// All five Result types are EMPTY structs. Assign to _ , never to a named variable
// you then try to read a field from — there are no fields.
type ContainerStartResult   struct{}
type ContainerRestartResult struct{}
type ContainerStopResult    struct{}
type ContainerKillResult    struct{}
type ContainerRemoveResult  struct{}
```

`Timeout` is `*int`, not `int` and not a `time.Duration`. It is **seconds**. `nil` means "the
container's configured timeout", which D12 rejects. Take the address of a package-level
`stopGraceSeconds` copy — never of the constant itself, which does not compile.

---

## Patterns to Mirror

### NAMING_CONVENTION
```go
// SOURCE: internal/httpapi/logs.go:22-48
// Grouped const block; a doc comment on the block explaining WHY these values
// exist, then per-constant comments. Message constants are msg<Thing><Outcome>.
const (
	// msgTooManyStreams is served when every stream slot is in use. Unlike the
	// other rejections this one is specific and actionable: the caller is an
	// authenticated device and the fix is on its side — close a log view.
	msgTooManyStreams = "too many concurrent log streams"
)
```

### ERROR_HANDLING
```go
// SOURCE: internal/httpapi/reads.go:86-96
// One shared mapper so the error → status mapping cannot drift between handlers.
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
// SOURCE: internal/dockerx/errors.go:29-37
// classify maps a raw Engine error onto the package's sentinels, wrapping with op.
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

### LOGGING_PATTERN
```go
// SOURCE: internal/httpapi/policygate.go:26-33
// slog typed attrs; device_id when it is available; never a formatted string.
attrs := []any{slog.String("operation", string(op))}
if device, ok := DeviceFrom(r.Context()); ok {
	attrs = append(attrs, slog.String("device_id", device.ID))
}
s.log.Warn("rejected request forbidden by host policy", attrs...)
```

### REPOSITORY_PATTERN
```go
// SOURCE: internal/state/devices.go:95-105
// Method on *Store, ctx first, no SQL escapes the package, every error wrapped
// with enough context to name the row it failed on.
func (s *Store) RecordDeviceCert(ctx context.Context, deviceID, serial string, notBefore, notAfter time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO device_certs (serial, device_id, not_before, not_after, issued_at)
		 VALUES (?, ?, ?, ?, ?)`,
		serial, deviceID, notBefore.Unix(), notAfter.Unix(), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record certificate %s for device %s: %w", serial, deviceID, err)
	}
	return nil
}
```

### NULLABLE_COLUMN_SCAN
```go
// SOURCE: internal/state/devices.go:209-239
// rowScanner abstracts *sql.Row and *sql.Rows; nullable Unix columns become
// zero-valued time.Time rather than a sentinel number.
type rowScanner interface{ Scan(dest ...any) error }

var lastSeenAt, revokedAt sql.NullInt64
if lastSeenAt.Valid {
	device.LastSeenAt = time.Unix(lastSeenAt.Int64, 0).UTC()
}
```

### SERVICE_PATTERN
```go
// SOURCE: internal/dockerx/containers.go:41-57
// Validate the ref BEFORE any Engine call, derive a timeout context, classify.
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
// SOURCE: internal/httpapi/device.go:123-146
// 204 with no body: WriteHeader only, never writeJSON with a nil body.
func (s *Server) handleUnpairSelf(w http.ResponseWriter, r *http.Request) {
	device, ok := DeviceFrom(r.Context())
	if !ok {
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}
	if err := s.st.RevokeDevice(r.Context(), device.ID); err != nil {
		s.log.Error("revoke self", slog.String("device_id", device.ID), slog.Any("err", err))
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

### CONTEXT_VALUE_PATTERN
```go
// SOURCE: internal/httpapi/middleware.go:20-27
// Empty-struct key type, never a string: a string key can collide with another
// package's context value, an empty struct type cannot.
type deviceCtxKey struct{}

func DeviceFrom(ctx context.Context) (state.Device, bool) {
	device, ok := ctx.Value(deviceCtxKey{}).(state.Device)
	return device, ok
}
```

### ROUTE_REGISTRATION
```go
// SOURCE: internal/httpapi/server.go:111-129
// A small local helper per route family, so the guard stack is written once.
read := func(pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpRead, h)))
}
read("GET /v1/containers", s.handleListContainers)
```

### CLI_SUBCOMMAND
```go
// SOURCE: cmd/devmon-agent/cli.go:103-123
// tabwriter table, header line, every Fprintf error checked and wrapped.
tw := tabwriter.NewWriter(os.Stdout, tabwriterMinWidth, tabwriterPadding, tabwriterPadding, ' ', 0)
if _, err := fmt.Fprintln(tw, "ID\tNAME\tPAIRED\tLAST SEEN\tSTATE"); err != nil {
	return fmt.Errorf("write device list header: %w", err)
}
```

### TEST_STRUCTURE
```go
// SOURCE: internal/dockerx/client_test.go:18-48
// t.Parallel at both levels, named table cases, explicit // Arrange // Act //
// Assert, failure messages in the form: got = X, want Y.
func TestNewUnreachableEngine(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, host string }{
		{name: "missing unix socket", host: "unix:///nonexistent/docker.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Act
			c, err := New(context.Background(), tt.host, "", testLogger())
			// Assert
			if err == nil {
				_ = c.Close()
				t.Fatalf("New(%q) error = nil, want an unreachable-engine failure", tt.host)
			}
		})
	}
}
```

---

## API Contract (after this phase)

| Route | Auth | Policy op | Min mode | Success |
|---|---|---|---|---|
| `POST /v1/containers/{id}/start` | client cert | `start` | `default` | 204 |
| `POST /v1/containers/{id}/restart` | client cert | `restart` | `default` | 204 |
| `POST /v1/containers/{id}/stop` | client cert | `stop` | `default` | 204 |
| `POST /v1/containers/{id}/kill` | client cert | `kill` | `full` | 204 |
| `DELETE /v1/containers/{id}` | client cert | `delete` | `full` | 204 |

Failure modes on all five:

| Condition | Status | Body |
|---|---|---|
| No / unknown / revoked client certificate | 401 | `{"error":"client certificate required"}` |
| Policy mode forbids the operation | 403 | `{"error":"operation not permitted by host policy"}` |
| Target is the agent's own container | 403 | `{"error":"the agent cannot act on itself"}` |
| Agent is containerised but cannot identify itself (D3) | 503 | `{"error":"agent cannot identify its own container"}` |
| Reference fails `ValidateRef` | 400 | `{"error":"invalid object reference"}` |
| Container does not exist | 404 | `{"error":"not found"}` |
| Delete of a running container (D10) | 409 | `{"error":"container is running"}` |
| Engine unreachable, timed out, or any other Engine error | 502 | `{"error":"docker engine unavailable"}` |
| Wrong method on a known path | 405 | ServeMux default |

Read routes gain one field:

```go
type ContainerSummary struct {   // 11 → 12 fields
	// … unchanged …
	Protected bool `json:"protected"`
}

type ContainerDetail struct {    // 24 → 25 fields
	// … unchanged …
	Protected bool `json:"protected"`
}
```

### Audit record shape

Written to the `audit` table created in Phase 1 (`internal/state/schema.go:46-55`) — the columns are
already correct and **no migration is required**.

```go
// internal/state/audit.go

// AuditEntry is one mutating operation, attributed to the device that requested it.
type AuditEntry struct {
	ID         int64
	OccurredAt time.Time
	DeviceID   string // always set: withAudit runs inside requireDevice (D15)
	Operation  string // policy.Operation value: start, restart, stop, kill, delete
	Target     string // the reference as the device supplied it (D21)
	Outcome    string // one of the Outcome* constants below
	Detail     string // resolved container ID, or a short reason; never an Engine message
}

const (
	OutcomeSuccess      = "success"
	OutcomeDeniedPolicy = "denied_policy"
	OutcomeDeniedSelf   = "denied_self"
	OutcomeNotFound     = "not_found"
	OutcomeInvalid      = "invalid"
	OutcomeConflict     = "conflict"
	OutcomeUnavailable  = "unavailable" // self ID unknown (D3)
	OutcomeEngineError  = "engine_error"
)
```

`Detail` must never carry a raw Engine error string: those can name the socket path or a host
mount. It carries the resolved 64-char container ID on paths where the pre-flight inspect
succeeded, and otherwise a short fixed reason.

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `internal/selfid/selfid.go` | CREATE | Candidate detection from `/proc` and the environment (D2, D6) |
| `internal/selfid/selfid_test.go` | CREATE | Table tests over fake `/proc` fixtures in a temp root |
| `internal/dockerx/self.go` | CREATE | Candidate verification against the Engine; `SelfID`/`SelfKnown`/`Containerized` accessors |
| `internal/dockerx/self_test.go` | CREATE | Verification order, first-confirmed-wins, none-confirmed |
| `internal/dockerx/lifecycle.go` | CREATE | The five Engine calls with the self-exclusion chokepoint (D1) |
| `internal/dockerx/lifecycle_test.go` | CREATE | Self-exclusion, idempotency, conflict mapping |
| `internal/state/audit.go` | CREATE | `AuditEntry`, `AppendAudit`, `ListAudit`, outcome constants |
| `internal/state/audit_test.go` | CREATE | Round-trip, ordering, nullable columns, interaction with `PruneAudit` |
| `internal/httpapi/lifecycle.go` | CREATE | `ContainerController` + the five handlers |
| `internal/httpapi/lifecycle_test.go` | CREATE | Per-route status matrix against a fake controller |
| `internal/httpapi/audit.go` | CREATE | `withAudit` middleware and its context entry (D14) |
| `internal/httpapi/audit_test.go` | CREATE | Exactly-one-row-per-request, including refusals |
| `internal/dockerx/client.go` | UPDATE | `New` gains the override argument and resolves self (D5); `Client` gains the self fields |
| `internal/dockerx/client_test.go` | UPDATE | Existing calls gain the new argument |
| `internal/dockerx/containers.go` | UPDATE | Mappers set `protected` (D18) |
| `internal/dockerx/containers_test.go` | UPDATE | Assert `protected` for self and non-self |
| `internal/dockerx/errors.go` | UPDATE | `ErrSelfProtected`, `ErrSelfUnknown`, `ErrConflict`, `ErrNotModified`; `classify` learns conflict and not-modified |
| `internal/dockerx/errors_test.go` | UPDATE | New classification cases |
| `internal/dockerx/types.go` | UPDATE | `Protected` on both container DTOs |
| `internal/dockerx/types_test.go` | UPDATE | Field counts 11→12 and 24→25 |
| `internal/config/config.go` | UPDATE | `DEVMON_SELF_CONTAINER_ID` |
| `internal/config/config_test.go` | UPDATE | Valid, invalid, and absent cases for the new variable |
| `internal/httpapi/reads.go` | UPDATE | Embed `ContainerController` into `DockerReader` (D7); extend `writeDockerError` |
| `internal/httpapi/reads_test.go` | UPDATE | `fakeDocker` grows five methods |
| `internal/httpapi/server.go` | UPDATE | The `mutate` route helper and five registrations |
| `cmd/devmon-agent/cli.go` | UPDATE | `audit list` subcommand (D19) |
| `cmd/devmon-agent/cli_test.go` | UPDATE | Dispatch and formatting tests |
| `cmd/devmon-agent/main.go` | UPDATE | `audit` dispatch; `dockerx.New` gains the override argument |
| `README.md` | UPDATE | Five API rows, the policy-mode table, self-exclusion, and `audit list` |

## NOT Building

- **Pause / unpause / rename / update / commit / prune** — outside the PRD's enumerated lifecycle set. Every one of them is one more Engine capability reachable from a phone.
- **`?force=true` on delete** — D10. Stop then delete.
- **`?signal=` on kill** — D11.
- **Container creation, `docker run`, image pull, compose deployment** — the PRD's largest explicit exclusion.
- **An audit route on the HTTPS API** — D20. Host access is the authority.
- **Operator-defined protected container lists** — a PRD "Won't". Only the agent's own fixed self-exclusion exists.
- **Bulk or multi-container operations** — one target per request, so one audit row per act.
- **Client-side confirmation flows** — the app's concern; server-side policy is the real boundary.
- **Changing `PruneAudit`, `Pruner`, or the retention configuration** — all shipped in Phase 1 and correct for the rows this phase starts writing.
- **A schema migration** — the `audit` table already matches (`schema.go:46-55`); adding a v3 rung for a table that needs no change would be churn with real downgrade risk.
- **Rate limiting on the new routes** — Phase 6, and these sit behind `requireDevice`.
- **Container resource stats** — a PRD "Could".

---

## Step-by-Step Tasks

### Task 1: Self-identification candidates (`internal/selfid`)
- **ACTION**: Create `internal/selfid/selfid.go` and `selfid_test.go`.
- **IMPLEMENT**:
  ```go
  // Package selfid discovers, without contacting the Docker Engine, the
  // candidate container IDs the current process might be running as.
  //
  // It deliberately returns UNVERIFIED candidates in priority order. No single
  // source is reliable: /proc/self/cgroup reports a bare "0::/" under cgroup v2
  // with a private namespace (the modern Docker default), $HOSTNAME is the short
  // ID only until someone passes --hostname, and mountinfo depends on the
  // storage driver. Verification needs the Engine, so it happens in
  // internal/dockerx (D2, D6).
  package selfid

  // Result is what Detect could learn from the filesystem alone.
  type Result struct {
      // Containerized reports whether this process is running inside a Docker
      // container, detected by /.dockerenv — which Docker creates in every
      // container regardless of base image, including distroless/static.
      Containerized bool
      // Candidates are container-ID candidates in priority order, longest and
      // most trustworthy first. May be empty even when Containerized is true.
      Candidates []string
  }

  // Detect gathers candidates. root is the filesystem root to read under ("/"
  // in production, a temp dir in tests). override, when non-empty, is the
  // operator's DEVMON_SELF_CONTAINER_ID and always sorts first.
  func Detect(root, override string, getenv func(string) string) Result
  ```
  Sources, in order: `override`; every 64-hex run found in `<root>/proc/self/mountinfo`; every
  64-hex run found in `<root>/proc/self/cgroup`; `getenv("HOSTNAME")` when it matches
  `^[0-9a-f]{12}$`. De-duplicate while preserving order. Compile the hex patterns once at package
  level with `regexp.MustCompile`.
- **MIRROR**: `internal/dockerx/ref.go` — a package-level compiled pattern whose doc comment names
  what it is defending against; `internal/config/config.go:308-336` for small pure validators.
- **IMPORTS**: `os`, `path/filepath`, `regexp`, `strings`.
- **GOTCHA**: `mountinfo` lines contain the ID inside a longer path
  (`/var/lib/docker/containers/<id>/hostname`). Extract with `FindAllString` over
  `[0-9a-f]{64}`, not by splitting on `/` — the field layout differs between kernel versions and
  the ID is not always in the same column.
- **GOTCHA**: A 64-hex run also appears in overlay `upperdir=` paths for the *image* layer on some
  drivers. That is why these are candidates, not answers — the Engine settles it in Task 2. Do not
  add cleverness here to pick "the right one".
- **GOTCHA**: `root` must be joined with `filepath.Join`, and a missing `/proc` must yield an empty
  slice rather than an error. The function returns no error at all: every source is best-effort.
- **GOTCHA**: `getenv` is a parameter, not a call to `os.Getenv`, matching `config.Load`'s reason —
  `t.Setenv` forbids `t.Parallel`.
- **VALIDATE**: `go test ./internal/selfid/ -race`. Table cases: no `/.dockerenv`; `/.dockerenv`
  with an empty `/proc`; a real-shaped `mountinfo` line; a cgroup-v1 line; `HOSTNAME=abc123def456`;
  `HOSTNAME=my-server` (rejected); an override that pre-empts everything.

### Task 2: Verify the self ID against the Engine
- **ACTION**: Create `internal/dockerx/self.go` and `self_test.go`; update `client.go` and `client_test.go`.
- **IMPLEMENT**:
  ```go
  // selfInfo is the agent's own container identity, resolved once at startup and
  // never written again — Client must stay immutable for its whole life (D5).
  type selfInfo struct {
      containerized bool
      id            string // full 64-char ID; empty when unresolved
  }

  func (c *Client) SelfID() string      { return c.self.id }
  func (c *Client) SelfKnown() bool     { return c.self.id != "" }
  func (c *Client) Containerized() bool { return c.self.containerized }
  ```
  `resolveSelf(ctx, override)` calls `selfid.Detect("/", override, os.Getenv)`, then
  `ContainerInspect`s each candidate in order and keeps the **full `res.Container.ID`** of the first
  one the Engine confirms. `New` gains a `selfOverride string` parameter and calls this after the
  existing `Ping`.
- **MIRROR**: SERVICE_PATTERN; the existing `New` structure in `client.go:35-63`.
- **IMPORTS**: add `github.com/scnplt/devmon-agent/internal/selfid`.
- **GOTCHA**: A candidate the Engine does not recognise is **normal**, not an error — the overlay
  false positive from Task 1 lands here. Swallow `ErrNotFound` per candidate and continue; only a
  non-not-found Engine failure is worth logging, and even that must not fail `New`.
- **GOTCHA**: Store the full ID from `res.Container.ID`, never the candidate string. A 12-hex
  `HOSTNAME` candidate would otherwise never compare equal to a resolved target's full ID, and
  self-exclusion would silently never fire — the exact failure this phase exists to prevent.
- **GOTCHA**: Log the three outcomes distinctly and at the right level. Resolved → `Info` with the
  ID. Not containerised → `Info` once, stating that self-exclusion is inapplicable (D4).
  Containerised but unresolved → **`Error`**, naming `DEVMON_SELF_CONTAINER_ID` as the fix (D3).
- **GOTCHA**: `New` must still return a usable client in every case. An agent that refuses to start
  because it could not find itself takes away reads and logs over a problem with a documented
  one-line fix.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run TestResolveSelf`. Tests inject a fake
  inspect function rather than a live daemon, matching `client_test.go`'s stated rule.

### Task 3: Lifecycle sentinels and error classification
- **ACTION**: Update `internal/dockerx/errors.go` and `errors_test.go`.
- **IMPLEMENT**:
  ```go
  // ErrSelfProtected is returned when a lifecycle operation targets the agent's
  // own container. This is a fixed rule, not a policy tier: stopping or deleting
  // the agent from the app would destroy the operator's only remote access, and
  // no configuration may opt into that (D1).
  var ErrSelfProtected = errors.New("the agent cannot act on its own container")

  // ErrSelfUnknown is returned when the agent is running in a container but
  // could not determine which one. Lifecycle operations fail closed rather than
  // proceed with the self-exclusion rule unenforceable (D3).
  var ErrSelfUnknown = errors.New("agent cannot identify its own container")

  // ErrConflict is returned when the Engine refuses an operation because of the
  // object's current state — in practice, deleting a running container (D10).
  var ErrConflict = errors.New("docker object is in a conflicting state")

  // ErrNotModified is returned when the Engine reports the object was already in
  // the requested state. Callers treat it as success (D9); it exists as a
  // distinct sentinel so the audit detail can record that nothing changed.
  var ErrNotModified = errors.New("docker object already in the requested state")
  ```
  Extend `classify` with `cerrdefs.IsNotModified` and `cerrdefs.IsConflict` branches, **checked
  before** the `IsNotFound` branch is irrelevant — order does not matter between them, but put
  `IsNotFound` first to keep the existing diff minimal.
- **MIRROR**: ERROR_HANDLING; the sentinel doc-comment style already in this file.
- **IMPORTS**: unchanged.
- **GOTCHA**: `cerrdefs` is already a direct dependency (promoted in Phase 3). No `go get` needed.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run TestClassify`.

### Task 4: The five lifecycle calls with the self-exclusion chokepoint
- **ACTION**: Create `internal/dockerx/lifecycle.go` and `lifecycle_test.go`.
- **IMPLEMENT**:
  ```go
  const (
      // lifecycleTimeout bounds every lifecycle Engine call. Larger than
      // callTimeout because a stop carries stopGraceSeconds of waiting plus
      // process teardown plus the pre-flight inspect, and smaller than the HTTP
      // server's 30s WriteTimeout so a slow stop becomes a 502 the agent
      // controls rather than a response that dies mid-body (D13).
      lifecycleTimeout = 20 * time.Second

      // stopGraceSeconds is how long the Engine waits for a container to exit
      // on its own before SIGKILL. Passed explicitly rather than left nil: nil
      // means "the container's configured timeout", which an operator can set
      // to -1 (wait forever) and hang the request (D12).
      stopGraceSeconds = 10
  )

  func (c *Client) StartContainer(ctx context.Context, ref string) error
  func (c *Client) RestartContainer(ctx context.Context, ref string) error
  func (c *Client) StopContainer(ctx context.Context, ref string) error
  func (c *Client) KillContainer(ctx context.Context, ref string) error
  func (c *Client) RemoveContainer(ctx context.Context, ref string) error
  ```
  Every one of the five is:
  ```go
  id, err := c.resolveTarget(ctx, ref)   // ValidateRef → inspect → self check
  if err != nil {
      return err
  }
  ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
  defer cancel()

  if _, err := c.api.ContainerX(ctx, id, client.ContainerXOptions{…}); err != nil {
      return classify("x container", err)
  }
  return nil
  ```
  and `resolveTarget` is the single chokepoint:
  ```go
  // resolveTarget validates ref, resolves it to a full container ID, and
  // enforces the agent's permanent self-exclusion. Every lifecycle method goes
  // through it — that is what makes "the agent can never act on itself" a
  // property of this layer rather than of five route registrations (D1).
  func (c *Client) resolveTarget(ctx context.Context, ref string) (string, error) {
      if err := ValidateRef(ref); err != nil {
          return "", err
      }
      if c.self.containerized && c.self.id == "" {
          return "", ErrSelfUnknown
      }
      detail, err := c.InspectContainer(ctx, ref)
      if err != nil {
          return "", err
      }
      if detail.ID == c.self.id && c.self.id != "" {
          return "", ErrSelfProtected
      }
      return detail.ID, nil
  }
  ```
  Options per call: start `client.ContainerStartOptions{}`; restart and stop
  `{Timeout: &grace}` with a package-level `grace := stopGraceSeconds` copy; kill
  `client.ContainerKillOptions{}` (empty Signal = SIGKILL, D11); remove
  `client.ContainerRemoveOptions{}` — all three booleans false (D10).
- **MIRROR**: SERVICE_PATTERN; `containers.go`'s validate-then-timeout-then-classify shape.
- **IMPORTS**: `context`, `time`, `github.com/moby/moby/client`.
- **GOTCHA**: The self check runs on the **resolved ID**, never on `ref`. A device can reach the
  agent by name (`devmon-agent`), by short ID, or by full ID; only the inspect result normalises all
  three. Comparing `ref` to `c.self.id` would let `devmon-agent` through.
- **GOTCHA**: `c.self.id != ""` is required in the comparison. Without it, an unresolved self ID
  (`""`) would match nothing and the guard would look like it worked — but the `ErrSelfUnknown`
  branch above already covers the containerised case, and the redundancy is deliberate: this is the
  one comparison in the codebase where a false negative destroys the product.
- **GOTCHA**: `Timeout` is `*int` in **seconds**. `&stopGraceSeconds` does not compile — you cannot
  take the address of an untyped constant. Declare `grace := stopGraceSeconds` inside the function.
- **GOTCHA**: All five `Result` types are empty structs. Assign the result to `_`.
- **GOTCHA**: `resolveTarget` makes one extra Engine call per mutation. That is the price of
  enforcing the rule on a normalised ID, and it is what supplies the resolved ID the audit row
  records (D21). Do not "optimise" it away by trusting `ref`.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run TestLifecycle`. Cases: target is self (by
  name, short ID, and full ID) → `ErrSelfProtected` on all five; containerised with no self ID →
  `ErrSelfUnknown` before any Engine call; unknown ref → `ErrNotFound`; traversal ref →
  `ErrInvalidRef` before any Engine call; remove on a running container → `ErrConflict`; start on a
  running container → `ErrNotModified`.

### Task 5: `protected` on the container DTOs
- **ACTION**: Update `internal/dockerx/types.go`, `containers.go`, `types_test.go`, `containers_test.go`.
- **IMPLEMENT**: Add `Protected bool \`json:"protected"\`` to `ContainerSummary` and
  `ContainerDetail`. Change `toContainerSummary` and `toContainerDetail` into methods on `*Client`
  (or pass `selfID string` as a second parameter — pick one and apply it to both) so they can set
  `Protected: s.ID == selfID && selfID != ""`. Update the doc comments to say 12 and 25 fields, and
  update `TestContainerSummaryFieldCount` / `TestContainerDetailFieldCount`.
- **MIRROR**: FIELD_ALLOWLIST_GUARD (`status_test.go:74-105`).
- **GOTCHA**: `protected` has **no** `omitempty`. A false value must appear in the JSON: an app that
  cannot distinguish "not protected" from "this agent version does not report protection" would
  quietly offer a delete button on the agent itself.
- **GOTCHA**: The two `*FieldCount` tests will fail until their expected counts are bumped. That is
  the intended friction Phase 3 designed in — bump them deliberately, do not delete them.
- **GOTCHA**: Prefer the parameter form (`toContainerSummary(s, selfID)`) over methods if the
  mappers are called from tests that have no `*Client`; check `containers_test.go` before choosing.
- **VALIDATE**: `go test ./internal/dockerx/ -race`; a summary whose ID equals the self ID marshals
  with `"protected":true`, and every other container with `"protected":false`.

### Task 6: The audit repository
- **ACTION**: Create `internal/state/audit.go` and `audit_test.go`.
- **IMPLEMENT**: `AuditEntry` and the `Outcome*` constants exactly as in the "Audit record shape"
  section, plus:
  ```go
  // AppendAudit records one mutating operation. It is called once per mutating
  // request, on every path including refusals (D14), so it must stay a single
  // INSERT against the pool's one connection.
  func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error

  // ListAudit returns the most recent entries first, capped at limit. It exists
  // for the host-side CLI (D19); nothing on the HTTPS API calls it (D20).
  func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)
  ```
  `AppendAudit` stamps `occurred_at` from `time.Now().Unix()` when `e.OccurredAt` is zero.
- **MIRROR**: REPOSITORY_PATTERN; NULLABLE_COLUMN_SCAN — `device_id`, `target`, and `detail` are all
  nullable in the schema, so scan them into `sql.NullString`.
- **IMPORTS**: `context`, `database/sql`, `fmt`, `time`.
- **GOTCHA**: The `audit` table already exists at schema v1 (`schema.go:46-55`) with exactly these
  columns. **Write no migration.** Adding a v3 rung for a table that needs no change is churn with
  real downgrade risk, and `currentSchemaVersion` must stay at 2.
- **GOTCHA**: `id` is `INTEGER PRIMARY KEY AUTOINCREMENT` — never supply it on insert.
- **GOTCHA**: `ListAudit` orders by `id DESC`, not `occurred_at DESC`. Second-resolution timestamps
  tie constantly under a burst, and `PruneAudit` already treats `id` as the ordering that matters
  (`store.go:272-276`).
- **GOTCHA**: The store's pool is `MaxOpenConns(1)`. `AppendAudit` must be one statement with no
  surrounding transaction — a transaction here would serialise reads behind every mutation for no
  benefit.
- **VALIDATE**: `go test ./internal/state/ -race -run TestAudit`. Cases: round-trip of every field;
  empty `DeviceID`/`Target`/`Detail` scan back as `""` not a panic; `ListAudit` ordering; `limit`
  respected; `PruneAudit` still removes rows this writes.

### Task 7: The `withAudit` middleware
- **ACTION**: Create `internal/httpapi/audit.go` and `audit_test.go`.
- **IMPLEMENT**:
  ```go
  // auditCtxKey is the context key under which withAudit stores the in-flight
  // entry. An empty struct type, not a string, for the same reason
  // deviceCtxKey is (middleware.go:20).
  type auditCtxKey struct{}

  // auditEntry is the mutable record for one in-flight mutating request. Inner
  // layers refine Outcome and Detail; withAudit writes exactly one row on the
  // way out, whatever happened (D14).
  type auditEntry struct {
      operation policy.Operation
      target    string
      outcome   string
      detail    string
  }

  // withAudit records one audit row per mutating request.
  //
  // Placement is load-bearing (D15). It sits INSIDE requireDevice, so every row
  // carries a real device — an unauthenticated caller is unattributable, and
  // letting one write rows would hand a scanner a way to flood the operator's
  // security record. It sits OUTSIDE requireOp, so refusals by policy are
  // recorded, which the PRD requires explicitly.
  func (s *Server) withAudit(op policy.Operation, next http.Handler) http.Handler

  // setAuditOutcome refines the in-flight entry. A no-op when the request is not
  // under withAudit, so a handler shared with a non-mutating route cannot panic.
  func setAuditOutcome(ctx context.Context, outcome, detail string)
  ```
  `withAudit` seeds the entry with `operation: op` and `target: r.PathValue("id")`, defaults
  `outcome` to `state.OutcomeSuccess`, runs `next`, then writes.
- **MIRROR**: CONTEXT_VALUE_PATTERN; MIDDLEWARE_PATTERN (`middleware.go:41-47`).
- **IMPORTS**: `context`, `log/slog`, `net/http`, `internal/policy`, `internal/state`.
- **GOTCHA**: The entry must be a **pointer** in the context. Storing a value means inner layers
  refine a copy and every row records `success`.
- **GOTCHA**: Write the row **after** `next.ServeHTTP` returns, not in a `defer` that could also
  fire on a panic path where `withRecovery` has already written a 500. A plain post-call write is
  correct; the panic case is rare, already logged, and a missing row there is better than a row
  claiming an outcome that never happened.
- **GOTCHA**: An `AppendAudit` failure is logged at ERROR and **never** changes the response (D16).
  The act already happened; a 500 here would tell the app the restart did not occur when it did.
- **GOTCHA**: `requireOp` must set the outcome before writing its 403. Add exactly one line to
  `policygate.go`: `setAuditOutcome(r.Context(), state.OutcomeDeniedPolicy, "")` immediately before
  `s.writeError(...)`. It is a no-op on the read routes, which are not under `withAudit`.
- **VALIDATE**: `go test ./internal/httpapi/ -race -run TestAudit`. Cases: success writes one row
  with outcome `success`; a policy refusal writes one row with `denied_policy` and never reaches the
  handler; a 401 writes **zero** rows; a store failure logs but still returns the handler's status;
  exactly one row per request on every path.

### Task 8: The five handlers and the `ContainerController` interface
- **ACTION**: Create `internal/httpapi/lifecycle.go`; update `internal/httpapi/reads.go`.
- **IMPLEMENT**:
  ```go
  // ContainerController is the mutating container surface. Declared here rather
  // than in dockerx because the consumer owns the contract, and embedded into
  // DockerReader rather than added as a NewServer parameter — exactly what
  // Phase 4 did with LogReader, so five existing test helpers stay untouched (D7).
  type ContainerController interface {
      StartContainer(ctx context.Context, ref string) error
      RestartContainer(ctx context.Context, ref string) error
      StopContainer(ctx context.Context, ref string) error
      KillContainer(ctx context.Context, ref string) error
      RemoveContainer(ctx context.Context, ref string) error
  }
  ```
  Add `ContainerController` to `DockerReader`'s embed list in `reads.go`. Then one shared handler
  body, parameterised by the action, so the five routes cannot drift:
  ```go
  // handleLifecycle runs one lifecycle action and answers 204. Every action
  // shares this body: five near-identical handlers is the repetition the DRY
  // rule exists to prevent, and a shared body means the audit refinement and
  // the error mapping cannot differ between operations.
  func (s *Server) handleLifecycle(op string, act func(context.Context, string) error) http.HandlerFunc
  ```
  Extend `writeDockerError` with the three new sentinels:
  ```go
  case errors.Is(err, dockerx.ErrSelfProtected):
      setAuditOutcome(r.Context(), state.OutcomeDeniedSelf, "")
      s.writeError(w, http.StatusForbidden, msgSelfProtected)
  case errors.Is(err, dockerx.ErrSelfUnknown):
      setAuditOutcome(r.Context(), state.OutcomeUnavailable, "")
      s.writeError(w, http.StatusServiceUnavailable, msgSelfUnknown)
  case errors.Is(err, dockerx.ErrConflict):
      setAuditOutcome(r.Context(), state.OutcomeConflict, "")
      s.writeError(w, http.StatusConflict, msgContainerConflict)
  ```
  New message constants: `msgSelfProtected = "the agent cannot act on itself"`,
  `msgSelfUnknown = "agent cannot identify its own container"`,
  `msgContainerConflict = "container is running"`.
- **MIRROR**: HTTP_HANDLER_PATTERN (the 204 shape from `device.go:123-146`); ERROR_HANDLING;
  NAMING_CONVENTION.
- **IMPORTS**: `context`, `errors`, `net/http`, `internal/dockerx`, `internal/state`.
- **GOTCHA**: `writeDockerError` currently takes `(w, op, err)`. It needs the request context to
  refine the audit entry — change the signature to `(w, r, op, err)` and update the eight read
  handlers accordingly. `setAuditOutcome` is a no-op there, so read behaviour is unchanged.
- **GOTCHA**: `ErrNotModified` must be treated as **success**, and it never reaches
  `writeDockerError`. Check for it in the handler: on `errors.Is(err, dockerx.ErrNotModified)`, set
  the audit detail to `"already in requested state"`, keep the outcome `success`, and write 204 (D9).
- **GOTCHA**: Every handler must tolerate `s.dc == nil` via the existing `requireDocker`, exactly as
  the read handlers do — the test helpers construct servers with nil dependencies deliberately.
- **GOTCHA**: `writeDockerError`'s existing `default` branch logs and returns 502. It must also set
  `state.OutcomeEngineError`. Add that in the shared helper, not in five handlers.
- **VALIDATE**: `go build ./...`.

### Task 9: Route registration
- **ACTION**: Update `internal/httpapi/server.go`.
- **IMPLEMENT**: After the log routes:
  ```go
  // Mutating operations. Three guards, and the order is load-bearing (D15):
  // requireDevice proves who is calling, withAudit records the attempt whatever
  // happens to it, requireOp proves the host's startup policy permits it.
  mutate := func(pattern string, op policy.Operation, h http.HandlerFunc) {
      mux.Handle(pattern, s.requireDevice(s.withAudit(op, s.requireOp(op, h))))
  }
  mutate("POST /v1/containers/{id}/start", policy.OpStart, s.handleStartContainer)
  mutate("POST /v1/containers/{id}/restart", policy.OpRestart, s.handleRestartContainer)
  mutate("POST /v1/containers/{id}/stop", policy.OpStop, s.handleStopContainer)
  mutate("POST /v1/containers/{id}/kill", policy.OpKill, s.handleKillContainer)
  mutate("DELETE /v1/containers/{id}", policy.OpDelete, s.handleRemoveContainer)
  ```
- **MIRROR**: ROUTE_REGISTRATION.
- **GOTCHA**: `requireOp` takes an `http.Handler`, not an `http.HandlerFunc`. The `read` and `logs`
  helpers already rely on the implicit conversion; keep the same shape.
- **GOTCHA**: `DELETE /v1/containers/{id}` and `GET /v1/containers/{id}` coexist — Go 1.22 method
  patterns make this unambiguous. Do not register a method-less pattern.
- **GOTCHA**: `POST /v1/containers/{id}/start` and `GET /v1/containers/{id}/logs` are different
  patterns under the same `{id}` wildcard; ServeMux handles this. No route ordering is required.
- **VALIDATE**: `go test ./internal/httpapi/ -race` — every pre-existing test still passes; a
  `GET /v1/containers/{id}/start` returns 405.

### Task 10: Wire self-identification through configuration and startup
- **ACTION**: Update `internal/config/config.go`, `config_test.go`, `cmd/devmon-agent/main.go`,
  `internal/dockerx/client_test.go`.
- **IMPLEMENT**:
  - `envSelfContainerID = "DEVMON_SELF_CONTAINER_ID"` in the const block; `SelfContainerID string`
    on `Config`; a `loader.selfContainerID()` that accepts the empty string (the normal case) and
    otherwise requires `^[0-9a-f]{12}$` or `^[0-9a-f]{64}$`.
  - `main.go:155` becomes `dockerx.New(ctx, cfg.DockerHost, cfg.SelfContainerID, log)`.
  - Existing `dockerx.New` call sites in tests gain `""`.
- **MIRROR**: `config.go`'s loader-method-per-field shape; `isValidDNSName` for the validator style.
- **GOTCHA**: An invalid override is a **startup configuration error**, not a warning. It is the
  documented fix for the one case where lifecycle is unavailable, and an operator who typos it must
  find out at start rather than when the delete button stays greyed out.
- **GOTCHA**: The override is *not* required and has no default. Absent is the normal path.
- **GOTCHA**: `config_test.go` is the largest table in the repo; add cases, do not restructure it.
- **VALIDATE**: `go test ./internal/config/ -race`; `go build ./...`.

### Task 11: Host-side `audit list`
- **ACTION**: Update `cmd/devmon-agent/cli.go`, `cli_test.go`, `main.go`.
- **IMPLEMENT**:
  - `runAuditCommand(ctx, cfg, args)` mirroring `runDeviceCommand`: `list` is the only subcommand,
    with a `--limit` flag defaulting to 100.
  - Columns: `WHEN`, `DEVICE`, `OPERATION`, `TARGET`, `OUTCOME`, `DETAIL`.
  - Dispatch in `main.go` beside the existing `device` branch:
    ```go
    if flag.Arg(0) == "audit" {
        return runAuditCommand(context.Background(), cfg, flag.Args()[1:])
    }
    ```
- **MIRROR**: CLI_SUBCOMMAND; `runDeviceList` (`cli.go:103-123`) line for line.
- **GOTCHA**: Reuse `openDeviceStore` — the same discard-backed logger rule applies. The CLI must
  never construct a `logging.Sink`.
- **GOTCHA**: Dispatch **before** `prepareStateDir` and the log sink, exactly as `device` does. A
  CLI invocation must not create directories or rotate logs.
- **GOTCHA**: Join the device ID to its name for the `DEVICE` column via `ListDevices`, mirroring
  `runDeviceRevoke`'s lookup. A raw 16-hex ID is useless to an operator reading an incident record.
- **GOTCHA**: A device whose row was deleted still has audit rows — print the bare ID rather than
  failing. The audit trail outliving the device is the point.
- **VALIDATE**: `go test ./cmd/devmon-agent/ -race`; manually,
  `devmon-agent audit list --limit 5` against a store with rows.

### Task 12: Docs and the manual sweep
- **ACTION**: Update `README.md`.
- **IMPLEMENT**:
  - Five rows in the API table (`README.md:193-234`), with their minimum policy mode.
  - Extend the policy-modes section (`README.md:117-127`) with which operations each mode unlocks
    and the fact that the mode is fixed at startup and cannot be widened by a client.
  - A new subsection, *The agent excludes itself*: what it means, how the agent identifies itself,
    what `protected: true` in listings signals, and what to do when `agent.log` reports it could not
    identify itself — set `DEVMON_SELF_CONTAINER_ID` to the container ID from `docker ps`.
  - A new subsection under device management, *The audit log*: what is recorded, where it lives, the
    retention variables that bound it, `devmon-agent audit list`, and the reason it is not reachable
    over the API (D20).
  - The new 403 / 409 / 503 rows in the failure-mode table.
- **MIRROR**: The README's voice — every statement carries its reason, as in the `/v1/status`
  paragraph at `README.md:335-341`.
- **GOTCHA**: English only, per `CLAUDE.md`.
- **VALIDATE**: The full gate sweep plus the manual checklist below.

---

## Testing Strategy

### Unit Tests

| Test | Input | Expected Output | Edge Case? |
|---|---|---|---|
| `TestDetectNotContainerized` | temp root with no `/.dockerenv` | `Containerized == false` | yes |
| `TestDetectFromMountinfo` | a real-shaped `mountinfo` line | the 64-hex ID as candidate 1 | no |
| `TestDetectFromCgroupV1` | `1:name=systemd:/docker/<64hex>` | the ID as a candidate | no |
| `TestDetectCgroupV2Empty` | `0::/` | no candidate from cgroup | **yes** |
| `TestDetectHostnameShortID` | `HOSTNAME=abc123def456` | accepted as a candidate | no |
| `TestDetectHostnameRejected` | `HOSTNAME=my-vps` | not a candidate | **yes** |
| `TestDetectOverrideWins` | override set plus other sources | override is candidate 1 | no |
| `TestDetectDeduplicates` | same ID in mountinfo and cgroup | one candidate | yes |
| `TestResolveSelfFirstConfirmed` | 3 candidates, the 2nd inspects | full ID of the 2nd | no |
| `TestResolveSelfStoresFullID` | 12-hex candidate confirms | the **64-char** ID is stored | **yes** |
| `TestResolveSelfNoneConfirmed` | every candidate not-found | `SelfKnown() == false`, `New` still succeeds | **yes** |
| `TestLifecycleRejectsSelfByName` | ref = the agent's container name | `ErrSelfProtected` | **yes** |
| `TestLifecycleRejectsSelfByShortID` | ref = 12-hex prefix of self | `ErrSelfProtected` | **yes** |
| `TestLifecycleRejectsSelfAllFive` | self as target, each of 5 methods | `ErrSelfProtected` × 5 | **yes** |
| `TestLifecycleSelfUnknown` | containerised, no self ID | `ErrSelfUnknown`, **no Engine call made** | **yes** |
| `TestLifecycleNotContainerized` | not containerised | operation proceeds | yes |
| `TestLifecycleInvalidRef` | `"../../info"` | `ErrInvalidRef`, no Engine call | yes |
| `TestRemoveRunningConflict` | Engine returns a conflict error | `ErrConflict` | yes |
| `TestStartAlreadyRunning` | Engine returns not-modified | `ErrNotModified` | yes |
| `TestStopTimeoutIsExplicit` | any stop | options carry `Timeout` = 10, not nil | **yes** |
| `TestContainerSummaryProtected` | summary ID == self ID | `"protected":true` | yes |
| `TestContainerSummaryFieldCount` | marshalled DTO | exactly 12 keys | guard |
| `TestContainerDetailFieldCount` | marshalled DTO | exactly 25 keys | guard |
| `TestAppendAuditRoundTrip` | a full entry | every field reads back | no |
| `TestAppendAuditNullableFields` | empty device/target/detail | `""`, no panic | yes |
| `TestListAuditOrdering` | 3 rows same second | newest `id` first | **yes** |
| `TestAuditRowPerSuccess` | policy allows, handler 204 | 1 row, `success` | no |
| `TestAuditRowPerPolicyDenial` | mode `default`, `DELETE` | 1 row, `denied_policy`; handler never ran | **yes** |
| `TestAuditRowPerSelfDenial` | target is self | 1 row, `denied_self` | **yes** |
| `TestNoAuditRowWhenUnauthenticated` | no client certificate | **0 rows** | **yes** |
| `TestNoAuditRowOnReads` | 8 read routes | 0 rows | **yes** |
| `TestAuditWriteFailureDoesNotFailRequest` | store returns an error | 204 still returned; ERROR logged | **yes** |
| `TestLifecyclePolicyMatrix` | 3 modes × 5 operations | 204 / 403 per `minMode` | no |
| `TestLifecycleRequiresDevice` × 5 | no `req.TLS` | 401 | yes |
| `TestLifecycleRejectsOtherMethods` | `GET /v1/containers/x/start` | 405 | yes |
| `TestLifecycleNilDockerReader` | `s.dc == nil` | 502, no panic | yes |
| `TestErrorBodiesLeakNothing` | every new failure path | no `StateDir`, `docker.sock`, `devmon.db`, no Engine text | **yes** |
| `TestAuditDetailNeverCarriesEngineText` | Engine error containing a socket path | `detail` has no path | **yes** |

### Edge Cases Checklist
- [ ] Agent not containerised → lifecycle works, INFO logged once
- [ ] Agent containerised, self resolved → self-targeting refused on all five operations
- [ ] Agent containerised, self **not** resolved → 503 on lifecycle, 200 on reads and logs
- [ ] `DEVMON_SELF_CONTAINER_ID` set to a valid but wrong ID → that container becomes protected (documented consequence of an operator override)
- [ ] `DEVMON_SELF_CONTAINER_ID` set to a malformed value → startup configuration error (exit 2)
- [ ] Start an already-running container → 204
- [ ] Stop an already-stopped container → 204
- [ ] Delete a running container → 409, container untouched
- [ ] Restart a container with a long shutdown → completes within `lifecycleTimeout` or 502
- [ ] Engine socket removed mid-operation → 502, agent stays up, audit row `engine_error`
- [ ] Revoked device → 401 and **zero** audit rows
- [ ] `read-only` mode → all five routes 403, five audit rows `denied_policy`
- [ ] `full` mode → all five succeed on a non-self container
- [ ] Audit retention prunes the rows this phase writes without touching device rows
- [ ] Container removed between the pre-flight inspect and the action → 404 or 502, one audit row

---

## Validation Commands

### Static Analysis
```bash
gofmt -l .
go vet ./...
```
EXPECT: both print nothing. (`gofmt -l .` reports the whole tree on a CRLF checkout — compare
against the pre-change output rather than expecting silence in that case.)

### Lint
```bash
make lint
```
EXPECT: clean.

### Security Scan
```bash
gosec ./...
```
EXPECT: no findings. Watch G104 in the new mappers and the CLI's `Fprintf` calls.

### Unit Tests
```bash
go test ./internal/selfid/ ./internal/dockerx/ ./internal/state/ ./internal/httpapi/ -race -v
```
EXPECT: all pass.

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
EXPECT: no change — this phase adds no dependency.

### End-to-End Validation (real host, real containers)
```bash
C="--cacert ca.pem --cert device.pem --key device.key"
H="https://$HOST:8443"

# Default mode: reversible operations allowed, destructive ones refused.
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/restart"   # 204
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/stop"      # 204
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/start"     # 204
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/start"     # 204 (idempotent)
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/kill"      # 403
curl -o /dev/null -w '%{http_code}\n' $C -X DELETE "$H/v1/containers/demo"           # 403

# Restart the agent with DEVMON_POLICY_MODE=full, then:
curl -o /dev/null -w '%{http_code}\n' $C -X DELETE "$H/v1/containers/demo"           # 409 (running)
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/demo/stop"      # 204
curl -o /dev/null -w '%{http_code}\n' $C -X DELETE "$H/v1/containers/demo"           # 204

# The headline guarantee, in the most permissive mode.
curl -o /dev/null -w '%{http_code}\n' $C -X DELETE "$H/v1/containers/devmon-agent"   # 403
curl -o /dev/null -w '%{http_code}\n' $C -X POST   "$H/v1/containers/devmon-agent/stop" # 403
curl $C "$H/v1/containers" | jq '.items[] | select(.protected)'                      # the agent

docker exec devmon-agent /usr/local/bin/devmon-agent audit list
```

### Manual Validation
- [ ] In `full` mode, every attempt to stop, kill, or delete the agent's own container is refused, by name, by short ID, and by full ID — and the agent is still running afterwards (PRD success metric: 100%)
- [ ] `docker ps` after the delete attempt shows the agent up with no restart
- [ ] The agent's row in `GET /v1/containers` carries `"protected": true`; every other row carries `"protected": false`
- [ ] Start the agent with `DEVMON_POLICY_MODE=read-only`; all five routes answer 403 and reads still answer 200
- [ ] Start with `DEVMON_POLICY_MODE` unset; restart/stop/start work, kill/delete are refused (safe default)
- [ ] `devmon-agent audit list` shows one row per attempt above, with the right device, operation, target, and outcome — including the refusals
- [ ] Revoke the device from the host, retry a restart: 401, and `audit list` gains **no** row
- [ ] Stop the Docker daemon; a restart request answers 502 and the audit row records `engine_error`; the agent container stays up
- [ ] Run the agent with `--hostname something-else` and confirm self-identification still resolves (the mountinfo path, not `$HOSTNAME`)
- [ ] Set `DEVMON_SELF_CONTAINER_ID` to a malformed value; confirm exit code 2 and a message naming the variable
- [ ] Force the unresolved case (override to a valid-format ID that does not exist): lifecycle answers 503, reads and logs still answer 200, and `agent.log` carries an ERROR naming the variable
- [ ] `agent.log` request lines still carry method/path/status/duration only — no container names, no references
- [ ] Confirm `agent.log` never contains a pairing code, key material, or a PEM block after this phase's changes

---

## Acceptance Criteria
- [ ] All 12 tasks complete
- [ ] Five routes registered, each behind `requireDevice`, `withAudit`, and `requireOp` in that order
- [ ] Self-exclusion is enforced in `internal/dockerx` on the resolved full container ID, and holds for name, short-ID, and full-ID references across all five operations
- [ ] An agent that cannot identify its own container refuses lifecycle operations with 503 and continues to serve reads, logs, pairing, and status
- [ ] Exactly one audit row per mutating request — successes, policy refusals, self refusals, and failures — and zero rows for unauthenticated requests and for reads
- [ ] Audit rows are attributed to the calling device and readable from the host with `devmon-agent audit list`
- [ ] Policy tiers enforced server-side: `read-only` refuses all five, `default` permits start/restart/stop, `full` additionally permits kill/delete
- [ ] `protected` appears in both container DTOs with no `omitempty`, and the two `*FieldCount` guards are updated deliberately
- [ ] No error body or audit `detail` carries an Engine message, a host path, or a state path
- [ ] `gofmt`, `go vet`, `golangci-lint`, `gosec` all clean
- [ ] `./internal/...` coverage ≥ 80%
- [ ] `go.mod` unchanged
- [ ] The PRD Phase 5 success signal is demonstrated on a real host

## Completion Checklist
- [ ] Code follows the patterns captured above
- [ ] Errors wrapped with `%w` and named context
- [ ] Logging uses `slog` typed attrs; failures logged with context, returned terse
- [ ] Tests are table-driven, `t.Parallel`, AAA-commented
- [ ] No hardcoded values — `lifecycleTimeout`, `stopGraceSeconds`, every `msg*`, and every outcome are named constants
- [ ] README API table, policy modes, self-exclusion, and audit sections updated
- [ ] PRD Phase 5 row moved to `complete` with links to this plan and its report
- [ ] No scope beyond the five lifecycle routes, the policy gate, and the audit trail

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| The agent deletes or stops itself, destroying the operator's only remote access | M | **CRITICAL** | D1's chokepoint in `dockerx`, keyed on the resolved full ID; D3's fail-closed 503 when identity is unknown; dedicated tests for name / short-ID / full-ID targeting on all five operations; a manual end-to-end check in the most permissive mode |
| Self-identification returns the **wrong** container, protecting the wrong thing and leaving the agent exposed | M | **CRITICAL** | Every candidate verified by `ContainerInspect` before acceptance; the full ID stored, never the candidate; `DEVMON_SELF_CONTAINER_ID` as the operator override; the resolved ID logged at startup so the operator can check it once |
| A destructive operation happens with no audit row | M | HIGH | D14's middleware makes the row structural rather than remembered; tests assert exactly one row on every path including refusals |
| A policy refusal is not audited, so a probing device leaves no trace | M | HIGH | D15's ordering places `withAudit` outside `requireOp`; a dedicated test asserts the `denied_policy` row |
| An unauthenticated caller floods the audit table and prunes the real record away | L | HIGH | D15 places `withAudit` inside `requireDevice`; a test asserts zero rows for a request with no client certificate |
| Code written from the pre-v29 Docker SDK does not compile | **H** | MEDIUM | Verified signatures transcribed above; the empty `Result` structs and the `*int` seconds `Timeout` are the two that differ most from memory |
| `cgroup` v2 with a private namespace makes detection fail on modern hosts, so lifecycle is 503 out of the box | **H** | HIGH | The mountinfo source is listed *before* cgroup precisely because of this; `/.dockerenv` distinguishes "not containerised" from "unidentifiable"; the override and its ERROR line are documented in the README |
| An operator sets a wrong override and quietly protects the wrong container | L | MEDIUM | The resolved ID is logged at startup; documented in the README as a consequence of the override |
| A slow `stop` exceeds the server's `WriteTimeout` and dies mid-response | M | MEDIUM | D12's explicit 10s grace plus D13's 20s call timeout, both inside the 30s `WriteTimeout` |
| `writeDockerError`'s signature change breaks the eight read handlers | **H** | LOW | Task 8 calls it out; the compiler catches every call site |
| The two `*FieldCount` guards fail and get deleted instead of updated | M | MEDIUM | Task 5 states the counts explicitly and says why the friction is intended |
| `PruneAudit` deletes destructive-operation history under a burst of failures | L | MEDIUM | `DEVMON_AUDIT_MAX_ROWS` defaults to 100000 against a few rows per incident; D17 keeps reads out of the table entirely |
| Audit `detail` leaks an Engine message naming the socket or a host mount | M | MEDIUM | `detail` carries only the resolved ID or a short fixed reason; a dedicated test asserts it |

## Notes

- **Phase 4 is `awaiting device validation`, not `complete`.** This phase depends on Phase 3 only,
  and touches `httpapi/logs.go` nowhere. The one shared file is `server.go`'s `routes()`, and the
  one shared type is `DockerReader` — which this phase extends by embedding, exactly as Phase 4 did.
  If Phase 4 needs changes from its device validation, they will not collide.

- **The `audit` table has been waiting since Phase 1.** `schema.go:43-45` records why it was created
  early and why it lives in SQLite rather than in `logs/`: so the host-side CLI can read it while
  the agent writes, and so retention is an indexed `DELETE` rather than file surgery. Phase 1's
  `Pruner` already enforces the retention budget on rows nothing was writing. This phase is the
  other half of a design that was deliberately split — which is why it needs no migration and no
  retention work.

- **`resolveTarget` is the most important twelve lines in the phase.** Everything else is
  conventional. Reviewers should spend their attention there: the ID normalisation, the
  `c.self.id != ""` guard, and the fact that all five methods go through it and none of them can
  reach the Engine any other way.

- **The extra inspect per mutation is deliberate.** It costs one Engine round trip and buys three
  things: the reference is normalised before the self check, a nonexistent container becomes a clean
  404 before anything is attempted, and the audit row can record the resolved ID alongside what the
  device actually asked for. Trading it away for latency would weaken all three.

- **204, not 200 with a body.** The Engine's lifecycle calls return before the container has
  finished changing state. Any body we sent would be a snapshot that is already stale — the same
  reason Phase 3 refused to cache Engine responses. Worth restating in the report, because it is the
  decision an app developer is most likely to push back on.

- **What this phase leaves for Phase 6**: rate limiting on the mutating routes, the security review
  against the risk table, and the automated installer, which now has one more decision to surface —
  the policy mode, which after this phase actually changes what a phone can do.
