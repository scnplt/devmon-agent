// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/certs"
	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// testServerForPairing is a fourth Server helper, additive to testServer,
// testServerWithCA, and testServerWithStore: handlePair needs both a real
// *state.Store and a real *certs.CA, and no existing helper carries both.
func testServerForPairing(t *testing.T) (*Server, *state.Store, *certs.CA) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, ListenAddr: ":8443", PolicyMode: policy.ModeDefault}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), testLogger())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ca, _, err := certs.LoadOrCreateCA(t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return NewServer(cfg, st, ca, nil, nil, testLogger()), st, ca
}

// generateCSRPEM builds a PEM-encoded PKCS#10 CSR from a fresh EC P-256 key,
// with a Subject the caller controls — used to prove the CN is ignored.
func generateCSRPEM(t *testing.T, subjectCN string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CSR key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: subjectCN},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func postPair(s *Server, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestHandlePairSucceeds(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, ca := testServerForPairing(t)
	code, _, err := st.MintPairingCode(context.Background(), "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	body, err := json.Marshal(pairRequest{
		PairingCode: code,
		CSRPEM:      generateCSRPEM(t, "attacker-controlled"),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	// Act
	rec := postPair(s, body)

	// Assert
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var resp pairResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeviceID == "" {
		t.Error("device_id is empty")
	}
	if resp.CACertificate != string(ca.CertPEM()) {
		t.Error("ca_certificate_pem does not match ca.CertPEM()")
	}

	block, _ := pem.Decode([]byte(resp.CertificatePEM))
	if block == nil {
		t.Fatalf("certificate_pem is not a PEM block: %q", resp.CertificatePEM)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	if leaf.Subject.CommonName != resp.DeviceID {
		t.Errorf("issued certificate CN = %q, want device_id %q (CSR subject must be ignored)",
			leaf.Subject.CommonName, resp.DeviceID)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("issued certificate does not verify against the CA pool: %v", err)
	}
}

func TestHandlePairCodeReusedFails(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, _ := testServerForPairing(t)
	code, _, err := st.MintPairingCode(context.Background(), "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	firstBody, err := json.Marshal(pairRequest{PairingCode: code, CSRPEM: generateCSRPEM(t, "first")})
	if err != nil {
		t.Fatalf("marshal first request: %v", err)
	}
	if rec := postPair(s, firstBody); rec.Code != http.StatusCreated {
		t.Fatalf("first pairing status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	secondBody, err := json.Marshal(pairRequest{PairingCode: code, CSRPEM: generateCSRPEM(t, "second")})
	if err != nil {
		t.Fatalf("marshal second request: %v", err)
	}

	// Act
	rec := postPair(s, secondBody)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second pairing status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error != msgPairFailed {
		t.Errorf("error = %q, want the terse %q", body.Error, msgPairFailed)
	}
}

func TestHandlePairMalformedCSRFails(t *testing.T) {
	t.Parallel()

	// Arrange
	s, st, _ := testServerForPairing(t)
	code, _, err := st.MintPairingCode(context.Background(), "Pixel 9")
	if err != nil {
		t.Fatalf("MintPairingCode() unexpected error: %v", err)
	}
	body, err := json.Marshal(pairRequest{PairingCode: code, CSRPEM: "not a pem csr"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	// Act
	rec := postPair(s, body)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	var respBody errorBody
	if err := json.NewDecoder(rec.Body).Decode(&respBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if respBody.Error != msgPairFailed {
		t.Errorf("error = %q, want the terse %q", respBody.Error, msgPairFailed)
	}
}

func TestHandlePairOversizedBodyFails(t *testing.T) {
	t.Parallel()

	// Arrange
	s, _, _ := testServerForPairing(t)
	body, err := json.Marshal(pairRequest{
		PairingCode: "irrelevant",
		CSRPEM:      strings.Repeat("x", maxPairBodyBytes+1024),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	// Act
	rec := postPair(s, body)

	// Assert
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
}
