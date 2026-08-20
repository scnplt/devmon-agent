// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/config"
)

func TestHealthTargetAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		listenAddr string
		want       string
	}{
		{name: "no host binds every interface, dial loopback", listenAddr: ":8443", want: "127.0.0.1:8443"},
		{name: "explicit 0.0.0.0 binds every interface, dial loopback", listenAddr: "0.0.0.0:8443", want: "127.0.0.1:8443"},
		{name: "explicit :: binds every interface, dial loopback", listenAddr: "[::]:8443", want: "127.0.0.1:8443"},
		{name: "specific address is dialed as configured", listenAddr: "192.0.2.10:9999", want: "192.0.2.10:9999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := healthTargetAddr(tt.listenAddr)
			if err != nil {
				t.Fatalf("healthTargetAddr(%q) unexpected error: %v", tt.listenAddr, err)
			}
			if got != tt.want {
				t.Errorf("healthTargetAddr(%q) = %q, want %q", tt.listenAddr, got, tt.want)
			}
		})
	}
}

func TestHealthTargetAddrRejectsMalformedListenAddr(t *testing.T) {
	t.Parallel()

	if _, err := healthTargetAddr("not-a-host-port"); err == nil {
		t.Fatal("healthTargetAddr() = nil error, want one for a malformed listen address")
	}
}

// tlsAddr strips the "https://" scheme httptest.NewTLSServer's URL carries,
// since probeStatus builds its own URL from a bare host:port.
func tlsAddr(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	const schemePrefix = "https://"
	return srv.URL[len(schemePrefix):]
}

func TestProbeStatusHealthyOn200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := probeStatus(context.Background(), tlsAddr(t, srv)); err != nil {
		t.Errorf("probeStatus() = %v, want nil on 200", err)
	}
}

// TestProbeStatusHealthyOn429 is the non-obvious case: a rate-limited response
// still proves the listener accepted the connection, ran the middleware chain,
// and answered — which is exactly what this probe measures. An operator who
// lowers DEVMON_RATE_STATUS_PER_MIN must not thereby make their container
// permanently unhealthy.
func TestProbeStatusHealthyOn429(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	if err := probeStatus(context.Background(), tlsAddr(t, srv)); err != nil {
		t.Errorf("probeStatus() = %v, want nil on 429 (rate-limited but alive)", err)
	}
}

func TestProbeStatusUnhealthyOnServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := probeStatus(context.Background(), tlsAddr(t, srv))
	if err == nil {
		t.Fatal("probeStatus() = nil error, want one on 500")
	}
	if got := err.Error(); !strings.Contains(got, "500") {
		t.Errorf("probeStatus() error = %q, want it to name the status code 500", got)
	}
}

func TestProbeStatusUnhealthyWhenNothingListens(t *testing.T) {
	t.Parallel()

	// No server bound here — 127.0.0.1:0 with a resolved-but-unused address
	// stands in for a wedged/absent listener.
	if err := probeStatus(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("probeStatus() = nil error, want one when nothing is listening")
	}
}

func TestRunHealthCommandRejectsTrailingArgument(t *testing.T) {
	t.Parallel()

	err := runHealthCommand(context.Background(), config.Config{ListenAddr: ":8443"}, []string{"extra"})
	if err == nil {
		t.Fatal("runHealthCommand() = nil error, want one for an unexpected trailing argument")
	}
}
