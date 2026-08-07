# devmon-agent

Go agent that exposes a narrow, mTLS-authenticated Docker control API so an Android
client can inspect and restart containers without SSH and without exposing the Docker socket.

- Module: `github.com/scnplt/devmon-agent`
- Go: 1.26
- License: AGPL-3.0-only

## Commands

Nothing is implemented yet — `go.mod` is created by Task 1 of the current plan, so these
commands only become runnable after that.

```bash
make build                                          # -> bin/devmon-agent
go build ./...

go test ./internal/... -race                        # tests (always -race)
go test ./internal/... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1          # floor is 80%

gofmt -l .                                          # must print nothing
go vet ./...
golangci-lint run ./...
gosec ./...                                         # must be clean

docker build -t devmon-agent:dev .
```

There is no dev server. The agent runs as a container; see `compose.example.yaml` once
Task 11 lands.

## Repo-specific notes

**Docker SDK is `github.com/moby/moby/client`, not `github.com/docker/docker/client`.**
The SDK split at Engine v29 and `docker/docker` is deprecated. The v29 API differs from
the widely-known one: the constructor is `client.New(opts...)` (not `NewClientWithOpts`),
every method takes an options struct, and `Ping` takes **two** arguments —
`Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})`. Code written from memory will
use the pre-v29 forms and will not compile.

**Build with `CGO_ENABLED=0`.** `modernc.org/sqlite` is pure Go, which keeps the binary
static so it runs on `distroless/static`. Its `database/sql` driver name is `"sqlite"`,
not `"sqlite3"`.

**Startup configuration is the security boundary.** Every knob is a `DEVMON_*` env var read
once at start and immutable thereafter — a client can never widen what the operator granted.
Never add a client-facing way to change policy mode or retention.

**Never log key material, pairing codes, or PEM bytes**, at any level.

## Planning docs

- PRD: `.claude/PRPs/prds/devmon-agent.prd.md` — scope, decisions log, phase table
- Plans: `.claude/PRPs/plans/*.plan.md` — one per phase; the plan is the implementation contract

Read the current phase's plan before writing code. It carries verified upstream API
signatures and the gotchas above with full rationale.
