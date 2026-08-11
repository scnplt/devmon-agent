// SPDX-License-Identifier: AGPL-3.0-only

// Package version carries build metadata stamped into the binary at link time.
package version

// Set at build time via:
//
//	-ldflags "-X github.com/scnplt/devmon-agent/internal/version.Version=..."
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)
