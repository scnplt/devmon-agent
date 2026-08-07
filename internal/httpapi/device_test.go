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
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/certs"
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

func mustMarshalRenew(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(renewRequest{CSRPEM: generateCSRPEM(t, "irrelevant")})
	if err != nil {
		t.Fatalf("marshal renew request: %v", err)
	}
	return body
}
