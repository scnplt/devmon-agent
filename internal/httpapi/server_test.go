package httpapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/dockerx"
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
	return NewServer(cfg, nil, nil, nil, testTLSConfig(t), testLogger())
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
