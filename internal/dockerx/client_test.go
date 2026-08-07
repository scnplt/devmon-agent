package dockerx

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewUnreachableEngine covers the "Docker socket absent at startup" edge
// case. A live daemon is deliberately not required: that belongs in the manual
// checklist, not in a unit test that must pass on any machine.
func TestNewUnreachableEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
	}{
		{name: "missing unix socket", host: "unix:///nonexistent/docker.sock"},
		{name: "closed tcp port", host: "tcp://127.0.0.1:1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			c, err := New(context.Background(), tt.host, testLogger())

			// Assert
			if err == nil {
				_ = c.Close()
				t.Fatalf("New(%q) error = nil, want an unreachable-engine failure", tt.host)
			}
			// The message must name the host, or the operator cannot tell a bad
			// DEVMON_DOCKER_HOST from a missing socket mount.
			if !strings.Contains(err.Error(), tt.host) {
				t.Errorf("error %q does not name the host %q", err, tt.host)
			}
		})
	}
}

func TestNewRejectsMalformedHost(t *testing.T) {
	t.Parallel()

	// Act
	c, err := New(context.Background(), "://not a host", testLogger())

	// Assert
	if err == nil {
		_ = c.Close()
		t.Fatal("New() error = nil, want a client construction failure")
	}
}
