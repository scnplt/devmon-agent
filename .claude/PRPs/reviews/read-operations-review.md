# Code Review: Read Operations (Phase 3)

**Reviewed**: 2026-08-08
**Branch**: `feat/read-operations` → `dev` (uncommitted working tree)
**Reviewers**: `ecc:security-reviewer`, `ecc:go-reviewer`, plus main-session verification of every claim
**Decision**: **APPROVE with comments** — no CRITICAL or HIGH findings

## Summary

The phase's stated security weight — D1/D2, that no secret ever leaves the agent — is enforced
*structurally* rather than by convention: the DTOs have no field an env var or volume driver
option could travel through, and that is proven by tests asserting on marshalled JSON rather
than on struct fields. Reference validation runs before every SDK call. Middleware ordering is
correct on all eight routes. Two MEDIUM findings were confirmed and fixed during review; the
rest are notes.

## Findings

### CRITICAL
None.

### HIGH
None.

### MEDIUM

**M1 — Unguarded zero timestamp produced a false date. FIXED.**
`internal/dockerx/containers.go` (`toContainerSummary`) and `images.go` (`toImageSummary`)
called `time.Unix(s.Created, 0).UTC().Format(time.RFC3339)` with no zero guard, so an Engine
value of `0` emitted `"1970-01-01T00:00:00Z"` — a plausible-looking but false timestamp the
Android client would render as a real date. The file already had the right pattern for this
class of problem (`zeroTimeToEmpty`).
*Fix*: added `unixToRFC3339(sec int64) string` returning `""` for `0`; used in both mappers.
Tests added for the zero case in both.

**M2 — Duplicated truncation and labels-default logic. FIXED.**
The four-line truncation block was copy-pasted verbatim across all four list methods, and the
`if labels == nil { labels = map[string]string{} }` pattern appeared five times. Confirmed by
inspection, not assumed.
*Fix*: extracted `truncate[T any](items []T) ([]T, bool)` and `defaultLabels(...)` into a new
`internal/dockerx/list.go`, along with the `maxListItems` const. `callTimeout` deliberately
stayed in `containers.go` — it governs inspect calls too, so filing it under listing helpers
would have been misleading. Behaviour is identical; the boundary is still
`len(items) > maxListItems`.

**M3 — Truncation tests re-implement production logic. NOT FIXED (accepted).**
Four tests assert truncation against a hand-copied `if len(items) > maxListItems` over a
synthetic slice rather than calling the real method. If production truncation diverged, none
would catch it. Left in place because `TestEngineListTruncation` covers the real call path
through a fake Engine with 500/501 fixtures, so the property *is* genuinely tested; the
synthetic tests are redundant rather than wrong. Worth deleting in a future cleanup.

**M4 — Request log now records caller-supplied references. NOT FIXED (needs a decision).**
`internal/httpapi/middleware.go` `withRequestLog` logs `r.URL.Path` at Debug. Its doc comment
promises "method, path, status, and duration only", and the plan's own manual-validation
checklist asserts `agent.log` carries "no container names, no reference values". Phase 3 adds
routes with path parameters (`/v1/containers/{id}`, `/v1/volumes/{name}`), so the logged path
now contains whatever reference the caller sent. The middleware is pre-existing Phase 2 code
and was not modified here — this phase changed what it *means*.

Not changed, because container IDs and volume names are not secrets, this is Debug-only, and
redacting the path segment would meaningfully degrade the log's diagnostic value. But the
plan's checklist line is now inaccurate and someone should decide deliberately: either accept
it and correct the expectation, or template the path in the log. Flagged rather than silently
resolved.

**Confirmed empirically during E2E validation.** The agent's Debug log carries entries like
`path=/v1/containers/cb6831df964c…` — the full reference. It carries no secrets: across the
entire run there were zero hits for env values, private key material, or pairing codes. So the
project's "never log key material" rule holds; only the checklist's stricter "no reference
values" wording is contradicted. Still a decision, not a defect.

### LOW

**L1** — `toContainerDetail` is 58 lines, over the project's 50-line guideline. Flat, not
nested; each guard block reads as part of one cohesive unit. Splitting it to hit the number
would hurt readability. Left as-is.

**L2 (INFO, no action)** — `ValidateRef` excludes `:`, so `GET /v1/images/{id}` cannot inspect
an image by digest. The `go-reviewer` raised this as "confirm against the client contract".
It is already settled: plan decision D6 and Task 3 both call for digest refs to fail, with a
test asserting it specifically so nobody widens the pattern later. Not an open question.

## Verified Properties

Each of these was checked against the source directly, not accepted on a reviewer's word:

- No `.Env` or `.Options` field access anywhere in production `dockerx` code — only doc comments.
- `ValidateRef` is the first statement in all four inspect methods, before any Engine call;
  `engine_test.go` proves an invalid ref reaches the Engine **zero** times.
- The regexp is anchored; Go's RE2 binds `^`/`$` to string boundaries with no multiline flag,
  so an embedded-newline bypass is not possible.
- All eight routes register as `requireDevice(requireOp(policy.OpRead, h))` — auth outside,
  policy inside, so an unauthenticated caller cannot probe the policy tier via 403-vs-401.
- Error bodies are terse; `TestErrorBodiesLeakNothing` asserts the state dir, `docker.sock`,
  and `devmon.db` appear in none of the 400/401/404/405/502 responses.
- Every Engine call derives a 15s timeout with `defer cancel()`; no leaked context on the
  validation-failure early-return path.
- Nil guards on every optional pointer, with tests that exercise the nil path rather than
  fully-populated fixtures.
- 500-item cap is a package constant, not derived from any request input.

## Validation Results

| Check | Result |
|---|---|
| `go vet ./...` | Pass |
| `gofmt` (changed files) | Pass |
| Tests (`go test ./... -race`) | Pass |
| Coverage `./internal/...` | Pass — 85.5% (floor 80%) |
| Build (`CGO_ENABLED=0`) | Pass |
| `golangci-lint` (v2.12.2) | Pass — `0 issues` repo-wide |
| `gosec` | Pass — `gosec ./...` → `0 issues`, 34 files, 4,468 lines |

Both were run after the initial review, once the tools were installed.

Worth stating precisely, because the two tools disagree at first glance: `gosec ./...` skips
`_test.go` files by default. `gosec -tests ./...` reports 5 findings and
`golangci-lint --enable gosec` reports 4 of the same set. All are in Phase 1–2 test files this
phase never touched — `certs/ca_test.go:346,367` and `certs/store_test.go:141,163` (G306,
fixtures writing `0o644` into a temp dir), `state/pairing_test.go:126` (G115,
`string(rune('a'+n))`). Production code is clean under both tools. Left out of this PR to avoid
widening the diff; worth a separate cleanup.

## Files Reviewed

Production: `internal/dockerx/{types,errors,ref,containers,images,networks,volumes,list}.go`,
`internal/httpapi/{reads,policygate,server}.go`, `cmd/devmon-agent/main.go`
Tests: `internal/dockerx/{types,errors,ref,containers,objects,engine}_test.go`,
`internal/httpapi/{reads,policygate,server,status,pair}_test.go`
Docs: `README.md`, `go.mod`

## Recommendation

**Approve for merge into `dev`.** Every precondition this review named has since been met:
`golangci-lint` and `gosec` run clean, and the full manual checklist has been worked against
Docker 29.6.1 / API 1.55 with a real paired device — including the `-e DB_PASSWORD=hunter2`
check, which is the assertion that closes the loop on this phase's central risk. The response
was 698 bytes and contained neither the secret nor an `env` key.

M4 needs a decision but does not block; it is now backed by evidence rather than inference.
