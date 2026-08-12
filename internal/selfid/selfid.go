// SPDX-License-Identifier: AGPL-3.0-only

// Package selfid discovers, without contacting the Docker Engine, the
// candidate container IDs the current process might be running as.
//
// It deliberately returns UNVERIFIED candidates in priority order. No single
// source is reliable: /proc/self/cgroup reports a bare "0::/" under cgroup v2
// with a private namespace (the modern Docker default), $HOSTNAME is the short
// ID only until someone passes --hostname, and mountinfo depends on the
// storage driver. Verification needs the Engine, so it happens in
// internal/dockerx (D2, D6).
package selfid

import (
	"os"
	"path/filepath"
	"regexp"
)

// hexID64Pattern matches a full 64-character Docker container ID wherever it
// appears in a longer line — mountinfo and cgroup lines embed the ID inside a
// path (e.g. "/var/lib/docker/containers/<id>/hostname"), so the field layout
// cannot be trusted across kernel versions and the ID must be extracted by
// shape instead.
var hexID64Pattern = regexp.MustCompile(`[0-9a-f]{64}`)

// hostnameID12Pattern matches the short container ID Docker writes to
// /etc/hostname by default. It is rejected the moment an operator passes
// --hostname, which is why it sorts last among the filesystem-derived
// candidates.
var hostnameID12Pattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Result is what Detect could learn from the filesystem alone.
type Result struct {
	// Containerized reports whether this process is running inside a Docker
	// container, detected by /.dockerenv — which Docker creates in every
	// container regardless of base image, including distroless/static.
	Containerized bool
	// Candidates are container-ID candidates in priority order, longest and
	// most trustworthy first. May be empty even when Containerized is true.
	Candidates []string
	// Override is the operator-supplied DEVMON_SELF_CONTAINER, or "" when
	// none was set. Carried alongside Candidates (rather than requiring the
	// caller to remember it separately) so internal/dockerx's confirmSelf can
	// tell whether a skipped candidate was the operator's explicit choice and
	// log accordingly.
	Override string
}

// Detect gathers candidates. root is the filesystem root to read under ("/"
// in production, a temp dir in tests). override, when non-empty, is the
// operator's DEVMON_SELF_CONTAINER and always sorts first. getenv is a
// parameter rather than a call to os.Getenv so callers can inject a fake
// environment in tests without t.Setenv, which forbids t.Parallel.
func Detect(root, override string, getenv func(string) string) Result {
	result := Result{
		Containerized: dockerenvExists(root),
		Override:      override,
	}

	var candidates []string
	if override != "" {
		candidates = append(candidates, override)
	}
	candidates = append(candidates, hexIDsIn(filepath.Join(root, "proc", "self", "mountinfo"))...)
	candidates = append(candidates, hexIDsIn(filepath.Join(root, "proc", "self", "cgroup"))...)
	if hostname := getenv("HOSTNAME"); hostnameID12Pattern.MatchString(hostname) {
		candidates = append(candidates, hostname)
	}

	result.Candidates = dedupe(candidates)
	return result
}

// dockerenvExists reports whether <root>/.dockerenv exists.
func dockerenvExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".dockerenv"))
	return err == nil
}

// hexIDsIn returns every 64-hex run found in path, in file order. A missing
// or unreadable file yields an empty slice, never an error: every source
// here is best-effort.
func hexIDsIn(path string) []string {
	// #nosec G304 -- path is assembled by Detect from a caller-supplied root
	// plus fixed literal segments ("proc/self/mountinfo", "proc/self/cgroup").
	// The root is "/" in production and a test temp dir otherwise; no part of
	// it comes from a request, a device, or any other untrusted source.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return hexID64Pattern.FindAllString(string(data), -1)
}

// dedupe removes repeated entries while preserving the order of first
// occurrence.
func dedupe(candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	result := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		result = append(result, c)
	}
	return result
}
