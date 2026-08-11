# Contributing

Thanks for looking. This is a security-sensitive project — the agent holds a
Docker socket — so the bar for merging is higher than the size of the codebase
suggests. Most of what follows is about making that bar cheap to clear.

## Before you write code

**Security issues do not go here.** See [SECURITY.md](SECURITY.md) and report
privately.

For anything else, open an issue first if the change is more than a fix. The
project follows a written PRD and phase plans (`.claude/PRPs/`), and a feature
that is deliberately out of scope is usually recorded there with the reasoning
— it saves you writing something that will be declined on principle.

## Language

**Everything in this repository is English.** Code comments, doc comments,
identifiers, log and error messages, markdown, commit messages, branch names,
PR titles and bodies, issue text. This holds regardless of the language a
discussion happens in.

## Branching

| Branch | Role |
|---|---|
| `main` | Production / release. Tagged `vX.Y.Z`. |
| `dev` | Integration. The default base for pull requests. |
| `<type>/<slug>` | One feature or fix. Short-lived, deleted after merge. |

Never commit directly to `main` or `dev`. Cut from `dev`, return to `dev`:

```bash
git checkout dev && git pull
git checkout -b feat/my-change
# ... commits ...
git push -u origin feat/my-change
gh pr create --base dev
```

Branch prefixes reuse the commit types: `feat/`, `fix/`, `refactor/`, `docs/`,
`test/`, `chore/`, `perf/`, `ci/`.

## Commits

```
<type>: <description>

<optional body explaining why, not what>
```

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.

Keep commits small enough to review on their own. The body is for the reason a
change is correct — the diff already says what changed.

## Gates

Every one of these must pass before a pull request is ready. CI runs them
again, on Linux.

```bash
gofmt -l .                                   # must print nothing
go vet ./...
go vet -tags e2e ./...
go build ./...
golangci-lint run ./...
gosec ./...                                  # must be clean
make vuln                                    # govulncheck; must report none

go test ./internal/... -race                 # always -race
go test ./internal/... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1   # floor is 80%

shellcheck -s sh install.sh
```

The end-to-end suite runs against a real Docker Engine and is not part of the
default `go test` run:

```bash
make e2e
make e2e-lint
```

On Windows, run anything that touches a Docker Engine from WSL2.

## Tests

Tests come first. Table-driven, `-race` clean, and named for the behaviour
under test rather than the function:

```go
func TestLoadRejectsZeroRateGuardedPerSec(t *testing.T) { ... }
```

Coverage over `./internal/...` must stay at or above 80%. The e2e suite does
not count toward that number — it asserts the wire contract, deliberately
sharing no production code with what it tests.

## Two things that catch everyone

**1. The Docker SDK is `github.com/moby/moby/client`, not
`github.com/docker/docker/client`.** The SDK split at Engine v29 and
`docker/docker` is deprecated. The v29 API differs from the one most examples
show: the constructor is `client.New(opts...)` (not `NewClientWithOpts`),
every method takes an options struct, and `Ping` takes **two** arguments:

```go
c.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
```

Code written from memory will use the pre-v29 forms and will not compile.

**2. Build with `CGO_ENABLED=0`.** `modernc.org/sqlite` is pure Go, which is
what keeps the binary static so it runs on `distroless/static`. Its
`database/sql` driver name is `"sqlite"`, not `"sqlite3"`.

## Things that will be declined

- **Any client-facing way to change policy mode, retention, or a rate limit.**
  Startup configuration is the security boundary: a client can never widen
  what the operator granted. This is not negotiable.
- **Logging key material, pairing codes, or PEM bytes**, at any level, for any
  reason.
- **Trusting `X-Forwarded-For`.** The documented deployment is direct inbound.
  Honouring a client-supplied forwarding header would let any caller mint a
  fresh rate-limiter key per request — worse than no limiter, because it looks
  like protection.
- **Widening `/v1/status`.** Its field allowlist is a security boundary: that
  endpoint answers without a client certificate.

## License

By contributing you agree your work is licensed under
[AGPL-3.0-only](LICENSE), the same as the rest of the project. Every `.go`
file carries `// SPDX-License-Identifier: AGPL-3.0-only` as its first line,
followed by a blank line — new files included.
