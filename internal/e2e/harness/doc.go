//go:build e2e

// Package harness runs the real devmon-agent against a real Docker Engine.
//
// It exists so the outstanding manual checklists from Phases 1-5 (PRD Phase
// 6) can be replayed unattended: it builds the shipped binary, starts it with
// a curated environment, pairs a device through the documented host-side
// command path, and drives every route over mTLS. Nothing in this package is
// imported outside internal/e2e (it lives under internal/, which is what
// makes that true), and every file in this package and its siblings carries
// this same build tag so the default `go build ./...` / `go test
// ./internal/... -race` gates never see it.
//
// Three things it deliberately refuses to do:
//
//  1. It never touches a container it did not create. Every fixture carries
//     the com.devmon.e2e label plus a per-run label, and cleanup filters on
//     both. The suite runs on a developer's own Engine, next to their own
//     containers (D11 of the phase plan).
//
//  2. It never passes the ambient environment to the agent under test. The
//     child process's environment is built from the test case alone, so a
//     developer with DEVMON_POLICY_MODE=full exported in their shell cannot
//     silently invalidate a read-only assertion (D12).
//
//  3. It never prints a pairing code, a private key, or PEM bytes — not even
//     in a failure message. CI logs are retained and world-readable, and the
//     repository's "never log key material" rule binds test output too.
package harness
