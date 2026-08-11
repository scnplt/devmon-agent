# PR Review: #11 — test: client-independent end-to-end suite (Phase 6)

**Reviewed**: 2026-08-10
**Author**: Sertan Canpolat
**Branch**: `test/client-independent-e2e` → `dev`
**Scope**: 33 files, +8,777 / −9
**Decision**: REQUEST CHANGES

> **Self-review disclosure**: this code was written by the same session that is now
> reviewing it. That weakens the signal — an author's blind spot is exactly what a
> reviewer is for. The findings below were reached by re-reading the files against the
> plan's own stated guarantees rather than against memory of what was intended, which is
> how the HIGH finding surfaced: it contradicts a promise the harness's own doc comment
> makes.

## Summary

A large, well-documented test-only change that adds a real end-to-end suite and has been
executed green against Docker 29.6.1. No production Go is touched, `go.mod`/`go.sum` are
unchanged, and the default gate is genuinely unaffected by the `e2e` build tag. One HIGH
finding: the orphan sweep does not filter by run, so two concurrent runs on one host
destroy each other's containers — including a running agent container — contradicting
both D11 and the harness's stated refusals.

## Findings

### CRITICAL

None. Every removal path is scoped to the `com.devmon.e2e` label, so a developer's own
containers are never listed, let alone removed. That is the property that actually
mattered here and it holds.

### HIGH

**H1 — The orphan sweep ignores the per-run label, so concurrent runs delete each other's
containers.**

`internal/e2e/api/main_test.go:65` and `internal/e2e/incontainer/main_test.go` (same
filter) and `internal/e2e/harness/engine.go:227` all list on `LabelSuite=1` alone and
force-remove everything returned:

```go
filters := client.Filters{}.Add("label", harness.LabelSuite+"=1")
// ... ContainerRemove(Force: true) for every item
```

`LabelRun` is written onto every fixture (`engine.go:144`) and onto the agent container
(`image.go:205`) but is never read by any cleanup path.

This contradicts three stated guarantees:

- The plan's LABELLED_FIXTURE pattern: *"Cleanup filters on runLabel, so a concurrent run
  on the same host cannot delete another run's containers."*
- `harness/doc.go`'s refusal #1: *"Every fixture carries the com.devmon.e2e label plus a
  per-run label, and cleanup filters on **both**."*
- The plan's edge-case checklist: *"Two concurrent suite runs on one host do not delete
  each other's fixtures."*

**Failure scenario**: run A is 30s into the in-container group with its agent container
up; run B starts, its `TestMain` sweeps, and run B force-removes run A's *running agent
container*. Run A then fails with unrelated-looking connection errors — the exact
"flakiness turns the suite into a check people re-run until it passes" risk the plan
lists.

**This also invalidates a ticked checklist item.** The concurrency check in the phase
report is recorded as passing, and it did pass — but only because both runs were launched
in the same instant, so each swept before either had created anything. The item passed by
timing luck, not by design, and the report should say so until the code matches the
promise.

**Suggested fix**: the sweep's purpose is cleaning up after a *crashed previous* run, which
by definition is not running now. Either skip containers whose `LabelRun` equals this
process's `runID` and whose creation time is within a plausible run window, or drop the
implicit sweep in favour of an explicit `make e2e-clean`. Filtering on `runID` alone is not
sufficient — it would leave a concurrent run's containers matched by *every other* run.

### MEDIUM

**M1 — `harness.SweepOrphans` is dead code.** `engine.go:220` is exported and never called;
both group `TestMain`s reimplement the same logic (`api/main_test.go:47`,
`incontainer/main_test.go`) with the comment *"the same filter harness.SweepOrphans uses,
reimplemented here without a *testing.T"*. Three copies of one routine is how H1 came to
exist in three places at once. Fold them into a single `harness` function that takes no
`*testing.T` (it never needed one — it only logs), and delete the exported wrapper.

**M2 — `unresolvableSelfID` and `selfUnknownBody` are now partly vestigial.**
`contract_selfid_test.go:38` declares `selfUnknownBody` for a 503 path that
`TestUnresolvableSelfIDFallsBackToDetection` no longer asserts (see Finding 1 in the phase
report). It is unreferenced now. Keep it only if a Phase 7 fix will restore the assertion —
and if so, say that in the comment; otherwise remove it.

### LOW

**L1 — `os.Chmod(stateDir, 0o777)`** (`image.go:180`) is correct and necessary for the
UID 65532 container user, and is already commented as test-only on a `t.TempDir()`. Noted
only because it will draw a reviewer's eye every time; the comment already answers it.

**L2 — Test-only `#nosec G402` annotations** on the four `InsecureSkipVerify` sites are each
justified in place and confined to the pairing bootstrap and readiness probes, which is
what D7 requires. Verified: no post-pairing client sets it.

## Validation Results

| Check | Result |
|---|---|
| `go vet ./...` | Pass |
| `go vet -tags e2e ./...` | Pass |
| `go build ./...` | Pass |
| `go test ./internal/... -race` | Pass — 10 packages, unchanged |
| e2e host-binary group | Pass — 58 passed, 2 skipped, 0 failed |
| e2e in-container group | Pass — 11 passed, 0 failed |
| e2e endurance group | Pass — 2 passed, 1820s |
| Module hygiene | Pass — `go.mod`/`go.sum` unchanged |

## Files Reviewed

All 33. Read in full: `harness/engine.go`, `harness/proxy.go`, `harness/image.go`,
`harness/rebind.go`, `api/main_test.go`, `incontainer/main_test.go`,
`incontainer/contract_selfid_test.go`, `api/contract_audit_test.go`,
`api/contract_startup_test.go`, `api/contract_logs_test.go`,
`incontainer/contract_selfexclusion_test.go`, `Makefile`, `.github/workflows/ci.yml`.
Scanned: the remaining contract files, `README.md`, the PRD and report.

## What Is Good

Worth recording, because the failure modes this phase was built to prevent were actually
prevented:

- The falsification round found a test that could not fail (`TestStreamResumeRepeatsAtMostOneLine`)
  and the fix tightened the assertion instead of excusing the inversion.
- Two of the first run's failures were correctly diagnosed as *test* bugs with the agent
  behaving correctly, rather than "fixed" by changing production code to match a wrong test.
- Finding 1 (silently ignored `DEVMON_SELF_CONTAINER_ID`) was recorded and left unfixed
  per D19 rather than folded into the phase.
- The 502 proxy, the abrupt-close primitive, and the wire-level key-set assertions all
  demonstrably fail when inverted.
