//go:build e2e

package harness

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The pinned mTLS client (D7). PairDevice performs the real client's exact
// sequence — mint a code with the host-side CLI while the agent is running
// (D8), generate an ECDSA P-256 key and CSR, POST /v1/pair over a
// deliberately unverified TLS connection exactly as the README's `curl -k`
// step does, then pin the returned CA for every request after that. After
// pairing, no client built here ever sets InsecureSkipVerify again; the two
// permitted uses are this bootstrap request and Agent.waitReady's readiness
// probe (agent.go).

const (
	pemTypeCertificateRequest = "CERTIFICATE REQUEST"

	// deviceRequestTimeout bounds Do's ordinary, non-streaming requests. The
	// streaming client stream.go builds from a Device must use a client with
	// no timeout instead, via TLSConfig — this default would kill a
	// 30-minute endurance run.
	deviceRequestTimeout = 15 * time.Second

	// pairBootstrapTimeout bounds only the one-shot, unverified pairing
	// request.
	pairBootstrapTimeout = 10 * time.Second
)

// pairRequestBody and pairResponseBody are declared here, not imported from
// internal/httpapi (D4): the suite must notice a renamed JSON field, which it
// cannot do if it shares a struct with the server that produces it.
type pairRequestBody struct {
	PairingCode string `json:"pairing_code"`
	CSRPEM      string `json:"csr_pem"`
}

type pairResponseBody struct {
	DeviceID       string `json:"device_id"`
	CertificatePEM string `json:"certificate_pem"`
	CACertificate  string `json:"ca_certificate_pem"`
	NotAfter       string `json:"not_after"`
}

// Device is one paired client: an ECDSA P-256 key, the certificate the agent
// issued for it, and an http.Client whose RootCAs pool contains ONLY the
// pinned CA. No field of it is safe to print directly — use String(), which
// redacts, or the exported ID and CAFingerprint fields.
type Device struct {
	ID            string
	CAFingerprint string // SHA-256 hex of the pinned CA, comparable across restarts

	baseURL   string
	tlsConfig *tls.Config
	client    *http.Client
}

// String renders a Device safely: an ID and a fingerprint, never the private
// key or certificate bytes.
func (d *Device) String() string {
	return fmt.Sprintf("Device{ID: %s, CAFingerprint: %s}", d.ID, d.CAFingerprint)
}

// TLSConfig returns the pinned TLS configuration for this device, for a
// caller (the SSE reader in stream.go) that needs an http.Client with a
// different timeout policy than Do's own client.
func (d *Device) TLSConfig() *tls.Config {
	return d.tlsConfig.Clone()
}

// PairDevice performs the full documented pairing sequence against a running
// agent and returns a client pinned to the CA it received. name is passed to
// the host-side `device pair-code` command and becomes the device's display
// name; it does not become the certificate's CommonName, which the agent
// derives from the device ID regardless of what the CSR asks for
// (internal/certs/issue.go).
func PairDevice(t *testing.T, a *Agent, name string) *Device {
	t.Helper()

	code := MintPairingCode(t, a, name)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	csrPEM, err := buildCSRPEM(key)
	if err != nil {
		t.Fatalf("build device CSR: %v", err)
	}

	resp, ok := bootstrapPair(t, a, code, csrPEM)
	if !ok {
		t.Fatalf("pairing did not return a usable response")
	}

	caCert, err := parseSingleCertPEM(resp.CACertificate)
	if err != nil {
		t.Fatalf("parse ca_certificate_pem from pair response: %v", err)
	}
	deviceCert, err := parseSingleCertPEM(resp.CertificatePEM)
	if err != nil {
		t.Fatalf("parse certificate_pem from pair response: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	tlsCert := tls.Certificate{
		Certificate: [][]byte{deviceCert.Raw},
		PrivateKey:  key,
	}

	tlsCfg := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}

	sum := sha256.Sum256(caCert.Raw)

	return &Device{
		ID:            resp.DeviceID,
		CAFingerprint: hex.EncodeToString(sum[:]),
		baseURL:       a.BaseURL,
		tlsConfig:     tlsCfg,
		client: &http.Client{
			Timeout:   deviceRequestTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

// buildCSRPEM generates a PKCS#10 CSR for key. The Subject is intentionally
// minimal: internal/certs/issue.go ignores it entirely, so any value here is
// equally valid, and asserting that is a security property a future test may
// exercise by inspecting the issued certificate's CN instead.
func buildCSRPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	template := x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "devmon-e2e-device"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &template, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate request: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificateRequest, Bytes: der}), nil
}

// bootstrapPair issues the one-time, unverified POST /v1/pair request — the
// exact shape of the README's `curl -k` step. InsecureSkipVerify is
// permitted here and nowhere else besides Agent.waitReady's readiness probe
// (D7): the device has no pinned CA yet, so there is nothing else it could
// verify against.
func bootstrapPair(t *testing.T, a *Agent, code string, csrPEM []byte) (pairResponseBody, bool) {
	t.Helper()

	reqBody, err := json.Marshal(pairRequestBody{PairingCode: code, CSRPEM: string(csrPEM)})
	if err != nil {
		t.Fatalf("marshal pair request: %v", err)
	}

	client := &http.Client{
		Timeout: pairBootstrapTimeout,
		Transport: &http.Transport{
			// Bootstrap pairing request only; see the package comment on why
			// this is one of exactly two permitted uses (D7).
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- bootstrap pairing request only, before any CA is pinned (D7)
		},
	}

	resp, err := client.Post(a.BaseURL+"/v1/pair", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /v1/pair: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /v1/pair response body: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/pair: status = %d, want %d; body = %s", resp.StatusCode, http.StatusCreated, redact(raw))
	}

	var body pairResponseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode /v1/pair response: %v; body = %s", err, redact(raw))
	}
	return body, true
}

// parseSingleCertPEM decodes exactly one PEM-encoded certificate.
func parseSingleCertPEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// Do issues an mTLS request against path on the device's pinned client and
// returns the status, headers, and raw body. It never follows a redirect and
// never retries: this suite asserts the exact response the agent produced,
// not what a well-behaved client would eventually see after working around
// it. body, when non-nil, is marshalled as JSON.
func (d *Device) Do(t *testing.T, method, path string, body any) (status int, hdr http.Header, raw []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body for %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, d.baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, raw
}

// JSON issues a GET and decodes the response into a map, so assertions are
// written against the WIRE rather than the agent's own DTO structs (D4). A
// non-2xx status still decodes when the body is valid JSON — callers that
// need to assert an error body's shape can do so from the same call.
func (d *Device) JSON(t *testing.T, method, path string) (status int, obj map[string]any) {
	t.Helper()

	status, _, raw := d.Do(t, method, path, nil)
	if len(raw) == 0 {
		return status, nil
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode JSON response for %s %s: %v; body = %s", method, path, err, redact(raw))
	}
	return status, obj
}

// AssertExactKeys fails the test when obj's key set differs from want in
// either direction. A missing key is a broken client; an extra key is an
// unreviewed disclosure — both are failures the wire-shape contract exists to
// catch.
func AssertExactKeys(t *testing.T, obj map[string]any, want []string) {
	t.Helper()

	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)

	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	if !equalStringSlices(got, wantSorted) {
		t.Fatalf("response key set = %v, want %v", got, wantSorted)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// redactPatterns strip anything a failing assertion must never put into
// retained, world-readable CI output: PEM blocks and code-shaped tokens (a
// pairing code is crypto/rand.Text() — a run of uppercase letters and
// digits at least 20 characters long).
var (
	pemBlockPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
	codeShapedToken = regexp.MustCompile(`\b[A-Z2-7]{20,}\b`)
)

// redact strips PEM blocks and pairing-code-shaped tokens from body before it
// is safe to include in a test failure message (the REDACTED_FAILURE_OUTPUT
// pattern). CI logs are retained and world-readable, and the repository rule
// against logging key material or pairing codes binds test output too.
func redact(body []byte) string {
	s := pemBlockPattern.ReplaceAllString(string(body), "[REDACTED PEM BLOCK]")
	s = codeShapedToken.ReplaceAllString(s, "[REDACTED]")
	return strings.TrimSpace(s)
}
