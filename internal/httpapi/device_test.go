// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
)

// pairedDevice is a device already paired against a live *state.Store and
// *certs.CA, plus the serial of the certificate that currently authenticates
// it. It exists so device.go tests can drive requireDevice without a real
// TLS handshake.
type pairedDevice struct {
	device state.Device
	serial *big.Int
}

// pairDeviceForTest issues a certificate for a fresh device and records it,
// mirroring what handlePair does, without going through the HTTP layer.
func pairDeviceForTest(t *testing.T, ctx context.Context, st *state.Store, ca *certs.CA, name string) pairedDevice {
	t.Helper()

	device, err := st.CreateDevice(ctx, name)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	csrPEM := generateCSRPEM(t, "irrelevant")
	csrDER, ok := decodeCSRPEM(csrPEM)
	if !ok {
		t.Fatalf("decodeCSRPEM: failed to decode generated CSR")
	}

	now := time.Now()
	certPEM, serialHex, notAfter, err := ca.IssueDeviceCert(csrDER, device.ID, now)
	if err != nil {
		t.Fatalf("IssueDeviceCert: %v", err)
	}
	if err := st.RecordDeviceCert(ctx, device.ID, serialHex, now, notAfter); err != nil {
		t.Fatalf("RecordDeviceCert: %v", err)
	}

	leaf, err := x509.ParseCertificate(certPEMBytes(t, certPEM))
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}

	return pairedDevice{device: device, serial: leaf.SerialNumber}
}

// certPEMBytes extracts the DER bytes from a PEM-encoded certificate, for
// tests that need to inspect the parsed certificate rather than its PEM
// text.
func certPEMBytes(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("certificate is not a PEM block: %q", certPEM)
	}
	return block.Bytes
}

// requestWithPeerSerial builds a guarded request carrying a single peer
// certificate whose serial number is serial.
func requestWithPeerSerial(method, path string, body []byte, serial *big.Int) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.TLS = peerCertWithSerial(serial)
	return req
}

func TestHandleRenewIssuesNewerCertificateAndKeepsOldSerialValid(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	oldDevice, err := st.DeviceByCertSerial(ctx, paired.serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial(old): %v", err)
	}

	body, err := json.Marshal(renewRequest{CSRPEM: generateCSRPEM(t, "irrelevant")})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", body, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp renewResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	block, err := x509.ParseCertificate(certPEMBytes(t, []byte(resp.CertificatePEM)))
	if err != nil {
		t.Fatalf("parse renewed certificate: %v", err)
	}
	newSerial := block.SerialNumber
	if newSerial.Cmp(paired.serial) == 0 {
		t.Error("renewed certificate has the same serial as the original")
	}

	notAfter, err := time.Parse(time.RFC3339, resp.NotAfter)
	if err != nil {
		t.Fatalf("not_after %q is not RFC3339: %v", resp.NotAfter, err)
	}
	if !notAfter.After(oldDevice.PairedAt) {
		t.Errorf("renewed not_after %v is not after paired_at %v", notAfter, oldDevice.PairedAt)
	}

	// Both the old and the new serial must still resolve to the same
	// device (D6): a lost renewal response must not strand the caller.
	fromOld, err := st.DeviceByCertSerial(ctx, paired.serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial(old serial after renewal): %v", err)
	}
	if fromOld.ID != paired.device.ID {
		t.Errorf("old serial resolves to device %q, want %q", fromOld.ID, paired.device.ID)
	}
	fromNew, err := st.DeviceByCertSerial(ctx, newSerial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial(new serial): %v", err)
	}
	if fromNew.ID != paired.device.ID {
		t.Errorf("new serial resolves to device %q, want %q", fromNew.ID, paired.device.ID)
	}
}

func TestHandleRenewSucceedsUnderReadOnlyPolicy(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	s.cfg.PolicyMode = policy.ModeReadOnly
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")

	body, err := json.Marshal(renewRequest{CSRPEM: generateCSRPEM(t, "irrelevant")})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", body, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert — renewal is never gated by policy mode: it is not a
	// privileged operation.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 under read-only policy; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRenewRejectsMalformedCSR(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")

	body, err := json.Marshal(renewRequest{CSRPEM: "not a pem csr"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", body, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUnpairSelfRevokesAndBlocksSubsequentRequests(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")

	unpairReq := requestWithPeerSerial(http.MethodDelete, "/v1/device/self", nil, paired.serial)
	unpairRec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(unpairRec, unpairReq)

	// Assert — 204 with no body.
	if unpairRec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", unpairRec.Code, unpairRec.Body.String())
	}
	if unpairRec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", unpairRec.Body.String())
	}

	device, err := st.DeviceByCertSerial(ctx, paired.serial.Text(16))
	if err != nil {
		t.Fatalf("DeviceByCertSerial after unpair: %v", err)
	}
	if !device.IsRevoked() {
		t.Error("device is not revoked after self-unpair")
	}

	// Act — the device's very next guarded request.
	nextReq := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", mustMarshalRenew(t), paired.serial)
	nextRec := httptest.NewRecorder()
	s.routes().ServeHTTP(nextRec, nextReq)

	// Assert
	if nextRec.Code != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401; body: %s", nextRec.Code, nextRec.Body.String())
	}
}

func TestHandleUnpairSelfSucceedsUnderEveryPolicyMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode policy.Mode
	}{
		{name: "read-only", mode: policy.ModeReadOnly},
		{name: "default", mode: policy.ModeDefault},
		{name: "full", mode: policy.ModeFull},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, st, ca := testServerForPairing(t)
			s.cfg.PolicyMode = tt.mode
			ctx := context.Background()
			paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
			req := requestWithPeerSerial(http.MethodDelete, "/v1/device/self", nil, paired.serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert — self-unpair is never gated by policy mode.
			if rec.Code != http.StatusNoContent {
				t.Errorf("status = %d, want 204 under %s policy; body: %s", rec.Code, tt.mode, rec.Body.String())
			}
		})
	}
}

// TestHandleRenewWritesAuditRowWithAuthenticatedDeviceID is issue #44's renew
// coverage: a renewal must write exactly one audit row, carrying the
// authenticated caller's own device ID (D15), never a path parameter (the
// route has none).
func TestHandleRenewWritesAuditRowWithAuthenticatedDeviceID(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", mustMarshalRenew(t), paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Operation != opRenew {
		t.Errorf("operation = %q, want %q", entries[0].Operation, opRenew)
	}
	if entries[0].Outcome != state.OutcomeSuccess {
		t.Errorf("outcome = %q, want %q", entries[0].Outcome, state.OutcomeSuccess)
	}
	if entries[0].DeviceID != paired.device.ID {
		t.Errorf("device_id = %q, want the authenticated device's ID %q", entries[0].DeviceID, paired.device.ID)
	}
}

// TestHandleUnpairSelfWritesAuditRowWithAuthenticatedDeviceID is issue #44's
// self-revoke coverage: revoking one's own access is the highest-value
// identity event, and must always leave an audit trail carrying the
// authenticated caller's own device ID.
func TestHandleUnpairSelfWritesAuditRowWithAuthenticatedDeviceID(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	req := requestWithPeerSerial(http.MethodDelete, "/v1/device/self", nil, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Operation != opUnpairSelf {
		t.Errorf("operation = %q, want %q", entries[0].Operation, opUnpairSelf)
	}
	if entries[0].Outcome != state.OutcomeSuccess {
		t.Errorf("outcome = %q, want %q", entries[0].Outcome, state.OutcomeSuccess)
	}
	if entries[0].DeviceID != paired.device.ID {
		t.Errorf("device_id = %q, want the authenticated device's ID %q", entries[0].DeviceID, paired.device.ID)
	}
}

// TestHandleRenewRateLimitedWritesNoAuditRow proves D7's ordering holds for
// the guarded identity routes too: withDeviceLimit sits inside requireDevice
// but outside withIdentityAudit, so a throttled renewal must never write a
// row.
func TestHandleRenewRateLimitedWritesNoAuditRow(t *testing.T) {
	t.Parallel()

	// Arrange — a guarded tier of burst 2 (guardedPerSec 1 x
	// guardedBurstMultiplier 2), so the third request in a row is throttled
	// before withIdentityAudit ever runs.
	dir := t.TempDir()
	cfg := config.Config{
		StateDir:          dir,
		ListenAddr:        ":8443",
		PolicyMode:        policy.ModeDefault,
		RateGuardedPerSec: 1,
	}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), testLogger())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ca, _, err := certs.LoadOrCreateCA(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	s := NewServer(cfg, st, ca, nil, nil, testLogger())
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")

	// Act — drain the device's burst of 2 with successful renewals.
	for i := 0; i < 2; i++ {
		req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", mustMarshalRenew(t), paired.serial)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("renewal %d status = %d, want 200; body: %s", i, rec.Code, rec.Body.String())
		}
	}
	rowsBeforeThrottle, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ListAudit before throttle: %v", err)
	}

	// Act — a third request, past the burst.
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", mustMarshalRenew(t), paired.serial)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third renewal status = %d, want 429", rec.Code)
	}
	rowsAfterThrottle, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ListAudit after throttle: %v", err)
	}
	if len(rowsAfterThrottle) != len(rowsBeforeThrottle) {
		t.Errorf("audit rows after throttled request = %d, want unchanged from %d",
			len(rowsAfterThrottle), len(rowsBeforeThrottle))
	}
}

func mustMarshalRenew(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(renewRequest{CSRPEM: generateCSRPEM(t, "irrelevant")})
	if err != nil {
		t.Fatalf("marshal renew request: %v", err)
	}
	return body
}

// TestHandleRenewRejectsOversizedBody covers decodeRenewRequest's 413 branch:
// a body past maxRenewBodyBytes must be rejected before it ever reaches
// decodeCSRPEM.
func TestHandleRenewRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	body, err := json.Marshal(renewRequest{CSRPEM: strings.Repeat("x", maxRenewBodyBytes+1024)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", body, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleRenewRejectsMalformedJSON covers decodeRenewRequest's 400 branch
// for a body that is not valid JSON at all.
func TestHandleRenewRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", []byte("{not json"), paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleRenewFailsClosedWithoutResolvedDevice is the mandatory GOTCHA for
// handleRenew: it only ever runs behind requireDevice, but if it somehow runs
// without a device resolved in the request context, it must fail closed with
// 500 rather than panic or proceed with a zero-value device ID.
func TestHandleRenewFailsClosedWithoutResolvedDevice(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _, _ := testServerForPairing(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/device/renew", bytes.NewReader(mustMarshalRenew(t)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// Act — the handler is called directly, bypassing requireDevice.
	s.handleRenew(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestHandleUnpairSelfFailsClosedWithoutResolvedDevice mirrors
// TestHandleRenewFailsClosedWithoutResolvedDevice for handleUnpairSelf.
func TestHandleUnpairSelfFailsClosedWithoutResolvedDevice(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _, _ := testServerForPairing(t)
	req := httptest.NewRequest(http.MethodDelete, "/v1/device/self", nil)
	rec := httptest.NewRecorder()

	// Act — the handler is called directly, bypassing requireDevice.
	s.handleUnpairSelf(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestHandleUnpairSelfRevokeFailureIsInternalError drives handleUnpairSelf's
// RevokeDevice error branch directly: a Device resolved in context whose ID
// was never actually persisted makes RevokeDevice return
// state.ErrDeviceNotFound, which handleUnpairSelf must map to 500 like any
// other store failure — it never reaches this handler through the real
// requireDevice path, since that middleware only ever injects a device it
// just looked up successfully.
func TestHandleUnpairSelfRevokeFailureIsInternalError(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _, _ := testServerForPairing(t)
	ctx := deviceContext("device-never-persisted")
	req := httptest.NewRequest(http.MethodDelete, "/v1/device/self", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Act
	s.handleUnpairSelf(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
}

// TestRenewDeviceIssueCertFailureIsInternalError drives renewDevice's
// IssueDeviceCert error branch: decodeCSRPEM only checks that the CSR is a
// well-formed "CERTIFICATE REQUEST" PEM block, not the key algorithm inside
// it, so an RSA-keyed CSR reaches renewDevice and fails at IssueDeviceCert
// (which requires ECDSA P-256).
func TestRenewDeviceIssueCertFailureIsInternalError(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	body, err := json.Marshal(renewRequest{CSRPEM: generateRSACSRPEM(t)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := requestWithPeerSerial(http.MethodPost, "/v1/device/renew", body, paired.serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	var respBody errorBody
	if err := json.NewDecoder(rec.Body).Decode(&respBody); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if respBody.Error != msgDeviceInternalError {
		t.Errorf("error = %q, want %q", respBody.Error, msgDeviceInternalError)
	}
}

// TestRenewDeviceRecordCertFailureReturnsError drives renewDevice's
// RecordDeviceCert error branch directly, unlike the other renewDevice tests
// above: a closed *state.Store makes IssueDeviceCert succeed (it never
// touches the store) but RecordDeviceCert fail, which is otherwise
// unreachable through the HTTP layer because requireDevice itself needs a
// live store to authenticate the caller.
func TestRenewDeviceRecordCertFailureReturnsError(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	ctx := context.Background()
	paired := pairDeviceForTest(t, ctx, st, ca, "Pixel 9")
	csrDER, ok := decodeCSRPEM(generateCSRPEM(t, "irrelevant"))
	if !ok {
		t.Fatalf("decodeCSRPEM: failed to decode generated CSR")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Act
	_, err := s.renewDevice(ctx, paired.device.ID, csrDER)

	// Assert
	if err == nil {
		t.Fatal("renewDevice() error = nil, want a store failure after the store was closed")
	}
}
