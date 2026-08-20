// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

// Package api is the host-binary group: it builds and runs the real
// devmon-agent binary, pairs through the documented host-side command path
// (device pair-code), and drives every route over mTLS. Every file in this
// package replays one section of a manual checklist from Phases 1-5, so it
// runs unattended against a real Docker Engine instead of a
// human with curl.
//
// What this package deliberately does NOT cover: anything that needs a
// containerised agent — self-identification via mountinfo, self-exclusion,
// crash-and-restart across a real `docker kill` — lives in the sibling
// internal/e2e/incontainer package instead, because only a real container
// gives those properties something to observe.
//
// There is deliberately no TestMain here, and no startup sweep. An implicit
// "remove every container carrying the suite label" pass cannot tell a
// crashed previous run's leftovers from a CONCURRENT run's live containers,
// so it would force-remove the latter — including another run's agent
// container, mid-test — while claiming to honour D11's per-run isolation.
// Every fixture is removed by the t.Cleanup that created it, by ID, which is
// stricter than any label filter. Cleaning up after a run that crashed hard
// enough to skip its own cleanups is an explicit operator action:
// `make e2e-clean`.
package api
