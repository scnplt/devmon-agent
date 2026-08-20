// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSplitSANs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       []string
		wantDNS  int
		wantAddr int
	}{
		{name: "dns only", in: []string{"a.example.com", "b.example.com"}, wantDNS: 2},
		{name: "ip only", in: []string{"203.0.113.7", "198.51.100.4"}, wantAddr: 2},
		{name: "mixed", in: []string{"a.example.com", "203.0.113.7"}, wantDNS: 1, wantAddr: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			dns, ips := splitSANs(tt.in)

			// Assert
			if len(dns) != tt.wantDNS {
				t.Errorf("len(dnsNames) = %d, want %d", len(dns), tt.wantDNS)
			}
			if len(ips) != tt.wantAddr {
				t.Errorf("len(ips) = %d, want %d", len(ips), tt.wantAddr)
			}
		})
	}
}
