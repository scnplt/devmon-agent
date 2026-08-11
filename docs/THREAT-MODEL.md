# Threat Model

devmon-agent holds a Docker socket. Anything that can drive its API reaches
root-equivalent power over the host it runs on
([`SECURITY.md:5-8`](../SECURITY.md)). This document says, with citations into
the code that makes each claim true, what the agent defends against, what it
does not, and what risks are accepted rather than fixed. If a claim here
cannot be traced to a line of code, it does not appear — see
[`CONTRIBUTING.md`](../CONTRIBUTING.md) if you find one that no longer
resolves.

This document does not contradict [`README.md`](../README.md) or the
[PRD](../.claude/PRPs/prds/devmon-agent.prd.md). Where the README already
states a security property, this document repeats its wording rather than
rephrasing it.

---

## Assets

### The CA private key (`ca.key`)

The agent is its own certificate authority. `ca.key` is the ECDSA private key
that signs every device's client certificate and the agent's own server
certificate. Whoever holds it can mint a certificate this agent will accept.

- Generated once and retained for the agent's lifetime:
  [`internal/certs/ca.go:58-88`](../internal/certs/ca.go) (`LoadOrCreateCA`).
- Stored at `$DEVMON_STATE_DIR/certs/ca.key`, file mode `0600`, written through
  an `os.Root` scoped to the certs directory so a path can never escape it:
  [`internal/certs/ca.go:38-43`](../internal/certs/ca.go) (`caKeyFileMode =
  0o600`), [`internal/certs/ca.go:132-154`](../internal/certs/ca.go)
  (`generateAndWriteCA`).
- Never leaves the process and is never logged: "Its private key never leaves
  this process; nothing in this package logs it, its PEM encoding, or any
  other key material."
  ([`internal/certs/ca.go:49-52`](../internal/certs/ca.go)).
- Its public fingerprint — and only the fingerprint — is deliberately exposed,
  on the unauthenticated `/v1/status` route, as the pinning anchor a client
  checks: [`internal/certs/ca.go:220-227`](../internal/certs/ca.go)
  (`Fingerprint`), [`internal/httpapi/status.go:17-31`](../internal/httpapi/status.go).

### The device registry

The set of paired devices, their certificate serials, and their revocation
state.

- Schema: `devices` and `device_certs` tables in
  [`internal/state/schema.go:70-89`](../internal/state/schema.go).
- Every authenticated request costs one indexed lookup against this table —
  there is no in-memory cache — which is what makes revocation immediate:
  `RevokeDevice` at
  [`internal/state/devices.go:172`](../internal/state/devices.go), checked on
  every guarded request by
  [`internal/httpapi/middleware.go:43-87`](../internal/httpapi/middleware.go)
  (`requireDevice`).
- Lives in `devmon.db`, mode `0600`:
  [`internal/state/store.go:46-48`](../internal/state/store.go)
  (`dbFileMode`), reinforced on every WAL sidecar at
  [`internal/state/store.go:106-123`](../internal/state/store.go)
  (`tightenPermissions`).

### The audit log

One row per mutating request — success, policy refusal, self-exclusion
refusal, or failure — attributing every state-changing call to the device that
made it.

- Written by `withAudit`, which sits **inside** `requireDevice`: an
  unauthenticated caller is unattributable and writes no row:
  [`internal/httpapi/audit.go:29-70`](../internal/httpapi/audit.go).
- Schema (unchanged since Phase 1):
  [`internal/state/schema.go:48-57`](../internal/state/schema.go).
- **Not reachable over the API, in any policy mode.** No route reads it — the
  full route table registers only status, pair, device renew/unpair, reads,
  logs, and the five mutating routes:
  [`internal/httpapi/server.go:149-208`](../internal/httpapi/server.go)
  (`routes`). It is read only through the host-side CLI
  (`device audit list`), which is host access, not API access.
- Retention is enforced on a fixed interval, independent of request traffic:
  [`internal/state/pruner.go:11-14`](../internal/state/pruner.go) and
  [`internal/state/pruner.go:44-56`](../internal/state/pruner.go).

### The Docker socket

`/var/run/docker.sock`, mounted read-only into the agent's container in the
documented deployment.

- `install.sh` mounts it `:ro`: "`:ro` does not prevent writes through the
  Docker API — the API is request/response over the socket — but it does
  prevent the socket file itself being replaced, and it states intent."
  ([`install.sh:465-468`](../install.sh)).
- The agent talks to it through `github.com/moby/moby/client`, constructed
  once at startup from `DEVMON_DOCKER_HOST`:
  [`internal/dockerx/client.go:42-46`](../internal/dockerx/client.go).
- The socket's GID is resolved from the host, not assumed, because it varies
  per distribution: [`install.sh:371-390`](../install.sh)
  (`resolve_socket_gid`).

---

## Trust boundaries

### internet → listening port

The single listening port serves both the unauthenticated `/v1/status` and
`/v1/pair` routes and every mTLS-guarded route. TLS 1.3 is the floor, with no
cipher-suite negotiation surface: [`internal/tlsconf/tlsconf.go:35-40`](../internal/tlsconf/tlsconf.go).
Everything reachable without a certificate is rate limited before its handler
runs — a global unauthenticated backstop plus a per-IP tier on each of the two
open routes: [`internal/httpapi/ratelimit.go:20-65`](../internal/httpapi/ratelimit.go)
(constants), [`internal/httpapi/ratelimit.go:105-144`](../internal/httpapi/ratelimit.go)
(`withGlobalUnauthLimit`, `withIPLimit`), wired at
[`internal/httpapi/server.go:157-163`](../internal/httpapi/server.go). The
limiter keys on `r.RemoteAddr`, never on `X-Forwarded-For`, because the
documented deployment is direct inbound and honouring a client-supplied header
would let any caller mint a fresh limiter key per request:
[`internal/httpapi/ratelimit.go:67-81`](../internal/httpapi/ratelimit.go)
(`clientIP`).

### intermediary → listening port

The boundary above assumes the device's TLS connection ends in the agent
process. A TLS-terminating intermediary — Cloudflare Tunnel's HTTP/HTTPS
ingress, or any reverse proxy in HTTP mode — moves that endpoint, and three
properties this model depends on do not survive the move.

Device authentication reads the certificate off the connection itself
([`internal/httpapi/middleware.go:44-50`](../internal/httpapi/middleware.go)),
and the intermediary's own connection to the agent carries none, so every
guarded route answers 401. The device's pinning of the agent's CA stops
describing what it is actually talking to, so an intermediary that is
compromised — or merely misconfigured — can answer for the agent without the
device being able to detect it. And `clientIP` resolves every caller to the
intermediary's address
([`internal/httpapi/ratelimit.go:67-81`](../internal/httpapi/ratelimit.go)),
collapsing both pre-authentication tiers into one budget shared by every
device.

An edge that verifies the client certificate on the agent's behalf and forwards
the result as a signed header does not restore the boundary; it relocates the
authority to that edge, whose configuration is not the operator's startup
configuration and therefore not the property the agent is built on
([`internal/config/config.go:3-9`](../internal/config/config.go)). The
operator-facing version of this, including what does work, is
[`README.md`](../README.md) under "Reaching it from outside".

### listening port → mTLS

The TLS listener uses `VerifyClientCertIfGiven`, not
`RequireAndVerifyClientCert`, because `/v1/status` must stay reachable without
a certificate on the same port: [`internal/tlsconf/tlsconf.go:18-30`](../internal/tlsconf/tlsconf.go).
The actual authentication boundary sits one layer up, in HTTP middleware:
`requireDevice` rejects any request without a verified client certificate
belonging to an active, registered device, and every rejection reason — no
certificate, unknown serial, revoked device — answers with the identical 401
body, so a scanner learns nothing that distinguishes the cases:
[`internal/httpapi/middleware.go:31-87`](../internal/httpapi/middleware.go).

### API → Engine

Once authenticated, a request's operation is checked against the host's
startup-fixed policy mode before it reaches the Docker Engine at all:
`requireOp` refuses with 403 when `s.cfg.PolicyMode.Allows(op)` is false:
[`internal/httpapi/policygate.go:20-41`](../internal/httpapi/policygate.go).
The policy mode itself cannot be widened by any client — it is read once at
startup from `DEVMON_POLICY_MODE` and is immutable thereafter:
[`internal/config/config.go:3-9`](../internal/config/config.go) (package
comment stating the security boundary). One target is refused regardless of
policy mode: the agent's own container, resolved by
[`internal/dockerx/self.go:42-99`](../internal/dockerx/self.go)
(`resolveSelf`, `confirmSelf`) and enforced against every lifecycle route.

### host shell → state directory

Host access (a shell on the box, or `docker exec` into the agent's own
container) is a stronger authority than any API credential: it is what runs
the device-management CLI and what can read or replace `$DEVMON_STATE_DIR`
directly. The agent detects when host-level tampering has left its identity
inconsistent — a state database with no certificate authority, or a
certificate authority with no state database — and refuses to start rather
than silently minting a fresh identity that would be indistinguishable from an
attacker replacing it outright:
[`internal/certs/store.go:234-271`](../internal/certs/store.go)
(`CheckIdentityConsistency`).

---

## Adversaries actually considered

**An internet scanner.** Reaches only `/v1/status` and `/v1/pair` without a
credential. `/v1/status`'s response is a strict field allowlist — API
version, agent version, policy mode, server time, CA fingerprint — carrying no
host, container, or credential data: "Its fields are a strict allowlist:
version, policy, server time, and the CA fingerprint. This endpoint may
inform, never issue."
([`internal/httpapi/status.go:17-31`](../internal/httpapi/status.go)). Both
routes are rate limited per IP with a shared global backstop
([`internal/httpapi/ratelimit.go:20-65`](../internal/httpapi/ratelimit.go)),
and an unmatched path gets `ServeMux`'s bare 404 rather than a route table a
scanner could map:
[`internal/httpapi/server.go:145-149`](../internal/httpapi/server.go). A
pairing code is single-use, expires in 10 minutes, and only its SHA-256 hash
is ever stored: [`internal/state/schema.go:91-101`](../internal/state/schema.go).

**A thief with an unlocked, paired phone.** The device's certificate carries
exactly the powers the host's `DEVMON_POLICY_MODE` grants, checked on every
mutating request by the same code path every other device uses
([`internal/httpapi/policygate.go:20-41`](../internal/httpapi/policygate.go))
— there is no per-device elevation. The device cannot widen its own policy
mode, cannot act on the agent's own container
([`internal/dockerx/self.go:42-99`](../internal/dockerx/self.go)), and cannot
read the audit log that would show what it did
([`internal/httpapi/server.go:149-208`](../internal/httpapi/server.go), no
audit route). Once the operator notices, `device revoke` takes effect on that
device's very next request, because there is no cache of paired devices to go
stale: [`internal/state/devices.go:172`](../internal/state/devices.go),
enforced at [`internal/httpapi/middleware.go:43-87`](../internal/httpapi/middleware.go).

**Someone who reads a VPS snapshot or a host backup.** A snapshot or backup
captures `$DEVMON_STATE_DIR` as bytes, which bypasses the filesystem
permissions that protect it on a running host. `ca.key` is stored unencrypted
(see Standing Accepted Risks below), so a reader of the backup can mint a
device certificate this agent will accept. The device registry and audit log
in `devmon.db` are also captured in full. See [`docs/BACKUP.md`](BACKUP.md)
for what this means for handling backups.

**A malicious container on the same host.** It faces the same trust boundary
as an internet scanner: nothing it can do to the agent's API surface bypasses
`requireDevice` without a valid client certificate
([`internal/httpapi/middleware.go:43-87`](../internal/httpapi/middleware.go)).
It cannot use the agent's API to stop or delete the agent's own container even
if it somehow obtained a valid device certificate, because self-exclusion is a
fixed rule, not a policy setting:
[`internal/dockerx/self.go:37-40`](../internal/dockerx/self.go)
(`Containerized`). What it can do is entirely a function of its own access to
the host's Docker socket, which is outside this agent's control — see "Not
defended" below.

---

## What is explicitly NOT defended

This list matches [`SECURITY.md`](../SECURITY.md)'s scope section verbatim in
substance, because the two documents describe the same boundary:

- **An attacker who already has root on the host.** Root can read `ca.key`
  directly off disk regardless of file mode, replace the agent binary, or
  bypass it entirely by talking to the Docker socket itself.
- **A compromised or malicious Docker Engine.** The agent trusts every
  response the Engine gives it; it has no way to verify the Engine's own
  integrity.
- **An operator who exposes the port to the internet with no VPN or firewall
  in front of it.** `install.sh`'s own closing message says so directly: "Do
  not expose this port to the open internet without a VPN or a firewall in
  front of it." ([`install.sh:622-623`](../install.sh)).
- **A deployment that terminates TLS in front of the agent.** The agent
  authenticates from the connection it terminates itself; an HTTPS reverse
  proxy or tunnel ingress is not a supported configuration and is outside this
  model, not a hardened case within it (see *intermediary → listening port*
  above).
- **Side-channel and timing attacks.**
- **Denial of service beyond what the rate limiter is documented to bound.**
  The limiter's constants — a 50 req/s global unauthenticated backstop, a
  30/min `/v1/status` default, a 5/min `/v1/pair` default, a 20/s guarded
  default — are a bound on request rate, not a guarantee against every form of
  resource exhaustion: [`internal/httpapi/ratelimit.go:20-65`](../internal/httpapi/ratelimit.go),
  [`internal/config/config.go:58-65`](../internal/config/config.go) (defaults).

---

## Standing accepted risks

**`ca.key` is stored unencrypted at rest.** In the README's own words:

> `ca.key` is stored unencrypted. There is nowhere to keep a passphrase that
> an unattended container restart could reach, so encrypting it would buy
> nothing but ceremony. The consequence is concrete and worth stating plainly:
> anyone who can read that file can mint a client certificate this agent will
> accept. It is protected by file mode `0600` and directory mode `0700` — and
> by nothing else. It is therefore present, in the clear, in every host backup
> and every VPS snapshot of this directory.
>
> — [`README.md:331-340`](../README.md)

The PRD records this as an accepted risk rather than a defect: "CA private key
readable in host backups and VPS snapshots" is Likelihood M, mitigated by
"restrictive file permissions" with encryption revisited "only if a credible
unlocking mechanism exists"
([`.claude/PRPs/prds/devmon-agent.prd.md:184`](../.claude/PRPs/prds/devmon-agent.prd.md)).

**No RBAC — every paired device has the same powers.** Policy is a single
mode fixed for the whole install, not a per-device grant: `requireOp` checks
only `s.cfg.PolicyMode`, the same value for every device
([`internal/httpapi/policygate.go:20-41`](../internal/httpapi/policygate.go)).
The README states the same property from the operator's side: "The mode is
fixed at startup and read once. No client can widen it... Changing the mode
means changing `DEVMON_POLICY_MODE` on the host and restarting the agent."
([`README.md:203-206`](../README.md)). A general operator-defined per-device
permission set is out of scope for this release
([`.claude/PRPs/prds/devmon-agent.prd.md:164`](../.claude/PRPs/prds/devmon-agent.prd.md)).

**Root-equivalent blast radius if the agent is compromised.** From
`SECURITY.md`: "Anything that can drive its API can start, stop, and delete
containers on the host, and — through Docker — reach root-equivalent power
over that host. A vulnerability here is not a 'container management bug'; it
is a host compromise."
([`SECURITY.md:5-8`](../SECURITY.md)). The PRD's Technical Risks table records
the same as the first row: "Agent compromise equals host root compromise
(Docker socket access is root-equivalent)," mitigated by "narrow enumerated
API surface rather than a socket proxy; mTLS with per-device credentials;
audit logging; treat security review as a release gate"
([`.claude/PRPs/prds/devmon-agent.prd.md:178`](../.claude/PRPs/prds/devmon-agent.prd.md)).

---

## Related documents

- [`SECURITY.md`](../SECURITY.md) — how to report a vulnerability, and this
  model's scope restated for reporters.
- [`docs/BACKUP.md`](BACKUP.md) — what to back up, why the backup is itself a
  credential, and how to restore.
- [`README.md`](../README.md) — the full configuration surface, the API
  contract, and the operator-facing explanation of every property cited here.
