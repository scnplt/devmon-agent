# devmon-agent

Go agent that exposes a narrow, mTLS-authenticated Docker control API so a paired
client can inspect and restart containers without SSH and without exposing the Docker socket.

- Module: `github.com/scnplt/devmon-agent`
- Go: 1.26
- License: AGPL-3.0-only

## Language (MANDATORY)

**All written artifacts in this repository are English-only**, regardless of the language
used in chat. This is non-negotiable and applies to:

- every `.md` file (README, rules, ADRs, docs)
- code comments and doc comments
- identifiers, log messages, and error strings
- commit messages, branch names, PR titles and bodies, issue text

If the user writes in another language, still produce these artifacts in English.

## Model routing (MANDATORY)

Planning runs on Opus, implementation runs on Sonnet. The session model stays Opus so the
main loop can orchestrate and review; it delegates rather than doing the work itself.

| Work | Who | Model |
|------|-----|-------|
| Task breakdown, architecture, design decisions | main session | Opus |
| Production Go code and its tests (`cmd/`, `internal/`, build files) | `go-implementer` agent — one task per invocation | Sonnet |
| Review of written code | `ecc:go-reviewer`, `ecc:security-reviewer` | Sonnet |

Rules:

- **The main session does not write production Go code.** Break the work into tasks,
  delegate each one to `go-implementer`, and verify its reported gate output. Independent
  tasks go out in parallel.
- Exception: a one-line mechanical fix the main session is already holding in context
  (a typo, a rename) may be edited directly rather than paying a delegation round trip.
- Docs, config, and `.claude/**` files are main-session work.
- In a `Workflow` script, set the tier explicitly per stage:
  `agent(prompt, {model: 'opus'})` for planning stages, `{model: 'sonnet'}` for
  implementation stages — a workflow agent otherwise inherits the Opus session model.
- **If you delegate, you own collection.** Never end a turn while a spawned agent is still
  running: a completed child cannot notify a parent whose turn has ended, so its result is
  lost. Wait for it, verify its gate output, then report.

Only one agent exists in this repository — `go-implementer` (`.claude/agents/`).
Reviewers are invoked as `ecc:go-reviewer` and `ecc:security-reviewer`. Any other agent
name is a mistake.

## Branching

`main` = production/release, `dev` = integration, every feature on its own
`<type>/<slug>` branch cut from `dev`. Never commit directly to `main` or `dev`.
Full model: `.claude/rules/ecc/common/git-workflow.md`.

## Commit cadence (MANDATORY)

**One commit per delegated task, not one commit per feature.** After a task's gates pass, the
main session commits that task before starting the next one. Never batch several completed
tasks into a single commit.

- The commit lands as soon as the task is verified — `go-implementer` still never touches git
  (see `.claude/agents/go-implementer.md`); the main session owns every commit.
- Subject line: `<type>: <what the task delivered>`, e.g. `feat: add container lifecycle calls
  with self-exclusion`.
- Tasks dispatched in parallel are committed one at a time as each returns, in the order they
  finish.
- Docs and config changes that round out a feature may share a final commit.

Rationale: a feature branch can run long enough to exhaust a usage window mid-flight.
Task-sized commits mean interrupted work resumes from the last verified task instead of losing
everything since the branch was cut, and each commit is small enough to review on its own.

## Commands

```bash
make build                                          # -> bin/devmon-agent
go build ./...

go test ./internal/... -race                        # tests (always -race)
go test ./internal/... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1          # floor is 90%

gofmt -l .                                          # must print nothing
go vet ./...
golangci-lint run ./...
gosec ./...                                         # must be clean
make shellcheck                                     # shellcheck -s sh install.sh

docker build -t devmon-agent:dev .
```

There is no dev server. The agent runs as a container: `./install.sh` sets one up from
scratch, and `compose.example.yaml` is the by-hand reference.

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

## Review artifacts (MANDATORY)

**Reviews are never committed.** A review — code review, security review, PR review — is
delivered where it is read: in the session for local work, or as a comment on the PR it
covers. It does not become a file in the repository.

- Do not create a committed review write-up anywhere in the tree. If a scratch copy helps,
  write it to the session scratchpad instead.
- A request to review means "perform it and report the verdict", never "produce a review
  document to commit".

## Rules files

`CLAUDE.md` is the single authority on model routing, delegation, workflow, commit cadence,
and the gate list. `.claude/rules/ecc/**` is trimmed to what this repository actually uses
and covers **style and git only**:

| File | Scope |
|------|-------|
| `common/coding-style.md` | KISS/DRY/YAGNI, file and function size, error handling |
| `common/git-workflow.md` | Branching model, commit format, PR workflow |
| `golang/coding-style.md` | Go formatting, interfaces, error wrapping, naming |

Nothing under `.claude/rules/` may route work to an agent or pick a model. If a generic
rule file reappears from an upstream ECC install and contradicts this file, this file wins
— delete the contradiction rather than annotating it.
