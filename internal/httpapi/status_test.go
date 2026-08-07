package httpapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/version"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testServer(t *testing.T, mode policy.Mode) *Server {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), ListenAddr: ":8443", PolicyMode: mode}
	return NewServer(cfg, nil, nil, testLogger())
}

// TestStatusFieldCount is the guard that stops a later phase from quietly
// widening a pre-authentication surface. Asserting only that the four expected
// keys are PRESENT would let a fifth slip in unnoticed; asserting the exact
// count forces any addition through a deliberate edit of this test. Phase 2
// changes 4 to 5 when ca_fingerprint lands.
func TestStatusFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	s := testServer(t, policy.ModeDefault)
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 4 {
		t.Fatalf("status payload has %d keys (%v), want exactly 4", len(body), keysOf(body))
	}
	for _, key := range []string{"api_version", "agent_version", "policy_mode", "server_time"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status payload is missing %q", key)
		}
	}
}

func TestStatusContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode policy.Mode
		want string
	}{
		{name: "read-only is advertised", mode: policy.ModeReadOnly, want: "read-only"},
		{name: "default is advertised", mode: policy.ModeDefault, want: "default"},
		{name: "full is advertised", mode: policy.ModeFull, want: "full"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := testServer(t, tt.mode)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

			// Assert
			var body statusResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.PolicyMode != tt.want {
				t.Errorf("policy_mode = %q, want %q", body.PolicyMode, tt.want)
			}
			if body.APIVersion != APIVersion {
				t.Errorf("api_version = %q, want %q", body.APIVersion, APIVersion)
			}
			if body.AgentVersion != version.Version {
				t.Errorf("agent_version = %q, want %q", body.AgentVersion, version.Version)
			}
		})
	}
}

func TestStatusHeadersAndTime(t *testing.T) {
	t.Parallel()

	// Arrange
	s := testServer(t, policy.ModeDefault)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	// Assert
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	ts, err := time.Parse(time.RFC3339, body.ServerTime)
	if err != nil {
		t.Fatalf("server_time %q is not RFC3339: %v", body.ServerTime, err)
	}
	if d := time.Since(ts); d > 5*time.Second || d < -5*time.Second {
		t.Errorf("server_time is %v away from now", d)
	}
	// UTC, never local: a client comparing timestamps across time zones would
	// otherwise silently compute the wrong skew.
	if ts.Location() != time.UTC {
		t.Errorf("server_time location = %v, want UTC", ts.Location())
	}
}

func TestStatusRejectsOtherMethods(t *testing.T) {
	t.Parallel()

	s := testServer(t, policy.ModeDefault)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			// Arrange
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, httptest.NewRequest(method, "/v1/status", nil))

			// Assert — the method-aware pattern is what produces 405 here;
			// registering the bare path would have answered 200.
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /v1/status = %d, want 405", method, rec.Code)
			}
		})
	}
}

// TestUnknownPathLeaksNothing covers the "no unauthenticated leakage" check.
func TestUnknownPathLeaksNothing(t *testing.T) {
	t.Parallel()

	// Arrange
	s := testServer(t, policy.ModeDefault)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/containers", nil))

	// Assert
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	for _, leak := range []string{s.cfg.StateDir, "devmon.db", "docker.sock"} {
		if leak != "" && bodyContains(rec.Body.String(), leak) {
			t.Errorf("404 body leaks %q: %s", leak, rec.Body.String())
		}
	}
}

func TestRequireDeviceRejectsWithoutCertificate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tlsS *tls.ConnectionState
	}{
		{name: "plain connection", tlsS: nil},
		{name: "tls with no peer certificates", tlsS: &tls.ConnectionState{}},
		{
			name: "tls with a peer certificate but no CA to verify it",
			tlsS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := testServer(t, policy.ModeDefault)
			guarded := s.requireDevice(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("guarded handler ran; Phase 1 must reject every request")
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/v1/protected", nil)
			req.TLS = tt.tlsS
			rec := httptest.NewRecorder()

			// Act
			guarded.ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgClientCertRequired {
				t.Errorf("error = %q, want the terse %q", body.Error, msgClientCertRequired)
			}
		})
	}
}

func TestWithRecoveryHidesPanicDetail(t *testing.T) {
	t.Parallel()

	// Arrange
	s := testServer(t, policy.ModeDefault)
	handler := s.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal detail")
	}))
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if bodyContains(rec.Body.String(), "secret internal detail") {
		t.Errorf("panic detail reached the client: %s", rec.Body.String())
	}
}

func TestWithRequestLogPreservesStatus(t *testing.T) {
	t.Parallel()

	// Arrange
	s := testServer(t, policy.ModeDefault)
	handler := s.withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	// Assert
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418; the recorder must not swallow it", rec.Code)
	}
}

func TestNewServerAppliesHardeningTimeouts(t *testing.T) {
	t.Parallel()

	// Arrange / Act
	s := testServer(t, policy.ModeDefault)

	// Assert — ReadHeaderTimeout is the Slowloris defence and gosec G114.
	if s.http.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", s.http.ReadHeaderTimeout, readHeaderTimeout)
	}
	if s.http.MaxHeaderBytes != maxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", s.http.MaxHeaderBytes, maxHeaderBytes)
	}
	if s.http.ReadTimeout != readTimeout || s.http.WriteTimeout != writeTimeout {
		t.Errorf("read/write timeouts = %v/%v, want %v/%v",
			s.http.ReadTimeout, s.http.WriteTimeout, readTimeout, writeTimeout)
	}
	if s.http.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", s.http.IdleTimeout, idleTimeout)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func bodyContains(body, needle string) bool {
	return needle != "" && strings.Contains(body, needle)
}
