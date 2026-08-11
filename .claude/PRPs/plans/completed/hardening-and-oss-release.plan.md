# Plan: Hardening & OSS Release (DevMon Agent Phase 7)

## Summary

Phase 7 is the release gate. It adds the one Must-have capability still missing from the
PRD's list — **rate limiting on the listening port, and especially on the unauthenticated
endpoint** — then discharges the remaining release obligations: a security review written
against the PRD's own risk table, an automated installer that resolves the docker socket
GID and the state-mount ownership the operator currently has to get right by hand, a
published threat model and backup procedure, and AGPL-3.0-only licensing with a tagged
image the installer can actually pull.

Everything here is checked against the Phase 6 contract suite, which is the regression net
this phase exists to change the agent against. Rate limiting is the only change in this
phase that can break an existing route, so it carries e2e contract tests of its own and
must leave all 76 existing tests green.

One production defect carries over from Phase 6 and is fixed here: an unrecognised
`DEVMON_SELF_CONTAINER_ID` is silently discarded with no log line
(`client-independent-e2e-report.md`, Finding 1). Security posture is sound — self-exclusion
still arms via detection — but the operator gets no signal that their explicit
configuration was thrown away.

## User Story

As **an operator about to point a Docker control agent at the public internet**,
I want **the port to survive automated scanning, an installer that cannot leave the state
mount wrong, and a written statement of what this thing can and cannot protect me from**,
so that **I can install it in one command and know exactly what I have accepted.**

## Problem → Solution

**Current state.** The listener has timeouts, body caps, and a concurrent-stream ceiling,
but nothing bounds *request rate*. `GET /v1/status` and `POST /v1/pair` are reachable
without a client certificate: an attacker can poll status forever for free, or grind
pairing codes at line rate. `internal/httpapi/pair.go:20-23` says so in a comment —
"there is no rate limiting until Phase 6" — and Phase 6 turned out to be the e2e suite,
so the debt is due here. Installation is a hand-assembled `docker run` with two host
prerequisites the README warns will "look like agent bugs". There is no `LICENSE` file
despite `CLAUDE.md`, `go.mod`, and the README all declaring AGPL-3.0-only. There is no
threat model outside prose scattered through the README. `ghcr.io/scnplt/devmon-agent:0.1.0`
is referenced by the README and `compose.example.yaml` and does not exist.

**Desired state.** Two rate-limiting tiers — per-IP on the pre-authentication surface with
a global backstop, per-device on the authenticated surface — configured by three
`DEVMON_RATE_*` variables read once at startup like every other knob. `install.sh` resolves
the socket GID, creates and chowns the state directory, writes a compose file, starts the
agent, and prints the CA fingerprint and the first pairing code. `LICENSE`, `SECURITY.md`,
`CONTRIBUTING.md`, and `docs/THREAT-MODEL.md` exist and are accurate. A tag-driven release
workflow publishes the image the installer pulls. A security review document walks the
PRD's fifteen-row risk table and states, per row, what mitigates it and what remains
accepted.

## Metadata

- **Complexity**: Large
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 7 — Hardening & OSS release
- **Depends on**: Phase 6 (complete, suite green against Docker 29.6.1)
- **Estimated files**: 14 created, 12 updated
- **Tasks**: 12
- **New module dependency**: `golang.org/x/time v0.15.0` — the first added since Phase 1

---

## Decisions Settled Before Planning

These were put to the user and answered. They are not open; an implementer must not
relitigate them.

| # | Decision | Choice | Rationale |
|---|---|---|---|
| **D1** | Rate-limiter primitive | `golang.org/x/time/rate` plus a registry written here | The token bucket itself is the part that is subtly wrong when hand-rolled, and `x/time/rate` is the primitive Docker and Kubernetes already use. Critically, `AllowN(t, n)` takes the time as an argument, so every test is deterministic with no sleeps. The registry — keying, eviction, capacity — is the part that is specific to this agent and is written here. |
| **D2** | Rate limits configurable | Three `DEVMON_RATE_*` env vars | The operator on a busy host and the operator behind a VPN want different ceilings, and a limit that cannot be raised becomes a limit that gets removed. Still read once at startup and immutable, so the security boundary in `internal/config`'s package comment is unchanged. |
| **D3** | Installer form | POSIX shell script, `install.sh` at the repo root | The operator has no binary and no image before installing, which rules out a Go subcommand without a second release artifact. `sh` and `docker` are the only prerequisites, and both are already required to run the agent at all. |
| **D4** | Phase 6 Finding 1 | Fix it — `Warn` naming the discarded override | The Phase 6 report recommends exactly this for Phase 7. The change is confined to `confirmSelf` and costs nothing; silently discarding an operator's explicit configuration is the kind of thing that burns an afternoon. |
| **D5** | Pre-auth key | Client IP from `r.RemoteAddr`, never `X-Forwarded-For` | The documented deployment is direct inbound (PRD "Connection model" decision) with no reverse proxy. Honouring a client-supplied forwarding header would let any caller mint a fresh limiter key per request and bypass the limiter completely — it would be worse than having none, because it would look like protection. |
| **D6** | Authenticated key | Device ID, not IP | A phone roams across mobile IPs mid-incident. Keying the authenticated tier by IP would punish exactly the network handover the product's headline scenario depends on, and a device ID is the stronger identifier anyway: it is proven by a client certificate rather than asserted by a packet header. |
| **D7** | Throttled calls are not audited | The device limiter sits *before* `withAudit` | An audited 429 lets an authenticated device inflate the audit table past `DEVMON_AUDIT_MAX_ROWS` and push real history out — the limiter would become the mechanism for destroying the record it exists beside. Throttling is logged at `Warn` to the operational log instead, which is size-bounded and disposable. |
| **D8** | Global pre-auth backstop is a constant, not a knob | `unauthGlobalPerSec` / `unauthGlobalBurst` in code | Per-IP alone does not stop a distributed scan. Deriving the global ceiling from the per-IP variables would mean an operator who raises one silently removes the other, and a fourth env var is install surface the PRD explicitly asks not to add for values nobody tunes. |
| **D9** | Registry overflow falls back to the global bucket | Not fail-open, not lockout | Refusing every new key when the table is full lets an attacker with N addresses lock the operator out. Ignoring the limiter when full is a trivial bypass. Falling back to the global bucket is strictly tighter than no limiter and still admits a legitimate operator whenever the global bucket has tokens. |
| **D10** | Security review is a document, not just a pass | `.claude/PRPs/reviews/phase-7-security-review.md` | The PRD's success signal is "security review passes with no unmitigated high-severity finding". That is only checkable against a written, per-row verdict. Findings it raises are fixed inside this phase or explicitly accepted in writing. |
| **D11** | The e2e suite is extended, not replaced | New `contract_ratelimit_test.go` in the existing host-binary group | Rate limiting is the only behaviour change in this phase that a client can observe. The Phase 6 rules still hold: `//go:build e2e`, assertions against the wire, no production code shared with the suite. |
| **D12** | Version `0.1.0` is what ships | Tag `v0.1.0`, image `ghcr.io/scnplt/devmon-agent:0.1.0` | The README and `compose.example.yaml` already name that tag. Publishing anything else means editing both and the installer, for no gain. |

---

## Mandatory Reading

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `internal/httpapi/middleware.go` | 1-151 | The middleware pattern every new wrapper must mirror: method on `*Server`, closure over `next`, terse rejection body, real reason to the log. `statusRecorder.Unwrap` explains why wrappers must not break `http.ResponseController`. |
| P0 | `internal/httpapi/server.go` | 19-144 | Where routes are registered and in what order they are wrapped. The `read`/`logs`/`mutate` helpers are where the new limiter is threaded in. `maxConcurrentStreams` (lines 37-44) is the precedent for a bound expressed as a constant with its reasoning inline. |
| P0 | `internal/config/config.go` | 1-160, 291-320 | The whole configuration contract: env key constants, defaults, `loader` accumulating every problem instead of failing on the first, `boundedInt`. Three new variables land here and nowhere else. |
| P0 | `internal/httpapi/policygate.go` | 1-39 | The closest existing analogue to the new middleware — a guard that rejects with a specific message, logs with device attribution, and is composed in a documented order. |
| P1 | `internal/httpapi/pair.go` | 16-43, 61-85 | The route the pre-auth limiter most exists for, and the comment at lines 20-23 that this phase discharges. |
| P1 | `internal/dockerx/self.go` | 40-87 | `confirmSelf`, the exact function Finding 1 lives in. |
| P1 | `internal/selfid/selfid.go` | 40-64 | Why the override is only the first candidate, which is what makes the silent discard possible. |
| P1 | `internal/e2e/harness/agent.go` | 106-135, 208-238, 328-353 | `AgentOptions.Env` is how a contract test gives one agent tiny limits. **`waitReady` polls `GET /v1/status`** — it spends tokens from the status bucket before any test runs. |
| P1 | `internal/e2e/api/contract_status_test.go` | all | The format contract for the new rate-limit contract tests. |
| P2 | `.github/workflows/ci.yml` | all | The two-bar CI shape. The release workflow is a sibling, not an edit to this file. |
| P2 | `README.md` | 1-120, 591-700 | The status paragraph and configuration table this phase updates, and the development section the installer joins. |
| P2 | `.claude/PRPs/reports/client-independent-e2e-report.md` | 155-190 | Finding 1 in full, including the test that was renamed to assert the current behaviour and must be revisited when it changes. |

## External Documentation

| Topic | Source | Pinned version | Key takeaway |
|---|---|---|---|
| Token bucket | `golang.org/x/time/rate` | **v0.15.0** | `NewLimiter(Limit, burst)`; `AllowN(t, n)` takes the time explicitly. |
| AGPL-3.0-only text | `https://www.gnu.org/licenses/agpl-3.0.txt` | GNU AGPL v3, 19 November 2007 | Verbatim text goes in `LICENSE`; the per-file header is the SPDX identifier only. |
| SPDX identifier | `https://spdx.org/licenses/AGPL-3.0-only.html` | — | `AGPL-3.0-only`, matching what `CLAUDE.md` already declares. |
| `Retry-After` | RFC 9110 §10.2.3 | — | On a 429 it is either an HTTP-date or **delay-seconds as a non-negative integer**. No fractional seconds. |
| 429 semantics | RFC 6585 §4 | — | 429 is the correct status; the response "MAY include a Retry-After header". |

### Verified SDK signatures — transcribe exactly

Verified in this session with `go doc` against `golang.org/x/time v0.15.0` downloaded into
a throwaway module (the planner never mutates this repo's `go.mod`). Do not write these
from memory.

```go
// golang.org/x/time/rate — verified v0.15.0

func NewLimiter(r Limit, b int) *Limiter
func Every(interval time.Duration) Limit          // interval -> Limit

func (lim *Limiter) AllowN(t time.Time, n int) bool  // the clock is an ARGUMENT
func (lim *Limiter) Allow() bool                     // == AllowN(time.Now(), 1)
func (lim *Limiter) Burst() int
func (lim *Limiter) Limit() Limit
func (lim *Limiter) TokensAt(t time.Time) float64    // tokens available at t
```

Two properties this plan depends on, both from the package documentation:

- *"Limiter is safe for simultaneous use by multiple goroutines."* The registry therefore
  needs a mutex only around its **map**, never around a `*Limiter` it hands out.
- *"It implements a token bucket of size b, initially full."* A fresh limiter starts with a
  full bucket, so a first-time caller is never throttled — which is what makes
  `TokensAt(now) >= float64(Burst())` a sound "this entry carries no state" eviction test.

Exact `go.mod` / `go.sum` lines, captured from the resolution above:

```
# go.mod — require block (direct)
golang.org/x/time v0.15.0

# go.sum
golang.org/x/time v0.15.0 h1:bbrp8t3bGUeFOx08pvsMYRTCVSMk89u4tKbNOZbp88U=
golang.org/x/time v0.15.0/go.mod h1:Y4YMaQmXwGQZoFaVFk4YpCt4FLQMYKZe9oeV/f4MSno=
```

`golang.org/x/time` is **not** currently in `go.sum` — the module's only `golang.org/x`
entries are `mod`, `sync`, `sys`, and `tools`, all indirect. This is a genuinely new
dependency edge, not a promotion.

---

## Patterns to Mirror

### MIDDLEWARE_SHAPE
```go
// SOURCE: internal/httpapi/policygate.go:25-39
func (s *Server) requireOp(op policy.Operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.PolicyMode.Allows(op) {
			attrs := []any{slog.String("operation", string(op))}
			if device, ok := DeviceFrom(r.Context()); ok {
				attrs = append(attrs, slog.String("device_id", device.ID))
			}
			s.log.Warn("rejected request forbidden by host policy", attrs...)
			setAuditOutcome(r.Context(), state.OutcomeDeniedPolicy, "")
			s.writeError(w, http.StatusForbidden, msgPolicyForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### NAMED_BOUND_WITH_REASONING
```go
// SOURCE: internal/httpapi/server.go:37-44
	// maxConcurrentStreams bounds simultaneous live log streams. Each holds a
	// goroutine, an Engine connection, and a socket for its entire life, so an
	// unbounded count is a file-descriptor exhaustion the agent inflicts on the
	// host it exists to protect. A constant rather than an env var: ...
	maxConcurrentStreams = 8
```

### CONFIG_KEY_AND_BOUND
```go
// SOURCE: internal/config/config.go:25-37, 291-306
const (
	envStateDir      = "DEVMON_STATE_DIR"
	envLogMaxTotalMB = "DEVMON_LOG_MAX_TOTAL_MB"
)

// boundedInt parses key as an integer no smaller than min. On any fault it
// records the problem and returns def, so parsing continues and every remaining
// variable is still checked.
func (l *loader) boundedInt(key string, def, min int) int {
	raw := l.raw(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(key, "%q is not an integer", raw)
		return def
	}
	if n < min {
		l.fail(key, "%d is below the minimum of %d", n, min)
		return def
	}
	return n
}
```

### TERSE_REJECTION
```go
// SOURCE: internal/httpapi/respond.go:38-47, middleware.go:13-15
// writeError returns a deliberately terse message.
// The caller may be unauthenticated and on the open internet, so error text must
// never carry the state path, the Docker host, a certificate subject, or the
// reason a credential was rejected.
const msgClientCertRequired = "client certificate required"
```

### TABLE_TEST
```go
// SOURCE: internal/policy/mode_test.go — the house table shape
func TestModeAllows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode policy.Mode
		op   policy.Operation
		want bool
	}{ /* ... */ }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.mode.Allows(tt.op); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.op, got, tt.want)
			}
		})
	}
}
```

### CONTRACT_ASSERTION
```go
// SOURCE: internal/e2e/api/contract_status_test.go — assert the wire, never the struct
status, hdr, raw := device.Do(t, http.MethodGet, "/v1/status", nil)
if status != http.StatusOK {
	t.Fatalf("GET /v1/status = %d, want 200\nbody: %s", status, redact(raw))
}
harness.AssertExactKeys(t, obj, []string{
	"api_version", "agent_version", "policy_mode", "server_time", "ca_fingerprint",
})
```

---

## Rate-Limiting Contract

This is the part of the phase a client can observe, so it is specified before the tasks.

### Tiers

| Tier | Applies to | Keyed by | Rate | Burst |
|---|---|---|---|---|
| **Status** | `GET /v1/status` | client IP | `DEVMON_RATE_STATUS_PER_MIN` (default 30) | = the per-minute count |
| **Pair** | `POST /v1/pair` | client IP | `DEVMON_RATE_PAIR_PER_MIN` (default 5) | = the per-minute count |
| **Global pre-auth** | both of the above, checked *first* | nothing — one bucket | `unauthGlobalPerSec` = 50 | `unauthGlobalBurst` = 100 |
| **Guarded** | every route behind `requireDevice` | device ID | `DEVMON_RATE_GUARDED_PER_SEC` (default 20) | `2 ×` the per-second rate |

Burst equals the whole window on the pre-auth tiers so a client that legitimately checks
status a few times in a row is not throttled for behaving normally. Guarded burst is `2 ×`
the rate because opening the container list fires one list call plus one inspect per
visible container, and a phone that shows twelve containers must not trip a limiter on its
first screen.

### Order of checks

```
GET /v1/status   ->  withGlobalUnauthLimit -> withIPLimit(status) -> handleStatus
POST /v1/pair    ->  withGlobalUnauthLimit -> withIPLimit(pair)   -> handlePair

guarded routes   ->  requireDevice -> withDeviceLimit -> [withAudit ->] requireOp -> handler
```

`withDeviceLimit` sits **after** `requireDevice` (it needs a device ID) and **before**
`withAudit` (D7). On mutating routes the current chain is
`requireDevice(withAudit(op, requireOp(op, h)))`; it becomes
`requireDevice(withDeviceLimit(withAudit(op, requireOp(op, h))))`.

### Rejection

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Cache-Control: no-store
X-Content-Type-Options: nosniff
Retry-After: 2

{"error":"rate limit exceeded"}
```

`Retry-After` is `ceil(1 / ratePerSecond)` seconds with a floor of 1 — an integer, per
RFC 9110 §10.2.3. It is derived from the configured rate rather than from a
`Reservation`, because `Reserve` consumes a token and this request has already been
refused. One message, `msgRateLimited = "rate limit exceeded"`, for every tier: which
bucket a caller hit is operator information and goes to the log.

### Logging

One `Warn` per rejection. Pre-auth tiers log the **tier name and nothing else** — not the
IP, which on this port is attacker-controlled input that would let a scanner write
arbitrary volume into a size-bounded log. The guarded tier logs `device_id`, which is the
agent's own identifier and is already logged by `requireOp`.

### What is deliberately not rate-limited

- **Connection establishment.** TLS handshakes are bounded by the OS accept queue and the
  existing `ReadHeaderTimeout`. A connection limiter belongs in front of the process
  (`iptables`, a VPN) and is named as such in the threat model.
- **Bytes.** Body size is already capped per route (`maxPairBodyBytes`,
  `maxRenewBodyBytes`, `maxHeaderBytes`).
- **Open streams.** `maxConcurrentStreams = 8` already bounds them, and an SSE stream
  spends exactly one guarded token at request time.

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `go.mod`, `go.sum` | UPDATE | `golang.org/x/time v0.15.0` (D1) |
| `internal/ratelimit/registry.go` | CREATE | Keyed limiter registry with capacity and eviction |
| `internal/ratelimit/registry_test.go` | CREATE | Table tests over a synthetic clock |
| `internal/config/config.go` | UPDATE | Three `DEVMON_RATE_*` variables |
| `internal/config/config_test.go` | UPDATE | Defaults, bounds, and aggregation for the new keys |
| `internal/httpapi/ratelimit.go` | CREATE | `withGlobalUnauthLimit`, `withIPLimit`, `withDeviceLimit`, `clientIP` |
| `internal/httpapi/ratelimit_test.go` | CREATE | Per-tier behaviour, 429 shape, key isolation |
| `internal/httpapi/server.go` | UPDATE | Registries on `Server`; limiters threaded into `routes()` |
| `internal/httpapi/server_test.go` | UPDATE | Existing route tests must not trip the new limiter |
| `internal/httpapi/pair.go` | UPDATE | Delete the stale "no rate limiting until Phase 6" comment |
| `internal/dockerx/self.go` | UPDATE | Finding 1 — `Warn` on a discarded override |
| `internal/dockerx/self_test.go` | UPDATE | Assert the warning fires and detection still wins |
| `internal/e2e/api/contract_ratelimit_test.go` | CREATE | The observable contract |
| `internal/e2e/api/contract_startup_test.go` | UPDATE | Invalid `DEVMON_RATE_*` is an exit-2 configuration fault |
| `install.sh` | CREATE | D3 |
| `docs/THREAT-MODEL.md` | CREATE | PRD scope item |
| `docs/BACKUP.md` | CREATE | PRD scope item; lifts and expands the README section |
| `LICENSE` | CREATE | AGPL-3.0 verbatim |
| `SECURITY.md` | CREATE | Vulnerability reporting for a public repo |
| `CONTRIBUTING.md` | CREATE | Branching, gates, model routing |
| `.github/workflows/release.yml` | CREATE | Tag-driven image publish to GHCR |
| `.github/workflows/ci.yml` | UPDATE | `shellcheck` job for `install.sh` |
| `.github/ISSUE_TEMPLATE/bug_report.yml` | CREATE | Must tell reporters not to paste state or PEM |
| `.github/ISSUE_TEMPLATE/config.yml` | CREATE | Routes security reports to `SECURITY.md`, not to a public issue |
| `README.md` | UPDATE | Status, rate-limit config, installer, license, doc links |
| `compose.example.yaml` | UPDATE | Rate variables; the Phase 6/7 installer note is now stale |
| `Makefile` | UPDATE | `make shellcheck` |
| `.claude/PRPs/reviews/phase-7-security-review.md` | CREATE | D10 |
| `.claude/PRPs/prds/devmon-agent.prd.md` | UPDATE | Phase 7 row → complete, plan and report links |
| `.claude/PRPs/reports/hardening-and-oss-release-report.md` | CREATE | Phase report |

## NOT Building

- **A reverse proxy, WAF, or `X-Forwarded-For` trust chain.** D5. Direct inbound is the
  documented deployment; a forwarding header would be a bypass wearing the costume of a
  feature.
- **IP allowlists or denylists, fail2ban integration, or ban lists.** Rate limiting is
  memoryless by design. Persisting a ban across restarts means new durable state, a new
  retention budget, and a new way for the operator to lock themselves out.
- **Any client-facing way to read or change the limits.** They are advertised nowhere,
  including `/v1/status`. Publishing the exact ceiling to an unauthenticated caller tells
  a scanner precisely how fast it may go without being noticed.
- **Encrypting `ca.key` at rest.** The PRD settles this: any unlocking secret would live on
  the same host. This phase *documents* it in the threat model; it does not revisit it.
- **A protected-container list.** PRD "Won't". The agent's own self-exclusion is unchanged.
- **Outbound notifications for security events.** PRD "Won't", post-MVP.
- **Signed images, SBOM, provenance attestation, or a Homebrew/apt package.** The release
  workflow publishes a tagged multi-arch image and nothing more. Supply-chain attestation
  is worth doing and is worth its own phase.
- **An uninstaller, or an installer that upgrades an existing deployment.** `install.sh`
  refuses to touch a state directory that already exists and says why. Upgrades stay
  `docker compose pull && docker compose up -d`, which the README documents.
- **Changing what `/v1/status` returns.** Its field allowlist is a PRD-level security
  boundary and this phase adds nothing to it.
- **Raising the coverage floor or folding e2e into the coverage number.** Both settled in
  Phase 6 and unchanged.

---

## Step-by-Step Tasks

Each task is one commit (`CLAUDE.md`, commit cadence). Tasks 1-5 are sequential; 6-11 are
mutually independent once 1-4 land and may be dispatched in parallel.

### Task 1: `internal/ratelimit` — keyed limiter registry

- **ACTION**: Add the `golang.org/x/time` dependency and create `internal/ratelimit`.
- **IMPLEMENT**:
  ```go
  // Package ratelimit bounds how often a caller may be served. ...
  package ratelimit

  // Registry hands out one token bucket per key, with a hard cap on how many
  // keys it will hold.
  type Registry struct {
      mu      sync.Mutex
      buckets map[string]*rate.Limiter
      limit   rate.Limit
      burst   int
      maxKeys int
  }

  func NewRegistry(limit rate.Limit, burst, maxKeys int) *Registry

  // Allow reports whether key may be served at now. The second return is false
  // when the registry is at capacity and could not admit key — the caller must
  // then fall back to its global bucket (see the package comment).
  func (r *Registry) Allow(key string, now time.Time) (allowed, keyed bool)

  // Len reports how many keys are currently held. Test and diagnostic use only.
  func (r *Registry) Len() int
  ```
  `Allow` behaviour, in order: look the key up and use it if present; otherwise, if
  `len(buckets) >= maxKeys`, sweep once for entries with `TokensAt(now) >= float64(burst)`
  and delete them; if room was made, insert a fresh limiter; if not, return
  `(false, false)` so the caller falls back to the global bucket (D9). Never start a
  goroutine and never call `time.Now()` — the clock is always an argument.
- **MIRROR**: `NAMED_BOUND_WITH_REASONING` for `maxKeys`; `TABLE_TEST` for the tests.
- **IMPORTS**: `sync`, `time`, `golang.org/x/time/rate`.
- **GOTCHA**: A full bucket means an idle key, because a limiter starts full and refills to
  its burst — that is what makes the sweep safe. Sweeping the *oldest* key instead would
  evict the attacker's throttled bucket and hand them a free reset, which is the exact
  inversion of what a limiter is for.
- **GOTCHA**: `go get golang.org/x/time@v0.15.0` then `go mod tidy`. Confirm the `require`
  block lists it as **direct** (no `// indirect` comment) and that `go.sum` gained exactly
  the two lines quoted above — no more.
- **TESTS FIRST**: single key allows up to burst then refuses; refusal clears after the
  refill interval on a synthetic clock; two keys are independent; at `maxKeys` a full
  bucket is evicted and the new key admitted; at `maxKeys` with every bucket drained,
  `Allow` returns `(false, false)`; concurrent `Allow` over 100 goroutines is clean
  under `-race`.
- **VALIDATE**:
  ```bash
  go test ./internal/ratelimit/... -race -count=1
  go vet ./internal/ratelimit/...
  go list -m golang.org/x/time
  ```
- **ACCEPTANCE**: The package passes with no sleep anywhere in its tests, and `go.mod`
  gained exactly one direct requirement.

### Task 2: Rate-limit configuration

- **ACTION**: Add three variables to `internal/config`.
- **IMPLEMENT**: New key constants `envRateStatusPerMin = "DEVMON_RATE_STATUS_PER_MIN"`,
  `envRatePairPerMin = "DEVMON_RATE_PAIR_PER_MIN"`, `envRateGuardedPerSec =
  "DEVMON_RATE_GUARDED_PER_SEC"`; defaults `30`, `5`, `20`; a shared minimum of `1`; three
  `int` fields on `Config` — `RateStatusPerMin`, `RatePairPerMin`, `RateGuardedPerSec` —
  each parsed with the existing `l.boundedInt`.
- **MIRROR**: `CONFIG_KEY_AND_BOUND` exactly. No new parsing helper.
- **GOTCHA**: A minimum of `1`, not `0`. Zero would read as "no requests permitted", which
  bricks the agent in a way that looks like a network fault, and there must be no value
  that disables the limiter — an operator who wants no ceiling raises the number.
- **GOTCHA**: These go through `boundedInt`, so they aggregate into the same
  `ValidationError` as every other fault. A test must prove three bad rate variables
  produce three problems in one error, not one.
- **TESTS FIRST**: extend the existing `config_test.go` table — defaults when unset; each
  key parsed; `0` and `-1` rejected naming the key and the minimum; a non-integer rejected;
  aggregation across all three.
- **VALIDATE**: `go test ./internal/config/... -race -count=1`
- **ACCEPTANCE**: `Load` returns the three values, and every fault names its own variable.

### Task 3: The limiter middleware and route wiring

- **ACTION**: Create `internal/httpapi/ratelimit.go` and thread it through `routes()`.
- **IMPLEMENT**:
  - Constants: `msgRateLimited = "rate limit exceeded"`, `headerRetryAfter =
    "Retry-After"`, `unauthGlobalPerSec = 50`, `unauthGlobalBurst = 100`,
    `guardedBurstMultiplier = 2`, `rateLimitMaxKeys = 4096`, `secondsPerMinute = 60`.
    Each carries its reasoning inline (D8, and the burst rationale from the contract
    section above).
  - `clientIP(r *http.Request) string` — `net.SplitHostPort(r.RemoteAddr)`, returning the
    raw `RemoteAddr` when it does not split. A comment stating that `X-Forwarded-For` is
    deliberately not consulted (D5).
  - `func (s *Server) tooManyRequests(w http.ResponseWriter, limit rate.Limit)` — sets
    `Retry-After` then calls `s.writeError(w, http.StatusTooManyRequests, msgRateLimited)`.
    The header must be set **before** `writeError`, which commits the status line.
  - `withGlobalUnauthLimit`, `withIPLimit(reg *ratelimit.Registry, next http.Handler)`, and
    `withDeviceLimit` — all methods on `*Server`, all mirroring `MIDDLEWARE_SHAPE`.
    `withIPLimit` falls back to the global bucket when `Allow` returns `keyed == false`
    (D9).
  - On `Server`: `unauthGlobal *rate.Limiter`, `statusLimits`, `pairLimits`,
    `deviceLimits *ratelimit.Registry`, all built in `NewServer` from `cfg`.
  - In `routes()`: wrap `GET /v1/status` and `POST /v1/pair` per the contract section;
    change the `read`, `logs`, and `mutate` helpers plus the two `/v1/device/*` routes to
    insert `withDeviceLimit` immediately inside `requireDevice`.
- **MIRROR**: `MIDDLEWARE_SHAPE`, `TERSE_REJECTION`, `NAMED_BOUND_WITH_REASONING`.
- **IMPORTS**: `net`, `math`, `time`, `golang.org/x/time/rate`,
  `github.com/scnplt/devmon-agent/internal/ratelimit`.
- **GOTCHA**: `withDeviceLimit` reads the device via `DeviceFrom(r.Context())`. If that
  returns `!ok` it must **reject with 500**, never pass through — a device limiter that
  silently degrades to no limiter the moment it is mis-composed is worse than absent.
- **GOTCHA**: Do not wrap the SSE stream route in anything that buffers. The limiter
  returns before calling `next` or not at all, and never wraps the `ResponseWriter`, so
  `statusRecorder.Unwrap` and `http.ResponseController` are unaffected. Do not introduce a
  second `ResponseWriter` wrapper here.
- **GOTCHA**: `NewServer` is called from `cmd/devmon-agent/main.go` and from several unit
  tests with a zero-ish `config.Config`. A zero `RateStatusPerMin` would build a limiter
  that refuses everything and turn dozens of existing tests red. `NewServer` must floor
  each rate at its package default when the config value is `< 1`, with a comment saying
  the floor exists for zero-value `Config` literals in tests and that `config.Load` can
  never produce one.
- **TESTS FIRST**: burst then 429; the 429 body is exactly `{"error":"rate limit
  exceeded"}` and carries an integer `Retry-After` ≥ 1 plus `Cache-Control: no-store`;
  two IPs are independent on the status tier; two device IDs are independent on the
  guarded tier; the pair tier is separate from the status tier (exhausting one leaves the
  other serving); `withDeviceLimit` with no device in context answers 500.
- **VALIDATE**:
  ```bash
  go test ./internal/httpapi/... -race -count=1
  go test ./internal/... -race
  gofmt -l . && go vet ./...
  ```
- **ACCEPTANCE**: Every pre-existing `httpapi` test still passes untouched except where a
  test genuinely fires more than a burst, and any such change is a test-side loop bound,
  never a raised limit.

### Task 4: Finding 1 — warn on a discarded self-ID override

- **ACTION**: Make a rejected `DEVMON_SELF_CONTAINER_ID` visible in `confirmSelf`.
- **IMPLEMENT**: Thread the override into `confirmSelf` (either on `selfid.Result` as an
  `Override string` field set by `Detect`, or as a parameter — prefer the field, so
  `resolveSelf` keeps its two-line body). When a candidate that equals the override is
  skipped, log
  `Warn("discarding DEVMON_SELF_CONTAINER_ID: the Engine does not recognise it", slog.String("container_id", override))`.
  Emit it for the not-found case too — that is precisely the case that is silent today.
- **MIRROR**: the existing `Warn` at `internal/dockerx/self.go:63-66`.
- **GOTCHA**: The override is a container ID, not a secret. Logging it is correct and is
  the entire point; it is the only thing that makes the message actionable.
- **GOTCHA**: This must not change control flow. Detection still proceeds to the next
  candidate and self-exclusion still arms. The `Error` at lines 80-81 for a fully
  unresolvable containerized agent is unchanged.
- **TESTS FIRST**: in `self_test.go`, against the existing fake Engine — an override the
  Engine does not know, plus a mountinfo candidate it does: the resolved ID is the
  mountinfo one, and the log contains the warning and the discarded ID. An override the
  Engine *does* know produces no warning.
- **VALIDATE**: `go test ./internal/dockerx/... -race -count=1`
- **ACCEPTANCE**: Phase 6 Finding 1 is closed and the report's "Recommended for Phase 7"
  is discharged.
- **FOLLOW-UP, same commit**: `internal/e2e/api/` (or `incontainer/`) holds
  `TestUnresolvableSelfIDFallsBackToDetection`, whose doc comment notes that a future fix
  should make the override appear in the log. Tighten it to **assert** the warning now
  appears. Grep for the test by name; do not guess its file.

### Task 5: Rate-limiting contract tests

- **ACTION**: Create `internal/e2e/api/contract_ratelimit_test.go`.
- **IMPLEMENT**: `//go:build e2e`. Cases:
  1. **Status tier** — agent started with `Env: {"DEVMON_RATE_STATUS_PER_MIN": "5"}`.
     Poll `/v1/status` in a loop until the first 429 (bounded at, say, 20 iterations);
     assert a 429 arrived, that `Retry-After` parses as an integer ≥ 1, and that the body
     is exactly `{"error":"rate limit exceeded"}`.
  2. **Pair tier is separate** — with `DEVMON_RATE_PAIR_PER_MIN=1`, drive `/v1/pair` to a
     429 with a junk code, then assert `/v1/status` still answers 200 on the same
     connection. Buckets are per tier, not per port.
  3. **Guarded tier is per device (D6)** — pair two devices against an agent with
     `DEVMON_RATE_GUARDED_PER_SEC=1`; drive device A to a 429 on `GET /v1/containers`;
     assert device B is still served 200 immediately.
  4. **Recovery** — after a 429 on the guarded tier, wait `Retry-After` seconds and assert
     the next request succeeds. This is the only place a sleep is acceptable, and it must
     sleep the advertised duration rather than a guessed one.
  5. **A tripped limiter writes no audit row** (D7) — with `full` policy, drive `POST
     /v1/containers/{id}/restart` past the limit, then `harness.ListAudit` and assert the
     429s produced no rows beyond the ones that actually executed.
- **MIRROR**: `CONTRACT_ASSERTION`; the existing `contract_status_test.go` file shape.
- **GOTCHA**: **`harness.waitReady` polls `GET /v1/status` before any test body runs**
  (`internal/e2e/harness/agent.go:328-353`) and spends tokens from the `127.0.0.1` status
  bucket. Never assert an exact request count before the first 429 — loop until it appears
  and assert only that it appears within the bound. Never set a limit of `1` on the status
  tier, or readiness itself may fail and the agent never starts.
- **GOTCHA**: Every test in this file needs its **own** agent, because the limits are
  startup configuration. Do not share a package-level agent.
- **GOTCHA**: `Device.Do` reuses one HTTP client and connection. The limiter keys on IP,
  not connection, so reuse is irrelevant to the result — but do not "fix" a failing test
  by opening new connections.
- **VALIDATE**:
  ```bash
  make e2e            # from WSL2 on Windows; the whole suite must stay green
  make e2e-lint
  ```
- **ACCEPTANCE**: The new cases pass and all 76 pre-existing e2e tests still pass. Also
  extend `contract_startup_test.go` with `DEVMON_RATE_GUARDED_PER_SEC=0` → exit code 2 and
  a stderr line naming the variable.

### Task 6: `install.sh`

- **ACTION**: Create the automated installer at the repo root.
- **IMPLEMENT**: POSIX `sh` (`#!/bin/sh`, `set -eu` — **not** `set -o pipefail`, which is
  not POSIX). In order:
  1. Preflight: `docker` on `PATH`; the daemon answers `docker info`; `docker compose`
     available (fall back to `docker-compose`, else fail with the install URL).
  2. Refuse to proceed if `$STATE_DIR` already exists and is non-empty — print that this
     looks like an existing installation and that upgrading is
     `docker compose pull && docker compose up -d`. Never overwrite state (see NOT Building).
  3. Prompt, each with the default in brackets and each overridable by a flag *and* by an
     environment variable for non-interactive use: public address (**required**, no
     default, validated as a DNS name or IP with no port or scheme), policy mode
     (`read-only|default|full`, default `default`), listen port (default `8443`), state
     directory (default `/var/lib/devmon`), operational log budget (days and MB), audit
     budget (days and rows).
  4. Resolve the socket GID with `stat -c '%g' /var/run/docker.sock`, falling back to
     `stat -f '%g'` for BSD `stat`. Fail loudly if neither works rather than guessing 999.
  5. `mkdir -p`, `chown 65532:65532`, `chmod 700` on the state directory — `sudo` only when
     not already root, and print each privileged command before running it.
  6. Write `compose.yaml` into a chosen install directory (default the current directory),
     refusing to clobber an existing one without `--force`.
  7. `docker compose up -d`, poll `https://<addr>:<port>/v1/status` with `curl -sk` until it
     answers or a timeout expires.
  8. Print the CA fingerprint from the status payload, tell the operator to record it off
     the host, then mint and print the first pairing code via
     `docker compose exec -T devmon-agent /usr/local/bin/devmon-agent device pair-code --name <name>`.
  9. `--help` documenting every flag and its environment-variable equivalent.
- **GOTCHA**: The image is `distroless/static:nonroot` — there is no shell inside. Every
  in-container command must be the absolute binary path, exactly as `README.md` and
  `cmd/devmon-agent/cli.go:1-4` describe. `docker compose exec sh -c ...` will not work.
- **GOTCHA**: The pairing code must reach the terminal and nothing else. Never write it to
  a file, never `tee` it into a log, never echo it inside a command that the script also
  logs. Same rule as `runDevicePairCode` (`cli.go:184-186`).
- **GOTCHA**: `DEVMON_PUBLIC_ADDR` is comma-separated and each entry is rejected if it
  contains `:` or `/` (`internal/config/config.go:333-341`). The prompt must reject
  `https://host` and `host:8443` *at prompt time*, with the reason, rather than letting the
  agent exit 2 twenty seconds later.
- **GOTCHA**: Do not `curl | sh` a fingerprint check into the script. The whole point of
  the fingerprint is that the operator records it from the host they trust.
- **TESTS**: Shell, so no Go test. `shellcheck -s sh install.sh` must be clean, plus a
  manual run recorded in the report (a fresh VPS or a throwaway VM). A `--dry-run` flag
  that prints the compose file and every command without executing anything makes the
  manual check cheap and is worth including.
- **VALIDATE**:
  ```bash
  shellcheck -s sh install.sh
  sh -n install.sh                       # parse check on any POSIX sh
  ./install.sh --dry-run --public-addr 127.0.0.1
  ```
- **ACCEPTANCE**: `shellcheck` clean, and a real run on a clean host takes an operator from
  nothing to a printed pairing code without hand-writing a `docker run` line — the PRD's
  Phase 7 success signal.

### Task 7: Security review against the PRD risk table

- **ACTION**: Write `.claude/PRPs/reviews/phase-7-security-review.md`; fix what it finds.
- **IMPLEMENT**: One section per row of the PRD's Technical Risks table (fifteen rows,
  `devmon-agent.prd.md:176-192`). Each states: the risk, the mitigation **as implemented**
  with a `file:line` citation, the residual risk, and a verdict of `mitigated`,
  `accepted`, or `open`. Then a checklist pass over `.claude/rules/ecc/common/security.md`
  and the OWASP-style items in `code-review.md`. Then:
  - `gosec ./...` clean, with every `#nosec` in the tree listed and each justification
    re-checked against what the code does today.
  - `golangci-lint run ./...` clean.
  - `govulncheck ./...` — new to this phase and worth running once here even if it does not
    join CI; record the version and the result.
  - A grep sweep proving no log line, error string, or response field can carry key
    material: `grep -rn "PEM\|\.key\|pairing_code\|PairingCode" --include=*.go internal/ cmd/`
    reviewed line by line.
  - Dispatch **`ecc:security-reviewer`** and **`ecc:go-reviewer`** over the phase diff, per
    `CLAUDE.md`'s review routing, and fold their findings in.
  - An explicit verdict on the new attack surface this phase adds: the limiter's own memory
    (`rateLimitMaxKeys × sizeof(rate.Limiter)`, computed, not estimated), and whether a
    429 leaks anything a 200 does not.
- **GOTCHA**: The PRD's success signal is "no unmitigated **high-severity** finding". A
  finding may be *accepted* — the unencrypted CA key is the standing example — but
  acceptance must be written down with its reasoning, not left implicit.
- **GOTCHA**: Any CRITICAL or HIGH finding is fixed inside this phase in its own commit
  before the phase closes. It does not become a Phase 8.
- **VALIDATE**:
  ```bash
  gosec ./...
  golangci-lint run ./...
  go run golang.org/x/vuln/cmd/govulncheck@latest ./...
  go test ./internal/... -race && go test ./cmd/... -race
  ```
- **ACCEPTANCE**: Every risk row carries a verdict with a citation; no open high-severity
  finding remains.

### Task 8: Threat model and backup documentation

- **ACTION**: Create `docs/THREAT-MODEL.md` and `docs/BACKUP.md`.
- **IMPLEMENT**: `THREAT-MODEL.md` covers — assets (CA key, device registry, audit log,
  the Docker socket itself); trust boundaries (internet → port; port → mTLS; API → Engine;
  host shell → state directory); the adversaries actually considered (internet scanner,
  thief with an unlocked paired phone, someone who reads a VPS snapshot, a malicious
  container on the same host); what is explicitly **not** defended (host root, a
  compromised Engine, an operator who exposes the port with no VPN and a weak network,
  side channels, denial of service beyond the rate limiter); and the standing accepted
  risks — unencrypted `ca.key`, no RBAC, root-equivalent blast radius if the agent is
  compromised. Every claim links to the code that makes it true.
  `BACKUP.md` promotes and expands the README's backup section: what to back up, why the
  backup is itself a credential, stop-then-copy for a consistent WAL, restore with
  ownership and modes, and what happens if `certs/` is lost (every device re-pairs, the
  fingerprint changes, and the agent says so loudly).
- **GOTCHA**: The threat model must not contradict the README or the PRD. Where the README
  already says it well (the `ca.key` note at `README.md:191-244` is the model), lift the
  wording rather than paraphrasing it into a second, slightly different claim.
- **VALIDATE**: Every `file:line` citation resolves; every internal link resolves.
- **ACCEPTANCE**: A reader can decide whether to run this on their host without reading the
  source.

### Task 9: AGPL-3.0 licensing and OSS repository furniture

- **ACTION**: Create `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, and the issue templates;
  add SPDX headers.
- **IMPLEMENT**:
  - `LICENSE` — the verbatim GNU AGPL v3 text, unmodified, with the copyright line filled
    in below the terms as the license's own instructions direct.
  - SPDX header as the **first line** of every `.go` file: `// SPDX-License-Identifier:
    AGPL-3.0-only`, then a blank line, then the existing package comment. Mechanical across
    all `.go` files including `_test.go` and the e2e files.
  - `SECURITY.md` — supported versions, private reporting via GitHub Security Advisories
    (never a public issue), response expectations, and a plain statement that the agent
    holds root-equivalent access to its host so a report is treated accordingly.
  - `CONTRIBUTING.md` — the branching model from
    `.claude/rules/ecc/common/git-workflow.md`, the gate commands from `CLAUDE.md`, the
    English-only rule, and the two Docker-SDK/`CGO_ENABLED` gotchas that catch everyone.
  - `.github/ISSUE_TEMPLATE/bug_report.yml` and `config.yml` — the bug template must warn,
    in the template body, not to paste `devmon.db`, anything from `certs/`, or a pairing
    code; `config.yml` sets `blank_issues_enabled: false` and points security reports at
    `SECURITY.md`.
- **GOTCHA**: The SPDX header goes **above** the package doc comment and must be separated
  from it by a blank line. Directly abutting it makes the SPDX line part of the package
  documentation, and it will appear in `go doc` output for every package.
- **GOTCHA**: Do not let the header displace `//go:build e2e` lines. A build constraint must
  precede the package clause and be followed by a blank line; verify with
  `go vet -tags e2e ./...` afterwards, which is the check that catches a build tag rendered
  inert.
- **VALIDATE**:
  ```bash
  gofmt -l .
  go build ./... && go vet ./... && go vet -tags e2e ./...
  go test ./internal/... -race
  grep -rLn "SPDX-License-Identifier" --include=*.go .   # must list nothing
  ```
- **ACCEPTANCE**: Every `.go` file carries the identifier, the build tags still bite, and
  `LICENSE` matches the upstream text byte for byte apart from the copyright line.

### Task 10: Release workflow and the shellcheck gate

- **ACTION**: Create `.github/workflows/release.yml`; add a `shellcheck` job to `ci.yml`
  and a `make shellcheck` target.
- **IMPLEMENT**:
  - `release.yml` triggers on `push: tags: ['v*']`. Permissions `contents: read`,
    `packages: write`, and nothing else. Steps: checkout; `docker/login-action` to
    `ghcr.io` with `GITHUB_TOKEN`; `docker/setup-buildx-action`;
    `docker/build-push-action` for `linux/amd64,linux/arm64`, passing `VERSION`, `COMMIT`,
    and `BUILD_TIME` build args exactly as the `image` CI job does; tags
    `ghcr.io/scnplt/devmon-agent:<version-without-v>` and `:latest`. Pin every action to a
    major version tag, matching the existing workflow's style.
  - `ci.yml` gains a `shellcheck` job gated on `github.base_ref == 'main'`, matching the
    other release-bar jobs, running `shellcheck -s sh install.sh`. Add the row to the
    README's CI table.
  - `Makefile` gains `shellcheck: shellcheck -s sh install.sh`, and `.PHONY` is updated.
- **GOTCHA**: `arm64` matters — a large share of self-hosted VPS and home-lab targets are
  ARM. The build is `CGO_ENABLED=0` pure Go onto `distroless/static`, so cross-compiling
  costs nothing but buildx setup. Verify the manifest afterwards with
  `docker buildx imagetools inspect`.
- **GOTCHA**: Do not fold release publishing into `ci.yml`. A workflow with
  `packages: write` triggered on every pull request is a credential exposed to every fork
  PR; keeping it tag-only is the isolation.
- **GOTCHA**: The first published tag must be `v0.1.0` (D12), because the README, the
  compose example, and the installer all name `0.1.0`.
- **VALIDATE**: `actionlint` if available; otherwise a YAML parse check and a careful read.
  Confirm the tag→image mapping strips the leading `v`.
- **ACCEPTANCE**: Pushing `v0.1.0` publishes a two-architecture image at the tag the
  installer pulls.

### Task 11: README, compose example, and PRD status

- **ACTION**: Bring every operator-facing document to Phase 7 truth.
- **IMPLEMENT**:
  - `README.md`:
    - The status paragraph at lines 6-13 still says **"Status: Phase 3 — read
      operations."** It is three phases stale. Rewrite it for a released 0.1.0 with the
      full surface.
    - Rewrite `## Install` around `install.sh`, keeping the manual `docker run` as a
      documented fallback rather than the headline.
    - Add the three `DEVMON_RATE_*` rows to the configuration table, and a short
      `### Rate limiting` subsection stating the tiers, the 429 shape with `Retry-After`,
      and — explicitly — that the authenticated tier keys on the device so a network
      handover is not punished.
    - Delete "An automated installer that resolves the socket GID for you is Phase 6" at
      line 49; it is now here and it is Phase 7.
    - Add a `## License` section and link `docs/THREAT-MODEL.md`, `docs/BACKUP.md`,
      `SECURITY.md`, `CONTRIBUTING.md`.
    - Add the `shellcheck` row to the CI table.
  - `compose.example.yaml`: add the three rate variables, commented; fix the stale
    "The Phase 6 installer resolves this automatically" to name `install.sh`.
  - PRD: Phase 7 row → `complete`, with links to this plan (moved to
    `plans/completed/`) and to the report.
- **GOTCHA**: `CLAUDE.md`'s Commands section still opens with "Nothing is implemented yet
  — `go.mod` is created by Task 1 of the current plan". Correct it, and add `make
  shellcheck`.
- **GOTCHA**: English only, everywhere, per `CLAUDE.md`.
- **VALIDATE**: Every internal link resolves; every command in the README is one that
  exists in the `Makefile`; the configuration table matches the constants in
  `internal/config/config.go` exactly.
- **ACCEPTANCE**: No document claims a phase state that is not true.

### Task 12: Verification sweep and phase report

- **ACTION**: Run every gate, then write
  `.claude/PRPs/reports/hardening-and-oss-release-report.md`.
- **IMPLEMENT**: Run the full gate list below and record actual output. Then the report,
  mirroring `client-independent-e2e-report.md`: Summary; Assessment vs Reality; Tasks
  Completed with commit SHAs; Validation Results; the environment proved against (Engine
  version, Go version, OS); the PRD success signals for this phase; defects found; the
  manual checklist; anything outstanding.
- **FALSIFICATION** — required, mirroring Phase 6's habit of proving a test can fail:
  1. Raise `DEVMON_RATE_STATUS_PER_MIN` to a huge value and confirm the rate-limit contract
     test goes **red** rather than passing because 429 never arrives.
  2. Remove `withDeviceLimit` from the `mutate` helper only and confirm the per-device case
     goes red — proving the test binds to the guarded tier and not to the pre-auth one.
  3. Point the installer at a state directory that already exists and confirm it refuses.
  Revert all three.
- **MANUAL CHECKLIST** (each ticked with evidence in the report):
  - [ ] `install.sh` run on a clean Linux host → paired device, no hand-written `docker run`
  - [ ] `install.sh --dry-run` prints the compose file and executes nothing
  - [ ] The installer refuses a non-empty existing state directory
  - [ ] A wrong `DEVMON_SELF_CONTAINER_ID` produces the Warn and self-exclusion still arms
  - [ ] `/v1/status` throttles and recovers, on a real host, with `Retry-After` honoured
  - [ ] Two paired devices: throttling one leaves the other served
  - [ ] `make e2e` green on a real Engine, version recorded
  - [ ] `make e2e-endurance` green — the limiter must not throttle a 30-minute stream
  - [ ] Full `-v` e2e output swept for PEM blocks and pairing-code-shaped strings: zero
  - [ ] `v0.1.0` published; `docker buildx imagetools inspect` shows amd64 and arm64
  - [ ] A fresh `docker compose pull` of the published tag starts and answers `/v1/status`
- **VALIDATE**: the full gate list below.
- **ACCEPTANCE**: Every gate green, every checklist item ticked or explicitly deferred with
  a reason, PRD row updated.

---

## Testing Strategy

### Unit tests

| Test | Input | Expected | Edge case? |
|---|---|---|---|
| `Registry.Allow` within burst | 5 calls, burst 5, fixed `now` | all true | |
| `Registry.Allow` past burst | 6th call, same `now` | false | |
| refill | 6th call at `now + interval` | true | |
| key isolation | keys `a` and `b` | independent buckets | |
| eviction at capacity | `maxKeys` full buckets, new key | new key admitted, `Len() <= maxKeys` | ✅ |
| capacity with all buckets drained | new key | `(false, false)` — caller falls back | ✅ |
| concurrency | 100 goroutines, `-race` | no race, no panic | ✅ |
| `clientIP` | `"1.2.3.4:5678"` | `"1.2.3.4"` | |
| `clientIP` IPv6 | `"[::1]:5678"` | `"::1"` | ✅ |
| `clientIP` unsplittable | `"garbage"` | `"garbage"`, no panic | ✅ |
| `clientIP` ignores XFF | `X-Forwarded-For: 9.9.9.9` set | still the `RemoteAddr` host | ✅ |
| 429 shape | tier exhausted | 429, integer `Retry-After` ≥ 1, `no-store`, exact body | |
| `withDeviceLimit` misuse | no device in context | 500, `next` never called | ✅ |
| config defaults | all three unset | 30 / 5 / 20 | |
| config bounds | `0`, `-1`, `"x"` | one problem each, naming the key | ✅ |
| config aggregation | three bad values | three problems in one error | ✅ |
| self-ID override discarded | override unknown, mountinfo known | mountinfo ID wins, Warn names the override | ✅ |
| self-ID override honoured | override known | no warning | |

### Edge cases checklist

- [ ] IPv6 client addresses
- [ ] A `RemoteAddr` that does not split
- [ ] A client-supplied `X-Forwarded-For` (must change nothing)
- [ ] Registry at capacity, all buckets drained
- [ ] Registry at capacity, some buckets full
- [ ] Zero-value `config.Config` reaching `NewServer` (test constructors)
- [ ] Rate limit hit on the SSE stream route — the connection must be refused cleanly, not
      opened and then torn down
- [ ] A 30-minute stream must not be throttled: it spends one token at request time
- [ ] Clock going backwards (`AllowN` with a `now` earlier than the last call) must not
      panic
- [ ] `install.sh` with an existing non-empty state directory
- [ ] `install.sh` on a host where `stat -c` is unavailable (BSD `stat`)
- [ ] `install.sh` with a public address containing a scheme or a port
- [ ] SPDX headers must not break `//go:build e2e`

---

## Validation Commands

### Static analysis
```bash
gofmt -l .          # must print nothing (CRLF checkout noise excepted — see Notes)
go vet ./...
go vet -tags e2e ./...
go build ./...
```

### Lint
```bash
golangci-lint run ./...
make e2e-lint
shellcheck -s sh install.sh
```

### Security
```bash
gosec ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
grep -rLn "SPDX-License-Identifier" --include=*.go .   # must list nothing
```

### Tests and coverage
```bash
CGO_ENABLED=1 go test ./internal/... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1              # floor is 80%
CGO_ENABLED=1 go test ./cmd/... -race
```

### End-to-end
```bash
make e2e                # both groups; from WSL2 on Windows
make e2e-endurance      # the limiter must not throttle a 30-minute stream
```

### Module hygiene
```bash
go mod tidy && git diff --exit-code go.mod go.sum   # tidy must be a no-op
go list -m all | grep golang.org/x/time             # exactly v0.15.0
```

### Image and release
```bash
make image
docker buildx imagetools inspect ghcr.io/scnplt/devmon-agent:0.1.0
```

---

## Acceptance Criteria

- [ ] All 12 tasks complete, one commit each
- [ ] Rate limiting enforced on every route, in the tiers and order specified above
- [ ] Three `DEVMON_RATE_*` variables validated at startup, aggregating with every other fault
- [ ] `golang.org/x/time v0.15.0` is the only added dependency; `go mod tidy` is a no-op
- [ ] Phase 6 Finding 1 closed and its e2e test tightened
- [ ] Security review written, every PRD risk row given a verdict, no open high-severity finding
- [ ] `install.sh` takes a clean host to a paired device; `shellcheck` clean
- [ ] `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, `docs/THREAT-MODEL.md`, `docs/BACKUP.md` exist and are accurate
- [ ] Every `.go` file carries the SPDX identifier
- [ ] `v0.1.0` published to GHCR for amd64 and arm64
- [ ] All 76 pre-existing e2e tests still green, plus the new rate-limit cases
- [ ] Coverage over `./internal/...` still ≥ 80%
- [ ] README claims no phase state that is not true
- [ ] PRD Phase 7 row → `complete`

## Completion Checklist

- [ ] New code is indistinguishable from the code around it
- [ ] Every bound is a named constant carrying its reasoning
- [ ] Errors wrapped with `%w` and context
- [ ] No log line, error string, or response field can carry key material, a pairing code, or PEM bytes
- [ ] No client-facing way to read or change a rate limit
- [ ] Tests written before implementation, table-driven, `-race` clean
- [ ] Every document in English
- [ ] Nothing added beyond this plan's scope

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| The limiter throttles legitimate app behaviour and the operator's first impression is a broken app | M | H | Burst sized to a real screenful (list + one inspect per container); per-device keying so a handover costs nothing; every limit raisable; the recovery case is an e2e test |
| `waitReady`'s status polling makes the rate-limit e2e tests flaky | H | M | Named as a Task 5 gotcha: loop to the first 429 rather than counting, and never set the status limit to 1 |
| The limiter registry becomes its own memory-exhaustion vector | M | H | Hard `maxKeys` cap, sweep-then-fall-back-to-global (D9), and a computed memory figure in the security review |
| Adding `golang.org/x/time` expands the supply chain of a security tool | L | M | `golang.org/x`, Go-team maintained, one package, no transitive requirements; version pinned and recorded in the plan |
| SPDX headers silently disable a `//go:build e2e` constraint | M | H | `go vet -tags e2e ./...` in Task 9's gate is exactly the check that catches it |
| `install.sh` works on the author's distro and nowhere else | M | H | POSIX `sh` not `bash`; both `stat` dialects; `--dry-run`; `shellcheck -s sh` in CI; manual run recorded in the report |
| The release workflow's `packages: write` is reachable from a fork PR | L | H | Tag-only trigger, never `pull_request`; permissions scoped per workflow |
| Publishing under AGPL-3.0 with an incorrect header sweep leaves files unlicensed | L | M | `grep -rLn` for the identifier is a gate, not a spot check |
| The security review turns up a HIGH finding late and blows the phase open | M | H | Task 7 runs before the docs and release tasks, not after; a HIGH finding is fixed in-phase by rule |

## Notes

- **Task ordering.** 1 → 2 → 3 → 4 → 5 are sequential; 6, 7, 8, 9, 10 are independent of
  each other once 5 lands and can be dispatched in parallel. 11 depends on 6, 8, 9, 10
  landing (it documents them). 12 is last.
- **Model routing** (`CLAUDE.md`): Tasks 1-5 are production Go and go to `go-implementer`
  on Sonnet, one task per invocation. Tasks 6-11 are shell, docs, config, and `.claude/**`
  — main-session work on Opus. Task 7's review agents are Sonnet. Task 12's report is
  main-session.
- **`gofmt -l .` lists the whole tree on this checkout** because of CRLF line endings; that
  is pre-existing and documented in every prior phase report. What must be verified is that
  files changed *by this phase* are clean, and that CI's `gofmt` job — which runs on a
  Linux checkout — stays green.
- **The Windows path is WSL2** for anything touching a Docker Engine, per Phase 6 (D6).
  `make e2e`, `make image`, and any `install.sh` trial must run there.
- **Phase 6's `harness.SweepOrphans` is gone.** If a rate-limit e2e run dies hard, recover
  with `make e2e-clean` and only when no other run is in flight.
- **The e2e suite adds no module dependency** — Phase 6's D1, still binding. `x/time` is a
  production dependency of `internal/ratelimit`; the suite itself must not import it.
