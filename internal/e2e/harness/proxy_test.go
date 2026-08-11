// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

import "testing"

// TestParseEngineHost is a pure-function test: no Docker Engine, no
// subprocess, nothing that needs t.Cleanup. It exists so the proxy's one
// piece of decoding logic is falsifiable without a live Engine in the loop —
// the same reasoning D16's proxy exists for, applied to its own parsing step.
func TestParseEngineHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		host        string
		wantNetwork string
		wantAddress string
		wantErr     bool
	}{
		{
			name:        "unix socket path",
			host:        "unix:///var/run/docker.sock",
			wantNetwork: "unix",
			wantAddress: "/var/run/docker.sock",
		},
		{
			name:        "tcp host and port",
			host:        "tcp://127.0.0.1:2375",
			wantNetwork: "tcp",
			wantAddress: "127.0.0.1:2375",
		},
		{
			name:    "npipe is rejected",
			host:    "npipe:////./pipe/docker_engine",
			wantErr: true,
		},
		{
			name:    "unparseable URL",
			host:    "://not a url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			network, address, err := parseEngineHost(tt.host)

			// Assert
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEngineHost(%q) error = nil, want an error", tt.host)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEngineHost(%q) error = %v, want nil", tt.host, err)
			}
			if network != tt.wantNetwork {
				t.Errorf("parseEngineHost(%q) network = %q, want %q", tt.host, network, tt.wantNetwork)
			}
			if address != tt.wantAddress {
				t.Errorf("parseEngineHost(%q) address = %q, want %q", tt.host, address, tt.wantAddress)
			}
		})
	}
}
