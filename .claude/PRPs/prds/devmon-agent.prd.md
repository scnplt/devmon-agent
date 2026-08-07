# DevMon Agent

## Problem Statement

A developer or small-team operator who runs Docker workloads on one or more VPS hosts has no safe way to inspect or control those containers from a phone. When a container crashes at 2am, or a deploy needs a restart while away from a laptop, the only options today are SSH from a mobile terminal (painful, and it puts a full shell on a phone), running a heavyweight management platform purely to gain a mobile client, or exposing the Docker socket over TCP — which hands unauthenticated root-equivalent control of the host to anyone who reaches the port. The cost of leaving this unsolved is measured in incident response time and in operators choosing the insecure shortcut.

## Evidence

- Market observation: existing Android options are almost all **Portainer clients** (Portainer Mobile, Pourtainer, Kontainer, AndroTainer), each requiring a separate Portainer server plus an API token. Managing containers from a phone currently means adopting an entire management platform first.
- Market observation: the open-source `Docker-Manager` Android app exists precisely because SSH-from-phone is the status quo pain — its stated purpose is avoiding SSH into the server.
- Known security anti-pattern: exposing `dockerd` on TCP 2375 (unencrypted) or 2376 is the common DIY route for remote control, and grants root-equivalent host access to whoever can reach the port.
- **Assumption — needs validation via user research:** that operators want a purpose-built lightweight agent rather than accepting Portainer as a dependency. No user interviews have been conducted.

## Proposed Solution

A single small Go binary, distributed as a container image, installed on each Docker host. It is the only component with access to the Docker socket, and it never exposes that socket. It listens on one TLS port and authenticates every client with **mutual TLS**: the agent acts as its own small certificate authority, issuing a unique client certificate to each mobile device during a one-time pairing step. Paired devices call a narrow, explicitly enumerated API — the agent exposes only the operations below, so a compromised client cannot reach arbitrary Docker Engine functionality.

This approach is chosen over (a) exposing the Docker socket, which has no authentication and unlimited blast radius; (b) building a Portainer client, which inherits a large dependency and its threat surface; and (c) an outbound relay/broker architecture, which removes the need to open a firewall port but requires operating relay infrastructure and designing end-to-end encryption across it. For MVP, network reachability — opening a port or running a VPN such as WireGuard/Tailscale — is explicitly the operator's responsibility.

## Key Hypothesis

We believe **a self-hosted, mTLS-authenticated Go agent exposing a narrow Docker control API** will **let operators diagnose and fix container problems from their phone without SSH and without exposing the Docker socket** for **developers and small teams running Docker on their own VPS hosts**.

We'll know we're right when **an operator can complete a full pair → inspect → read logs → restart cycle on a real server from the mobile app, and reports that they would use it instead of SSH during an incident**.

## What We're NOT Building

- **Role-based access control / multi-user permissions** — MVP treats any paired device as fully trusted; adding roles before anyone uses the tool is speculative.
- **A relay or broker service for NAT-bound hosts** — network reachability is the operator's responsibility in MVP. Revisit if home-lab users become a real segment.
- **Container creation, `docker run`, image pull/build, compose/stack deployment** — the requested scope is operating what already exists, not provisioning. Creation is where most of the security blast radius lives.
- **Podman, Kubernetes, or Swarm support** — the requested operation set maps directly onto the Docker Engine API.
- **Metrics, dashboards, alerting, historical retention** — this is a control plane, not a monitoring stack.
- **Persisting the logs of the containers being managed** — Docker's own log driver owns those, and they disappear when a container is removed. Mirroring them into agent storage means continuous ingestion, disk budgeting, and retention policy: a separate product surface. Only the agent's own logs and audit log are persisted.
- **A managed/hosted service** — self-hosted only.
- **The Android application itself** — a separate project; this PRD covers only the server-side agent and the contract it exposes.

## Success Metrics

| Metric | Target | How Measured |
|---|---|---|
| Time from incident notification to corrective action (restart/inspect) | Faster than the SSH-from-phone baseline | Timed dogfooding runs; baseline must be recorded first |
| Successful pairing on first attempt | 100% across the operator's own hosts | Manual installation trials on clean VPS instances |
| Unauthorized request reaches Docker | 0 | Security test: connections without a valid client certificate, and with a revoked one, must be rejected |
| Live-log stream stability | Stream survives ≥30 min and mobile network handover without data loss | Manual endurance test on a real device |
| Pairings surviving a restart and an image upgrade | 100% — no device ever has to re-pair after a normal upgrade | Upgrade rehearsal on a host with multiple paired devices |
| Agent logs available after a crash-restart | 100% of pre-crash entries readable | Kill the agent container mid-operation and inspect the persisted log |
| Operations refused when the host's policy forbids them | 100%, enforced server-side | Send forbidden operations from a client that ignores the advertised policy |
| Destructive operations available on an agent started with no configuration | 0 | Start the agent with nothing set and attempt delete |
| Agent surviving a delete attempt in the most permissive mode | 100% | Attempt deletion of the agent from a fully privileged client |
| Host disk consumed by agent logging at defaults | Bounded and small enough to be safe on a minimal VPS | Run a high-activity workload for 24h and measure the state directory |
| Delay before a host-revoked device loses access | Immediate — no agent restart required | Revoke from the host while the device holds an open session |
| Certificate renewals requiring user interaction | 0 for a device in ordinary use | Run a device across a renewal window with a short test certificate lifetime |
| Agent resource footprint on idle | TBD — needs a target once a baseline is measured | Container memory/CPU measurement |
| Adoption (stars, deployments) | TBD — not meaningful pre-release | GitHub, post-launch |

## Open Questions

- [ ] Do the default log budgets survive contact with a genuinely busy host? The one-day and size limits are a considered starting point rather than a measured one, to be revisited against real usage in a later version.
- [ ] When the operator needs to restart the agent itself, is documented host access an acceptable answer, or does that deserve a dedicated recovery command?

---

## Users & Context

**Primary User**
- **Who**: A developer or solo/small-team operator who self-hosts services on one or more VPS instances with static IPs, is comfortable with Docker and the command line, and is the sole administrator of those hosts.
- **Current behavior**: SSHes into the box from a laptop. From a phone, either uses an awkward mobile SSH client or waits until back at a computer. Some install Portainer solely to get a usable remote UI.
- **Trigger**: A service is down, a deploy misbehaved, or a log needs checking — and the operator is away from their laptop.
- **Success state**: Opened the app, saw the failing container, read the last lines of its log, restarted it, and confirmed it came back healthy — in under a minute, without a shell.

**Job to Be Done**
When **something breaks on my server and I only have my phone**, I want to **see what my containers are doing and restart or stop the one that's broken**, so I can **fix the problem immediately instead of waiting until I'm at a computer or exposing my Docker socket to do it**.

**Non-Users**
- Enterprise platform teams needing SSO, audit compliance, and fine-grained RBAC — the security model here is deliberately single-tenant.
- Kubernetes operators — the primitives are Docker's, not Kubernetes'.
- Users who want to *deploy* applications from their phone — provisioning is out of scope.
- Operators whose hosts sit behind NAT with no VPN and no ability to open a port — unsupported in MVP by design.

---

## Solution Detail

### Core Capabilities (MoSCoW)

| Priority | Capability | Rationale |
|---|---|---|
| Must | Encrypted, mutually authenticated transport (mTLS) | The entire premise is being safer than an exposed Docker socket. Without this there is no product. |
| Must | One-time device pairing that issues a per-device credential | Multiple devices must connect with distinct credentials; pairing must not require hand-editing certificates on the server. |
| Must | Multiple simultaneously paired client devices | Explicit user requirement. |
| Must | Container list and inspect | The entry point for every diagnostic path. |
| Must | Container lifecycle: start, restart, stop, kill, delete | The core corrective actions; the reason the tool exists. |
| Must | Container logs (historical, bounded) | Diagnosis without logs is guesswork. |
| Must | Live log streaming | The user's explicit request and the hardest transport requirement — it constrains the protocol choice, so it cannot be deferred. |
| Must | Image list and inspect | Explicit user requirement. |
| Must | Network list and inspect | Explicit user requirement. |
| Must | Volume list and inspect | Explicit user requirement. |
| Must | Audit log of every mutating operation | Destructive operations are reachable from a phone; there must be a server-side record of what happened and which device did it. |
| Must | Persistent state on a documented host-mounted path | Identity, pairings, and history must survive restarts and image upgrades. Without this the product silently resets itself and every device must re-pair. |
| Must | Persistent agent and audit logs, surviving restarts and crashes | Diagnosing why the agent restarted requires the logs from before it restarted; an audit log that dies with the container is not an audit log. |
| Must | Loud, explicit handling of missing or unreadable state on start | A regenerated identity is indistinguishable from an attack. The agent must say so rather than quietly issuing itself a new one. |
| Must | Log rotation with retention limits the operator sets at container startup | Persistent logs grow without bound and will otherwise fill the host disk — an outage caused by the monitoring tool itself. Hosts differ wildly in available disk, so the operator chooses the budget rather than the agent guessing. |
| Must | Operation policy fixed at container startup, expressed as named modes | A production host and a test host want different powers. Binding the policy to the agent's startup configuration means a compromised or misused phone cannot escalate beyond what the operator granted — the policy can only be changed with host access. Named modes are chosen over per-operation lists because an operator must be able to state a host's posture in one word. |
| Must | Safe default policy: destructive operations denied unless explicitly enabled | The operator who reads no documentation gets the conservative behavior. Anyone who wants to delete containers from a phone has to say so deliberately. |
| Must | Agent advertises its policy and API version to connected clients | The app must disable what the host forbids instead of offering buttons that fail, and must detect an incompatible agent before the user hits an error. |
| Must | Versioned API contract | The Android app ships independently; unversioned drift will break users. |
| Must | Proactive device certificate renewal over the authenticated channel | Renewal must happen while the existing certificate is still valid — once it expires there is no authenticated channel left to renew it on. Silent to the user by design. |
| Must | Unauthenticated status endpoint (informational only) | When a client cannot complete a mutually authenticated handshake, it must still be able to distinguish "my credential expired" from "this server's identity changed", because those need opposite user guidance. It exposes version, CA fingerprint, server time, and policy — never host data, and never credentials. |
| Must | Distribution as a container image with an **automated installer**, covering the state mount, policy mode, and retention limits | Installation is the first thing every user meets and the moment every durable decision is made at once. A hand-assembled command line is where operators will omit the state mount and later lose every pairing. |
| Must | Separate retention budgets for operational logs and the audit log | They are different artifacts: operational logs are high-volume and valuable for hours, the audit log is a few lines per day whose entire value is historical. One shared one-day budget would quietly destroy the security record to make room for debug output. |
| Should | Server certificate re-issuance when the host address changes | A VPS IP change would otherwise force every device to re-pair despite the CA being intact. |
| Must | Rate limiting on the listening port, and especially on the unauthenticated endpoint | The port is internet-facing by design, and an endpoint reachable without a client certificate is a pre-authentication surface. Promoted from Should once that endpoint was accepted. |
| Must | Self-unpair from the app — and nothing beyond self | A device can remove its own pairing: the agent forgets the device, the app forgets the server. Allowed under every policy mode, since giving up your own access is not a privileged act. A device can never act on *another* device: with no roles there is no administrator client, so any such capability would belong equally to a compromised phone. |
| Must | The agent permanently excludes itself from lifecycle operations | Stopping or deleting the agent from the app would destroy the operator's only remote access, and a broken configuration would compound it. This is a fixed rule rather than a configurable one — an operator cannot opt into cutting the branch they are sitting on. The agent stays visible in listings, marked as protected. |
| Must | Host-side command-line device management, runnable inside the container | The recovery path that self-unpair cannot cover: revoking a *lost or stolen* device, which by definition is not in the operator's hands. Also covers listing paired devices and obtaining a pairing code. Requiring host access is the correct authority level — it is the same access that installed the agent. |
| Could | Agent health and version endpoint | Lets the app warn about incompatible or unreachable agents. |
| Could | Container resource stats (CPU/memory snapshot) | Useful context during diagnosis, but not required to act. |
| Won't | Container creation, image pull/build, stack deployment | Deferred — largest blast radius, not needed to validate the hypothesis. |
| Won't | RBAC and multi-user accounts | Deferred — no evidence of team demand yet. |
| Won't | Operator-defined protected container list | Deferred. Policy modes constrain which operations are permitted; a protected list would additionally constrain which containers they may target, letting a permissive host still shield one database. Worth revisiting once real usage shows whether modes alone are too coarse. The agent's own self-exclusion is unaffected and remains a fixed rule. |
| Won't | Outbound notifications for security events | Planned for post-MVP as a pluggable channel — email, Gotify, and similar — alerting the operator when a device pairs, a certificate is renewed, a device is revoked, or state is lost. Deferred because it adds configuration and delivery concerns to a tool whose appeal is being self-contained. **Notifications only — a channel must never carry a pairing code or QR**, which would place host-level credentials in an insecure channel. |
| Won't | Relay/broker for NAT traversal | Deferred — requires operating infrastructure; revisit after MVP feedback. |
| Won't | Podman / Kubernetes / Swarm | Deferred — different primitives, different product. |

### MVP Scope

The minimum that tests the hypothesis: an agent that installs as a container on a VPS, pairs with an Android device once, and from then on serves that device — and any additional paired device — the full read set (containers, images, networks, volumes: list and inspect), container logs including live streaming, and the full container lifecycle set (start, restart, stop, kill, delete), with every mutating call recorded in an audit log. Anything that does not serve the pair → inspect → read logs → act loop is not MVP.

### User Flow

**Critical path — first use:**
1. Operator runs the agent container on the host and opens its port (their responsibility).
2. Agent generates its identity on first start and displays a short-lived pairing code/QR in its console output.
3. Operator adds the server in the Android app and supplies the pairing code.
4. Agent verifies the code, issues that device its own credential, and records it as a paired device.
5. App lists the host's containers.

**Critical path — incident:**
1. Operator opens the app; the paired server's containers are listed with their state.
2. Taps the unhealthy container; sees its details and recent logs.
3. Optionally opens the live log stream to watch behavior in real time.
4. Taps restart; the agent performs it and records the action in the audit log.
5. The container's state updates to running.

---

## Technical Approach

**Feasibility**: HIGH

Every operation requested maps directly onto a well-documented Docker Engine API endpoint, and Go has first-class support for both that API and for TLS with client-certificate verification. There is no novel research risk. The genuinely non-trivial parts are the pairing/credential-issuance flow and keeping a live log stream healthy across mobile network conditions.

**Architecture Notes**
- The agent is the sole holder of Docker socket access, mounted into its container. It never proxies arbitrary Docker API calls — it exposes only the enumerated operations, so the client's reachable surface is a deliberate subset of Docker's.
- Mutual TLS is the authentication mechanism, not a transport detail: the agent acts as a small internal CA, and pairing is the act of issuing a device a certificate. Identity and encryption are therefore the same mechanism.
- Live log streaming requires a long-lived bidirectional channel, unlike the other operations which are request/response. This is the single most protocol-constraining requirement.
- **All durable state lives on a documented host-mounted path** (a bind mount rather than an anonymous volume, so the operator can see, back up, and restore it, and so a routine `docker compose down -v` cannot silently destroy every pairing). This holds the CA key and certificate, the server certificate, the paired-device registry, the revocation list, the audit log, and the agent's own operational logs.
- **Losing that state is loud, never silent.** From the phone's perspective a regenerated CA is indistinguishable from a machine-in-the-middle attack, so the agent must detect missing state on start, refuse to quietly mint a fresh identity as if nothing happened, and tell the operator explicitly that every device must re-pair. Training users to click through certificate-change warnings would dismantle the only protection the pinning model provides.
- **CA private key is stored unencrypted at rest in MVP**, protected by file permissions and host security. Encrypting it would require an unlocking secret that itself has to come from somewhere on the same host, which moves the problem rather than solving it. The consequence — VPS snapshots and backups now contain root-equivalent key material — belongs in the documented threat model.
- **Long-lived CA, expiring device certificates.** The CA outlives individual devices so pairings survive upgrades; device certificates expire so a forgotten device does not retain access forever. A CA whose expiry is simply set far in the future would fail every user simultaneously on one date, so renewal must be designed, not deferred.
- The server certificate is bound to a hostname/IP. If the host's address changes, the agent must be able to re-issue its own server certificate from the retained CA without forcing a re-pair.
- Agent logs and the audit log persist across restarts and crashes — the operator investigating *why* the agent restarted needs the logs from before it did. Both grow without bound and therefore need rotation.
- **The agent's powers are fixed by its startup configuration, not by the client.** Operation policy and retention limits are supplied when the container is created. Changing them requires host access, so a phone — however compromised — can never grant itself more than the operator granted. That this needs a container restart is a feature, not a limitation.
- **The mode is the only configurable limit on what a client may do**, with one fixed exception: the agent itself can never be stopped or deleted through the API. Destroying the agent from the phone would remove the operator's only remote access, and a half-broken configuration would compound it, so this is a rule rather than a setting. The agent stays visible in listings, marked as protected, so the operator sees why its controls are unavailable. A general operator-defined protected list is deliberately deferred.
- **Policy is a small set of named modes**, each a superset of the one before: a read-only posture (list, inspect, logs), a default posture that adds reversible lifecycle control (start, restart, stop), and a full posture that additionally permits the irreversible operations (kill, delete). **The default when nothing is configured is the middle one** — useful out of the box, but incapable of destroying anything. The active mode is advertised to clients so the app can present only what the host permits.
- **Two revocation paths exist because they cover different failures.** From the app, a device can unpair *itself* — the agent forgets it and the app forgets the server. That handles a device the operator still holds. A lost or stolen device cannot be handled that way, so the agent also ships a small command set runnable on the host inside its own container, able to list paired devices, revoke any of them, and issue a pairing code. Host access is the right authority for that, since it is the same access that installed the agent in the first place.
- **The command-line path and the running agent share one state store**, so changes made on the host must take effect in the live agent — a revoked device must lose access immediately, not at the next restart. Correct concurrent access to the state store is therefore a functional requirement, not an implementation detail.
- **Operational logs default to one day, bounded by a size cap as well.** An age limit alone does not protect the disk: a chatty agent on a busy host can exceed a small VPS's free space inside a single day. Whichever limit is reached first triggers rotation, and the operator can raise either. The stated goal is that the agent must never be the reason a small VPS runs out of disk.
- **The audit log is budgeted separately and kept far longer.** It records one line per mutating operation, so its volume is trivial next to operational logging, while its value is entirely in being able to look back. Applying the same one-day window to both would trade the security record away for debug output.
- **Certificate renewal is proactive and silent.** A client renews well before expiry, while its current certificate still authenticates it. Waiting until expiry is a design dead end: the only channel capable of authorizing renewal is the one that just stopped working. The client does not need to ask the agent when its certificate expires — it holds the certificate and can read that locally.
- **One endpoint does not require a client certificate**, so a client that can no longer complete a mutual handshake can still tell *why*. An expired device credential and a changed server identity produce the same connection failure but demand opposite user guidance — one says "get a new pairing code", the other says "this may be an attack". Comparing the CA fingerprint separates them, and including server time lets a client distinguish real expiry from its own clock being wrong. The endpoint is still TLS; only client authentication is waived. Its hard boundary: **it may inform, never issue.** An unauthenticated renewal or pairing path would nullify the entire authentication model.
- An agent that can delete containers can delete itself. Self-management needs a defined behavior.

**Technical Risks**

| Risk | Likelihood | Mitigation |
|---|---|---|
| Agent compromise equals host root compromise (Docker socket access is root-equivalent) | M | Narrow enumerated API surface rather than a socket proxy; mTLS with per-device credentials; audit logging; treat security review as a release gate |
| Pairing flow is the weakest link — a leaked or long-lived pairing code grants permanent host access | M | Short-lived, single-use pairing codes; require console access on the host to obtain one |
| Lost or stolen paired device retains full access until the operator reaches the host | M | Host-side revocation commands; short enough device certificate lifetimes that an unrevoked device eventually ages out; revocation takes effect in the running agent immediately |
| Host-side changes and the running agent disagree about who is paired — a revoked device keeps working | M | Single shared state store with correct concurrent access; revocation verified to take effect without an agent restart |
| Live log streams break on mobile network handover or drain battery | H | Explicit endurance and handover testing; resumable streams; client-side backoff |
| Operator destroys state accidentally (`docker compose down -v`, recreating without the mount) and every device must re-pair | M | Documented bind mount rather than an anonymous volume; install guide states the mount and backup procedure; agent reports loudly on start when state is missing |
| CA private key readable in host backups and VPS snapshots | M | Accepted for MVP and stated in the threat model; restrictive file permissions; revisit encryption-at-rest only if a credible unlocking mechanism exists |
| CA expiry breaks every deployment on the same date | L | Renewal path designed in Phase 2 rather than deferred; long-lived CA paired with shorter-lived device certificates |
| Persistent logs fill the host disk and cause the outage the tool exists to prevent | M | Size and age based rotation from the first release; documented defaults |
| Host IP or hostname changes and invalidates the server certificate | M | Agent re-issues its own server certificate from the retained CA without requiring a re-pair |
| The unauthenticated endpoint becomes an attack or fingerprinting surface — it makes every deployment discoverable by internet-wide scanning | H | Strict allowlist of returned fields (version, CA fingerprint, server time, policy) with no host or container data; aggressive rate limiting; the endpoint can never issue or renew a credential |
| A device that stays offline past its certificate expiry cannot renew and requires host access to recover | M | Renewal window set to a fraction of certificate lifetime so ordinary use always renews in time; expiry period long enough to tolerate a normally-used device being idle; the unauthenticated endpoint explains the situation instead of showing a bare failure |
| Future email notifications leak security-relevant information, or are misused to deliver pairing credentials | M | Notifications only, never credentials; treat as a post-MVP feature with its own review |
| Internet-exposed port attracts automated scanning and abuse | H | Rate limiting; reject non-authenticated connections cheaply; document VPN as the recommended hardening step |
| Android app and agent version drift breaks users | M | Versioned API contract from the first release |

---

## Implementation Phases

<!--
  STATUS: pending | in-progress | complete
  PARALLEL: phases that can run concurrently (e.g., "with 3" or "-")
  DEPENDS: phases that must complete first (e.g., "1, 2" or "-")
  PRP: link to generated plan file once created
-->

| # | Phase | Description | Status | Parallel | Depends | PRP Plan |
|---|-------|-------------|--------|----------|---------|----------|
| 1 | Secure foundation & persistence | Agent runs as a container, serves TLS, talks to the Docker socket, exposes a health/version check, and persists state and its own logs across restarts | complete | - | - | [secure-foundation-and-persistence.plan.md](../plans/completed/secure-foundation-and-persistence.plan.md) · [report](../reports/secure-foundation-and-persistence-report.md) |
| 2 | Identity, pairing & revocation | Agent CA, pairing codes, per-device credentials, device registry, renewal, self-unpair, host-side device commands, missing-state handling | pending | - | 1 | - |
| 3 | Read operations | Container/image/network/volume list and inspect | pending | with 4 | 2 | - |
| 4 | Logs & live streaming | Historical logs plus a resilient live stream channel | pending | with 3 | 2 | - |
| 5 | Lifecycle, policy & audit | start/restart/stop/kill/delete, server-side mode enforcement, agent self-exclusion, audit logging | pending | - | 3 | - |
| 6 | Hardening & OSS release | Rate limiting, security review, automated installer, threat-model docs, AGPL-3.0 release | pending | - | 4, 5 | - |

### Phase Details

**Phase 1: Secure foundation & persistence**
- **Goal**: Prove the agent can run as a container, terminate TLS, reach the Docker Engine, and keep state and logs across restarts.
- **Scope**: Container image and build, the startup configuration surface (state path, log retention limits, operation policy), validation of that configuration with a clear failure on bad input, TLS listener, Docker connectivity, the durable state layout on a host bind mount, persistent agent logging with rotation, and the unauthenticated status endpoint carrying version, server time, and policy.
- **Success signal**: The agent runs on a real VPS and reports its version and policy over TLS without a client certificate; after a crash and restart, logs written before the crash are still readable; retention limits demonstrably bound log growth.

**Phase 2: Identity, pairing & revocation**
- **Goal**: A device can be paired once and thereafter authenticate on its own credential; several devices can be paired independently; identity survives upgrades; access can be withdrawn both by the device itself and by the operator on the host.
- **Scope**: Internal CA, short-lived single-use pairing codes, credential issuance, paired-device registry, certificate lifetimes, proactive silent renewal over the authenticated channel, self-unpair from the client, host-side commands to list/revoke devices and issue pairing codes, explicit handling of missing or unreadable state, server certificate re-issuance on host address change, and extending the status endpoint with the CA fingerprint so a client can diagnose a failed handshake.
- **Success signal**: Two separate clients pair and connect with distinct credentials; an unpaired client is rejected; both survive an agent restart *and* an image upgrade; a client whose certificate is near expiry renews without any user interaction; a device revoked from the host loses access immediately, without restarting the agent; a client holding an expired certificate and a client facing a regenerated CA can tell those two cases apart from the status endpoint alone; starting with the state directory removed produces an explicit operator-facing error rather than a silently regenerated identity.

**Phase 3: Read operations**
- **Goal**: Full read visibility into the host's Docker objects.
- **Scope**: List and inspect for containers, images, networks, and volumes.
- **Success signal**: A paired client retrieves accurate data for all four object types on a host with real workloads.

**Phase 4: Logs & live streaming**
- **Goal**: Diagnose from a phone without a shell.
- **Scope**: Bounded historical log retrieval and a live streaming channel.
- **Success signal**: A live stream runs for 30+ minutes across a network handover without losing data or the connection.

**Phase 5: Lifecycle, policy & audit**
- **Goal**: Act on the problem, with a record of who did what.
- **Scope**: start, restart, stop, kill, delete; server-side enforcement of the operation mode and of the agent's permanent self-exclusion; audit log entry per mutating call, attributed to the calling device, including calls refused by policy.
- **Success signal**: Every permitted lifecycle operation succeeds against a real container and produces a correct, attributed audit entry; an agent started with a restrictive mode refuses forbidden operations regardless of what the client sends; the agent itself resists deletion even in the most permissive mode; the client is told what is available rather than discovering it through failures.

**Phase 6: Hardening & OSS release**
- **Goal**: Safe to point at the public internet and to publish.
- **Scope**: Rate limiting, security review against the risk table, the automated installer covering state mount / policy mode / retention, threat-model and backup documentation, AGPL-3.0 licensing and release.
- **Success signal**: Security review passes with no unmitigated high-severity finding; a third party goes from nothing to a paired device using the installer alone, without hand-writing a `docker run` line.

### Parallelism Notes

Phases 1 and 2 are strictly sequential — everything else depends on an authenticated channel existing. Once pairing works, **Phase 3 (read operations) and Phase 4 (logs/streaming) are independent**: they share no logic, since read operations are request/response and log streaming is a long-lived channel, so they can be built concurrently. Phase 5 depends on Phase 3 because acting on a container presupposes being able to identify and inspect it. Phase 6 is the release gate and depends on the full surface being complete.

---

## Decisions Log

| Decision | Choice | Alternatives | Rationale |
|---|---|---|---|
| Connection model | Direct inbound, mTLS | Outbound relay/broker; hybrid | Simplest and lowest-latency; no infrastructure to operate. Port exposure/VPN is explicitly the operator's responsibility for MVP. |
| Authentication | Per-device credentials issued at pairing | Shared API token; OIDC | Per-device credentials make revocation and audit attribution possible; OIDC is a heavy dependency for a self-hosted single-operator tool. |
| Access control | None beyond "paired" in MVP | RBAC from day one | No evidence of team demand yet; adding roles before users exist is speculative. |
| Topology | One agent per Docker host; app manages many hosts | One agent fronting multiple remote hosts | Keeps the agent simple and its blast radius contained to a single host. |
| Docker surface | Narrow enumerated operations | Generic Docker API proxy | A proxy would re-create the exposed-socket problem the product exists to solve. |
| Runtime target | Docker Engine only | Podman, Kubernetes, Swarm | The requested operation set is Docker's; other runtimes are a different product. |
| Provisioning | Excluded from scope | Include create/run/pull/deploy | Largest security blast radius; unnecessary to validate the hypothesis. |
| State storage | Documented host bind mount | Anonymous/named volume; container-local (ephemeral) | Visible to the operator, straightforward to back up and restore, and not destroyed by a routine `down -v`. Container-local storage would reset every pairing on upgrade. |
| CA key at rest | Unencrypted, permission-protected, disclosed in the threat model | Encrypt at rest | Any unlocking secret would have to live on the same host, relocating the problem rather than solving it. Honest disclosure beats security theater. |
| Certificate lifetimes | Long-lived CA, expiring device certificates | Long-lived everything; short-lived CA | Pairings survive upgrades while forgotten devices age out. A far-future CA expiry would fail every deployment at once. |
| Behavior on missing state | Refuse to silently regenerate; report explicitly and require re-pairing | Auto-generate a new identity and continue | Silent regeneration presents users with a signal identical to a machine-in-the-middle attack, and teaching them to dismiss it destroys the pinning model. |
| Log persistence | Agent's own logs and audit log on the state mount, with rotation | Container-local logs; ship to an external system | An agent that loses its logs when it crashes cannot explain why it crashed. External log shipping is a dependency this tool should not require. |
| Managed containers' logs | Not persisted by the agent | Mirror into agent storage | Docker's log driver owns them; mirroring is a separate product surface with its own disk and retention problems. |
| Log retention limits | Operator-supplied at startup; operational logs default to one day plus a size cap; the audit log gets its own, longer budget | Fixed defaults; client-configurable; one shared budget; age limit only | An age limit alone does not bound disk use on a busy host, so both apply and the first reached wins — the agent must never fill a small VPS. The audit log is separated because it is small and its value is historical. Startup configuration keeps all of it out of reach of a compromised client. |
| Destructive operation policy | Named modes supplied at container startup; **destructive operations denied by default** | Per-operation allow lists; always allowed; client-side confirmation only | An operator must be able to describe a host's posture in one word, and the operator who configures nothing must get the safe outcome. Client-side confirmation protects against slips, not against a compromised client; a server-side policy only host access can change is the real boundary. |
| Device revocation | Self-unpair from the app, plus host-side commands for revoking any device | App-driven revocation of other devices; host-only revocation | Self-unpair covers devices the operator still holds; only a host-side path can revoke a device that has been lost or stolen. With no roles there is no administrator client, so cross-device control would belong equally to a compromised phone. |
| Protected containers | Only the agent's own permanent self-exclusion; no configurable list in MVP | Operator-defined protected list; no protection at all | Modes cover the common case, and every additional startup setting is surface the operator must understand at install time. The agent's own exclusion stays non-configurable, because opting into it would mean opting into losing remote access entirely. Revisit if modes prove too coarse in practice. |
| Notification channels | None in MVP; a pluggable channel later (email, Gotify, and similar) | Email-only later; in MVP | Self-hosters already run their own notification services; committing to a single transport would fit fewer of them than a pluggable one. Not needed to validate the hypothesis. |
| Installation | Automated installer script | Documented manual `docker run`/compose only | Installation is where the state mount, policy mode, and retention limits are all decided at once; leaving that to hand-assembled command lines is where operators will lose their pairings. |
| Certificate renewal | Proactive and silent, over the authenticated channel | Renew on expiry; operator-driven renewal | Once a certificate expires there is no authenticated channel left to authorize its replacement. Renewing early is the only design that does not strand the user. |
| Detecting identity change from the client | One TLS endpoint that waives *client* authentication, returning version, CA fingerprint, server time, and policy | No endpoint (client cannot diagnose); an authenticated-only endpoint (unusable in exactly the failure case it is needed for) | Expired credential and changed server identity are indistinguishable from a bare handshake failure yet require opposite user guidance. Accepted cost: a pre-authentication surface, mitigated by a strict field allowlist and rate limiting, and forbidden from issuing anything. |
| API versioning | Versioned from the first release | Version when it first breaks | The mobile app and the agent are released independently; retrofitting a version negotiation after users are running mismatched pairs is far more expensive. |
| Operator notifications | Post-MVP, email, notification-only | In MVP; carrying pairing QR codes by email | Out-of-band alerting for "a new device paired" is genuinely valuable. Emailing a pairing code would put root-equivalent host access into a channel that is stored in plaintext, forwardable, and breachable. |
| License | **AGPL-3.0-only** | MIT/Apache-2.0; GPL-3.0 | Modifications must credit this project and remain open source. Permissive licenses do not require reciprocity. Plain GPL-3.0 does not reliably trigger on network-only use — and this software is a network daemon, the exact case AGPL's §13 was written for. GPL-family notice-preservation covers the attribution requirement. |

---

## Research Summary

**Market Context**

The Android market for Docker management is dominated by **Portainer clients** — Portainer Mobile (official, updated May 2026), Pourtainer, Kontainer, and AndroTainer are all front-ends that require a running Portainer server plus an API access token. They solve the UI problem by inheriting an entire management platform. Separately, open-source apps such as `Docker-Manager` exist explicitly to avoid SSHing into the server from a phone, confirming that the underlying pain is real and currently unserved by a lightweight option. The common DIY alternative — exposing `dockerd` over TCP — is a well-known security anti-pattern that grants root-equivalent host access to anyone who can reach the port.

The gap this product targets: a purpose-built, minimal, self-contained agent that carries its own authentication and requires no third-party management platform.

Sources:
- [Portainer Mobile (Google Play)](https://play.google.com/store/apps/details?id=com.umegs.portainer_mobile)
- [Pourtainer (AppBrain)](https://www.appbrain.com/app/portainer-docker-pourtainer/com.pourtainer.mobile)
- [Kontainer / Portainer Mobile: Docker Mgt (Google Play)](https://play.google.com/store/apps/details?id=com.devculi.kontainer)
- [AndroTainer (GitHub)](https://github.com/dokeraj/AndroTainer)
- [Docker-Manager (GitHub)](https://github.com/theSoberSobber/Docker-Manager)

**Technical Context**

No codebase exists — this is a greenfield Go project. Every requested operation corresponds to a documented Docker Engine API endpoint, and Go's standard library supports TLS with client-certificate verification directly, so feasibility is high and the research risk is low. The concentration of difficulty is in two places: the pairing and credential-issuance flow, and keeping live log streams healthy over mobile networks. The third — durable state — has been resolved into decisions rather than left open: a documented host bind mount holding the CA, device registry, audit log, and the agent's own rotated logs, with missing state treated as an explicit operator-facing failure rather than a silent re-identification.

---

*Generated: 2026-08-07*
*Status: DRAFT - needs validation*
