// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e && !linux

package harness

// DockerSocketGID has no POSIX GID to report on this platform. It exists so
// this package compiles under `go vet -tags e2e ./...` on a developer's own
// (often non-Linux) machine, which is the first place that command runs; the
// in-container group itself only ever executes from a Linux Engine (D6).
func DockerSocketGID(string) (string, bool) {
	return "", false
}
