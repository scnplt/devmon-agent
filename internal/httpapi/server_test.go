package httpapi

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/tlsconf"
)

func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	certPEM, keyPEM, err := certs.GenerateServerCert([]string{"localhost"}, time.Now())
	if err != nil {
		t.Fatalf("generate test cert: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build keypair: %v", err)
	}
	return tlsconf.Build(pair, nil)
}

func runnableServer(t *testing.T, addr string) *Server {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), ListenAddr: addr, PolicyMode: policy.ModeDefault}
	return NewServer(cfg, nil, testTLSConfig(t), testLogger())
}

// TestRunShutsDownCleanly is the graceful-shutdown contract: SIGTERM cancels the
// context, Run drains and returns nil. A non-nil return here would make Docker
// record every clean stop as a failed container.
func TestRunShutsDownCleanly(t *testing.T) {
	t.Parallel()

	// Arrange — port 0 lets the OS pick, so parallel tests never collide.
	s := runnableServer(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let the listener bind
	cancel()

	// Assert
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil after a cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

func TestRunReportsListenFailure(t *testing.T) {
	t.Parallel()

	// Arrange — an unusable address, so ListenAndServeTLS fails immediately.
	s := runnableServer(t, "127.0.0.1:-1")

	// Act
	err := s.Run(context.Background())

	// Assert
	if err == nil {
		t.Fatal("Run() = nil, want a listen failure")
	}
	if !bodyContains(err.Error(), "serve https on") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}
