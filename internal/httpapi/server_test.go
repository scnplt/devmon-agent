// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
	"github.com/scnplt/devmon-agent/internal/tlsconf"
)

// freeTCPAddr reserves a free TCP port by binding then immediately closing a
// listener, so a test can start the real server on a known address rather
// than the OS-assigned ":0" — needed here because the test dials the server
// itself, which requires knowing the port in advance.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close port reservation: %v", err)
	}
	return addr
}

func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	ca, _, err := certs.LoadOrCreateCA(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("load or create test CA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"localhost"}, time.Now())
	if err != nil {
		t.Fatalf("issue test cert: %v", err)
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
	return NewServer(cfg, nil, nil, nil, testTLSConfig(t), testLogger())
}

// TestRunShutsDownCleanly is the graceful-shutdown contract: SIGTERM cancels the
// context, Run drains and returns nil. A non-nil return here would make Docker
// record every clean stop as a failed container.
func TestRunShutsDownCleanly(t *testing.T) {
	t.Parallel()

	// Arrange — a known port, reserved and released up front, so parallel
	// tests never collide and this test can dial it to confirm the listener
	// has actually bound before cancelling.
	addr := freeTCPAddr(t)
	s := runnableServer(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	// Act
	go func() { done <- s.Run(ctx) }()
	waitForListening(t, addr, 2*time.Second)
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

// deadlineAwareRecorder adds a no-op SetWriteDeadline to httptest.ResponseRecorder,
// which implements neither Flush's nor SetWriteDeadline's optional interfaces on
// its own (Flush is a no-op method, but SetWriteDeadline is absent entirely).
// http.ResponseController requires the latter to reach the underlying writer
// through Unwrap, so this stands in for a real net.Conn-backed ResponseWriter.
type deadlineAwareRecorder struct {
	*httptest.ResponseRecorder
	deadlineCleared bool
}

func (d *deadlineAwareRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlineCleared = t.IsZero()
	return nil
}

// TestStatusRecorderIsFlushable is D8's regression test: withRequestLog wraps
// every response in statusRecorder, and http.ResponseController can only reach
// the wrapped writer's Flush and SetWriteDeadline through an Unwrap method.
// Without it, both calls fail with http.ErrNotSupported and the SSE stream in
// logs.go both buffers forever and dies at the server's 30s WriteTimeout.
func TestStatusRecorderIsFlushable(t *testing.T) {
	t.Parallel()

	// Arrange
	rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	rc := http.NewResponseController(sr)

	// Act
	flushErr := rc.Flush()
	deadlineErr := rc.SetWriteDeadline(time.Time{})

	// Assert
	if flushErr != nil {
		t.Errorf("Flush() error = %v, want nil", flushErr)
	}
	if deadlineErr != nil {
		t.Errorf("SetWriteDeadline() error = %v, want nil", deadlineErr)
	}
}

// TestLogRoutePrecedence proves ServeMux resolves the three container
// patterns to their own handlers rather than to one another, even though
// they are one path segment apart: "/v1/containers/{id}",
// "/v1/containers/{id}/logs", and "/v1/containers/{id}/logs/stream" must each
// reach the code that answers it, not a neighbour's.
func TestLogRoutePrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		setFd  func(fd *fakeDocker)
		verify func(t *testing.T, body []byte)
	}{
		{
			name: "inspect container reaches the inspect handler",
			path: "/v1/containers/abc123",
			setFd: func(fd *fakeDocker) {
				fd.inspectContainerFn = func(context.Context, string) (dockerx.ContainerDetail, error) {
					return dockerx.ContainerDetail{ID: "from-inspect"}, nil
				}
			},
			verify: func(t *testing.T, body []byte) {
				var got dockerx.ContainerDetail
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ID != "from-inspect" {
					t.Errorf("id = %q, want from-inspect", got.ID)
				}
			},
		},
		{
			name: "historical logs reach the historical handler",
			path: "/v1/containers/abc123/logs",
			setFd: func(fd *fakeDocker) {
				fd.containerLogsFn = func(context.Context, string, dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
					return dockerx.ListResult[dockerx.LogLine]{Items: []dockerx.LogLine{{Line: "from-historical"}}}, nil
				}
			},
			verify: func(t *testing.T, body []byte) {
				var got dockerx.ListResult[dockerx.LogLine]
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(got.Items) != 1 || got.Items[0].Line != "from-historical" {
					t.Errorf("items = %+v, want one item with line from-historical", got.Items)
				}
			},
		},
		{
			name: "stream logs reach the stream handler",
			path: "/v1/containers/abc123/logs/stream",
			setFd: func(fd *fakeDocker) {
				fd.streamContainerLogsFn = func(_ context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
					return emit(dockerx.LogLine{Line: "from-stream"})
				}
			},
			verify: func(t *testing.T, body []byte) {
				if !bodyContains(string(body), "from-stream") {
					t.Errorf("body = %q, want it to contain from-stream", body)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setFd(fd)
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			// deadlineAwareRecorder, not a bare httptest.ResponseRecorder: the
			// stream route's sseWriter needs a working SetWriteDeadline to
			// commit its response at all (D7/D8), and the plain recorder
			// implements neither Flush's nor SetWriteDeadline's optional
			// interfaces.
			rec := &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			tc.verify(t, rec.Body.Bytes())
		})
	}
}

// TestRunReturnsShutdownError covers both shutdown's error branch and Run's
// propagation of it: a request still in flight when the grace window expires
// makes http.Server.Shutdown return context.DeadlineExceeded, and Run must
// surface that rather than swallow it.
//
// The branch is still reachable after #41: shutdown cancels lifecycleCtx —
// the parent of every request context — before it drains, so a handler that
// honours its context unwinds at once and Shutdown returns nil (that contract
// is TestShutdownEndsLiveStreamPromptlyAndReturnsNil), but a handler that
// ignores it for longer than shutdownGrace still holds the window open. The
// blocking handler below stands in for one, since no production handler
// ignores its context on purpose.
//
// It replaces a connection stalled mid-request-headers, whose premise was not
// stable (issue #72). net/http counts a StateNew connection as idle once its
// state stamp is more than five seconds old, and that stamp has one-second
// granularity while shutdownGrace is itself five seconds: a connection
// registered just before a second boundary crossed the threshold inside the
// grace window, was closed as idle, and Shutdown returned nil; one registered
// just after did not. Which side of the boundary it landed on was the coin
// flip, and a probe confirms it — cancelling the base context first makes no
// difference to the outcome, while delaying the shutdown by 1.2s flips it from
// always-DeadlineExceeded to always-nil. A request sitting in a handler is
// StateActive, is never promoted to idle, and so is not timing-dependent.
func TestRunReturnsShutdownError(t *testing.T) {
	// Not t.Parallel(): this test deliberately holds a request open for
	// shutdownGrace (5s) so its background goroutines do not skew
	// TestStreamGoroutineDoesNotLeak's before/after count if the two run
	// concurrently.

	// Arrange — a known port, since this test dials the server itself.
	addr := freeTCPAddr(t)
	s := runnableServer(t, addr)

	// The handler is swapped for a blocking one rather than reached through
	// the real routes: the contract under test is Run and shutdown, not
	// routing, and no production handler ignores its context on purpose.
	entered := make(chan struct{})
	release := make(chan struct{})
	s.http.Handler = http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(entered)
		// Deliberately not selecting on r.Context().Done(): ignoring
		// cancellation is the whole premise.
		<-release
	})
	// Released after the assertion, so the handler goroutine and its
	// connection unwind before the test ends.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- s.Run(ctx) }()
	// The listener has to be bound before this test can dial it. Poll for a
	// successful connect instead of sleeping a fixed 50ms (issue #54).
	waitForListening(t, addr, 2*time.Second)

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only, talks to our own ephemeral server
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET /v1/status HTTP/1.1\r\nHost: example\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Synchronize on the handler actually running, so the request is provably
	// in flight — and therefore the connection provably active — before the
	// grace window starts.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	// Act
	cancel()

	// Assert
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() = nil, want a shutdown-timeout error with a request still in flight")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
		}
		if !bodyContains(err.Error(), "shut down http server") {
			t.Errorf("error %q does not identify the failing step", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Run() did not return after shutdownGrace elapsed")
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

// issueMTLSClientCert issues a real device client certificate against ca and
// records it in st, mirroring pairDevice's own sequence, and returns a
// tls.Certificate a real net/http client can present at the handshake — the
// counterpart pairDeviceForTest and pairDeviceForRead deliberately skip by
// driving requireDevice without a real TLS connection. This helper exists
// only for the shutdown test below, which needs a genuine in-flight
// connection to prove Shutdown actually unblocks it, not merely a fake
// r.TLS.PeerCertificates value.
func issueMTLSClientCert(t *testing.T, ctx context.Context, st *state.Store, ca *certs.CA) tls.Certificate {
	t.Helper()

	device, err := st.CreateDevice(ctx, "shutdown test device")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "irrelevant"},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	now := time.Now()
	certPEM, serialHex, notAfter, err := ca.IssueDeviceCert(csrDER, device.ID, now)
	if err != nil {
		t.Fatalf("IssueDeviceCert: %v", err)
	}
	if err := st.RecordDeviceCert(ctx, device.ID, serialHex, now, notAfter); err != nil {
		t.Fatalf("RecordDeviceCert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build client key pair: %v", err)
	}
	return clientCert
}

// TestShutdownEndsLiveStreamPromptlyAndReturnsNil is issue #41's regression:
// a live SSE log stream, held open by a real client over a real connection,
// must not pin Shutdown for the full shutdownGrace window. Before the fix,
// nothing cancelled the stream handler's context on shutdown, so Shutdown
// blocked until shutdownGrace elapsed, returned context.DeadlineExceeded, and
// Run propagated it — turning an ordinary `docker stop` into a reported
// failure. After the fix the stream's context is cancelled as soon as
// shutdown starts, the handler returns immediately, and Shutdown (and Run)
// complete well under the grace period with a nil error.
func TestShutdownEndsLiveStreamPromptlyAndReturnsNil(t *testing.T) {
	t.Parallel()

	// Arrange — a real server with a real listener, real CA-backed mTLS, and
	// a fake Docker stream that blocks until its context is cancelled, just
	// like the real Engine SDK call does while a client is watching logs.
	addr := freeTCPAddr(t)
	dir := t.TempDir()
	log := testLogger()

	ca, _, err := certs.LoadOrCreateCA(t.TempDir(), log)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}
	serverPair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build server key pair: %v", err)
	}
	tlsCfg := tlsconf.Build(serverPair, ca.Pool())

	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), log)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	streamStarted := make(chan struct{})
	fd := &fakeDocker{
		streamContainerLogsFn: func(ctx context.Context, _ string, _ dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
			// Emit one line first so the SSE response headers commit
			// immediately, rather than waiting on the 20s keepalive ticker
			// for the client's Do() to see a response at all.
			if err := emit(dockerx.LogLine{Timestamp: "t1", Stream: "stdout", Line: "watching"}); err != nil {
				return err
			}
			close(streamStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	cfg := config.Config{StateDir: dir, ListenAddr: addr, PolicyMode: policy.ModeDefault}
	s := NewServer(cfg, st, ca, fd, tlsCfg, log)

	clientCert := issueMTLSClientCert(t, context.Background(), st, ca)

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	waitForListening(t, addr, 2*time.Second)

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{clientCert},
				InsecureSkipVerify: true, //nolint:gosec // test-only, talks to our own ephemeral server
			},
		},
	}
	reqCtx, cancelReq := context.WithTimeout(context.Background(), shutdownGrace+5*time.Second)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://"+addr+"/v1/containers/c1/logs/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	select {
	case <-streamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not reach the fake Docker call in time")
	}

	streamEnded := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(streamEnded)
	}()

	// Act
	shutdownStart := time.Now()
	cancelRun()

	// Assert — Shutdown completes well under shutdownGrace, and Run reports
	// no error: a stream terminated by shutdown is a clean termination.
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Errorf("Run() = %v, want nil after shutdown with a live stream", runErr)
		}
		if elapsed := time.Since(shutdownStart); elapsed >= shutdownGrace {
			t.Errorf("shutdown took %v, want well under the %v grace period", elapsed, shutdownGrace)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Run() did not return after shutdown with a live stream")
	}

	select {
	case <-streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the client's stream read did not end after shutdown")
	}
}
