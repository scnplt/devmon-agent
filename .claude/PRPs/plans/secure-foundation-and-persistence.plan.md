# Plan: Secure Foundation & Persistence (DevMon Agent Phase 1)

## Summary

Stand up the DevMon Agent as a Go binary shipped in a container: it parses and validates its entire startup configuration, opens a durable state store on a host bind mount, connects to the Docker Engine, terminates TLS on a single listening port, and serves one unauthenticated informational endpoint (`GET /v1/status`). Its own logs persist across crashes and are bounded by both age and size. No authentication, no Docker read/write operations, and no pairing exist yet — this phase builds the skeleton every later phase hangs off, and proves the durability and configuration guarantees the PRD treats as Must.

## User Story

As an **operator running Docker on a VPS**, I want **to install the DevMon agent as a container with an explicit state mount, policy mode, and log retention budget**, so that **the agent starts with a safe, validated configuration, keeps its identity and logs across restarts and upgrades, and tells a client its version and policy over TLS before any credential exists**.

## Problem → Solution

**Current state:** Empty repository. No Go module, no build, no container image, no state layout. The PRD's durability guarantees ("no device ever has to re-pair after a normal upgrade", "100% of pre-crash entries readable", "agent must never be the reason a small VPS runs out of disk") have no implementation.

**Desired state:** `docker run` with a bind-mounted state directory produces a running agent that: fails loudly and specifically on bad configuration; creates a versioned SQLite state store and a self-signed server certificate on first start; serves TLS 1.3 on one port; answers `GET /v1/status` without a client certificate with exactly four fields; writes rotating persistent logs; pings the Docker Engine at startup and refuses to run if it cannot; and shuts down gracefully on SIGTERM.

## Metadata

- **Complexity**: Large (greenfield subsystem, ~22 files, ~1400 lines incl. tests)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 1 — Secure foundation & persistence
- **Estimated Files**: 22 created (11 source packages, 8 test files, 3 build/ops files)
- **Module path**: `github.com/scnplt/devmon-agent` (from `git remote origin`)
- **Go toolchain**: go1.26.4 (verified locally); `go.mod` declares `go 1.26`
- **Docker Engine**: 29.6.1 (verified locally) — determines the SDK module, see Gotcha G1

---

## Decisions Settled Before Planning

These were open in the PRD and were resolved with the user before this plan was written. They are binding for all later phases.

| Decision | Choice | Consequence for Phase 1 |
|---|---|---|
| API transport | **REST/JSON over HTTPS + WebSocket for live logs** | Phase 1 builds a `net/http` TLS server with Go 1.22+ method-aware `ServeMux` patterns. No protobuf toolchain. WebSocket is Phase 4 only. |
| Durable state store | **SQLite (`modernc.org/sqlite`, CGO-free) in WAL mode**, certs/keys as separate PEM files | Phase 1 owns schema creation, migration versioning, WAL/busy_timeout pragmas, and corruption detection. Cross-process concurrency (agent ↔ `docker exec` CLI) comes free — this is what the PRD calls "a functional requirement, not an implementation detail". |

---

## UX Design

**Internal/operator-facing change.** There is no mobile UX in this phase (the Android app is out of scope for the whole PRD). The user-facing surface is the operator's terminal and one HTTP response.

### Before

```
┌──────────────────────────────────────────────┐
│ $ docker run ... ???                         │
│                                              │
│ Nothing exists. Operator's only remote       │
│ option is `ssh root@vps` from a phone, or    │
│ `-H tcp://0.0.0.0:2375` (root-equivalent,    │
│ unauthenticated, to the whole internet).     │
└──────────────────────────────────────────────┘
```

### After

```
┌──────────────────────────────────────────────────────────────┐
│ $ docker run -d --name devmon-agent \                        │
│     -v /var/lib/devmon:/var/lib/devmon \                     │
│     -v /var/run/docker.sock:/var/run/docker.sock:ro \        │
│     --group-add $(stat -c '%g' /var/run/docker.sock) \       │
│     -p 8443:8443 \                                           │
│     -e DEVMON_PUBLIC_ADDR=vps.example.com \                  │
│     ghcr.io/scnplt/devmon-agent:0.1.0                        │
│                                                              │
│ $ docker logs devmon-agent                                   │
│ level=INFO msg="state store opened" path=/var/lib/devmon/... │
│                  first_run=true schema_version=1             │
│ level=WARN msg="no server certificate found, generating      │
│                  self-signed" sans="vps.example.com"         │
│ level=INFO msg="docker engine reachable" api_version=1.52    │
│ level=INFO msg="agent listening" addr=:8443 policy=default   │
│                  version=0.1.0                               │
│                                                              │
│ $ curl -k https://vps.example.com:8443/v1/status             │
│ {"api_version":"v1","agent_version":"0.1.0",                 │
│  "policy_mode":"default","server_time":"2026-08-07T…Z"}      │
│                                                              │
│ …and on a misconfiguration, before anything starts:          │
│ $ docker run … -e DEVMON_POLICY_MODE=admin …                 │
│ invalid configuration:                                       │
│   DEVMON_POLICY_MODE: "admin" is not a valid policy mode     │
│       (want one of: read-only, default, full)                │
│   DEVMON_LOG_MAX_AGE_DAYS: "x" is not an integer             │
│ (exit 2)                                                     │
└──────────────────────────────────────────────────────────────┘
```

### Interaction Changes

| Touchpoint | Before | After | Notes |
|---|---|---|---|
| Install | N/A | `docker run` with 3 mounts + env; automated installer deferred to Phase 6 | Phase 1 ships a documented `docker run` + `compose.example.yaml`; the PRD's Must-have installer is Phase 6 |
| Bad config | N/A | All errors aggregated, printed to stderr, exit code 2 — never a partial start | PRD: "validation of that configuration with a clear failure on bad input" |
| Diagnosis without a credential | N/A | `GET /v1/status` over TLS, no client cert, exactly 4 fields | PRD: "may inform, never issue"; strict field allowlist |
| Crash | N/A | Pre-crash log lines readable in `$STATE_DIR/logs/agent.log` | Success signal for this phase |
| Disk growth | N/A | Bounded by whichever of age or total size is hit first | PRD: "the agent must never be the reason a small VPS runs out of disk" |

---

## Mandatory Reading

There is **no existing code** — this is the first commit in the repository. "Mandatory reading" is therefore the project's own rule files (which stand in for discovered code conventions) and the verified upstream API surface captured in this plan.

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 (critical) | `.claude/PRPs/prds/devmon-agent.prd.md` | 196–248 | Implementation Phases + Phase 1 detail; defines exact scope and success signal |
| P0 (critical) | `.claude/PRPs/prds/devmon-agent.prd.md` | 153–172 | Architecture Notes — bind mount rationale, policy modes, loud-on-missing-state, retention split |
| P0 (critical) | `.claude/rules/ecc/golang/coding-style.md` | all | gofmt/goimports mandatory; wrap errors with `%w`; accept interfaces return structs |
| P0 (critical) | This plan, § Patterns to Mirror | all | No codebase exists — the canonical patterns for this repo are **defined here** and must be followed by later phases |
| P1 (important) | `.claude/rules/ecc/golang/testing.md` | all | Table-driven tests, `-race` always, 80% coverage floor |
| P1 (important) | `.claude/rules/ecc/golang/security.md` | all | `gosec ./...`, `context.Context` with timeouts everywhere |
| P1 (important) | `.claude/rules/ecc/common/coding-style.md` | all | Files 200–400 lines typical / 800 max; functions <50 lines; no magic numbers |
| P2 (reference) | `.claude/rules/ecc/golang/patterns.md` | all | Functional options, constructor DI — used by `httpapi.NewServer` |
| P2 (reference) | `.claude/PRPs/prds/devmon-agent.prd.md` | 252–279 | Decisions Log — do not relitigate any of these during implementation |

## External Documentation

All rows below were **fetched and verified on 2026-08-07**, not recalled. Versions are the live `@latest` from `proxy.golang.org`.

| Topic | Source | Key Takeaway |
|---|---|---|
| Docker Go SDK module split | [moby/moby discussion #52404](https://github.com/moby/moby/discussions/52404) | `github.com/docker/docker` is **deprecated**; v28.x was the last release publishing it. Use `github.com/moby/moby/client` + `github.com/moby/moby/api`. |
| Docker client package | [pkg.go.dev/github.com/moby/moby/client](https://pkg.go.dev/github.com/moby/moby/client) | `client.New(opts...)` is current; `NewClientWithOpts` deprecated. Every method now takes an options struct and returns a `*Result` type. |
| `ContainerLogs` shape | [moby/moby client/container_logs.go](https://github.com/moby/moby/blob/master/client/container_logs.go) | `ContainerLogsResult` is an `interface{ io.ReadCloser }`; caller **must** close. Demux via `github.com/moby/moby/api/pkg/stdcopy`. (Phase 4 — recorded here so it is not re-researched.) |
| SQLite driver | [pkg.go.dev/modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | Driver name is `"sqlite"` (not `sqlite3`). DSN supports `_journal_mode=WAL`, `_busy_timeout=5000`, `_txlock=immediate`. Pure Go — builds with `CGO_ENABLED=0`. |
| Log rotation | [gopkg.in/natefinch/lumberjack.v2 v2.2.1](https://pkg.go.dev/gopkg.in/natefinch/lumberjack.v2) | `MaxSize` is **per file in MB**, not a total budget. `MaxAge` prunes only *at rotation time* — see Gotcha G4. |
| WebSocket (Phase 4) | [github.com/coder/websocket v1.8.15](https://pkg.go.dev/github.com/coder/websocket) | Recorded for the chosen transport; **not** a Phase 1 dependency. |

### Pinned Dependency Versions

```
go 1.26

require (
    github.com/moby/moby/client v0.5.1          // verified @latest 2026-07-27
    github.com/moby/moby/api    v1.55.0         // verified @latest 2026-06-18
    modernc.org/sqlite          v1.56.0         // verified @latest 2026-08-03
    gopkg.in/natefinch/lumberjack.v2 v2.2.1     // verified @latest (stable since 2023)
)
```

### Research Notes

```
KEY_INSIGHT: The Docker Go SDK split into independently versioned `client` and `api`
             modules at Engine v29. The local daemon is 29.6.1.
APPLIES_TO:  internal/dockerx, go.mod
GOTCHA:      Any code written from memory will import `github.com/docker/docker/client`
             and call `client.NewClientWithOpts(client.FromEnv,
             client.WithAPIVersionNegotiation())` then `cli.Ping(ctx)` with ONE argument.
             All three are wrong for v29. See Gotcha G1 for the verified form.

KEY_INSIGHT: modernc.org/sqlite is a pure-Go transpilation — no libsqlite3, no CGO.
APPLIES_TO:  internal/state, Dockerfile
GOTCHA:      Build with CGO_ENABLED=0 so the binary is static and can run on
             distroless/static. If CGO is left on, the image needs glibc.

KEY_INSIGHT: Go's crypto/tls cannot enforce client certificates per-route — ClientAuth
             is a property of the listener, decided during the handshake.
APPLIES_TO:  internal/tlsconf, internal/httpapi/middleware.go
GOTCHA:      The PRD requires ONE port serving both mTLS routes and one unauthenticated
             route. RequireAndVerifyClientCert would reject the status endpoint at the
             handshake, before any HTTP routing happens. See Gotcha G3.
```

---

## Patterns to Mirror

> **This repository has no prior code.** The snippets below are therefore *normative* — they establish the conventions Phases 2–6 will mirror. Write them exactly as specified. Where a snippet is quoted from an external source it is marked `// UPSTREAM:` with the verified reference.

### NAMING_CONVENTION

```go
// Packages: single lowercase word, no underscores, no plurals.
//   internal/config  internal/state  internal/certs  internal/tlsconf  internal/httpapi
// Exported constructors: New<Type>, returning a concrete *struct (never an interface).
// Options structs are named <Type>Options and passed by value.
// Env var keys are UPPER_SNAKE with a DEVMON_ prefix, declared as constants.

const (
    envStateDir   = "DEVMON_STATE_DIR"
    envListenAddr = "DEVMON_LISTEN_ADDR"
    envPolicyMode = "DEVMON_POLICY_MODE"
)

// Accept interfaces, return structs (rules/ecc/golang/coding-style.md:16)
func NewServer(cfg config.Config, st *state.Store, log *slog.Logger) *Server
```

### ERROR_HANDLING

```go
// Always wrap with %w and a caller-meaningful message.
// SOURCE OF RULE: .claude/rules/ecc/golang/coding-style.md:20-28

if err != nil {
    return nil, fmt.Errorf("open state store at %s: %w", path, err)
}

// Sentinel errors for conditions callers branch on. Declared at package top.
var (
    ErrStateCorrupt   = errors.New("state store is unreadable or corrupt")
    ErrSchemaTooNew   = errors.New("state store was written by a newer agent version")
)

// Config validation AGGREGATES rather than failing on the first problem — an operator
// fixing a docker run line one variable at a time is a bad first experience.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
    return "invalid configuration:\n  " + strings.Join(e.Problems, "\n  ")
}
```

### LOGGING_PATTERN

```go
// log/slog with structured key=value attrs. No fmt.Printf, no third-party logger.
// Levels: Debug = per-request detail; Info = lifecycle events; Warn = recoverable
// surprises; Error = the operation failed. Never log key material, pairing codes,
// or certificate private keys — not even at Debug.

log.Info("agent listening",
    slog.String("addr", cfg.ListenAddr),
    slog.String("policy", cfg.PolicyMode.String()),
    slog.String("version", version.Version),
)

log.Error("docker engine unreachable",
    slog.String("host", cfg.DockerHost),
    slog.Any("err", err),
)
```

### REPOSITORY_PATTERN

```go
// Data access is a struct wrapping *sql.DB, in internal/state. Callers never see
// *sql.DB or SQL strings. Every method takes a context.Context first.
// Mirrors .claude/rules/ecc/common/patterns.md § Repository Pattern.

type Store struct {
    db  *sql.DB
    log *slog.Logger
}

func (s *Store) PruneAudit(ctx context.Context, maxAge time.Duration, maxRows int) (int64, error)
func (s *Store) SchemaVersion(ctx context.Context) (int, error)
func (s *Store) Close() error
```

### SERVICE_PATTERN

```go
// Constructor dependency injection — no globals, no init(), no package-level state.
// Mirrors .claude/rules/ecc/golang/patterns.md § Dependency Injection.
// Long-running components expose Run(ctx) error and stop when ctx is cancelled.

type Rotator struct {
    lj       *lumberjack.Logger
    interval time.Duration
    log      *slog.Logger
}

func NewRotator(lj *lumberjack.Logger, interval time.Duration, log *slog.Logger) *Rotator

func (r *Rotator) Run(ctx context.Context) error {
    t := time.NewTicker(r.interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-t.C:
            if err := r.lj.Rotate(); err != nil {
                r.log.Error("log rotation failed", slog.Any("err", err))
            }
        }
    }
}
```

### HTTP_HANDLER_PATTERN

```go
// Go 1.22+ method-aware ServeMux patterns. Handlers are methods on *Server so they
// reach injected deps. Responses go through one helper so the envelope never drifts.
// Envelope mirrors .claude/rules/ecc/common/patterns.md § API Response Format,
// flattened for a status payload that must not leak structure to scanners.

mux.HandleFunc("GET /v1/status", s.handleStatus)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, http.StatusOK, statusResponse{
        APIVersion:   APIVersion,
        AgentVersion: version.Version,
        PolicyMode:   s.cfg.PolicyMode.String(),
        ServerTime:   time.Now().UTC().Format(time.RFC3339),
    })
}

func writeJSON(w http.ResponseWriter, code int, body any) {
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Cache-Control", "no-store")
    w.WriteHeader(code)
    if err := json.NewEncoder(w).Encode(body); err != nil {
        // Response already committed; nothing to do but record it.
        slog.Default().Error("write response", slog.Any("err", err))
    }
}
```

### TEST_STRUCTURE

```go
// Table-driven, subtests via t.Run, AAA sections marked with comments.
// SOURCE OF RULE: .claude/rules/ecc/golang/testing.md + common/testing.md § AAA.
// Filesystem tests use t.TempDir() — never a fixed path, never t.Cleanup(os.RemoveAll).

func TestParsePolicyMode(t *testing.T) {
    tests := []struct {
        name    string
        in      string
        want    Mode
        wantErr bool
    }{
        {name: "empty string defaults to middle tier", in: "", want: ModeDefault},
        {name: "read-only parses", in: "read-only", want: ModeReadOnly},
        {name: "unknown mode is rejected", in: "admin", wantErr: true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Arrange / Act
            got, err := ParseMode(tt.in)
            // Assert
            if tt.wantErr {
                if err == nil {
                    t.Fatalf("ParseMode(%q) = %v, want error", tt.in, got)
                }
                return
            }
            if err != nil {
                t.Fatalf("ParseMode(%q) unexpected error: %v", tt.in, err)
            }
            if got != tt.want {
                t.Errorf("ParseMode(%q) = %v, want %v", tt.in, got, tt.want)
            }
        })
    }
}
```

---

## Startup Configuration Contract

Every knob is an environment variable, read once at start, immutable thereafter. This is the PRD's core security property: *"the agent's powers are fixed by its startup configuration, not by the client"* — changing any of these requires host access and a container restart.

| Env var | Type | Default | Validation |
|---|---|---|---|
| `DEVMON_STATE_DIR` | path | `/var/lib/devmon` | Must be absolute; must exist or be creatable; must be writable |
| `DEVMON_LISTEN_ADDR` | host:port | `:8443` | Must parse via `net.SplitHostPort`; port 1–65535 |
| `DEVMON_PUBLIC_ADDR` | comma list | *(required)* | ≥1 entry; each a valid DNS name or IP; used as server-cert SANs |
| `DEVMON_POLICY_MODE` | enum | `default` | One of `read-only`, `default`, `full` |
| `DEVMON_DOCKER_HOST` | URL | `unix:///var/run/docker.sock` | Scheme must be `unix` or `tcp` |
| `DEVMON_LOG_LEVEL` | enum | `info` | One of `debug`, `info`, `warn`, `error` |
| `DEVMON_LOG_MAX_AGE_DAYS` | int | `1` | ≥1 |
| `DEVMON_LOG_MAX_TOTAL_MB` | int | `64` | ≥8 (below this, per-file size rounds to 0 — see G4) |
| `DEVMON_AUDIT_MAX_AGE_DAYS` | int | `365` | ≥1; **must be ≥ `DEVMON_LOG_MAX_AGE_DAYS`** |
| `DEVMON_AUDIT_MAX_ROWS` | int | `100000` | ≥1000 |

Two cross-field rules, both from the PRD's "separate retention budgets" Must:

1. `DEVMON_AUDIT_MAX_AGE_DAYS >= DEVMON_LOG_MAX_AGE_DAYS` — rejecting the inverse prevents the exact failure the PRD warns about: *"one shared one-day budget would quietly destroy the security record to make room for debug output."*
2. `DEVMON_LOG_MAX_TOTAL_MB >= 8` — see Gotcha G4 for the arithmetic.

## State Directory Layout

```
$DEVMON_STATE_DIR/                 (bind mount — NOT an anonymous volume)   0700
├── devmon.db                      SQLite, WAL mode                          0600
├── devmon.db-wal                  (created by SQLite)
├── devmon.db-shm                  (created by SQLite)
├── certs/                                                                   0700
│   ├── server.crt                 self-signed in P1, CA-issued in P2        0644
│   ├── server.key                 EC P-256 private key                      0600
│   ├── ca.crt                     ← Phase 2
│   └── ca.key                     ← Phase 2                                 0600
└── logs/                                                                    0700
    ├── agent.log                  current, lumberjack-managed               0600
    └── agent-2026-08-07T….log.gz  rotated + compressed                      0600
```

The audit log lives in `devmon.db` (table `audit`), **not** in `logs/` — that is what the SQLite decision bought: `docker exec` CLI reads and agent writes safely interleave, and retention is a `DELETE` with an index rather than file surgery. Phase 1 creates the table and the pruner; Phase 5 writes rows to it.

## Database Schema (v1)

```sql
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- row: ('version', '1')

-- Populated in Phase 2. Created now so Phase 2 needs no migration.
CREATE TABLE IF NOT EXISTS devices (
    id             TEXT PRIMARY KEY,      -- opaque device id
    name           TEXT NOT NULL,
    cert_serial    TEXT NOT NULL UNIQUE,
    cert_not_after INTEGER NOT NULL,      -- unix seconds
    paired_at      INTEGER NOT NULL,
    last_seen_at   INTEGER,
    revoked_at     INTEGER                -- NULL = active
);
CREATE INDEX IF NOT EXISTS idx_devices_serial  ON devices(cert_serial);
CREATE INDEX IF NOT EXISTS idx_devices_revoked ON devices(revoked_at);

-- Written in Phase 5. Created + pruned now.
CREATE TABLE IF NOT EXISTS audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at INTEGER NOT NULL,         -- unix seconds
    device_id   TEXT,                     -- NULL for host-side CLI actions
    operation   TEXT NOT NULL,
    target      TEXT,
    outcome     TEXT NOT NULL,            -- 'allowed' | 'denied' | 'error'
    detail      TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_occurred ON audit(occurred_at);
```

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `go.mod`, `go.sum` | CREATE | Module `github.com/scnplt/devmon-agent`, `go 1.26`, pinned deps |
| `.gitignore` | CREATE | Ignore `/bin`, `*.db*`, `*.key`, `*.crt`, `coverage.out` |
| `Makefile` | CREATE | Single source of truth for build/test/lint/scan commands |
| `internal/version/version.go` | CREATE | `Version`, `Commit`, `BuildTime` — set via `-ldflags -X` |
| `internal/policy/mode.go` | CREATE | `Mode` enum + `ParseMode` + `Allows(op)`; the PRD's named tiers |
| `internal/policy/mode_test.go` | CREATE | Table test: parsing, defaulting, tier supersetting |
| `internal/config/config.go` | CREATE | Env parsing, defaults, cross-field validation, aggregated errors |
| `internal/config/config_test.go` | CREATE | Table test over the full env matrix incl. every rejection |
| `internal/logging/logging.go` | CREATE | slog handler over a lumberjack sink + stderr tee |
| `internal/logging/rotator.go` | CREATE | Daily `Rotate()` ticker — lumberjack will not do this itself (G4) |
| `internal/logging/logging_test.go` | CREATE | Verifies persistence across writer reopen and size-bounded growth |
| `internal/state/store.go` | CREATE | `Open`, `Close`, `SchemaVersion`, `PruneAudit`, first-run detection |
| `internal/state/schema.go` | CREATE | Embedded DDL + migration application |
| `internal/state/pruner.go` | CREATE | Periodic audit retention enforcement (`Run(ctx)`) |
| `internal/state/store_test.go` | CREATE | First-run, reopen, corrupt-file, schema-too-new, prune, concurrency |
| `internal/certs/selfsigned.go` | CREATE | EC P-256 self-signed server cert with SANs from config |
| `internal/certs/store.go` | CREATE | Load-or-generate, strict file permissions, SAN-drift detection |
| `internal/certs/certs_test.go` | CREATE | Generation, reload stability, SAN drift, key file mode 0600 |
| `internal/tlsconf/tlsconf.go` | CREATE | `*tls.Config` builder — TLS 1.3 floor, per-route auth seam (G3) |
| `internal/tlsconf/tlsconf_test.go` | CREATE | ClientAuth mode selection, TLS version floor |
| `internal/dockerx/client.go` | CREATE | Docker Engine client + startup ping, v29 SDK (G1) |
| `internal/httpapi/server.go` | CREATE | `*http.Server` with timeouts, mux, `Run(ctx)`, graceful shutdown |
| `internal/httpapi/status.go` | CREATE | `GET /v1/status` — strict 4-field allowlist |
| `internal/httpapi/middleware.go` | CREATE | Request logging, panic recovery, `requireDevice` seam for Phase 2 |
| `internal/httpapi/respond.go` | CREATE | `writeJSON` / `writeError` envelope helpers |
| `internal/httpapi/status_test.go` | CREATE | `httptest` — asserts the response has exactly 4 keys |
| `cmd/devmon-agent/main.go` | CREATE | Wiring, context fan-out, signal handling, exit codes |
| `Dockerfile` | CREATE | Multi-stage: `golang:1.26-alpine` → `distroless/static:nonroot` |
| `compose.example.yaml` | CREATE | Documents the bind mount, socket mount, `group_add`, env |
| `README.md` | CREATE | Install, config table, state layout, backup note |
| `.claude/PRPs/prds/devmon-agent.prd.md` | UPDATE | Phase 1 row: status → `in-progress`, PRP Plan → this file |

## NOT Building

Explicitly out of scope for this plan. Do not add any of these, even if they seem small.

- **Any Docker read operation** — no container/image/network/volume list or inspect. Phase 3. `dockerx` exposes only `Ping` in Phase 1.
- **Any log retrieval or streaming** — no `ContainerLogs`, no WebSocket dependency. Phase 4.
- **The CA, pairing codes, device certificates, the device registry writer, revocation, renewal** — Phase 2. Phase 1 creates the `devices` table and stops there.
- **`ca_fingerprint` in the status response** — Phase 2 adds the fifth field. Phase 1's response has exactly four keys, and the test asserts exactly four.
- **Client certificate verification** — Phase 1 has no CA to verify against. `tlsconf` takes a `clientCAs *x509.CertPool` parameter that Phase 1 passes as `nil`; the `requireDevice` middleware exists and returns `401` unconditionally.
- **Audit log writing** — Phase 5. Phase 1 creates the table and the pruner only.
- **Rate limiting** — Phase 6, per the PRD's phase table. Phase 1 gets free hardening via `ReadHeaderTimeout` and `MaxHeaderBytes` only.
- **The automated installer** — Phase 6. Phase 1 ships `compose.example.yaml` and README prose.
- **Server certificate re-issuance on address change** — Phase 2 (needs the CA). Phase 1 only *detects* SAN drift and logs a `WARN`.
- **The "loud on missing identity" check** — Phase 2. Phase 1's first-run path legitimately creates state, because there is no identity to lose yet. `state.Open` returns a `FirstRun bool` so Phase 2 can hang that check off it without refactoring.
- **Host-side `docker exec` CLI** — Phase 2.
- **Prometheus metrics, tracing, profiling endpoints** — not in the PRD at all.
- **Agent self-exclusion from lifecycle ops** — Phase 5 (there are no lifecycle ops yet).

---

## Step-by-Step Tasks

### Task 1: Module scaffold and build tooling

- **ACTION**: Initialise the Go module and the build entry points.
- **IMPLEMENT**:
  - `go mod init github.com/scnplt/devmon-agent`; set `go 1.26` in `go.mod`.
  - `internal/version/version.go`:
    ```go
    package version

    // Set at build time via -ldflags "-X github.com/scnplt/devmon-agent/internal/version.Version=..."
    var (
        Version   = "dev"
        Commit    = "none"
        BuildTime = "unknown"
    )
    ```
  - `Makefile` targets: `build`, `test`, `test-race`, `cover`, `lint`, `sec`, `image`. Put the `-ldflags` string here so it is never retyped.
  - `.gitignore`: `/bin/`, `*.db`, `*.db-wal`, `*.db-shm`, `*.key`, `*.crt`, `coverage.out`, `.env`.
- **MIRROR**: NAMING_CONVENTION (package = single lowercase word).
- **IMPORTS**: none.
- **GOTCHA**: Do **not** commit a `vendor/` directory. Do **not** add `github.com/docker/docker` — see G1.
- **VALIDATE**: `go build ./...` succeeds; `make build` produces `bin/devmon-agent`.

---

### Task 2: Policy modes

- **ACTION**: Implement the PRD's three named policy tiers as a typed enum with superset semantics.
- **IMPLEMENT**: `internal/policy/mode.go`
    ```go
    package policy

    type Mode int

    const (
        ModeReadOnly Mode = iota // list, inspect, logs
        ModeDefault              // + start, restart, stop     ← default when unset
        ModeFull                 // + kill, delete
    )

    type Operation string

    const (
        OpRead    Operation = "read"
        OpLogs    Operation = "logs"
        OpStart   Operation = "start"
        OpRestart Operation = "restart"
        OpStop    Operation = "stop"
        OpKill    Operation = "kill"
        OpDelete  Operation = "delete"
    )

    // minMode is the lowest tier that permits each operation. Tiers are supersets,
    // so Allows is a single comparison.
    var minMode = map[Operation]Mode{
        OpRead: ModeReadOnly, OpLogs: ModeReadOnly,
        OpStart: ModeDefault, OpRestart: ModeDefault, OpStop: ModeDefault,
        OpKill: ModeFull, OpDelete: ModeFull,
    }

    func (m Mode) Allows(op Operation) bool {
        min, ok := minMode[op]
        return ok && m >= min   // unknown operation => denied
    }

    func (m Mode) String() string { ... }        // "read-only" | "default" | "full"
    func ParseMode(s string) (Mode, error)       // "" => ModeDefault
    ```
- **MIRROR**: NAMING_CONVENTION, ERROR_HANDLING (return a formatted error naming the valid values).
- **IMPORTS**: `fmt`, `strings`.
- **GOTCHA**: `ParseMode("")` **must** return `ModeDefault`, not `ModeReadOnly`. The PRD is explicit: *"The default when nothing is configured is the middle one — useful out of the box, but incapable of destroying anything."* An unknown operation must return `false`, so Phase 5 adding an operation without a `minMode` entry fails closed.
- **VALIDATE**: `go test ./internal/policy/ -race`; the table covers `""`, all three names, mixed case rejection, and `Allows` for all 7 ops × 3 modes (21 assertions).

---

### Task 3: Configuration parsing and validation

- **ACTION**: Parse the entire startup surface from the environment, apply defaults, and validate with aggregated errors.
- **IMPLEMENT**: `internal/config/config.go`
    ```go
    package config

    type Config struct {
        StateDir      string
        ListenAddr    string
        PublicAddrs   []string
        PolicyMode    policy.Mode
        DockerHost    string
        LogLevel      slog.Level
        LogMaxAge     time.Duration
        LogMaxTotalMB int
        AuditMaxAge   time.Duration
        AuditMaxRows  int
    }

    // Load reads configuration from getenv (os.Getenv in production, a fake in tests).
    // It returns *ValidationError listing EVERY problem found, never just the first.
    func Load(getenv func(string) string) (Config, error)
    ```
  Derived paths as methods so no other package concatenates them:
    ```go
    func (c Config) DBPath() string       { return filepath.Join(c.StateDir, "devmon.db") }
    func (c Config) CertsDir() string     { return filepath.Join(c.StateDir, "certs") }
    func (c Config) LogsDir() string      { return filepath.Join(c.StateDir, "logs") }
    func (c Config) AgentLogPath() string { return filepath.Join(c.LogsDir(), "agent.log") }
    ```
  All defaults as named constants at package top — no literals inside `Load`.
- **MIRROR**: ERROR_HANDLING (`ValidationError` aggregation), NAMING_CONVENTION (`env*` constants).
- **IMPORTS**: `fmt`, `log/slog`, `net`, `net/url`, `os`, `path/filepath`, `strconv`, `strings`, `time`, `github.com/scnplt/devmon-agent/internal/policy`.
- **GOTCHA**:
  - Take `getenv func(string) string` as a parameter rather than calling `os.Getenv` directly. Tests must not mutate process env — `t.Setenv` forbids `t.Parallel()`, and this package's table test is the largest in the repo.
  - Enforce **both** cross-field rules from § Startup Configuration Contract. The audit-vs-operational age rule is a PRD requirement, not a nicety.
  - `DEVMON_PUBLIC_ADDR` has **no default**. A missing value is a validation error, because a server certificate with no SAN is useless and the failure would otherwise surface as an opaque TLS error on the phone weeks later.
  - Validate each `PublicAddrs` entry with `net.ParseIP(e) != nil || isValidDNSName(e)`. Reject entries containing `:` or `/`.
- **VALIDATE**: `go test ./internal/config/ -race -cover`; table asserts (a) all-defaults path, (b) every field's happy override, (c) every rejection message, (d) that a config with **three** simultaneous errors reports all three.

---

### Task 4: Persistent logging with bounded growth

- **ACTION**: Build the slog logger writing to a rotated file on the state mount plus stderr, and the ticker that makes age-based retention actually happen.
- **IMPLEMENT**: `internal/logging/logging.go`
    ```go
    package logging

    // maxBackups is the number of rotated files kept. Total on-disk budget is
    // divided across (maxBackups + 1) files because lumberjack's MaxSize is
    // PER FILE, not a total. See the Rotator doc comment.
    const maxBackups = 3

    type Sink struct {
        Logger  *slog.Logger
        rotator *Rotator
        lj      *lumberjack.Logger
    }

    func NewSink(cfg config.Config) (*Sink, error) {
        if err := os.MkdirAll(cfg.LogsDir(), 0o700); err != nil {
            return nil, fmt.Errorf("create logs dir %s: %w", cfg.LogsDir(), err)
        }
        lj := &lumberjack.Logger{
            Filename:   cfg.AgentLogPath(),
            MaxSize:    cfg.LogMaxTotalMB / (maxBackups + 1), // MB PER FILE
            MaxBackups: maxBackups,
            MaxAge:     int(cfg.LogMaxAge.Hours() / 24),      // days
            Compress:   true,
            LocalTime:  false,
        }
        w := io.MultiWriter(lj, os.Stderr)
        h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel})
        ...
    }

    func (s *Sink) Run(ctx context.Context) error { return s.rotator.Run(ctx) }
    func (s *Sink) Close() error                  { return s.lj.Close() }
    ```
  `internal/logging/rotator.go` — exactly the SERVICE_PATTERN snippet, with `interval = 24h`.
- **MIRROR**: SERVICE_PATTERN (`Run(ctx) error`), LOGGING_PATTERN, ERROR_HANDLING.
- **IMPORTS**: `context`, `fmt`, `io`, `log/slog`, `os`, `time`, `gopkg.in/natefinch/lumberjack.v2`, `github.com/scnplt/devmon-agent/internal/config`.
- **GOTCHA** *(G4 — the single most likely bug in this task)*:
  1. **`MaxSize` is per-file, in MB.** Setting `MaxSize: cfg.LogMaxTotalMB` yields a real budget of `LogMaxTotalMB × (maxBackups+1)` — 4× the operator's stated limit. The PRD's promise is "the agent must never be the reason a small VPS runs out of disk"; a silent 4× overshoot breaks it. Divide.
  2. **`MaxSize: 0` means unlimited**, not zero. This is why config validation floors `DEVMON_LOG_MAX_TOTAL_MB` at 8 — at `maxBackups = 3`, integer division of anything below 4 yields 0 and disables the size cap entirely. The floor of 8 leaves margin.
  3. **lumberjack rotates on size only.** `MaxAge` prunes old files *when a rotation occurs*. A quiet agent never fills a file, never rotates, and therefore never applies the one-day limit — its `agent.log` would grow forever below the size cap and hold entries from months ago. The `Rotator` ticker calling `lj.Rotate()` every 24h is what makes `DEVMON_LOG_MAX_AGE_DAYS` real. Do not delete it as redundant.
  4. Tee to stderr as well as the file so `docker logs devmon-agent` works — that is the operator's first diagnostic and the UX diagram above depends on it.
- **VALIDATE**: `go test ./internal/logging/ -race`. Tests: (a) write, close, reopen a second `Sink` on the same `t.TempDir()`, assert the first run's lines are still in the file (this is the "logs survive a crash" success signal); (b) construct with `LogMaxTotalMB: 8`, assert `lj.MaxSize == 2`; (c) call `rotator.lj.Rotate()` directly and assert a second file appears.

---

### Task 5: SQLite state store

- **ACTION**: Open (or create) the state database with correct concurrency pragmas, apply the v1 schema, detect first-run and corruption, and implement audit retention.
- **IMPLEMENT**: `internal/state/schema.go` holds the DDL from § Database Schema (v1) as a `const schemaV1 = ` string plus `const currentSchemaVersion = 1`.
  `internal/state/store.go`:
    ```go
    package state

    import _ "modernc.org/sqlite" // registers driver name "sqlite"

    var (
        ErrStateCorrupt = errors.New("state store is unreadable or corrupt")
        ErrSchemaTooNew = errors.New("state store was written by a newer agent version")
    )

    type Store struct {
        db       *sql.DB
        log      *slog.Logger
        FirstRun bool // true when the database file did not exist before Open
    }

    func Open(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
        _, statErr := os.Stat(path)
        firstRun := errors.Is(statErr, fs.ErrNotExist)

        dsn := "file:" + path +
            "?_journal_mode=WAL" +
            "&_busy_timeout=5000" +
            "&_foreign_keys=1" +
            "&_synchronous=NORMAL" +
            "&_txlock=immediate"

        db, err := sql.Open("sqlite", dsn)
        if err != nil {
            return nil, fmt.Errorf("open state store at %s: %w", path, err)
        }
        db.SetMaxOpenConns(1)   // see GOTCHA
        db.SetMaxIdleConns(1)
        db.SetConnMaxLifetime(0)

        if err := db.PingContext(ctx); err != nil {
            db.Close()
            return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, path, err)
        }
        // integrity_check catches a truncated/garbage file that Ping accepts.
        var res string
        if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
            db.Close()
            return nil, fmt.Errorf("%w: %s: integrity_check=%q", ErrStateCorrupt, path, res)
        }
        ...
    }

    func (s *Store) SchemaVersion(ctx context.Context) (int, error)
    func (s *Store) PruneAudit(ctx context.Context, maxAge time.Duration, maxRows int) (int64, error)
    func (s *Store) Close() error
    ```
  `PruneAudit` runs both rules in one transaction:
    ```sql
    DELETE FROM audit WHERE occurred_at < ?;
    DELETE FROM audit WHERE id NOT IN (
        SELECT id FROM audit ORDER BY id DESC LIMIT ?
    );
    ```
  `internal/state/pruner.go` uses the SERVICE_PATTERN `Run(ctx)` shape, ticking every 6h and calling `PruneAudit`.
- **MIRROR**: REPOSITORY_PATTERN, ERROR_HANDLING (sentinel errors), SERVICE_PATTERN (pruner).
- **IMPORTS**: `context`, `database/sql`, `errors`, `fmt`, `io/fs`, `log/slog`, `os`, `time`, `_ "modernc.org/sqlite"`.
- **GOTCHA**:
  - The driver name is **`"sqlite"`**, not `"sqlite3"`. `sql.Open` will not error on a wrong name until first use, so the failure appears far from the cause.
  - `SetMaxOpenConns(1)` is deliberate. WAL permits one writer and many readers, but `database/sql` hands out connections opaquely, so a multi-connection pool produces intermittent `SQLITE_BUSY` under concurrent writes. At this workload (a handful of writes per minute) a single connection costs nothing and removes an entire class of flaky failure. `_busy_timeout=5000` then covers contention with the *other process* — the Phase 2 `docker exec` CLI — which is the case that actually matters.
  - `_txlock=immediate` takes the write lock at `BEGIN` rather than on first write, which prevents the deadlock where two processes both start deferred transactions and then both try to upgrade.
  - Detect first-run by `os.Stat` **before** `sql.Open`. `sql.Open` is lazy and creates the file on first use, so checking afterwards always reports "exists".
  - `PRAGMA integrity_check` is required in addition to `Ping`. A zero-length or truncated `devmon.db` — the realistic outcome of a botched restore — passes `Ping` and then fails obscurely at query time. The PRD demands loud, early failure.
  - Set the file mode to `0600` after creation (`os.Chmod`); SQLite creates it `0644` by default and it will hold device records.
- **VALIDATE**: `go test ./internal/state/ -race -cover`. Tests: (a) `Open` on an empty `t.TempDir()` → `FirstRun == true`, `SchemaVersion() == 1`; (b) close and reopen → `FirstRun == false`, still v1; (c) write 32 bytes of garbage to the path, `Open` → `errors.Is(err, ErrStateCorrupt)`; (d) set `schema_meta.version = 99`, `Open` → `errors.Is(err, ErrSchemaTooNew)`; (e) insert 100 audit rows across a 400-day span, `PruneAudit(ctx, 365*24h, 50)` → 50 remain and none older than the cutoff; (f) two `Store`s open on one file, concurrent writes with `-race`, no `SQLITE_BUSY`.

---

### Task 6: Server certificate — generate, persist, reload

- **ACTION**: Produce a self-signed EC server certificate on first start, reuse it on subsequent starts, and detect when the configured SANs no longer match it.
- **IMPLEMENT**: `internal/certs/selfsigned.go`
    ```go
    package certs

    const (
        serverCertValidity = 398 * 24 * time.Hour // browser/mobile max for leaf certs
        serverKeyFileMode  = 0o600
        serverCrtFileMode  = 0o644
    )

    // GenerateServerCert produces a self-signed EC P-256 leaf for the given SANs.
    // Phase 2 replaces this with CA-issued certificates; the returned shape is
    // identical so tlsconf and httpapi need no change.
    func GenerateServerCert(sans []string, notBefore time.Time) (certPEM, keyPEM []byte, err error)
    ```
  Build the template with `x509.Certificate{ KeyUsage: DigitalSignature|KeyEncipherment, ExtKeyUsage: []{ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: false }`, split `sans` into `DNSNames` and `IPAddresses` via `net.ParseIP`, and use a `crypto/rand`-generated 128-bit serial.

  `internal/certs/store.go`:
    ```go
    // LoadOrCreateServerCert returns the persisted server keypair, generating and
    // writing one if absent. sanDrift is true when the existing certificate does
    // not cover every address in sans — Phase 1 only reports it; Phase 2 re-issues.
    func LoadOrCreateServerCert(dir string, sans []string, log *slog.Logger) (tls.Certificate, sanDrift bool, err error)
    ```
- **MIRROR**: ERROR_HANDLING (`%w` wrapping), LOGGING_PATTERN (a `WARN` on generation and on drift).
- **IMPORTS**: `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/tls`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `fmt`, `log/slog`, `math/big`, `net`, `os`, `path/filepath`, `time`.
- **GOTCHA**:
  - Write the key with `os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)`, not `os.WriteFile` then `Chmod`. The chmod version leaves a window where the private key is world-readable, and `gosec` flags it (G0306). `O_EXCL` additionally prevents two racing starts from interleaving writes.
  - Marshal the EC key with `x509.MarshalPKCS8PrivateKey` and PEM type `"PRIVATE KEY"`. `MarshalECPrivateKey` + `"EC PRIVATE KEY"` also works with Go but is the less portable form for the Android client.
  - Do **not** log the key, the key path's contents, or the PEM bytes — LOGGING_PATTERN forbids it. Log the SANs and the `NotAfter` only.
  - 398 days is not arbitrary: it is the CA/Browser Forum leaf maximum that mobile TLS stacks enforce. A 10-year self-signed cert is silently rejected by modern Android.
  - SAN drift is a `WARN`, **not** a fatal error, in Phase 1. Re-issuance needs the CA, which does not exist until Phase 2. Failing here would make a VPS IP change unrecoverable without deleting state — the exact outcome the PRD is trying to prevent.
- **VALIDATE**: `go test ./internal/certs/ -race`. Tests: (a) generate into `t.TempDir()`, parse the result, assert `DNSNames`/`IPAddresses` match the input and `NotAfter ≈ now+398d`; (b) call `LoadOrCreateServerCert` twice, assert the serial is identical (no silent regeneration); (c) call again with an extra SAN, assert `sanDrift == true` and the serial is *still* unchanged; (d) `os.Stat(server.key).Mode().Perm() == 0o600`.

---

### Task 7: TLS configuration builder

- **ACTION**: Build the `*tls.Config` for the single listening port, with the seam that lets one port serve both authenticated and unauthenticated routes.
- **IMPLEMENT**: `internal/tlsconf/tlsconf.go`
    ```go
    package tlsconf

    // Build returns the TLS config for the agent's single listening port.
    //
    // clientCAs is nil in Phase 1 (no CA exists yet) and non-nil from Phase 2.
    // When it is non-nil the mode is VerifyClientCertIfGiven, NOT
    // RequireAndVerifyClientCert — see the doc comment below.
    func Build(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
        cfg := &tls.Config{
            Certificates: []tls.Certificate{cert},
            MinVersion:   tls.VersionTLS13,
            NextProtos:   []string{"h2", "http/1.1"},
        }
        if clientCAs != nil {
            cfg.ClientCAs = clientCAs
            cfg.ClientAuth = tls.VerifyClientCertIfGiven
        }
        return cfg
    }
    ```
- **MIRROR**: NAMING_CONVENTION (`Build` returning a concrete `*tls.Config`).
- **IMPORTS**: `crypto/tls`, `crypto/x509`.
- **GOTCHA** *(G3 — an architectural constraint, not a detail)*:
  - `ClientAuth` is a property of the **listener**, settled during the TLS handshake, before any HTTP request line is parsed. Go's `crypto/tls` offers no per-route control. The PRD mandates one port that serves mTLS-protected routes *and* `GET /v1/status` without a client certificate. `tls.RequireAndVerifyClientCert` would terminate the status request at the handshake, making the endpoint unreachable in exactly the failure case it exists for.
  - Therefore: **`tls.VerifyClientCertIfGiven`** — a presented certificate is fully verified against `ClientCAs` and a bad one is rejected at the handshake; an absent certificate lets the connection through to HTTP, where the `requireDevice` middleware (Task 9) enforces authentication per route. Authentication strength is unchanged; the enforcement point moves from the handshake to the router for one allowlisted path.
  - `MinVersion: tls.VersionTLS13` is a deliberate greenfield choice — both peers are ours, TLS 1.3 removes the entire downgrade/cipher-negotiation surface, and Android has supported it since API 29.
  - Do **not** set `CipherSuites`; it is ignored for TLS 1.3 and setting it signals a misunderstanding to reviewers.
  - Do **not** set `InsecureSkipVerify` anywhere. `gosec` G402 will fail the build, correctly.
- **VALIDATE**: `go test ./internal/tlsconf/ -race`: `Build(cert, nil)` → `ClientAuth == tls.NoClientCert` and `ClientCAs == nil`; `Build(cert, pool)` → `ClientAuth == tls.VerifyClientCertIfGiven`; both → `MinVersion == tls.VersionTLS13`.

---

### Task 8: Docker Engine connectivity

- **ACTION**: Construct the Docker client and verify the Engine is reachable at startup.
- **IMPLEMENT**: `internal/dockerx/client.go`
    ```go
    package dockerx

    import "github.com/moby/moby/client"

    const pingTimeout = 5 * time.Second

    type Client struct {
        api *client.Client
        log *slog.Logger
    }

    // New dials the Docker Engine and verifies it responds. A host whose Engine is
    // unreachable cannot serve any agent operation, so this is a fatal startup error
    // rather than a degraded mode.
    func New(ctx context.Context, host string, log *slog.Logger) (*Client, error) {
        api, err := client.New(
            client.WithHost(host),
            client.WithUserAgent("devmon-agent/"+version.Version),
        )
        if err != nil {
            return nil, fmt.Errorf("create docker client for %s: %w", host, err)
        }

        pctx, cancel := context.WithTimeout(ctx, pingTimeout)
        defer cancel()

        res, err := api.Ping(pctx, client.PingOptions{NegotiateAPIVersion: true})
        if err != nil {
            api.Close()
            return nil, fmt.Errorf("ping docker engine at %s: %w", host, err)
        }
        log.Info("docker engine reachable",
            slog.String("api_version", res.APIVersion),
            slog.String("os_type", res.OSType),
        )
        return &Client{api: api, log: log}, nil
    }

    func (c *Client) Close() error { return c.api.Close() }
    ```
- **MIRROR**: SERVICE_PATTERN (constructor DI), ERROR_HANDLING, LOGGING_PATTERN.
- **IMPORTS**: `context`, `fmt`, `log/slog`, `time`, `github.com/moby/moby/client`, `github.com/scnplt/devmon-agent/internal/version`.
- **GOTCHA** *(G1 — verified against upstream on 2026-08-07; every clause here contradicts the pre-v29 API that a model will produce from memory)*:
  - Import **`github.com/moby/moby/client`**. `github.com/docker/docker/client` is deprecated and its last module release was v28.5.2; the local daemon is 29.6.1.
  - The constructor is **`client.New(opts...)`**. `client.NewClientWithOpts` still exists but is deprecated.
  - **`Ping` takes two arguments** — `Ping(ctx, client.PingOptions{...})` — and returns `(client.PingResult, error)`. The one-argument `Ping(ctx)` is the pre-v29 signature and will not compile.
  - Version negotiation moved onto `PingOptions.NegotiateAPIVersion`. **`client.WithAPIVersionNegotiation()` is deprecated** — do not pass it as an `Opt`.
  - `client.FromEnv` reads `DOCKER_HOST` from the process environment. Use `client.WithHost(cfg.DockerHost)` instead, so the agent's socket path comes from *its own* validated configuration and cannot be redirected by an unrelated env var.
  - For later phases, recorded so this is not re-researched: `ContainerList(ctx, client.ContainerListOptions{All: true})` returns `(ContainerListResult, error)`; `ContainerLogs(ctx, id, client.ContainerLogsOptions{...})` returns a `ContainerLogsResult` which **is** an `io.ReadCloser` the caller must `Close()`, demultiplexed with `stdcopy.StdCopy` from `github.com/moby/moby/api/pkg/stdcopy`.
  - Expose **nothing beyond `New`/`Close` in this phase.** Adding a `ContainerList` wrapper "since it's easy" is Phase 3 scope and is listed under NOT Building.
- **VALIDATE**: `go build ./internal/dockerx/` compiles against the pinned SDK (this alone catches every signature error above). `go test ./internal/dockerx/ -race` asserts that `New` with `unix:///nonexistent.sock` returns an error wrapping the host string. **Do not** write a test requiring a live Docker daemon — that belongs in the manual checklist.

---

### Task 9: HTTP server, middleware, and the status endpoint

- **ACTION**: Build the TLS HTTP server, its middleware chain, and the single Phase 1 route.
- **IMPLEMENT**:
  `internal/httpapi/respond.go` — `writeJSON` exactly as in HTTP_HANDLER_PATTERN, plus:
    ```go
    type errorBody struct {
        Error string `json:"error"`
    }

    // writeError returns a deliberately terse message. This port is internet-facing
    // and the PRD forbids leaking host detail to unauthenticated callers.
    func writeError(w http.ResponseWriter, code int, msg string) {
        writeJSON(w, code, errorBody{Error: msg})
    }
    ```
  `internal/httpapi/status.go`:
    ```go
    // APIVersion is the contract version the Android app negotiates against.
    // PRD: "Versioned API contract" — the app ships independently of the agent.
    const APIVersion = "v1"

    // statusResponse is the ONLY payload served without a client certificate.
    // Its fields are a strict allowlist (PRD: "may inform, never issue"): version,
    // policy, server time — and, from Phase 2, the CA fingerprint. It must never
    // carry host, container, or credential data.
    type statusResponse struct {
        APIVersion   string `json:"api_version"`
        AgentVersion string `json:"agent_version"`
        PolicyMode   string `json:"policy_mode"`
        ServerTime   string `json:"server_time"`
    }
    ```
  `internal/httpapi/middleware.go`:
    ```go
    // requireDevice rejects any request without a verified client certificate.
    // Phase 1 has no CA, so every request is rejected — the routes it guards do
    // not exist yet. Phase 2 fills in the device lookup against state.Store.
    func (s *Server) requireDevice(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
                writeError(w, http.StatusUnauthorized, "client certificate required")
                return
            }
            // Phase 2: resolve r.TLS.PeerCertificates[0] to a non-revoked device.
            writeError(w, http.StatusUnauthorized, "client certificate required")
        })
    }

    func (s *Server) withRecovery(next http.Handler) http.Handler   // 500 + log, never a stack trace to the client
    func (s *Server) withRequestLog(next http.Handler) http.Handler // Debug level; log method+path+status+duration, never headers
    ```
  `internal/httpapi/server.go`:
    ```go
    const (
        readHeaderTimeout = 10 * time.Second
        readTimeout       = 30 * time.Second
        writeTimeout      = 30 * time.Second
        idleTimeout       = 120 * time.Second
        maxHeaderBytes    = 16 << 10 // 16 KiB
        shutdownGrace     = 15 * time.Second
    )

    type Server struct {
        cfg  config.Config
        st   *state.Store
        log  *slog.Logger
        http *http.Server
    }

    func NewServer(cfg config.Config, st *state.Store, tlsCfg *tls.Config, log *slog.Logger) *Server

    // Run serves until ctx is cancelled, then drains with shutdownGrace.
    func (s *Server) Run(ctx context.Context) error
    ```
- **MIRROR**: HTTP_HANDLER_PATTERN, SERVICE_PATTERN (`Run(ctx) error`), LOGGING_PATTERN.
- **IMPORTS**: `context`, `crypto/tls`, `encoding/json`, `errors`, `log/slog`, `net/http`, `time`, plus internal `config`, `state`, `version`.
- **GOTCHA**:
  - Register with Go 1.22+ method patterns: `mux.HandleFunc("GET /v1/status", s.handleStatus)`. Registering `"/v1/status"` alone also matches `POST`, `DELETE`, and every other method.
  - `srv.ListenAndServeTLS("", "")` — **both arguments empty**. The certificate comes from `TLSConfig.Certificates`; passing paths here would re-read from disk and bypass everything Task 6 built.
  - `Run` must treat `http.ErrServerClosed` as success: `if err := ...; err != nil && !errors.Is(err, http.ErrServerClosed) { return err }`. Otherwise a clean SIGTERM shutdown exits non-zero and Docker records the container as failed.
  - `ReadHeaderTimeout` is the one that matters on an internet-facing port — without it a Slowloris client holds a connection open indefinitely. `gosec` G114 flags its absence.
  - `writeError` messages must stay generic. Do not include the state path, the Docker host, or the reason a certificate failed.
  - Do **not** register a catch-all `/` handler that returns anything descriptive. An unmatched path gets `ServeMux`'s default 404.
- **VALIDATE**: `go test ./internal/httpapi/ -race -cover`. The status test uses `httptest.NewRequest` + `httptest.NewRecorder` and asserts: 200; `Content-Type: application/json`; `server_time` parses as RFC3339 and is within 5s of now; `policy_mode` reflects the injected config; and — decoding into `map[string]any` — **`len(body) == 4`**. That last assertion is the guard that stops a later phase from quietly widening a pre-authentication surface, and Phase 2 must consciously change it to 5.

---

### Task 10: Wire it together in `main`

- **ACTION**: Compose all components, run them concurrently, and shut down cleanly.
- **IMPLEMENT**: `cmd/devmon-agent/main.go`
    ```go
    // Exit codes: 0 clean shutdown, 1 runtime failure, 2 invalid configuration.
    const (
        exitOK      = 0
        exitFailure = 1
        exitConfig  = 2
    )

    func main() {
        if err := run(); err != nil {
            var vErr *config.ValidationError
            if errors.As(err, &vErr) {
                fmt.Fprintln(os.Stderr, vErr.Error())
                os.Exit(exitConfig)
            }
            fmt.Fprintf(os.Stderr, "devmon-agent: %v\n", err)
            os.Exit(exitFailure)
        }
    }
    ```
  `run()` in strict order — every step's failure is fatal and specific:
  1. `flag`: support only `-version`, printing `version.Version`/`Commit`/`BuildTime` and exiting 0.
  2. `config.Load(os.Getenv)`.
  3. `os.MkdirAll(cfg.StateDir, 0o700)`, then `certs/` and `logs/`.
  4. `logging.NewSink(cfg)` — from here on, everything logs through it.
  5. `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`.
  6. `state.Open(ctx, cfg.DBPath(), log)`; `defer st.Close()`; log `first_run` and `schema_version`.
  7. `certs.LoadOrCreateServerCert(cfg.CertsDir(), cfg.PublicAddrs, log)`; `WARN` on drift.
  8. `dockerx.New(ctx, cfg.DockerHost, log)`; `defer dc.Close()`.
  9. `tlsconf.Build(cert, nil)` — `nil` because there is no CA until Phase 2.
  10. `httpapi.NewServer(...)`.
  11. Start `sink.Run`, `pruner.Run`, and `srv.Run` concurrently on the same `ctx`; wait for all three; return the first non-`context.Canceled` error.
  12. Log `"agent listening"` per LOGGING_PATTERN before blocking.
- **MIRROR**: ERROR_HANDLING, LOGGING_PATTERN, SERVICE_PATTERN.
- **IMPORTS**: `context`, `errors`, `flag`, `fmt`, `os`, `os/signal`, `sync`, `syscall`, plus all internal packages.
- **GOTCHA**:
  - `main` does nothing but call `run()` and map its error to an exit code. `os.Exit` skips `defer`, so any `defer` in `main` would silently never run — including the log flush.
  - Configuration failures must exit **before** the log sink is created, so they land on stderr as plain readable text rather than as structured slog lines. This is why step 2 precedes step 4.
  - `signal.NotifyContext` handles both SIGINT and SIGTERM. Docker sends SIGTERM; omitting it means `docker stop` always takes the full 10s timeout and then SIGKILLs, which corrupts the WAL.
  - Close in reverse order of construction: HTTP server drains → Docker client → state store → log sink. Closing the state store while a request is in flight is the classic shutdown bug.
  - Use `sync.WaitGroup` or three goroutines with an error channel. `golang.org/x/sync/errgroup` is fine but adds a dependency for ~15 lines — prefer stdlib.
- **VALIDATE**: `go build ./cmd/devmon-agent/`. Manual: start with a valid config, `curl -k https://localhost:8443/v1/status`, `docker stop` → exit code 0 and a `"shutting down"` line in `agent.log`. Start with `DEVMON_POLICY_MODE=bogus` → exit code 2 and a readable stderr list.

---

### Task 11: Container image and compose example

- **ACTION**: Produce a minimal static image and the reference run configuration.
- **IMPLEMENT**: `Dockerfile`
    ```dockerfile
    FROM golang:1.26-alpine AS build
    WORKDIR /src
    COPY go.mod go.sum ./
    RUN go mod download
    COPY . .
    ARG VERSION=dev
    ARG COMMIT=none
    ARG BUILD_TIME=unknown
    RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w \
          -X github.com/scnplt/devmon-agent/internal/version.Version=${VERSION} \
          -X github.com/scnplt/devmon-agent/internal/version.Commit=${COMMIT} \
          -X github.com/scnplt/devmon-agent/internal/version.BuildTime=${BUILD_TIME}" \
        -o /out/devmon-agent ./cmd/devmon-agent

    FROM gcr.io/distroless/static-debian12:nonroot
    COPY --from=build /out/devmon-agent /usr/local/bin/devmon-agent
    USER nonroot:nonroot
    EXPOSE 8443
    ENTRYPOINT ["/usr/local/bin/devmon-agent"]
    ```
  `compose.example.yaml`:
    ```yaml
    services:
      devmon-agent:
        image: ghcr.io/scnplt/devmon-agent:0.1.0
        restart: unless-stopped
        ports:
          - "8443:8443"
        volumes:
          # A BIND MOUNT, deliberately — not a named volume. The operator can see,
          # back up, and restore it, and `docker compose down -v` cannot destroy it.
          - /var/lib/devmon:/var/lib/devmon
          - /var/run/docker.sock:/var/run/docker.sock:ro
        group_add:
          # Must equal the host's docker group GID: stat -c '%g' /var/run/docker.sock
          - "999"
        environment:
          DEVMON_PUBLIC_ADDR: vps.example.com
          DEVMON_POLICY_MODE: default
          DEVMON_LOG_MAX_AGE_DAYS: "1"
          DEVMON_LOG_MAX_TOTAL_MB: "64"
    ```
- **MIRROR**: N/A (build artifacts).
- **IMPORTS**: N/A.
- **GOTCHA**:
  - `CGO_ENABLED=0` is mandatory. `modernc.org/sqlite` is pure Go, so the binary stays static and runs on `distroless/static`. Leaving CGO on produces a dynamically linked binary that exits immediately on distroless with a confusing "no such file or directory" for a file that plainly exists.
  - **Two host-side prerequisites that will otherwise look like agent bugs**, both consequences of running as `nonroot` (UID 65532):
    1. The bind-mount directory must be owned by that UID: `sudo mkdir -p /var/lib/devmon && sudo chown 65532:65532 /var/lib/devmon`. Otherwise startup fails at `MkdirAll` with `permission denied`.
    2. The container needs the host's docker group: `--group-add $(stat -c '%g' /var/run/docker.sock)`. Otherwise `dockerx.New` fails the ping with `permission denied` on the socket. The GID varies per host — hardcoding 999 works on Debian/Ubuntu and not elsewhere. The Phase 6 installer resolves this automatically; Phase 1 documents it.
  - Mount the socket `:ro`. It does not prevent writes through the Docker API (the API is request/response over the socket), but it does prevent the socket file itself being replaced, and it states intent.
  - Do not `apk add ca-certificates` — the agent makes no outbound TLS connections. `distroless/static-debian12` already carries a CA bundle if a later phase needs one.
- **VALIDATE**: `docker build -t devmon-agent:dev .` succeeds; `docker run --rm devmon-agent:dev -version` prints the injected version; `docker image inspect` shows a final size under ~25 MB.

---

### Task 12: Test sweep, lint, and security scan

- **ACTION**: Bring the package to the project's quality bar and close out the phase.
- **IMPLEMENT**:
  - Fill any coverage gaps found by `go test -coverprofile`; the floor is **80%** per `.claude/rules/ecc/common/testing.md`.
  - `README.md`: the config table from § Startup Configuration Contract, the state layout tree, the two host prerequisites from Task 11, and an explicit backup instruction (`tar czf devmon-backup.tgz /var/lib/devmon` with the agent stopped).
  - Verify `gofmt -l .` and `goimports -l .` are both empty.
  - Update `.claude/PRPs/prds/devmon-agent.prd.md` Phase 1 row: status → `complete`.
- **MIRROR**: TEST_STRUCTURE.
- **IMPORTS**: N/A.
- **GOTCHA**: `cmd/devmon-agent` and generated schema constants will pull the total down; measure coverage over `./internal/...` and state that scope in the Makefile so the number is honest rather than gamed.
- **VALIDATE**: every command in § Validation Commands passes.

---

## Testing Strategy

### Unit Tests

| Test | Input | Expected Output | Edge Case? |
|---|---|---|---|
| `ParseMode` default | `""` | `ModeDefault` | ✔ PRD-critical |
| `ParseMode` invalid | `"admin"` | error naming all three valid modes | ✔ |
| `Mode.Allows` matrix | 7 ops × 3 modes | read-only denies start; default denies delete; full allows all | ✔ |
| `Mode.Allows` unknown op | `Operation("exec")` | `false` (fails closed) | ✔ |
| `config.Load` defaults | only `DEVMON_PUBLIC_ADDR` set | all documented defaults | |
| `config.Load` missing SAN | empty env | `ValidationError` naming `DEVMON_PUBLIC_ADDR` | ✔ |
| `config.Load` aggregation | 3 simultaneous bad values | all 3 listed in one error | ✔ |
| `config.Load` audit < log age | audit=1, log=7 | rejected, message cites the PRD rule | ✔ |
| `config.Load` size floor | `LOG_MAX_TOTAL_MB=4` | rejected (would zero the cap) | ✔ |
| `logging` persistence | write → close → reopen → read | first run's lines still present | ✔ crash-survival signal |
| `logging` per-file math | `LogMaxTotalMB=8` | `lj.MaxSize == 2` | ✔ |
| `state.Open` first run | empty dir | `FirstRun=true`, schema v1 | |
| `state.Open` reopen | existing db | `FirstRun=false`, schema v1 | |
| `state.Open` corrupt | 32 random bytes at path | `errors.Is(err, ErrStateCorrupt)` | ✔ |
| `state.Open` newer schema | `version=99` | `errors.Is(err, ErrSchemaTooNew)` | ✔ |
| `PruneAudit` | 100 rows / 400-day span, keep 365d & 50 rows | 50 remain, none past cutoff | ✔ |
| `state` concurrency | 2 `Store`s, concurrent writes, `-race` | no error, no `SQLITE_BUSY` | ✔ cross-process contract |
| `certs` generate | 1 DNS + 1 IP SAN | both in the parsed cert; `NotAfter≈+398d` | |
| `certs` idempotence | two `LoadOrCreate` calls | identical serial | ✔ no silent re-identity |
| `certs` SAN drift | reload with an added SAN | `sanDrift=true`, serial unchanged | ✔ |
| `certs` key mode | after generation | `0600` | ✔ security |
| `tlsconf` no CA | `Build(cert, nil)` | `NoClientCert`, TLS 1.3 floor | |
| `tlsconf` with CA | `Build(cert, pool)` | `VerifyClientCertIfGiven` | ✔ G3 |
| `dockerx.New` bad socket | `unix:///nonexistent.sock` | error wrapping the host string | ✔ |
| `handleStatus` shape | any request | 200, JSON, **exactly 4 keys** | ✔ pre-auth allowlist |
| `handleStatus` time | any request | `server_time` RFC3339, within 5s of now | |
| `requireDevice` no cert | request with `r.TLS.PeerCertificates` empty | 401, terse body | ✔ |

### Edge Cases Checklist

- [ ] Empty input — `config.Load` with a completely empty environment
- [ ] Maximum size input — `DEVMON_PUBLIC_ADDR` with 20 comma-separated SANs
- [ ] Invalid types — non-numeric values in every integer env var
- [ ] Concurrent access — two `state.Store` handles writing simultaneously under `-race`
- [ ] Network failure — Docker socket absent at startup (fatal, specific message)
- [ ] Permission denied — state dir not writable by the process UID (fatal at `MkdirAll`)
- [ ] Corrupt persisted state — truncated `devmon.db`, garbage `server.key`
- [ ] Clock behaviour — `server_time` is UTC and RFC3339, never local time
- [ ] Restart idempotence — two consecutive starts do not change the certificate serial or the schema version

---

## Validation Commands

### Static Analysis

```bash
gofmt -l .
goimports -l .
go vet ./...
```
EXPECT: All produce **no output**. Any listed file is unformatted or has a vet finding.

### Lint

```bash
golangci-lint run ./...
```
EXPECT: Zero issues. (If `golangci-lint` is not installed, `go vet ./...` is the minimum bar and the Makefile's `lint` target should say so.)

### Security Scan

```bash
gosec ./...
```
EXPECT: Zero findings. Watch specifically for G114 (missing `ReadHeaderTimeout`), G302/G306 (permissive file modes on the key), G402 (weak TLS config). All three are pre-empted by Tasks 6, 7, and 9 — a finding means one of those gotchas was missed.

### Unit Tests

```bash
go test ./internal/... -race
```
EXPECT: `ok` for every package, zero race reports.

### Full Test Suite with Coverage

```bash
go test ./internal/... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1
```
EXPECT: All pass; total coverage **≥ 80%** per `.claude/rules/ecc/common/testing.md`.

### Build Verification

```bash
go build ./...
CGO_ENABLED=0 go build -o bin/devmon-agent ./cmd/devmon-agent
docker build -t devmon-agent:dev .
docker run --rm devmon-agent:dev -version
```
EXPECT: Clean build; the container prints the injected version and exits 0.

### Database Validation

```bash
# With the agent stopped:
sqlite3 /var/lib/devmon/devmon.db "PRAGMA integrity_check; PRAGMA journal_mode;"
sqlite3 /var/lib/devmon/devmon.db "SELECT value FROM schema_meta WHERE key='version';"
sqlite3 /var/lib/devmon/devmon.db ".tables"
```
EXPECT: `ok`; `wal`; `1`; tables `audit devices schema_meta`.

### End-to-End Validation (real host)

```bash
sudo mkdir -p /var/lib/devmon && sudo chown 65532:65532 /var/lib/devmon
docker run -d --name devmon-agent \
  -v /var/lib/devmon:/var/lib/devmon \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -p 8443:8443 \
  -e DEVMON_PUBLIC_ADDR=vps.example.com \
  devmon-agent:dev

curl -sk https://localhost:8443/v1/status | jq .
openssl s_client -connect localhost:8443 -tls1_3 </dev/null 2>&1 | grep -E 'Protocol|Verify'
```
EXPECT: JSON with exactly `api_version`, `agent_version`, `policy_mode`, `server_time`; TLS 1.3 negotiated; self-signed verify error (expected — no CA until Phase 2).

### Manual Validation

- [ ] **Crash survival (phase success signal)**: run the agent, confirm lines in `/var/lib/devmon/logs/agent.log`, `docker kill devmon-agent`, restart, confirm the **pre-kill lines are still present** and new lines append below them.
- [ ] **Restart identity stability**: record `openssl x509 -in /var/lib/devmon/certs/server.crt -serial -noout`, restart the container, confirm the serial is **unchanged**.
- [ ] **Policy advertisement**: start with `DEVMON_POLICY_MODE=read-only`, confirm `/v1/status` reports `"policy_mode":"read-only"`.
- [ ] **Config failure UX**: start with `DEVMON_POLICY_MODE=bogus` *and* `DEVMON_LOG_MAX_AGE_DAYS=x`; confirm **both** problems are listed and the exit code is 2.
- [ ] **Missing SAN**: start with no `DEVMON_PUBLIC_ADDR`; confirm exit code 2 naming that variable.
- [ ] **Docker unreachable**: start without the socket mount; confirm a specific fatal `ping docker engine` error, not a nil-pointer panic.
- [ ] **Retention bound (phase success signal)**: start with `DEVMON_LOG_MAX_TOTAL_MB=8` and `DEVMON_LOG_LEVEL=debug`, drive traffic until rotation occurs, confirm `du -sh /var/lib/devmon/logs/` stays **at or below ~8 MB** and that `.gz` backups exist.
- [ ] **Graceful shutdown**: `docker stop devmon-agent`; confirm it exits within ~1s (not the 10s SIGKILL timeout) with exit code 0.
- [ ] **Method routing**: `curl -skX POST https://localhost:8443/v1/status` returns 405, not 200.
- [ ] **No unauthenticated leakage**: `curl -sk https://localhost:8443/v1/containers` returns 404/401 with a terse body containing no path, hostname, or version detail.

---

## Acceptance Criteria

- [ ] All 12 tasks completed
- [ ] All validation commands pass
- [ ] Tests written and passing under `-race`; `./internal/...` coverage ≥ 80%
- [ ] No `go vet` findings, no `gosec` findings
- [ ] `gofmt`/`goimports` clean
- [ ] `GET /v1/status` returns exactly four fields over TLS 1.3 with no client certificate — **PRD Phase 1 success signal**
- [ ] Log lines written before a `docker kill` are readable after restart — **PRD Phase 1 success signal**
- [ ] Log directory size stays within `DEVMON_LOG_MAX_TOTAL_MB` under sustained load — **PRD Phase 1 success signal**
- [ ] Server certificate serial and schema version are stable across restarts
- [ ] Invalid configuration exits 2 with every problem listed, before any state is touched

## Completion Checklist

- [ ] Code follows the patterns defined in § Patterns to Mirror (these are now the repo's conventions)
- [ ] Every error wrapped with `%w` and a caller-meaningful message
- [ ] Logging uses `log/slog` with structured attrs; no key material, pairing codes, or PEM bytes logged at any level
- [ ] No package-level mutable state, no `init()`, no globals
- [ ] All timeouts, sizes, and limits are named constants — no magic numbers
- [ ] Every file under 400 lines; every function under 50 lines
- [ ] README documents config, state layout, the two host prerequisites, and the backup procedure
- [ ] Nothing from § NOT Building was implemented
- [ ] PRD Phase 1 row updated to `complete`; Phase 2 becomes eligible

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Implementation writes the pre-v29 Docker SDK API from memory and does not compile | **H** | Low (caught at build) | Gotcha G1 gives every verified signature; `go build ./internal/dockerx/` is the check |
| `RequireAndVerifyClientCert` chosen, making `/v1/status` unreachable without a cert | **H** | High (breaks the phase's success signal *and* Phase 2's diagnosis path) | Gotcha G3 explains the constraint; `tlsconf` test asserts the mode |
| lumberjack `MaxSize` treated as a total budget → 4× the operator's stated disk limit | M | High (breaks "never fill a small VPS") | Gotcha G4 + a unit test asserting `MaxSize == total/(backups+1)` |
| Age-based retention silently never applies on a quiet agent | M | Medium | The `Rotator` ticker is a named task with its own test; its doc comment says why it is not redundant |
| `SQLITE_BUSY` flakiness once the Phase 2 CLI writes concurrently | M | Medium | `SetMaxOpenConns(1)` + `_busy_timeout=5000` + `_txlock=immediate`; a two-handle concurrent test in Phase 1 |
| Container cannot read the Docker socket or write the state dir as `nonroot` | **H** | Medium (looks like an agent bug, is a host setup issue) | Documented in Task 11 and the README; resolved automatically by the Phase 6 installer |
| Scope creep into Phase 2/3 ("the CA is easy", "one container list won't hurt") | M | Medium | § NOT Building enumerates each temptation explicitly |
| A later phase widens the pre-auth status payload without noticing | M | High (security surface) | `handleStatus` test asserts the response has **exactly 4 keys**, forcing a conscious change |
| CGO left enabled → binary fails on distroless with a misleading error | M | Low | `CGO_ENABLED=0` in both the Dockerfile and the Makefile; image smoke test in Task 11 |

## Notes

**Phase seams deliberately left in place.** Four are pre-built so Phase 2 is additive rather than a refactor: `tlsconf.Build` already takes a `clientCAs` parameter (Phase 1 passes `nil`); `requireDevice` middleware exists and rejects everything; `state.Store.FirstRun` exists so the "loud on missing identity" check has somewhere to live; and the `devices` table is created at schema v1 so Phase 2 adds no migration.

**Why the status payload is tested for an exact key count.** The PRD calls this endpoint an accepted pre-authentication surface, mitigated by a strict field allowlist, and rates its risk H. A test asserting only that the four expected keys are *present* would let a future field slip in unnoticed. Asserting the map length forces any addition through a deliberate test edit. Phase 2 will change 4 → 5 when `ca_fingerprint` lands.

**The two PRD Open Questions are untouched by this phase.** Whether the default log budgets survive a genuinely busy host is answerable only with the measurement the manual retention check produces — record the numbers when running it. Whether agent self-restart needs a dedicated recovery command is a Phase 5/6 question.

**Audit rows are pruned but never written in this phase.** Building the pruner now costs ~30 lines, keeps all retention logic and configuration in one phase where it can be validated together, and means Phase 5 adds only an `INSERT`. The pruner is a no-op against an empty table, so it is safe to ship.

**Deviation from the PRD's Phase 1 line worth noting:** the phase description mentions the status endpoint carrying "version, server time, and policy" — this plan implements exactly those three plus `api_version`, which the PRD separately mandates as a Must ("versioned API contract", "detect an incompatible agent before the user hits an error"). A client cannot act on a version it cannot read, so `api_version` belongs in the first release of the endpoint rather than being retrofitted.
