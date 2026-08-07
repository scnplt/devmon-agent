# Plan: Identity, Pairing & Revocation (DevMon Agent Phase 2)

## Summary

Turn the agent into its own certificate authority. It issues a unique client certificate to each
Android device during a one-time, host-authorised pairing step, authenticates every subsequent
request against that certificate, renews it silently before expiry, and lets access be withdrawn
either by the device itself or by an operator with host access — taking effect in the running agent
immediately, with no restart.

This is the phase that makes the product's core claim true: safer than an exposed Docker socket.
Everything after it (reads, logs, lifecycle) sits behind the channel built here.

## User Story

As an operator who self-hosts Docker on a VPS,
I want to pair my phone with the agent once and have it authenticate on its own credential thereafter,
So that I can reach my containers without SSH, and revoke a lost phone without rebuilding the host.

## Problem → Solution

**Current state (end of Phase 1)**: the agent terminates TLS with a self-signed server certificate,
reaches the Docker Engine, and persists state and logs. `tlsconf.Build` is called with `nil` client
CAs, so no client certificate can be verified against anything. `requireDevice` fails closed and
guards no route. `GET /v1/status` is the only endpoint. The `devices` table exists and is empty.

**Desired state**: the agent holds a long-lived CA on the state mount, issues 90-day device
certificates against operator-generated single-use pairing codes, verifies every guarded request
against the device registry on each call, renews certificates over the authenticated channel before
they expire, re-issues its own server certificate from the CA when the host address changes, refuses
to start when its identity is half-missing, and exposes host-side `device` subcommands for listing,
revoking, and minting pairing codes.

## Metadata

- **Complexity**: Large (16 files, 12 tasks; ~1,400 lines of production Go plus tests)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 2 — Identity, pairing & revocation
- **Depends on**: Phase 1 (complete)
- **Estimated Files**: 16 changed (9 created, 7 updated)

---

## Decisions Settled Before Planning

These were open questions during PRD review. They are settled here so implementation never has to
re-litigate them. Each one has a rationale because each one has a plausible-looking wrong answer.

| # | Decision | Choice | Why not the alternative |
|---|---|---|---|
| D1 | How the device's private key is created | Device generates its own keypair and sends a **PKCS#10 CSR**; the agent signs it and never sees a private key | The alternative — agent generates the keypair and ships the key to the phone — puts a private key on the wire and in the agent's memory and logs-adjacent code paths. A CSR flow makes key exfiltration structurally impossible. |
| D2 | What authenticates the pairing call | The **pairing code itself**, on a route that requires no client certificate | The device has no certificate yet, so it cannot be mTLS-authenticated. This does not violate the PRD's "may inform, never issue" rule — that rule constrains `GET /v1/status` specifically. `POST /v1/pair` is a distinct, code-authenticated route. |
| D3 | Pairing code strength | `crypto/rand.Text()` — 26 base32 characters, ~130 bits | Rate limiting is Phase 6. A 6-digit human-friendly code would be brute-forceable in seconds on an internet-facing port before that lands. High entropy makes the code safe **independently** of rate limiting, which is the only responsible ordering. |
| D4 | Pairing code at rest | Stored as **SHA-256 of the code**, never in plaintext | The state directory is readable in VPS snapshots and backups (an accepted MVP risk per the PRD). A plaintext pending code in a backup is a live credential. |
| D5 | Revocation propagation | **No in-memory device cache.** Every guarded request does one indexed SQLite lookup | This is the entire mechanism behind "a revoked device loses access immediately, without restarting the agent". Any cache — even a 5-second one — turns a hard guarantee into a race. At a few requests per minute the lookup cost is irrelevant. |
| D6 | Renewal overlap | The **old certificate stays valid until its own `not_after`**; the row is kept with `superseded_at` set | If the renewal response is lost in transit, the device still holds the old certificate. Invalidating it on issue would strand exactly the user renewal exists to protect. Revocation targets the **device**, not a certificate, so overlap never weakens it. |
| D7 | Revocation granularity | Revoking marks the **device** revoked; all its certificates die at once | A per-certificate revocation list would let a device survive revocation by renewing first. |
| D8 | Host-side CLI shape | **Subcommands on the same binary**, run via `docker exec` | The image is `distroless/static:nonroot` — there is no shell and no second binary. `docker exec devmon-agent /usr/local/bin/devmon-agent device list` is the only shape that works. |
| D9 | Missing-state detection | A **consistency matrix** over (state DB present, CA present) — both absent is a genuine first run; exactly one present is fatal | A bare "is the CA missing?" check cannot tell a first install from a destroyed state directory, and getting that wrong means either a false alarm on every fresh install or silent re-identification after data loss. |
| D10 | Server certificate on address change | **Re-issued from the retained CA**, automatically, on detected SAN drift | Clients pin the **CA**, not the leaf, so re-issuance is invisible to them. This is what makes a VPS IP change survivable without re-pairing. Phase 1 already detects the drift and logs it; this phase acts on it. |
| D11 | `last_seen_at` write cost | Updated only when the stored value is older than 60s | Writing on every request, against a pool capped at `MaxOpenConns(1)`, would serialise reads behind a write for no operational benefit. |

---

## UX Design

### Before

```
┌──────────────────────────────────────────────────────────┐
│  Operator                                                 │
│    $ docker run ... devmon-agent                          │
│    agent listening addr=:8443 policy=default              │
│                                                           │
│  Phone                                                    │
│    GET /v1/status  ──────────────►  200 {version, policy} │
│    (anything else) ──────────────►  404                   │
│                                                           │
│  There is no way to become a client.                      │
└──────────────────────────────────────────────────────────┘
```

### After

```
┌──────────────────────────────────────────────────────────────────────┐
│  Operator (host, once per device)                                     │
│    $ docker exec devmon-agent /usr/local/bin/devmon-agent \           │
│          device pair-code --name "Pixel 9"                            │
│    Pairing code: K25YDO5XNWWNL2GQJUKB6TTFYZ                           │
│    Expires:      2026-08-07T14:12:00Z (10 minutes, single use)        │
│                                                                       │
│  Phone (once)                                                         │
│    generates keypair ─► POST /v1/pair {code, csr}                     │
│                     ◄── 201 {device_id, cert, ca_cert, not_after}     │
│    pins ca_cert                                                       │
│                                                                       │
│  Phone (every time after)                                             │
│    mTLS handshake with its own cert ──► guarded routes                │
│    near expiry: POST /v1/device/renew ──► 200 {cert, not_after}       │
│    user taps "forget server": DELETE /v1/device/self ──► 204          │
│                                                                       │
│  Operator (phone lost)                                                │
│    $ ... device list                                                  │
│    ID        NAME      PAIRED       LAST SEEN   STATE                 │
│    d3f9a1c2  Pixel 9   2026-08-07   2m ago      active                │
│    $ ... device revoke d3f9a1c2                                       │
│    revoked d3f9a1c2 (Pixel 9)                                         │
│    → the phone's very next request fails. No restart.                 │
└──────────────────────────────────────────────────────────────────────┘
```

### Interaction Changes

| Touchpoint | Before | After | Notes |
|---|---|---|---|
| `GET /v1/status` | 4 fields | 5 fields — adds `ca_fingerprint` | Lets a client tell "my credential expired" from "this server's identity changed". `status_test.go:54` asserts the exact count and must change 4 → 5. |
| Guarded routes | none exist | `requireDevice` resolves a real device | Rejection stays the terse `client certificate required` — never *why*. |
| First start, empty state | generates self-signed server cert | generates CA **and** CA-issued server cert; logs the fingerprint at WARN | The operator should record the fingerprint; it is what detects a later identity change. |
| First start, partial state | not detected | **fatal, specific error** | PRD requirement: a regenerated identity is indistinguishable from an attack. |
| Host address change | WARN only | WARN **then re-issues** the server cert from the CA | No re-pair. |
| Host CLI | none | `device list` / `device revoke` / `device pair-code` | Same binary, via `docker exec`. |

---

## Mandatory Reading

Read these before writing any code. Line numbers are current as of this plan.

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `internal/state/store.go` | 1–195 | The store contract, `FirstRun`, the DSN pragmas, and `verifyAndMigrate` — which this phase extends into a real migration ladder. |
| P0 | `internal/state/schema.go` | 1–48 | The v1 schema and the comment claiming this phase adds rows, not migrations. Task 1 revises that intent deliberately. |
| P0 | `internal/certs/selfsigned.go` | 25–111 | Certificate template conventions, PEM types, serial entropy, the 398-day leaf rule, and `splitSANs`. |
| P0 | `internal/certs/store.go` | 16–162 | `os.OpenRoot` + `O_EXCL` write discipline, the both-or-neither keypair check, and the drift reporting this phase acts on. |
| P0 | `internal/httpapi/middleware.go` | 9–35 | `requireDevice` as Phase 1 left it, including the exact `_ = next` placeholder to replace. |
| P0 | `internal/tlsconf/tlsconf.go` | 10–47 | Why `VerifyClientCertIfGiven` rather than `RequireAndVerifyClientCert`. Do not "fix" this. |
| P1 | `internal/httpapi/respond.go` | 9–47 | `writeJSON` / `writeError` — the single exit point for every body, and the rule that error text stays terse. |
| P1 | `internal/httpapi/status.go` | 15–40 | The strict-allowlist comment on `statusResponse`, which already names `ca_fingerprint` as this phase's addition. |
| P1 | `internal/httpapi/server.go` | 63–75 | `routes()` and the Go 1.22+ method-pattern requirement. |
| P1 | `internal/config/config.go` | 85–98 | Derived-path methods. New paths belong here, never concatenated by hand. |
| P1 | `cmd/devmon-agent/main.go` | 52–145 | `run`/`serve` construction order and `runAll`. |
| P2 | `internal/httpapi/status_test.go` | 30–62, 188–233 | `TestStatusFieldCount` (the 4→5 edit) and the `requireDevice` table test to extend. |
| P2 | `internal/state/store_test.go` | 17–90 | Test helpers (`testLogger`, `tempDBPath`, `openStore`) and the Arrange/Act/Assert comment style. |
| P2 | `Dockerfile` | 29–36 | `distroless/static:nonroot`, no shell — the constraint behind D8. |
| P2 | `Makefile` | 31–55 | Gate commands. `-race` needs `CGO_ENABLED=1`; the shipped binary does not. |

## External Documentation

No third-party research is needed: the CA, CSR, and verification path is entirely `crypto/x509`, and
this phase adds no new module dependency. Every API below was **compile-verified against the
project's own toolchain (go1.26.4)** rather than recalled — the probe output is reproduced so the
implementer can trust the shapes.

| Topic | Source | Key Takeaway |
|---|---|---|
| Pairing code entropy | `crypto/rand.Text()` (Go 1.24+) | Returns a 26-character base32 string, ~130 bits. Verified: `rand.Text len: 26 sample: K25YDO5XNWWNL2GQJUKB6TTFYZ`. |
| CSR handling | `crypto/x509` | `x509.ParseCertificateRequest(der)` then `csr.CheckSignature()` (method, no args, returns `error`). Verified both return `<nil>` on a well-formed CSR. |
| Signing a leaf from a CSR | `x509.CreateCertificate(rand.Reader, leafTmpl, caCert, csr.PublicKey, caKey)` | `csr.PublicKey` is `any`; for a P-256 CSR it type-asserts to `*ecdsa.PublicKey`. Verified. |
| Client-cert verification | `leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})` | Returned `<nil>` for a CA-issued leaf carrying `ExtKeyUsageClientAuth`. **A leaf without that EKU is rejected by `crypto/tls` during the handshake** — omitting it is the single most likely silent failure in this phase. |
| Serial as a registry key | `big.Int.Text(16)` / `new(big.Int).SetString(s, 16)` | Round-trips exactly. Verified. Use lowercase hex, no `0x`, no colons. |
| CA constraints | `x509.Certificate` | `IsCA: true`, `BasicConstraintsValid: true`, `MaxPathLenZero: true`, `KeyUsage: CertSign \| CRLSign`. Verified to sign and verify. |

```
KEY_INSIGHT: A CA-issued client certificate MUST carry ExtKeyUsage{ExtKeyUsageClientAuth}.
APPLIES_TO:  Task 4 (CA.IssueDeviceCert)
GOTCHA:      crypto/tls verifies peer certificates with KeyUsages=ClientAuth. A leaf missing the
             EKU fails at the TLS handshake with an opaque "bad certificate" alert, before any
             handler or middleware runs — so no agent log line will name the real cause. The unit
             test in Task 4 must assert the EKU explicitly; the handshake test in Task 7 will not
             tell you which of a dozen things was wrong.

KEY_INSIGHT: MaxPathLenZero must be set on the CA, not just MaxPathLen: 0.
APPLIES_TO:  Task 4 (certs.LoadOrCreateCA)
GOTCHA:      x509 treats MaxPathLen=0 as "unset" unless MaxPathLenZero is true. Without it the CA
             encodes no path-length constraint at all, permitting sub-CAs.
```

---

## Patterns to Mirror

Every snippet below is copied verbatim from this repository. New code must be indistinguishable
from it.

### NAMING_CONVENTION
```go
// SOURCE: internal/config/config.go:22-27
// Environment variable keys. Declared as constants so a typo is a compile error
// and so error messages can name the exact variable the operator must fix.
const (
	envStateDir        = "DEVMON_STATE_DIR"
	envListenAddr      = "DEVMON_LISTEN_ADDR"
```
```go
// SOURCE: internal/certs/store.go:16-24
// File names and modes within $DEVMON_STATE_DIR/certs.
const (
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"

	certsDirMode      = 0o700
	serverKeyFileMode = 0o600
	serverCrtFileMode = 0o644
)
```
Unexported package-level constants for every file name, mode, timeout, and limit. No literal
appears twice. Every constant carries a comment saying why that value.

### ERROR_HANDLING
```go
// SOURCE: internal/state/store.go:27-33
var (
	// ErrStateCorrupt means the file exists but is not a usable database — the
	// realistic outcome of a botched restore or a truncated copy.
	ErrStateCorrupt = errors.New("state store is unreadable or corrupt")
	// ErrSchemaTooNew means the store was written by a newer agent version.
	ErrSchemaTooNew = errors.New("state store was written by a newer agent version")
)
```
```go
// SOURCE: internal/certs/store.go:36-38
	if err := os.MkdirAll(dir, certsDirMode); err != nil {
		return tls.Certificate{}, false, fmt.Errorf("create certs dir %s: %w", dir, err)
	}
```
Sentinels only where a caller must branch. Every other error is wrapped with `%w` and a lowercase
verb-phrase naming the operation and its subject. Never `errors.New` inline for a wrapped cause.

### LOGGING_PATTERN
```go
// SOURCE: internal/dockerx/client.go:58-61
	log.Info("docker engine reachable",
		slog.String("api_version", res.APIVersion),
		slog.String("os_type", res.OSType),
	)
```
```go
// SOURCE: internal/certs/store.go:60-64
		log.Warn("server certificate does not cover every configured address",
			slog.Any("configured", sans),
			slog.Any("covered_dns", leaf.DNSNames),
			slog.Any("covered_ip", ipStrings(leaf.IPAddresses)),
		)
```
Lowercase message, no punctuation, typed `slog` attributes with `snake_case` keys. The injected
`*slog.Logger` only — never `slog.Default()`. **Never log key material, pairing codes, or PEM
bytes, at any level** (`internal/certs/selfsigned.go:8-9`). Log a certificate's serial, subject, and
expiry; never its bytes.

### REPOSITORY_PATTERN
```go
// SOURCE: internal/state/store.go:1-7
// Package state owns the agent's durable store: the device registry, the audit
// trail, and the schema version that gates upgrades.
//
// The store is a SQLite database in WAL mode on the operator's bind mount.
// Callers never see *sql.DB or a SQL string — everything goes through methods on
// Store, each taking a context.Context first.
```
```go
// SOURCE: internal/state/store.go:203-231
func (s *Store) PruneAudit(ctx context.Context, maxAge time.Duration, maxRows int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin audit prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	...
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit audit prune: %w", err)
	}
```
No SQL string escapes the `state` package. Every method takes `ctx` first. Multi-statement writes
go in a transaction with `defer tx.Rollback()` before the commit.

### SERVICE_PATTERN
```go
// SOURCE: internal/state/pruner.go:28-36
func NewPruner(store *Store, maxAge time.Duration, maxRows int, log *slog.Logger) *Pruner {
	return &Pruner{
		store:    store,
		maxAge:   maxAge,
		maxRows:  maxRows,
		interval: defaultPruneInterval,
		log:      log,
	}
}
```
Constructor injection, dependencies as parameters, no package-level mutable state, no singletons.
Long-lived components expose `Run(ctx) error` and are passed to `runAll` in `main`.

### HTTP_HANDLER_PATTERN
```go
// SOURCE: internal/httpapi/status.go:33-40
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, statusResponse{
		APIVersion:   APIVersion,
		AgentVersion: version.Version,
		PolicyMode:   s.cfg.PolicyMode.String(),
		ServerTime:   time.Now().UTC().Format(time.RFC3339),
	})
}
```
```go
// SOURCE: internal/httpapi/server.go:70-72
	// The Go 1.22+ method pattern matters. Registering "/v1/status" alone would
	// also match POST, DELETE, and everything else.
	mux.HandleFunc("GET /v1/status", s.handleStatus)
```
Handlers are methods on `*Server`, named `handleX`. Every route registers a method. Every response
goes through `writeJSON`/`writeError`. Request and response types are unexported structs with
`json` tags declared next to the handler.

### TEST_STRUCTURE
```go
// SOURCE: internal/httpapi/status_test.go:64-90
func TestStatusContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode policy.Mode
		want string
	}{
		{name: "read-only is advertised", mode: policy.ModeReadOnly, want: "read-only"},
		...
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := testServer(t, tt.mode)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

			// Assert
			var body statusResponse
```
```go
// SOURCE: internal/state/store_test.go:17-34
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, testLogger())
	if err != nil {
		t.Fatalf("Open(%s) unexpected error: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
```
`t.Parallel()` at both levels. Literal `// Arrange` / `// Act` / `// Assert` comments. Table entries
have prose `name` fields. `t.Fatalf` for setup faults, `t.Errorf` for assertions. Failure messages
follow `got = X, want Y` and often add *why it matters* (`store_test.go:89`).

---

## State Directory Layout (after this phase)

```
$DEVMON_STATE_DIR/                 (0700)
├── devmon.db                      (0600)  schema v2
├── devmon.db-wal / -shm           (0600)
├── certs/                         (0700)
│   ├── ca.crt                     (0644)  ← NEW  long-lived, 10 years
│   ├── ca.key                     (0600)  ← NEW  unencrypted (PRD-accepted risk)
│   ├── server.crt                 (0644)  now CA-issued, was self-signed
│   └── server.key                 (0600)
└── logs/                          (0700)
    └── agent.log                  (0600)
```

## Database Schema (v2)

```sql
-- devices: one row per paired device. Identity only; certificates live separately
-- so a renewal is an INSERT, not a destructive UPDATE (see D6).
CREATE TABLE devices (
    id           TEXT PRIMARY KEY,   -- 16 lowercase hex chars
    name         TEXT NOT NULL,
    paired_at    INTEGER NOT NULL,
    last_seen_at INTEGER,
    revoked_at   INTEGER             -- NULL = active
);
CREATE INDEX idx_devices_revoked ON devices(revoked_at);

-- device_certs: every certificate ever issued to a device. Auth resolves the
-- peer's serial here, then checks the owning device is not revoked.
CREATE TABLE device_certs (
    serial        TEXT PRIMARY KEY,  -- big.Int.Text(16), lowercase, no 0x
    device_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    not_before    INTEGER NOT NULL,
    not_after     INTEGER NOT NULL,
    issued_at     INTEGER NOT NULL,
    superseded_at INTEGER             -- set on renewal; the cert stays valid (D6)
);
CREATE INDEX idx_device_certs_device ON device_certs(device_id);

-- pairing_codes: pending single-use codes. The code is never stored in plaintext (D4).
CREATE TABLE pairing_codes (
    code_hash   TEXT PRIMARY KEY,    -- hex SHA-256 of the code
    device_name TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,             -- NULL = unused; set atomically on redemption
    used_by     TEXT                 -- device id, once redeemed
);
CREATE INDEX idx_pairing_codes_expires ON pairing_codes(expires_at);
```

All timestamps are Unix seconds, matching the v1 convention in `schema.go`.

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `internal/state/schema.go` | UPDATE | Add `schemaV2`, bump `currentSchemaVersion` to 2, add the migration ladder. |
| `internal/state/store.go` | UPDATE | Replace the single-schema apply with versioned migrations; add new sentinels. |
| `internal/state/devices.go` | CREATE | Device + certificate registry methods. |
| `internal/state/pairing.go` | CREATE | Pairing-code mint and atomic single-use redemption. |
| `internal/state/devices_test.go` | CREATE | Registry tests, including concurrent redemption. |
| `internal/certs/ca.go` | CREATE | `LoadOrCreateCA`, `Fingerprint`, `Pool`. |
| `internal/certs/issue.go` | CREATE | `IssueDeviceCert` (CSR → leaf) and `IssueServerCert` (CA-signed). |
| `internal/certs/store.go` | UPDATE | Server cert now CA-issued; re-issue on SAN drift; identity consistency check. |
| `internal/certs/ca_test.go` | CREATE | CA properties, issuance, EKU, verification. |
| `internal/httpapi/pair.go` | CREATE | `POST /v1/pair`. |
| `internal/httpapi/device.go` | CREATE | `POST /v1/device/renew`, `DELETE /v1/device/self`. |
| `internal/httpapi/middleware.go` | UPDATE | `requireDevice` resolves a real device; adds context injection. |
| `internal/httpapi/status.go` | UPDATE | Add `ca_fingerprint`. |
| `internal/httpapi/server.go` | UPDATE | New routes; `NewServer` takes the CA fingerprint and issuer. |
| `internal/httpapi/*_test.go` | UPDATE | 4→5 field count; new endpoint and auth tests. |
| `cmd/devmon-agent/cli.go` | CREATE | `device list\|revoke\|pair-code`. |
| `cmd/devmon-agent/main.go` | UPDATE | CA load, consistency check, CLI dispatch, wire `tlsconf.Build(cert, pool)`. |
| `README.md` | UPDATE | Pairing walkthrough, CLI reference, fingerprint guidance. |

## NOT Building

Explicitly out of scope. Adding any of these is scope creep, not thoroughness.

- **Rate limiting** — Phase 6. D3 makes the pairing code safe without it; do not add a limiter here.
- **Any container, image, network, or volume operation** — Phases 3–5. `dockerx` gains no methods.
- **Audit log writes** — Phase 5. The `audit` table stays empty; do not write to it, even for pairing.
- **A CRL or OCSP responder** — revocation is a registry lookup (D5). No CRL is published.
- **Encrypting `ca.key` at rest** — the PRD settles this explicitly; document it, do not solve it.
- **Cross-device management from the app** — a device may act on itself and nothing else.
- **Notifications on pair/revoke** — post-MVP, `Won't` in the PRD.
- **CA renewal or rotation tooling** — the CA is 10-year; rotation is a later concern.
- **Changing `VerifyClientCertIfGiven`** — `tlsconf.go:10-27` explains why. Leave it.

---

## Step-by-Step Tasks

Tasks 1–2, 3–4, and 9 are the natural parallel groups; 5 onward serialise on them. Each task is
sized for one `go-implementer` invocation.

### Task 1: Schema v2 and the migration ladder
- **ACTION**: Extend `internal/state/schema.go` and `verifyAndMigrate` in `store.go`.
- **IMPLEMENT**: Bump `currentSchemaVersion` to `2`. Add `schemaV2` containing the three statements
  in the Database Schema section. Replace the unconditional `ExecContext(ctx, schemaV1)` in
  `verifyAndMigrate` (`store.go:147`) with a ladder: read the current version, then apply each
  migration whose target exceeds it, in order, **inside a single transaction per step**, stamping
  `schema_meta.version` in that same transaction. A fresh store applies v1 then v2.
- **MIRROR**: REPOSITORY_PATTERN — SQL stays in the package; the transaction uses
  `defer tx.Rollback()` before `Commit`, exactly as `PruneAudit` does.
- **IMPORTS**: no new ones.
- **GOTCHA**: The v1 `devices` table has `cert_serial`/`cert_not_after` columns that v2 replaces.
  **Drop and recreate `devices` in the v2 migration.** This is safe and is the only time it will be:
  Phase 1 never wrote a device row, so every existing deployment's table is provably empty. Do not
  attempt `ALTER TABLE` gymnastics — preserving data that cannot exist is complexity for nothing.
  Note the deviation from `schema.go:13-15`, which predicted no Phase 2 migration; update that
  comment rather than leaving it to mislead the next reader.
- **GOTCHA**: `_foreign_keys=1` is already in the DSN (`store.go:127`), so `device_certs`'s
  `REFERENCES` is enforced. Insert the device row before its certificate row.
- **GOTCHA**: `reconcileVersion` (`store.go:155-175`) currently stamps the version only when the row
  is absent. The ladder subsumes that logic — rewrite it rather than layering on top, or a v1 store
  will migrate and then keep reporting v1.
- **VALIDATE**: `go test ./internal/state/... -race`. New tests: a fresh store reports version 2 and
  has all five tables; a v1 store on disk (build one by exec'ing `schemaV1` and stamping version 1)
  migrates to v2 and keeps its `schema_meta` row; a store stamped version 3 still returns
  `ErrSchemaTooNew`.

### Task 2: Device and certificate registry
- **ACTION**: Create `internal/state/devices.go` and `internal/state/devices_test.go`.
- **IMPLEMENT**:
  ```go
  type Device struct {
      ID         string
      Name       string
      PairedAt   time.Time
      LastSeenAt time.Time // zero when never seen
      RevokedAt  time.Time // zero when active
  }
  func (d Device) IsRevoked() bool

  func (s *Store) CreateDevice(ctx context.Context, name string) (Device, error)
  func (s *Store) DeleteDevice(ctx context.Context, id string) error
  func (s *Store) RecordDeviceCert(ctx context.Context, deviceID, serial string, notBefore, notAfter time.Time) error
  func (s *Store) SupersedePriorCerts(ctx context.Context, deviceID, keepSerial string) error
  func (s *Store) DeviceByCertSerial(ctx context.Context, serial string) (Device, error)
  func (s *Store) ListDevices(ctx context.Context) ([]Device, error)
  func (s *Store) RevokeDevice(ctx context.Context, id string) error
  func (s *Store) TouchDevice(ctx context.Context, id string) error
  ```
  `CreateDevice` generates the ID as 8 random bytes hex-encoded via `crypto/rand.Read`.
  `DeviceByCertSerial` joins `device_certs` to `devices` and returns `ErrDeviceNotFound` when the
  serial is unknown; it returns the device **even when revoked**, so the caller can distinguish and
  log precisely. `RevokeDevice` is idempotent and returns `ErrDeviceNotFound` for an unknown id.
  `DeleteDevice` exists only for the pairing rollback path in Task 7. `TouchDevice` implements D11:
  `UPDATE ... SET last_seen_at=? WHERE id=? AND (last_seen_at IS NULL OR last_seen_at < ?)`.
- **MIRROR**: REPOSITORY_PATTERN for method shape and transactions; ERROR_HANDLING for the new
  `ErrDeviceNotFound` sentinel (declare it beside `ErrStateCorrupt` in `store.go`).
- **IMPORTS**: `crypto/rand`, `encoding/hex`, `database/sql`, `errors`, `fmt`, `time`, `context`.
- **GOTCHA**: Times are Unix seconds in the DB and `time.Time` in Go — convert at the boundary in
  this package only. Nullable columns (`last_seen_at`, `revoked_at`) must scan into `sql.NullInt64`;
  scanning a NULL into `int64` fails at runtime, not compile time.
- **GOTCHA**: `TouchDevice`'s 60-second threshold is a named constant (`lastSeenResolution`), not a
  literal — see NAMING_CONVENTION.
- **VALIDATE**: `go test ./internal/state/... -race`. Cover: create → look up by serial; revoked
  device is returned with `IsRevoked()` true; unknown serial gives `ErrDeviceNotFound`; `TouchDevice`
  twice in quick succession performs one write (assert `last_seen_at` is unchanged by the second).

### Task 3: Pairing-code mint and redemption
- **ACTION**: Create `internal/state/pairing.go`; tests in `devices_test.go`.
- **IMPLEMENT**:
  ```go
  const pairingCodeTTL = 10 * time.Minute

  // MintPairingCode returns the PLAINTEXT code to show the operator once. Only its
  // hash is persisted (D4); the plaintext is unrecoverable afterwards.
  func (s *Store) MintPairingCode(ctx context.Context, deviceName string) (code string, expiresAt time.Time, err error)

  // RedeemPairingCode atomically marks the code used and returns the device name
  // it was minted for. It is the single-use guarantee.
  func (s *Store) RedeemPairingCode(ctx context.Context, code, deviceID string) (deviceName string, err error)

  func (s *Store) PrunePairingCodes(ctx context.Context) (int64, error)
  ```
  `MintPairingCode` uses `rand.Text()` and stores the hex SHA-256 of the code.
  `RedeemPairingCode` runs `UPDATE pairing_codes SET used_at=?, used_by=? WHERE code_hash=? AND
  used_at IS NULL AND expires_at > ?` and treats `RowsAffected() == 0` as `ErrPairingCodeInvalid`.
- **MIRROR**: REPOSITORY_PATTERN; ERROR_HANDLING for `ErrPairingCodeInvalid`.
- **IMPORTS**: `crypto/rand`, `crypto/sha256`, `encoding/hex`.
- **GOTCHA**: **The single-use guarantee lives in that one conditional UPDATE**, not in a
  read-then-write pair. A `SELECT` followed by an `UPDATE` is a TOCTOU race that two simultaneous
  pairing attempts with the same leaked code would win. The DSN's `_txlock=immediate`
  (`store.go:128`) plus the `used_at IS NULL` predicate is what makes it atomic.
- **GOTCHA**: `RedeemPairingCode` needs the device name **and** must set `used_by` in one statement.
  Use `RETURNING device_name` on the UPDATE (SQLite 3.35+; `modernc.org/sqlite` v1.56 supports it)
  rather than an UPDATE followed by a SELECT, which reintroduces the race.
- **GOTCHA**: `ErrPairingCodeInvalid` must be **one** error covering unknown, expired, and
  already-used. Three distinct errors would let a caller enumerate code state, and the handler must
  not tell a client which it was.
- **GOTCHA**: Never log the code. `MintPairingCode` returns it; only the CLI prints it, to stdout.
- **VALIDATE**: `go test ./internal/state/... -race`. Cover: mint → redeem succeeds; second redeem
  of the same code fails; expired code fails (mint, then rewrite `expires_at` into the past
  directly); **N goroutines redeeming one code concurrently produce exactly one success** — this is
  the test that justifies the conditional UPDATE, and it must run under `-race`.

### Task 4: The certificate authority
- **ACTION**: Create `internal/certs/ca.go` and `internal/certs/issue.go`.
- **IMPLEMENT**:
  ```go
  const (
      caValidity         = 10 * 365 * 24 * time.Hour
      deviceCertValidity = 90 * 24 * time.Hour
      caCertFile         = "ca.crt"
      caKeyFile          = "ca.key"
      caKeyFileMode      = 0o600
      caCrtFileMode      = 0o644
  )

  type CA struct { cert *x509.Certificate; key *ecdsa.PrivateKey }

  func LoadOrCreateCA(dir string, log *slog.Logger) (*CA, bool, error) // bool = created
  func (c *CA) Fingerprint() string          // hex SHA-256 over cert.Raw
  func (c *CA) CertPEM() []byte
  func (c *CA) Pool() *x509.CertPool
  func (c *CA) IssueDeviceCert(csrDER []byte, commonName string, now time.Time) (certPEM []byte, serial string, notAfter time.Time, err error)
  func (c *CA) IssueServerCert(sans []string, now time.Time) (certPEM, keyPEM []byte, err error)
  ```
  `IssueDeviceCert` parses the CSR, calls `csr.CheckSignature()`, rejects a non-ECDSA or non-P-256
  public key, then signs a leaf with `KeyUsage: DigitalSignature` and
  `ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}`.
- **MIRROR**: `selfsigned.go:53-98` for template construction, serial generation
  (`rand.Int` over `1<<serialBits`), clock-skew tolerance (`notBefore.Add(-time.Minute)`), and PEM
  encoding with the existing `pemTypeCertificate` / `pemTypePrivateKey` constants.
  `store.go:92-116` for the `os.OpenRoot` + `writeExclusive` write discipline.
- **IMPORTS**: `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/sha256`, `crypto/x509`,
  `crypto/x509/pkix`, `encoding/hex`, `encoding/pem`, `math/big`.
- **GOTCHA**: `ExtKeyUsageClientAuth` is mandatory — see the KEY_INSIGHT block above. Assert it in
  the unit test; the handshake will not tell you.
- **GOTCHA**: `MaxPathLenZero: true` alongside `IsCA: true`, or the CA permits sub-CAs.
- **GOTCHA**: `IssueServerCert` keeps the existing 398-day `serverCertValidity`, **not** `caValidity`
  — the comment at `selfsigned.go:26-29` explains that mobile stacks reject longer leaves.
- **GOTCHA**: Write `ca.key` first with `O_EXCL` at `0600` (never `WriteFile`-then-`Chmod`; gosec
  G306 and a real world-readable window), and remove it if the `ca.crt` write then fails — mirror
  `store.go:99-116` exactly.
- **GOTCHA**: `csr.Subject` is attacker-controlled. Ignore it entirely and set the CN from the
  agent's own `commonName` argument (the device ID). A CSR asking to be `CN=admin` must not get it.
- **VALIDATE**: `go test ./internal/certs/... -race`. Cover: created CA has `IsCA`, `MaxPathLenZero`,
  `CertSign`; a second `LoadOrCreateCA` on the same dir returns the same fingerprint and
  `created=false`; an issued device cert verifies against `Pool()` with
  `KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}`; a CSR with a broken signature is
  rejected; a CSR requesting `CN=admin` yields a cert whose CN is the supplied device ID; `ca.key`
  is mode `0600`.

### Task 5: Server certificate from the CA, and the identity consistency check
- **ACTION**: Update `internal/certs/store.go`.
- **IMPLEMENT**: Change `LoadOrCreateServerCert` to take a `*CA` and issue from it instead of
  self-signing. On detected `sanDrift`, **re-issue**: log at WARN, remove `server.crt`/`server.key`,
  and write a fresh CA-issued pair covering the current SANs (D10). Add:
  ```go
  // ErrIdentityIncomplete reports that the state directory holds some of the
  // agent's identity but not all of it — the signature of a partial restore or a
  // destroyed bind mount, not of a first install.
  var ErrIdentityIncomplete = errors.New("agent identity is incomplete")

  func CheckIdentityConsistency(certsDir string, dbExisted bool) error
  ```
  implementing D9: `(dbExisted=false, ca absent)` → nil, genuine first run;
  `(true, absent)` or `(false, present)` → `ErrIdentityIncomplete` naming which half is missing and
  telling the operator that every device must re-pair if they proceed by clearing the directory;
  `(true, present)` → nil.
- **MIRROR**: ERROR_HANDLING sentinel style; LOGGING_PATTERN for the drift warning already at
  `store.go:60-64`.
- **IMPORTS**: no new ones.
- **GOTCHA**: Re-issuance must remove **both** files before writing, or `writeExclusive`'s `O_EXCL`
  fails with "file exists" and the agent refuses to start after an address change — the exact
  outcome D10 exists to prevent.
- **GOTCHA**: `LoadOrCreateServerCert` currently returns `(tls.Certificate, bool, error)` where the
  bool is drift. Once drift triggers re-issuance the bool is always false at return; drop it and fix
  the one caller (`main.go:122`) rather than returning a value that can no longer be true.
- **GOTCHA**: Keep `keypairExists`'s both-or-neither check. It is a different failure (a torn write)
  from `ErrIdentityIncomplete` (a lost directory) and both messages should survive.
- **VALIDATE**: `go test ./internal/certs/... -race`. Cover: server cert chains to the CA; changing
  the SAN set re-issues and the new leaf covers the new address while the CA fingerprint is
  unchanged; the four consistency-matrix cases.

### Task 6: `requireDevice` resolves a real device
- **ACTION**: Update `internal/httpapi/middleware.go`.
- **IMPLEMENT**: Replace the Phase 1 placeholder (`middleware.go:29-33`, including `_ = next`) with:
  extract `r.TLS.PeerCertificates[0]`, format `SerialNumber.Text(16)`, call
  `s.st.DeviceByCertSerial`, reject on `ErrDeviceNotFound` or `IsRevoked()`, call `TouchDevice`,
  inject the device into the request context, and call `next`. Add:
  ```go
  type deviceCtxKey struct{}
  func DeviceFrom(ctx context.Context) (state.Device, bool)
  ```
- **MIRROR**: the existing `requireDevice` shape and `writeError` usage; LOGGING_PATTERN for the
  rejection log line.
- **IMPORTS**: `context`, `errors`, `github.com/scnplt/devmon-agent/internal/state`.
- **GOTCHA**: **Every rejection reason returns the same body and status** — the terse
  `msgClientCertRequired` with 401. Unknown serial, revoked device, and no certificate must be
  indistinguishable to the client (`respond.go:38-44`). The *reason* goes to the agent log, where
  the operator can read it and a scanner cannot.
- **GOTCHA**: A `TouchDevice` failure must **not** fail the request. Log it and continue; a
  last-seen write is bookkeeping, not authorisation.
- **GOTCHA**: `deviceCtxKey` is an empty struct type, not a string — a string key can collide with
  another package's context value.
- **GOTCHA**: `testServer` (`status_test.go:24-28`) passes `nil` for the store. Any test exercising
  the new lookup path needs a real `*state.Store`; add a second helper rather than changing the
  existing one, which several passing tests rely on.
- **VALIDATE**: `go test ./internal/httpapi/... -race`. Extend the existing table at
  `status_test.go:188` with: unknown serial → 401; revoked device → 401; active device → handler runs
  and `DeviceFrom` returns it. Assert the revoked and unknown bodies are byte-identical.

### Task 7: `POST /v1/pair`
- **ACTION**: Create `internal/httpapi/pair.go`.
- **IMPLEMENT**:
  ```go
  type pairRequest struct {
      PairingCode string `json:"pairing_code"`
      CSRPEM      string `json:"csr_pem"`
  }
  type pairResponse struct {
      DeviceID       string `json:"device_id"`
      CertificatePEM string `json:"certificate_pem"`
      CACertificate  string `json:"ca_certificate_pem"`
      NotAfter       string `json:"not_after"`   // RFC3339
  }
  ```
  Handler order: decode with a body size limit → decode the CSR PEM → `CreateDevice` →
  `RedeemPairingCode` → `IssueDeviceCert` → `RecordDeviceCert` → 201. The device name comes from the
  redeemed code, never from the request.
- **MIRROR**: HTTP_HANDLER_PATTERN; register as `mux.HandleFunc("POST /v1/pair", s.handlePair)`.
- **IMPORTS**: `encoding/pem`, `net/http`, `errors`, `time`.
- **GOTCHA**: Wrap the body in `http.MaxBytesReader(w, r.Body, maxPairBodyBytes)` with
  `maxPairBodyBytes = 8 << 10`. This route is reachable **without a client certificate**; an
  unbounded JSON body is a trivial memory-exhaustion vector and there is no rate limiting until
  Phase 6.
- **GOTCHA**: If issuance or recording fails **after** the code was redeemed, the operator's code is
  spent and the device got nothing. Order the sequence so failure leaves no orphan: create the
  device row first, redeem second, issue third; on any post-creation failure call `DeleteDevice` and
  log at ERROR. Return 500 with a terse body.
- **GOTCHA**: A failed pairing returns **401 with a terse body** for every cause — bad code, expired
  code, used code, malformed CSR. Do not distinguish; do not echo the code back; do not log the code.
- **VALIDATE**: `go test ./internal/httpapi/... -race`. Cover: valid code + CSR → 201, and the
  returned cert verifies against the CA pool; the same code twice → second is 401; malformed CSR →
  401; oversized body → 413; the response CN equals the returned `device_id`.

### Task 8: `POST /v1/device/renew` and `DELETE /v1/device/self`
- **ACTION**: Create `internal/httpapi/device.go`.
- **IMPLEMENT**: Both routes wrapped in `requireDevice`. `handleRenew` takes `{csr_pem}`, issues a
  new certificate for the **calling device's own ID**, records it, calls `SupersedePriorCerts`, and
  returns `{certificate_pem, not_after}`. `handleUnpairSelf` calls `RevokeDevice` on the calling
  device and returns 204.
- **MIRROR**: HTTP_HANDLER_PATTERN; `DeviceFrom(r.Context())` from Task 6.
- **IMPORTS**: same as Task 7.
- **GOTCHA**: The device ID comes **only** from `DeviceFrom` — never from the request body or a path
  parameter. Accepting an ID from the client is how "a device can act only on itself" becomes "any
  device can revoke any other", which the PRD forbids outright.
- **GOTCHA**: `SupersedePriorCerts` sets `superseded_at`; it must **not** delete rows or shorten
  validity (D6). The old certificate keeps working until its own expiry so a lost response cannot
  strand the device.
- **GOTCHA**: Self-unpair is permitted under **every** policy mode. Do not gate it on
  `policy.Mode.Allows` — giving up your own access is not a privileged act.
- **VALIDATE**: `go test ./internal/httpapi/... -race`. Cover: renew returns a cert with a later
  `not_after` and a different serial, and **both** old and new serials still resolve to the device;
  self-unpair returns 204 and the device's next request is 401; renew under `ModeReadOnly` succeeds.

### Task 9: Status endpoint gains `ca_fingerprint`
- **ACTION**: Update `internal/httpapi/status.go`, `server.go`, `status_test.go`.
- **IMPLEMENT**: Add `CAFingerprint string` with tag `json:"ca_fingerprint"` to `statusResponse`.
  Pass the fingerprint into `NewServer` and store it on `Server`. Update the allowlist comment at
  `status.go:15-20`, which currently says the fingerprint arrives "from Phase 2".
- **MIRROR**: HTTP_HANDLER_PATTERN.
- **IMPORTS**: none.
- **GOTCHA**: `status_test.go:54` asserts `len(body) != 4`. Change it to 5 and add
  `"ca_fingerprint"` to the key list at line 57. The comment at `status_test.go:30-34` explicitly
  anticipates this edit — update it too, so it keeps guarding rather than describing a past state.
- **GOTCHA**: The fingerprint is a **public** value — it is the pinning anchor, and publishing it is
  the point. Nothing else about the CA may join it.
- **VALIDATE**: `go test ./internal/httpapi/... -race`; assert the field is 64 lowercase hex
  characters and matches `ca.Fingerprint()`.

### Task 10: Host-side `device` subcommands
- **ACTION**: Create `cmd/devmon-agent/cli.go`; dispatch from `run` in `main.go`.
- **IMPLEMENT**: Before the server path, if `flag.Arg(0) == "device"`, dispatch to
  `runDeviceCommand(ctx, cfg, args)` and return. Subcommands:
  - `device list` — tabular: ID, NAME, PAIRED, LAST SEEN, STATE (`active` / `revoked`).
  - `device revoke <id>` — calls `RevokeDevice`; prints `revoked <id> (<name>)`; exit 1 on unknown.
  - `device pair-code --name <name>` — calls `MintPairingCode`; prints the code and its expiry.
- **MIRROR**: SERVICE_PATTERN for construction; `main.go:39-50` for the exit-code discipline.
- **IMPORTS**: `text/tabwriter`, `flag`, `os`.
- **GOTCHA**: The CLI opens the **same** SQLite file as the running agent. That is intended and is
  what WAL + `_busy_timeout=5000` (`store.go:117-122`) was configured for. It must **not** touch
  `certs/` at all — `LoadOrCreateCA` from a second process could race the agent's own start.
- **GOTCHA**: The CLI runs via `docker exec`, so it inherits the container's environment and
  `config.Load` succeeds unchanged. It must not require any variable the agent does not already
  need.
- **GOTCHA**: Print the pairing code to **stdout only**. Never to the logger — the log file is
  persisted and rotated, and a pairing code in `agent.log` is a durable credential
  (`selfsigned.go:8-9`). The CLI should not construct a `logging.Sink` at all.
- **GOTCHA**: `flag.Parse()` at `main.go:54` consumes the arguments; the subcommand check must read
  `flag.Arg(0)` after it, and `device`'s own flags need a separate `flag.NewFlagSet`.
- **VALIDATE**: `go build ./...`, then against a running container:
  `docker exec devmon-agent /usr/local/bin/devmon-agent device pair-code --name test` prints a
  26-character code; `device list` shows the device after pairing; `device revoke <id>` makes the
  paired client's next call fail **without restarting the agent**.

### Task 11: Wire it together in `main`
- **ACTION**: Update `cmd/devmon-agent/main.go`.
- **IMPLEMENT**: In `serve`, after `state.Open` and before certificate loading:
  `certs.CheckIdentityConsistency(cfg.CertsDir(), !st.FirstRun)` → fatal on error;
  `certs.LoadOrCreateCA(cfg.CertsDir(), log)` → on `created==true`, log at WARN with the fingerprint
  and the "record this" guidance; `certs.LoadOrCreateServerCert(cfg.CertsDir(), cfg.PublicAddrs, ca,
  log)`; `tlsconf.Build(cert, ca.Pool())` — replacing the `nil` at `main.go:135`; pass
  `ca.Fingerprint()` and the CA into `httpapi.NewServer`.
- **MIRROR**: the existing construction-order comment at `main.go:100-103`; `runAll` is unchanged.
- **IMPORTS**: none new.
- **GOTCHA**: The consistency check must run **before** `LoadOrCreateCA`, or the CA is created and
  the check it was supposed to trigger can never fire again.
- **GOTCHA**: Update the two stale comments at `main.go:121` ("re-issuance needs the Phase 2 CA")
  and `main.go:133-134` ("nil client CAs: there is no CA until Phase 2"). Leaving them is how the
  next reader concludes mTLS is still unfinished.
- **VALIDATE**: `go build ./...`; `make lint`; start the agent on a clean state dir and confirm the
  CA fingerprint appears once at WARN; restart and confirm it does not reappear as a creation event.

### Task 12: Gates, docs, and the manual sweep
- **ACTION**: Run every gate; update `README.md`.
- **IMPLEMENT**: README gains a pairing walkthrough (mint → pair → verify), the `device` CLI
  reference, guidance to record the CA fingerprint at install, and a note that `ca.key` is
  unencrypted at rest and therefore present in host backups and VPS snapshots.
- **MIRROR**: existing README tone.
- **VALIDATE**: every command in Validation Commands below, plus the manual checklist.

---

## Testing Strategy

### Unit Tests

| Test | Input | Expected Output | Edge Case? |
|---|---|---|---|
| Fresh store migrates to v2 | empty dir | version 2, five tables | |
| v1 store migrates to v2 | store stamped v1 | version 2, `schema_meta` preserved | ✓ |
| v3 store refuses to open | store stamped v3 | `ErrSchemaTooNew` | ✓ |
| Device lookup by serial | issued cert serial | the owning device | |
| Revoked device still resolves | revoked device's serial | device with `IsRevoked()` true | ✓ |
| Unknown serial | random hex | `ErrDeviceNotFound` | ✓ |
| `TouchDevice` throttles | two calls < 60s apart | one write | ✓ |
| Pairing code single use | same code twice | 1 success, 1 `ErrPairingCodeInvalid` | ✓ |
| **Concurrent redemption** | N goroutines, one code | **exactly one success** | ✓ |
| Expired pairing code | `expires_at` in the past | `ErrPairingCodeInvalid` | ✓ |
| CA is a CA | fresh CA | `IsCA`, `MaxPathLenZero`, `CertSign` | |
| CA is stable | `LoadOrCreateCA` twice | same fingerprint, `created=false` | |
| Device cert has ClientAuth EKU | issued cert | EKU present; verifies against pool | ✓ |
| CSR subject is ignored | CSR with `CN=admin` | cert CN = device ID | ✓ |
| Broken CSR signature | tampered CSR | error | ✓ |
| Non-P-256 CSR key | RSA CSR | error | ✓ |
| `ca.key` permissions | fresh CA | mode `0600` | ✓ |
| Server cert chains to CA | fresh state | verifies against CA pool | |
| SAN drift re-issues | changed `PublicAddrs` | new leaf covers it, same CA fingerprint | ✓ |
| Identity matrix | 4 (db, ca) combinations | 2 nil, 2 `ErrIdentityIncomplete` | ✓ |
| Auth rejects uniformly | unknown / revoked / absent cert | identical 401 bodies | ✓ |
| Pair happy path | valid code + CSR | 201, cert verifies | |
| Pair body too large | 9 KiB body | 413 | ✓ |
| Renew keeps old cert valid | renew once | both serials resolve | ✓ |
| Renew under read-only | `ModeReadOnly` | succeeds | ✓ |
| Self-unpair | authenticated DELETE | 204; next request 401 | |
| Status has 5 fields | `GET /v1/status` | exactly 5, incl. 64-hex fingerprint | ✓ |

### Edge Cases Checklist
- [ ] Empty input — blank pairing code, empty CSR, empty device name
- [ ] Maximum size input — oversized pair body (413), oversized CSR
- [ ] Invalid types — malformed JSON, non-PEM CSR, PEM of the wrong block type
- [ ] Concurrent access — simultaneous redemption of one code; CLI revoking while the agent serves
- [ ] Clock skew — cert `notBefore` one minute in the past (existing convention)
- [ ] Permission denied — read-only state mount at CA creation
- [ ] Partial failure — issuance fails after device creation (row cleaned up)
- [ ] Restart durability — pairings survive a restart **and** an image rebuild
- [ ] Renewal response lost — old certificate still authenticates

---

## Validation Commands

### Static Analysis
```bash
gofmt -l .
go vet ./...
```
EXPECT: `gofmt -l` prints nothing; `go vet` is silent.

### Lint
```bash
make lint
```
EXPECT: clean, or `golangci-lint not installed — go vet was the only lint run`.

### Security Scan
```bash
gosec ./...
```
EXPECT: no findings. Watch G306 (file permissions) and G404 (weak random) — both are live risks in
this phase; every random value here must come from `crypto/rand`.

### Unit Tests
```bash
CGO_ENABLED=1 go test ./internal/... -race
```
EXPECT: all pass. `-race` is mandatory — the concurrent-redemption test is meaningless without it.

### Full Test Suite with Coverage
```bash
make cover
```
EXPECT: total coverage ≥ 80%.

### Build Verification
```bash
go build ./...
make build
make image
```
EXPECT: `bin/devmon-agent` builds; the image builds with `CGO_ENABLED=0`.

### Database Validation
```bash
# With the agent stopped:
sqlite3 "$DEVMON_STATE_DIR/devmon.db" "SELECT value FROM schema_meta WHERE key='version';"
sqlite3 "$DEVMON_STATE_DIR/devmon.db" ".tables"
```
EXPECT: version `2`; tables `audit devices device_certs pairing_codes schema_meta`.

### End-to-End Validation (real host)
```bash
docker exec devmon-agent /usr/local/bin/devmon-agent device pair-code --name "Pixel 9"
# pair a client with the printed code, then:
docker exec devmon-agent /usr/local/bin/devmon-agent device list
docker exec devmon-agent /usr/local/bin/devmon-agent device revoke <id>
curl --cacert ca.crt --cert dev.crt --key dev.key https://host:8443/v1/status
```
EXPECT: pairing succeeds; `device list` shows the device; after `revoke`, the client's next
authenticated call returns 401 **with the agent still running**.

### Manual Validation
- [ ] Two devices pair with distinct codes and receive distinct certificates
- [ ] Both survive an agent restart
- [ ] Both survive `make image` + recreate (same bind mount) — **no device re-pairs**
- [ ] A device near expiry renews with no user interaction
- [ ] A revoked device loses access immediately, no restart
- [ ] `DEVMON_PUBLIC_ADDR` changed → server cert re-issued, CA fingerprint unchanged, no re-pair
- [ ] `rm -rf $DEVMON_STATE_DIR/certs` with the DB intact → explicit `ErrIdentityIncomplete`, not a
      silent new identity
- [ ] `GET /v1/status` on a client holding an expired cert returns a fingerprint matching the pinned
      one (expiry, not attack); after wiping state entirely, the fingerprint differs (attack signal)
- [ ] `grep -riE 'PRIVATE KEY|pairing|BEGIN CERT' $DEVMON_STATE_DIR/logs/agent.log` → no credential
      material

---

## Acceptance Criteria
- [ ] All 12 tasks completed
- [ ] Every validation command passes
- [ ] Coverage ≥ 80% over `./internal/...`
- [ ] `gofmt -l .` silent, `go vet` clean, `gosec` clean
- [ ] Two clients pair with distinct credentials; an unpaired client is rejected
- [ ] Pairings survive a restart **and** an image upgrade
- [ ] A near-expiry client renews with no user interaction
- [ ] A host-revoked device loses access immediately, without an agent restart
- [ ] An expired credential and a regenerated CA are distinguishable from `/v1/status` alone
- [ ] A removed state directory produces an explicit operator error, never a silent new identity
- [ ] No key material, PEM bytes, or pairing code appears in any log at any level

## Completion Checklist
- [ ] Code follows the discovered patterns
- [ ] Error handling matches the codebase style (`%w`, sentinels only where branched on)
- [ ] Logging follows conventions (typed attrs, snake_case, injected logger)
- [ ] Tests follow AAA with `t.Parallel()` at both levels
- [ ] No hardcoded values — every threshold is a named constant
- [ ] Stale Phase 1 comments updated: `main.go:121`, `main.go:133-134`, `middleware.go:22`,
      `status.go:17-18`, `status_test.go:30-34`, `schema.go:13-15`, `tlsconf.go:12-13`
- [ ] README updated
- [ ] No scope additions from the NOT Building list

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Device cert missing `ClientAuth` EKU; handshake fails opaquely | M | High | Explicit unit assertion in Task 4; the KEY_INSIGHT block calls it out as the top silent failure |
| Pairing code brute-forced before Phase 6 rate limiting | L | Critical | ~130-bit code (D3), 10-minute TTL, single-use enforced by a conditional UPDATE |
| Race lets one code pair two devices | M | High | Atomic conditional UPDATE with `RETURNING` (Task 3); concurrent test under `-race` |
| Renewal response lost; device stranded | M | High | Old cert valid to its own expiry (D6); test asserts both serials resolve |
| Revocation not immediate because of a cache | M | Critical | No cache by design (D5); e2e check with the agent running |
| v1→v2 migration corrupts a real device registry | L | High | Provably empty in Phase 1; transactional migration; version stamped in the same tx |
| CLI and agent contend on SQLite | M | Medium | WAL + `_busy_timeout=5000` + `_txlock=immediate`, already configured; CLI never touches `certs/` |
| Pairing code leaks into `agent.log` | L | Critical | Code returned, never logged; CLI prints to stdout and builds no log sink; grep in the manual checklist |
| Consistency check fires on a legitimate first install | M | Medium | Matrix keyed on (db, ca) together (D9), not on the CA alone; all four cases unit-tested |
| `ca.key` in VPS snapshots | M | High | Accepted and documented per the PRD; `0600` + `0700` dir; README states it |

## Notes

- **Phase 1 comments that predict this phase are load-bearing.** `schema.go:13-15` predicted "no
  migrations"; Task 1 deviates and must say why in the code. Several others (`middleware.go:22`,
  `status.go:17-18`, `tlsconf.go:12-13`) describe Phase 1 as a temporary state — each must be
  rewritten as it is fulfilled, or the codebase will read as unfinished forever.
- **`tlsconf.Build` needs no signature change** — it already accepts `*x509.CertPool` and handles
  non-nil correctly. Phase 1 anticipated this precisely; passing `ca.Pool()` is the whole change.
- **No new module dependency.** `go.mod` is untouched.
- **Model routing**: per `CLAUDE.md`, every task here goes to `go-implementer` (Sonnet), one task per
  invocation. Tasks 1–2, 3–4, and 9 are independent and can be dispatched in parallel.
- **All `crypto/x509` API shapes in this plan were compile-verified against go1.26.4**, not recalled.
  The probe covered `rand.Text`, CA creation, CSR parse + `CheckSignature`, leaf signing, pool
  verification with `ClientAuth`, and hex serial round-trip.
