# Security Policy

## What this agent is

devmon-agent holds a Docker socket. Anything that can drive its API can start,
stop, and delete containers on the host, and — through Docker — reach
root-equivalent power over that host. A vulnerability here is not a "container
management bug"; it is a host compromise. Reports are treated accordingly.

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x | Yes |
| < 0.1.0 | No — pre-release, never published |

Only the latest published minor version receives fixes. There is no long-term
support branch.

## Reporting a vulnerability

**Do not open a public issue.** A public report on this project is a
disclosure of a live path to someone's host.

Report privately through GitHub Security Advisories:

1. Go to the [Security tab](https://github.com/scnplt/devmon-agent/security/advisories)
2. Choose **Report a vulnerability**

That channel stays private between you and the maintainers until an advisory
is published.

### What to include

- The version or commit you tested
- The Docker Engine version and host OS
- What an attacker gains, and what access they need to start with — a finding
  that requires host root is a different thing from one reachable from the
  open port
- Reproduction steps, ideally as requests rather than prose

### What never to include

Never paste any of the following into a report, an issue, or a log excerpt:

- Anything from `certs/` — `ca.key` is the agent's entire identity
- `devmon.db` — it holds the device registry
- A pairing code, even an expired one
- PEM blocks of any kind

Redact them. A report is readable without them.

## Response expectations

This is a small project maintained in spare time. Expect:

- **Acknowledgement**: within 7 days
- **Initial assessment**: within 14 days
- **A fix, or a written acceptance**: depends on severity. A finding reachable
  from the listening port without a client certificate is the highest
  priority there is here.

Some risks are **accepted rather than fixed**, and are written down as such in
[docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) — the unencrypted CA key at rest
is the standing example. If your report matches an already-accepted risk, you
will get that document and the reasoning, not silence.

## Scope

**In scope**: the listening port and everything reachable from it, the mTLS
and pairing flow, the policy gate, self-exclusion, the audit record, the state
directory's contents and permissions, `install.sh`, and the published image.

**Out of scope** — these are documented non-defences, not bugs:

- An attacker who already has root on the host
- A compromised or malicious Docker Engine
- An operator who exposes the port to the internet with no VPN or firewall in
  front of it, contrary to the README
- Denial of service beyond what the rate limiter is documented to bound
- Side-channel and timing attacks

See [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) for the full model.
