# Development

How to build, test, and gate a change locally, what the end-to-end suite does,
and what CI runs. The contribution process — branching, commits, the full gate
list — is in [`CONTRIBUTING.md`](../CONTRIBUTING.md).

## Make targets

```bash
make build          # -> bin/devmon-agent
make test           # unit tests
make test-race      # with -race (needs a C toolchain)
make cover          # prints total coverage over ./internal/...; CI enforces the 90% floor
make lint           # gofmt + go vet + golangci-lint when installed
make sec            # gosec
make vuln           # govulncheck, pinned to the same version CI uses
make shellcheck     # shellcheck -s sh install.sh scripts/check-doc-citations.sh
make doc-citations  # every citation in docs/*.md still resolves
make openapi-lint   # redocly lint docs/openapi.yaml (needs npx)
make image          # docker build
make clean
```

`-race` needs `CGO_ENABLED=1`, which `test-race` and `cover` set for
themselves; the shipped binary is still built with `CGO_ENABLED=0`.

## End-to-end suite

`internal/e2e/` runs the real agent against a real Docker Engine, with no phone
and no emulator anywhere in the loop. It is the executable definition of the API
contract: assertions are written against the wire — status codes, headers, and
JSON decoded into `map[string]any` — never against the agent's own structs, so a
renamed JSON tag fails the suite instead of silently breaking a client.

Two groups:

- **`internal/e2e/api`** — builds the real binary, starts it as a host process,
  pairs through the documented `devmon-agent device pair-code` path, and drives
  every route over pinned mTLS.
- **`internal/e2e/incontainer`** — builds the image and runs the agent as a
  container, which is the only way to exercise self-identification through
  `/proc/self/mountinfo` and the self-exclusion guarantee.

```bash
make e2e             # both groups, ~5-10 min against a local Engine
make e2e-container   # the in-container group alone
make e2e-endurance   # the 30-minute stream and the retention budget
make e2e-health      # TestContainerListReportsHealth alone, against an Engine 29+ endpoint
make e2e-lint        # go vet -tags e2e, plus golangci-lint when installed
make e2e-clean       # remove containers a crashed run left behind
```

Every container the suite creates is removed by the cleanup that created it,
addressed by ID — so two runs on one host never disturb each other. `e2e-clean`
exists for the case where a run died hard enough to skip its own cleanups, and
is deliberately manual: it matches on the shared `com.devmon.e2e` label, which
cannot distinguish a dead run's leftovers from a live run's containers. Run it
only when no e2e run is in flight.

Every file carries `//go:build e2e`, so nothing here compiles into `make build`,
`make test`, `make lint`, or `make cover`, and the suite adds no module
dependency — `go.mod` is unchanged by it.

Four environment variables tune the harness. **They are not agent
configuration**: the harness reads them and never passes them to the agent,
whose own environment is built explicitly from each test case.

| Variable | Effect |
|---|---|
| `DEVMON_E2E_REQUIRE=1` | An unreachable Engine becomes a hard failure instead of a skip. CI sets it. |
| `DEVMON_E2E_ENDURANCE=1` | Runs the 30-minute stream and the retention test, which otherwise skip. |
| `DEVMON_E2E_DOCKER_HOST` | Engine endpoint for the suite, when it is not the default socket. |
| `DEVMON_E2E_KEEP=1` | Keeps fixture containers and state directories after a failure, and prints their paths. |

Without an Engine, every test **skips** with a reason naming the endpoint —
visibly, in `go test` output — rather than passing quietly.

Every `make e2e*` target runs with `-v`, and that is load-bearing rather than
cosmetic: `go test` prints a skip and its reason **only** under `-v`. Without
it, a package whose tests all skipped reports a bare `ok`, indistinguishable
from one that ran and passed them — a green run would silently imply coverage
it does not have. The output is long; the alternative is a log that cannot tell
you which of the reasons below fired.

`TestContainerListReportsHealth` additionally needs Engine 29+ / API v1.52: the
`ContainerSummary.Health` field the list projection reads was only added at
that version, so on an older Engine (even a reachable, healthy one) the test
skips with that reason instead of failing, and `DEVMON_E2E_REQUIRE=1` does not
override that skip — it is a genuine Engine capability floor, not a missing
Engine. `make e2e-health` runs that one test alone so it can be pointed at an
Engine 29 endpoint.

**On Windows, run these from a WSL2 shell.** The agent accepts only `unix://`
and `tcp://` Docker endpoints, so Docker Desktop's default
`npipe:////./pipe/docker_engine` cannot be given to it at all, and the
in-container group depends on Linux bind-mount ownership semantics. A
Windows-native run skips with exactly that explanation.

## Continuous integration

`.github/workflows/ci.yml` runs the same gates on GitHub Actions, scaled to the
branch a pull request targets:

| Job | Runs on | What it does |
|---|---|---|
| `test` | every PR, and pushes to `main` | `go build ./...`, the doc-citation check, race tests over `./internal/...` with the 90% coverage floor, and race tests over `./cmd/...` |
| `lint` | PRs into `main`, and pushes to `main` | `gofmt`, `go vet`, `golangci-lint` |
| `image` | PRs into `main`, and pushes to `main` | `docker build` of the release image |
| `shellcheck` | PRs into `main`, and pushes to `main` | `shellcheck -s sh install.sh` |
| `gosec` | PRs into `main`, and pushes to `main` | `gosec ./...` |
| `openapi` | PRs into `main`, and pushes to `main` | `redocly lint docs/openapi.yaml` |
| `govulncheck` | PRs into `main`, and pushes to `main` | `govulncheck ./...` — known vulnerabilities in the dependencies and the Go toolchain, which `gosec` does not look for |
| `e2e` | PRs into `main`, and pushes to `main` | `make e2e` against the runner's Docker Engine, with `DEVMON_E2E_REQUIRE=1`, plus `make e2e-lint` |
| `e2e-health` | PRs into `main`, and pushes to `main` | `make e2e-health` against an Engine 29 docker-in-docker service, the one assertion the runner's own Engine is too old to make |

`dev` is the integration branch, so a PR into it gets fast feedback from `test`
alone; the full release bar applies on the way into `main`. The eight
`main`-only jobs are gated on `github.base_ref == 'main'`, which GitHub
populates for `pull_request` events only, **or** `github.ref ==
'refs/heads/main'`, which covers the push of the merge commit. Neither is true
on a `dev` PR, so they are skipped there — not queued. The second half of the
gate is what makes `main`'s HEAD carry its own release-bar result: `main`'s
ruleset is not strict, so the merge commit can differ from the tree the PR
tested, and without it that commit would only ever have been checked by
`test`.

Toolchain versions come from `go.mod`, and the linter and scanner versions are
pinned in the workflow's `env` block — bump them there, deliberately, rather
than tracking `latest`.

## Things that bite

Two things that will bite anyone writing code here from memory:

- **The Docker SDK is `github.com/moby/moby/client`**, not
  `github.com/docker/docker/client`. The SDK split at Engine v29 and the old
  module is deprecated. The constructor is `client.New(opts...)`, every method
  takes an options struct, and `Ping` takes **two** arguments. The pre-v29 forms
  will not compile.
- **Build with `CGO_ENABLED=0`.** `modernc.org/sqlite` is pure Go, which keeps
  the binary static so it runs on `distroless/static`. Its `database/sql` driver
  name is `"sqlite"`, not `"sqlite3"`.

Never log key material, pairing codes, or PEM bytes, at any level.
[`CONTRIBUTING.md`](../CONTRIBUTING.md) lists the rest of what will be
declined on principle.
