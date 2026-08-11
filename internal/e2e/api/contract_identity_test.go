// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package api

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/e2e/harness"
)

// This file replays Phase 2's manual checklist: two devices pairing
// independently, pairings and server identity surviving a restart, a
// changed DEVMON_PUBLIC_ADDR re-issuing only the server leaf, immediate
// revocation, self-unpair under every policy mode, renewal keeping the old
// certificate usable, and the two diagnostic signals a client uses to tell
// "my code was already used" from "the server's identity changed under me".
// identity-pairing-and-revocation.plan.md:908-922.
//
// What this file deliberately does NOT cover: the *timing* of proactive
// renewal (renewing before expiry with no user interaction) and a genuinely
// expired client certificate diagnosing itself — both are named client-side
// in the phase plan's Coverage Map, because producing them needs a real
// wall-clock wait or a shortened certificate lifetime, and D19 forbids the
// latter.

// renewResponseBody is declared here, not imported from internal/httpapi
// (D4): the suite must notice a renamed JSON field, which it cannot do if it
// shares a struct with the server that produces it.
type renewResponseBody struct {
	CertificatePEM string `json:"certificate_pem"`
	NotAfter       string `json:"not_after"`
}

// serverLeafCertificate opens a fresh TLS connection to a using d's pinned
// trust and client credentials, completes the handshake, and returns the
// certificate the server presented — read from the live connection state,
// never from certs/server.crt on disk, because the claim under test is what
// a client sees.
func serverLeafCertificate(t *testing.T, a *harness.Agent, d *harness.Device) *x509.Certificate {
	t.Helper()

	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", a.Port), d.TLSConfig())
	if err != nil {
		t.Fatalf("TLS dial %s: %v", a.BaseURL, err)
	}
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatalf("%s presented no certificate", a.BaseURL)
	}
	return state.PeerCertificates[0]
}

// TestTwoDevicesPairIndependently asserts two operator-minted pairing codes
// yield two devices with distinct IDs and distinct certificate serials, both
// immediately usable, and both visible in `device list`.
func TestTwoDevicesPairIndependently(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	d1 := harness.PairDevice(t, a, "two-devices-first")
	d2 := harness.PairDevice(t, a, "two-devices-second")

	if d1.ID == d2.ID {
		t.Fatalf("both devices share ID %s, want distinct IDs", d1.ID)
	}
	if d1.CertSerialHex == d2.CertSerialHex {
		t.Fatalf("both devices share certificate serial %s, want distinct serials", d1.CertSerialHex)
	}

	for name, d := range map[string]*harness.Device{"first": d1, "second": d2} {
		status, _, _ := d.Do(t, http.MethodGet, "/v1/containers", nil)
		if status != http.StatusOK && status != http.StatusBadGateway {
			t.Errorf("%s device GET /v1/containers status = %d, want 200 or 502", name, status)
		}
	}

	rows := harness.ListDevices(t, a)
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.ID] = true
	}
	if !seen[d1.ID] {
		t.Errorf("device list does not contain the first device %s: %+v", d1.ID, rows)
	}
	if !seen[d2.ID] {
		t.Errorf("device list does not contain the second device %s: %+v", d2.ID, rows)
	}
}

// TestPairingsSurviveRestart asserts both of two paired devices still
// authenticate after the agent restarts on the same state directory, and the
// CA fingerprint each pinned at pairing time is unchanged.
func TestPairingsSurviveRestart(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	stateDir := t.TempDir()

	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	d1 := harness.PairDevice(t, a1, "restart-first")
	d2 := harness.PairDevice(t, a1, "restart-second")
	a1.Stop(t)

	// The agent's listen port is re-allocated on every start; a Device
	// minted against a1 must be repointed at a2's BaseURL before it can be
	// used again (harness.Device.Rebind).
	a2 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	d1 = d1.Rebind(a2)
	d2 = d2.Rebind(a2)

	for name, d := range map[string]*harness.Device{"first": d1, "second": d2} {
		t.Run(name, func(t *testing.T) {
			status, obj := d.JSON(t, http.MethodGet, "/v1/status")
			if status != http.StatusOK {
				t.Fatalf("GET /v1/status status = %d, want %d", status, http.StatusOK)
			}
			if obj["ca_fingerprint"] != d.CAFingerprint {
				t.Errorf("ca_fingerprint after restart = %v, want %s (unchanged)", obj["ca_fingerprint"], d.CAFingerprint)
			}
		})
	}

	// Independently confirm the mTLS handshake itself still succeeds for
	// both devices against the restarted agent, not just the status route.
	for name, d := range map[string]*harness.Device{"first": d1, "second": d2} {
		status, _, _ := d.Do(t, http.MethodGet, "/v1/containers", nil)
		if status != http.StatusOK && status != http.StatusBadGateway {
			t.Errorf("%s device GET /v1/containers after restart: status = %d, want 200 or 502", name, status)
		}
	}
}

// TestIdentityStableAcrossRestart asserts the server certificate's own
// serial number, read from the TLS peer certificate a client sees, is
// identical before and after a restart on the same state directory — the
// server keypair is persisted, not re-minted, on every start.
func TestIdentityStableAcrossRestart(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	stateDir := t.TempDir()

	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	d := harness.PairDevice(t, a1, "identity-stable")
	before := serverLeafCertificate(t, a1, d)
	a1.Stop(t)

	a2 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	after := serverLeafCertificate(t, a2, d)

	if before.SerialNumber.Cmp(after.SerialNumber) != 0 {
		t.Errorf("server certificate serial = %s after restart, want %s (unchanged)",
			after.SerialNumber.Text(16), before.SerialNumber.Text(16))
	}
}

// TestServerCertReissuedOnAddressChange asserts a restart with an additional
// DEVMON_PUBLIC_ADDR entry re-issues only the server leaf: its SANs change,
// the CA fingerprint every paired device pinned does not, and the already
// paired device keeps working with no re-pair.
func TestServerCertReissuedOnAddressChange(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	stateDir := t.TempDir()
	const extraSAN = "devmon-e2e-extra-host.example"

	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir, PublicAddr: "127.0.0.1"})
	d := harness.PairDevice(t, a1, "address-change")
	before := serverLeafCertificate(t, a1, d)
	a1.Stop(t)

	a2 := harness.StartAgent(t, harness.AgentOptions{
		StateDir:   stateDir,
		PublicAddr: "127.0.0.1," + extraSAN,
	})
	after := serverLeafCertificate(t, a2, d)
	// The listen port is re-allocated on every start; d must be repointed
	// at a2 before it can be used again (harness.Device.Rebind).
	d = d.Rebind(a2)

	if containsName(before.DNSNames, extraSAN) {
		t.Fatalf("the ORIGINAL certificate already covers %s; the test fixture is not exercising a change", extraSAN)
	}
	if !containsName(after.DNSNames, extraSAN) {
		t.Errorf("reissued certificate DNS names = %v, want it to include %s", after.DNSNames, extraSAN)
	}
	if before.SerialNumber.Cmp(after.SerialNumber) == 0 {
		t.Errorf("server certificate serial unchanged after an address change; a reissue was expected")
	}

	status, obj := d.JSON(t, http.MethodGet, "/v1/status")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/status status = %d, want %d", status, http.StatusOK)
	}
	if obj["ca_fingerprint"] != d.CAFingerprint {
		t.Errorf("ca_fingerprint changed after a server-leaf reissue = %v, want %s (unchanged)", obj["ca_fingerprint"], d.CAFingerprint)
	}

	// The already-paired device works with no re-pair: it never had to
	// touch the reissued leaf, only trust the CA that signed it.
	status, _, _ = d.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Errorf("paired device GET /v1/containers after address change: status = %d, want 200 or 502", status)
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestRevokedDeviceLosesAccessImmediately asserts a device revoked from the
// host CLI while the agent runs loses access on its very next request — no
// restart in between — and that revoking one device leaves an unrelated,
// still-paired device unaffected.
func TestRevokedDeviceLosesAccessImmediately(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	revoked := harness.PairDevice(t, a, "revoke-target")
	other := harness.PairDevice(t, a, "revoke-bystander")

	// Falsifiability: before revocation, the device this test is about to
	// revoke can in fact reach a guarded route.
	if status, _, _ := revoked.Do(t, http.MethodGet, "/v1/status", nil); status != http.StatusOK {
		t.Fatalf("sanity check before revocation: GET /v1/status status = %d, want %d", status, http.StatusOK)
	}

	harness.RevokeDevice(t, a, revoked.ID)

	status, obj := revoked.JSON(t, http.MethodGet, "/v1/containers")
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked device GET /v1/containers status = %d, want %d", status, http.StatusUnauthorized)
	}
	harness.AssertExactKeys(t, obj, []string{"error"})
	if obj["error"] != "client certificate required" {
		t.Errorf(`revoked device rejection error = %v, want "client certificate required"`, obj["error"])
	}

	status, _, _ = other.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Errorf("bystander device GET /v1/containers after an unrelated revocation: status = %d, want 200 or 502", status)
	}
}

// TestUnpairSelfWorksUnderEveryMode asserts DELETE /v1/device/self succeeds
// under read-only, default, and full policy modes alike — giving up your own
// access is never a privileged act — and that the device is rejected on its
// next request afterward.
func TestUnpairSelfWorksUnderEveryMode(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	modes := []string{"read-only", "", "full"}
	for _, mode := range modes {
		label := mode
		if label == "" {
			label = "default"
		}
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			a := harness.StartAgent(t, harness.AgentOptions{PolicyMode: mode})
			d := harness.PairDevice(t, a, "unpair-self-"+label)

			status, _, _ := d.Do(t, http.MethodDelete, "/v1/device/self", nil)
			if status != http.StatusNoContent {
				t.Fatalf("DELETE /v1/device/self status = %d, want %d", status, http.StatusNoContent)
			}

			status, _, _ = d.Do(t, http.MethodGet, "/v1/containers", nil)
			if status != http.StatusUnauthorized {
				t.Errorf("GET /v1/containers after self-unpair: status = %d, want %d", status, http.StatusUnauthorized)
			}
		})
	}
}

// TestRenewIssuesUsableCertAndKeepsOldValid is the automatable half of "a
// device near expiry renews with no user interaction": POST
// /v1/device/renew with a fresh CSR issues a certificate that authenticates,
// and the OLD certificate keeps authenticating too — it is superseded for
// bookkeeping, never revoked. The *timing* of when a real client decides to
// renew is a client-side policy decision and is not exercised here.
func TestRenewIssuesUsableCertAndKeepsOldValid(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	old := harness.PairDevice(t, a, "renew-target")

	newKey := harness.GenerateDeviceKey(t)
	newCSR := harness.DeviceCSRPEM(t, newKey, "devmon-e2e-device")

	status, _, raw := old.Do(t, http.MethodPost, "/v1/device/renew", map[string]string{"csr_pem": string(newCSR)})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/device/renew status = %d, want %d", status, http.StatusOK)
	}

	var resp renewResponseBody
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode renew response: %v", err)
	}
	if resp.CertificatePEM == "" || resp.NotAfter == "" {
		t.Fatalf("renew response missing certificate_pem or not_after: %+v", resp)
	}

	renewed := harness.NewDeviceFromRenewal(t, old, newKey, resp.CertificatePEM)
	if renewed.CertSerialHex == old.CertSerialHex {
		t.Errorf("renewed certificate serial equals the original %s, want a fresh serial", old.CertSerialHex)
	}

	status, _, _ = renewed.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Errorf("renewed certificate GET /v1/containers: status = %d, want 200 or 502", status)
	}

	// The old certificate is superseded, not revoked: it must keep working.
	status, _, _ = old.Do(t, http.MethodGet, "/v1/containers", nil)
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Errorf("old certificate GET /v1/containers after renewal: status = %d, want 200 or 502", status)
	}
}

// TestPairingCodeIsSingleUse asserts redeeming the same pairing code twice
// yields the first device on attempt one, and 401 on attempt two — the
// operator's code is spent by success, not merely by having been seen.
func TestPairingCodeIsSingleUse(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	code := harness.MintPairingCode(t, a, "single-use")

	key1 := harness.GenerateDeviceKey(t)
	csr1 := harness.DeviceCSRPEM(t, key1, "devmon-e2e-device")
	status, _ := harness.TryPairDevice(t, a, code, csr1)
	if status != http.StatusCreated {
		t.Fatalf("first redemption: status = %d, want %d", status, http.StatusCreated)
	}

	key2 := harness.GenerateDeviceKey(t)
	csr2 := harness.DeviceCSRPEM(t, key2, "devmon-e2e-device")
	status, _ = harness.TryPairDevice(t, a, code, csr2)
	if status != http.StatusUnauthorized {
		t.Errorf("second redemption of the same code: status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestUnknownAndMalformedPairingCodesAreIndistinguishable asserts an unknown
// pairing code and a malformed CSR against a valid code both yield the exact
// same 401 body — a scanner probing this open route learns nothing that
// tells the two cases apart.
func TestUnknownAndMalformedPairingCodesAreIndistinguishable(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})

	key := harness.GenerateDeviceKey(t)
	csr := harness.DeviceCSRPEM(t, key, "devmon-e2e-device")

	unknownStatus, unknownBody := harness.TryPairDevice(t, a, "DOESNOTEXIST00000000", csr)

	validCode := harness.MintPairingCode(t, a, "malformed-csr")
	malformedStatus, malformedBody := harness.TryPairDevice(t, a, validCode, []byte("not a csr"))

	if unknownStatus != http.StatusUnauthorized {
		t.Errorf("unknown code: status = %d, want %d", unknownStatus, http.StatusUnauthorized)
	}
	if malformedStatus != http.StatusUnauthorized {
		t.Errorf("malformed CSR: status = %d, want %d", malformedStatus, http.StatusUnauthorized)
	}
	if string(unknownBody) != string(malformedBody) {
		t.Errorf("unknown-code body %q and malformed-CSR body %q differ; a caller could tell the two failures apart",
			unknownBody, malformedBody)
	}
}

// TestCSRSubjectIsIgnored asserts a CSR that asks for CN=admin still gets a
// certificate whose CommonName is the agent's own device ID — the CSR
// Subject is attacker-controlled input and internal/certs/issue.go must
// never honor it.
func TestCSRSubjectIsIgnored(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a := harness.StartAgent(t, harness.AgentOptions{})
	code := harness.MintPairingCode(t, a, "csr-subject-ignored")

	key := harness.GenerateDeviceKey(t)
	csr := harness.DeviceCSRPEM(t, key, "admin")

	status, raw := harness.TryPairDevice(t, a, code, csr)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/pair status = %d, want %d", status, http.StatusCreated)
	}

	var resp struct {
		DeviceID       string `json:"device_id"`
		CertificatePEM string `json:"certificate_pem"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}

	commonName := certificateCommonName(t, resp.CertificatePEM)
	if commonName == "admin" {
		t.Fatalf("issued certificate CommonName = %q, the CSR's requested Subject was honored", commonName)
	}
	if commonName != resp.DeviceID {
		t.Errorf("issued certificate CommonName = %q, want the device ID %q", commonName, resp.DeviceID)
	}
}

// certificateCommonName parses a single PEM-encoded certificate and returns
// its Subject.CommonName, for tests that need to inspect what the agent
// actually issued rather than what a CSR asked for.
func certificateCommonName(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("no PEM block found in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.Subject.CommonName
}

// TestWrongCAIsRejected proves D7's pinning is load-bearing: a client
// trusting a different agent's CA fails the handshake against this one, even
// though its own certificate was validly issued.
func TestWrongCAIsRejected(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	a1 := harness.StartAgent(t, harness.AgentOptions{})
	a2 := harness.StartAgent(t, harness.AgentOptions{})

	d1 := harness.PairDevice(t, a1, "wrong-ca-source")

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: d1.TLSConfig()},
	}
	_, err := client.Get(a2.BaseURL + "/v1/status")
	if err == nil {
		t.Fatalf("a client pinned to agent 1's CA reached agent 2 successfully, want a handshake failure")
	}
}

// TestWipedStateChangesCAFingerprint asserts a freshly initialised state
// directory produces a different CA fingerprint than the one before it —
// the signal a client uses to tell "the server's identity changed" (an
// attack, or a genuine reset) apart from a merely expired credential, whose
// fingerprint would be unchanged. The expired-credential half of this
// diagnosis is client-side (see the package-level doc comment).
func TestWipedStateChangesCAFingerprint(t *testing.T) {
	t.Parallel()
	harness.RequireEngine(t)

	stateDir := t.TempDir()

	a1 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	before := harness.PairDevice(t, a1, "wipe-before")
	a1.Stop(t)

	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("wipe state directory %s: %v", stateDir, err)
	}

	// The agent itself recreates DEVMON_STATE_DIR if it is missing
	// (cmd/devmon-agent/main.go); the wipe above must leave nothing for it
	// to find, which is the point of this test.
	a2 := harness.StartAgent(t, harness.AgentOptions{StateDir: stateDir})
	after := harness.PairDevice(t, a2, "wipe-after")

	if before.CAFingerprint == after.CAFingerprint {
		t.Errorf("ca_fingerprint unchanged after wiping the state directory = %s, want a distinct value", after.CAFingerprint)
	}
}
