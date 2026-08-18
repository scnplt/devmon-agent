// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
	"github.com/scnplt/devmon-agent/internal/version"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testServer(t *testing.T, mode policy.Mode) *Server {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), ListenAddr: ":8443", PolicyMode: mode}
	return NewServer(cfg, nil, nil, nil, nil, testLogger())
}

// testServerWithCA is a second helper, additive to testServer, for tests that
// need a real CA to exercise handleStatus's fingerprint derivation. Several
// passing tests rely on testServer constructing a Server with a nil CA, so
// that helper stays unchanged rather than being widened.
func testServerWithCA(t *testing.T, mode policy.Mode) (*Server, *certs.CA) {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), ListenAddr: ":8443", PolicyMode: mode}
	ca, _, err := certs.LoadOrCreateCA(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return NewServer(cfg, nil, ca, nil, nil, testLogger()), ca
}

// testServerWithStore is a third helper, additive to testServer and
// testServerWithCA, for tests that exercise requireDevice's live lookup
// against a real *state.Store. testServer keeps passing nil, which several
// passing tests rely on.
func testServerWithStore(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, ListenAddr: ":8443", PolicyMode: policy.ModeDefault}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), testLogger())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(cfg, st, nil, nil, nil, testLogger()), st
}

// peerCertWithSerial builds a *tls.ConnectionState carrying a single peer
// certificate whose serial number is serial, for driving requireDevice
// without a real TLS handshake.
func peerCertWithSerial(serial *big.Int) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{SerialNumber: serial}},
	}
}

// TestStatusFieldCount is the guard that stops a later phase from quietly
// widening a pre-authentication surface. Asserting only that the five expected
// keys are PRESENT would let a sixth slip in unnoticed; asserting the exact
// count forces any addition through a deliberate edit of this test. Phase 2
// added ca_fingerprint, changing the count from 4 to 5.
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
	if len(body) != 5 {
		t.Fatalf("status payload has %d keys (%v), want exactly 5", len(body), keysOf(body))
	}
	for _, key := range []string{"api_version", "agent_version", "policy_mode", "server_time", "ca_fingerprint"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status payload is missing %q", key)
		}
	}
}

// TestStatusCAFingerprint asserts the fingerprint is the real pinning anchor:
// 64 lowercase hex characters, equal to ca.Fingerprint().
func TestStatusCAFingerprint(t *testing.T) {
	t.Parallel()

	// Arrange
	s, ca := testServerWithCA(t, policy.ModeDefault)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	// Assert
	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.CAFingerprint != ca.Fingerprint() {
		t.Errorf("ca_fingerprint = %q, want %q", body.CAFingerprint, ca.Fingerprint())
	}
	if len(body.CAFingerprint) != 64 {
		t.Errorf("ca_fingerprint length = %d, want 64", len(body.CAFingerprint))
	}
	for _, r := range body.CAFingerprint {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Errorf("ca_fingerprint = %q, contains non-lowercase-hex rune %q", body.CAFingerprint, r)
			break
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
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/system/info", nil))

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

// TestRequireDeviceRejectsWithoutCertificate covers the cases requireDevice
// rejects before it ever needs a store lookup, so testServer's nil store is
// safe to use here. The case of a presented certificate whose serial must be
// resolved against a real registry — unknown, revoked, or active — lives in
// TestRequireDeviceResolvesRealDevice, which uses testServerWithStore.
func TestRequireDeviceRejectsWithoutCertificate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tlsS *tls.ConnectionState
	}{
		{name: "plain connection", tlsS: nil},
		{name: "tls with no peer certificates", tlsS: &tls.ConnectionState{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := testServer(t, policy.ModeDefault)
			guarded := s.requireDevice(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("guarded handler ran; no client certificate was presented")
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

// TestRequireDeviceResolvesRealDevice extends the requireDevice table above
// with cases that need a live *state.Store: an unknown serial and a revoked
// device must both fail with the identical terse 401 the no-certificate case
// produces, while an active device's certificate must let the wrapped
// handler run and hand it the resolved Device via DeviceFrom.
func TestRequireDeviceResolvesRealDevice(t *testing.T) {
	t.Parallel()

	// Arrange — a store with one active and one revoked device, each with a
	// certificate serial recorded against it.
	s, st := testServerWithStore(t)
	ctx := context.Background()

	active, err := st.CreateDevice(ctx, "active device")
	if err != nil {
		t.Fatalf("CreateDevice(active): %v", err)
	}
	activeSerial := big.NewInt(101)
	notBefore := time.Now()
	notAfter := notBefore.Add(90 * 24 * time.Hour)
	if err := st.RecordDeviceCert(ctx, active.ID, activeSerial.Text(16), notBefore, notAfter); err != nil {
		t.Fatalf("RecordDeviceCert(active): %v", err)
	}

	revoked, err := st.CreateDevice(ctx, "revoked device")
	if err != nil {
		t.Fatalf("CreateDevice(revoked): %v", err)
	}
	revokedSerial := big.NewInt(202)
	if err := st.RecordDeviceCert(ctx, revoked.ID, revokedSerial.Text(16), notBefore, notAfter); err != nil {
		t.Fatalf("RecordDeviceCert(revoked): %v", err)
	}
	if err := st.RevokeDevice(ctx, revoked.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	unknownSerial := big.NewInt(303)

	tests := []struct {
		name       string
		tlsS       *tls.ConnectionState
		wantStatus int
		wantRun    bool
	}{
		{name: "no client certificate", tlsS: nil, wantStatus: http.StatusUnauthorized},
		{name: "unknown certificate serial", tlsS: peerCertWithSerial(unknownSerial), wantStatus: http.StatusUnauthorized},
		{name: "revoked device", tlsS: peerCertWithSerial(revokedSerial), wantStatus: http.StatusUnauthorized},
		{name: "active device", tlsS: peerCertWithSerial(activeSerial), wantStatus: http.StatusOK, wantRun: true},
	}

	var rejectionBodies []string

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not run in parallel: rejectionBodies is collected across cases.

			// Arrange
			var ran bool
			var gotDevice state.Device
			var gotOK bool
			guarded := s.requireDevice(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				gotDevice, gotOK = DeviceFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/v1/protected", nil)
			req.TLS = tt.tlsS
			rec := httptest.NewRecorder()

			// Act
			guarded.ServeHTTP(rec, req)

			// Assert
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ran != tt.wantRun {
				t.Errorf("handler ran = %v, want %v", ran, tt.wantRun)
			}
			if tt.wantRun {
				if !gotOK {
					t.Error("DeviceFrom returned ok=false for a guarded handler behind an active device")
				}
				if gotDevice.ID != active.ID {
					t.Errorf("DeviceFrom device id = %q, want %q", gotDevice.ID, active.ID)
				}
			} else {
				var body errorBody
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Error != msgClientCertRequired {
					t.Errorf("error = %q, want the terse %q", body.Error, msgClientCertRequired)
				}
				rejectionBodies = append(rejectionBodies, rec.Body.String())
			}
		})
	}

	// Assert — every rejection reason must be byte-identical: the client
	// cannot distinguish "no certificate" from "unknown serial" from
	// "revoked device".
	for i := 1; i < len(rejectionBodies); i++ {
		if rejectionBodies[i] != rejectionBodies[0] {
			t.Errorf("rejection body %d = %q, want byte-identical to %q", i, rejectionBodies[i], rejectionBodies[0])
		}
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

// TestWriteJSONLogsEncodeFailureWithoutPanicking covers writeJSON's encode-
// failure branch: a channel value cannot be marshalled to JSON, and by the
// time Encode fails the status line and headers are already committed, so
// the only thing left to do is log it rather than try to correct the
// response.
func TestWriteJSONLogsEncodeFailureWithoutPanicking(t *testing.T) {
	t.Parallel()

	// Arrange
	log, buf := newCapturingLogger()
	s := NewServer(config.Config{StateDir: t.TempDir(), ListenAddr: ":8443", PolicyMode: policy.ModeDefault}, nil, nil, nil, nil, log)
	rec := httptest.NewRecorder()

	// Act — must not panic despite the unencodable body.
	s.writeJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	// Assert
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (already committed before the encode failure)", rec.Code)
	}
	if !bodyContains(buf.String(), "write response") {
		t.Errorf("log = %q, want it to mention the failed write", buf.String())
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
