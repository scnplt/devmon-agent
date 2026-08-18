// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newDeviceCSR builds a PKCS#10 CSR signed by a fresh EC P-256 key, with
// subjectCN as the (attacker-controlled) subject the agent must ignore.
func newDeviceCSR(t *testing.T, subjectCN string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	template := x509.CertificateRequest{
		Subject: pkix.Name{CommonName: subjectCN},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &template, key)
	if err != nil {
		t.Fatalf("create device CSR: %v", err)
	}
	return der
}

func TestLoadOrCreateCAProperties(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()

	// Act
	ca, created, err := LoadOrCreateCA(dir, testLogger())

	// Assert
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh directory, want true")
	}
	if !ca.cert.IsCA {
		t.Error("IsCA = false, want true")
	}
	if !ca.cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if !ca.cert.MaxPathLenZero {
		t.Error("MaxPathLenZero = false, want true (unset permits sub-CAs)")
	}
	if ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("KeyUsage lacks CertSign")
	}
	if ca.cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("KeyUsage lacks CRLSign")
	}
	wantNotAfter := time.Now().Add(caValidity)
	if diff := ca.cert.NotAfter.Sub(wantNotAfter); diff > time.Hour || diff < -time.Hour {
		t.Errorf("NotAfter = %v, want ~%v (10-year CA)", ca.cert.NotAfter, wantNotAfter)
	}
}

func TestLoadOrCreateCAIsStable(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	first, firstCreated, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("first LoadOrCreateCA() unexpected error: %v", err)
	}
	if !firstCreated {
		t.Fatal("first created = false, want true")
	}

	// Act
	second, secondCreated, err := LoadOrCreateCA(dir, testLogger())

	// Assert
	if err != nil {
		t.Fatalf("second LoadOrCreateCA() unexpected error: %v", err)
	}
	if secondCreated {
		t.Error("second created = true, want false — a stable identity must not regenerate")
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("fingerprint changed across reload: %s then %s", first.Fingerprint(), second.Fingerprint())
	}
}

func TestIssueDeviceCertHasClientAuthEKUAndVerifiesAgainstPool(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	csrDER := newDeviceCSR(t, "admin")
	now := time.Now()

	// Act
	certPEM, serial, notAfter, err := ca.IssueDeviceCert(csrDER, "d3f9a1c2", now)

	// Assert
	if err != nil {
		t.Fatalf("IssueDeviceCert() unexpected error: %v", err)
	}
	if serial == "" {
		t.Error("serial is empty, want a hex string")
	}
	if strings.HasPrefix(serial, "0x") || strings.ToLower(serial) != serial {
		t.Errorf("serial = %q, want lowercase hex with no 0x prefix", serial)
	}

	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCertificate {
		t.Fatalf("certificate PEM block = %v, want %s", block, pemTypeCertificate)
	}
	if len(rest) != 0 {
		t.Errorf("trailing bytes after certificate PEM: %d", len(rest))
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued device certificate: %v", err)
	}

	// CSR subject asked for CN=admin; the agent must ignore it entirely.
	if leaf.Subject.CommonName != "d3f9a1c2" {
		t.Errorf("CommonName = %q, want the supplied device ID d3f9a1c2 (CSR subject must be ignored)",
			leaf.Subject.CommonName)
	}
	if leaf.IsCA {
		t.Error("IsCA = true, want false for a device leaf")
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("ExtKeyUsage = %v, want [ClientAuth] — crypto/tls rejects a client leaf without it",
			leaf.ExtKeyUsage)
	}
	// X.509 encodes time at one-second resolution, so compare within a
	// tolerance rather than requiring bit-for-bit equality.
	if diff := notAfter.Sub(leaf.NotAfter); diff > time.Second || diff < -time.Second {
		t.Errorf("returned notAfter = %v, want ~%v (issued certificate's NotAfter)", notAfter, leaf.NotAfter)
	}

	// Assert — verifies against the CA's own pool with ClientAuth required.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("Verify() against CA pool with ClientAuth = %v, want nil", err)
	}
}

func TestIssueDeviceCertRejectsBrokenSignature(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	csrDER := newDeviceCSR(t, "tamperable")
	// Corrupt the last byte, which lands in the signature for a CSR built
	// this way — CheckSignature must catch it rather than CreateCertificate
	// signing over an unverified request.
	csrDER[len(csrDER)-1] ^= 0xFF

	// Act
	_, _, _, err = ca.IssueDeviceCert(csrDER, "dev-id", time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueDeviceCert() error = nil, want a failure on a tampered CSR signature")
	}
}

func TestIssueDeviceCertRejectsNonECDSAKey(t *testing.T) {
	t.Parallel()

	// Arrange — an RSA CSR, which the device flow never produces but a
	// malicious client could send.
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	template := x509.CertificateRequest{Subject: pkix.Name{CommonName: "rsa-device"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &template, rsaKey)
	if err != nil {
		t.Fatalf("create RSA CSR: %v", err)
	}

	// Act
	_, _, _, err = ca.IssueDeviceCert(csrDER, "dev-id", time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueDeviceCert() error = nil, want a failure on a non-ECDSA CSR key")
	}
}

func TestIssueDeviceCertRejectsNonP256Curve(t *testing.T) {
	t.Parallel()

	// Arrange — an EC key on P-384, which is a supported curve but not the
	// one the device flow uses.
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384 key: %v", err)
	}
	template := x509.CertificateRequest{Subject: pkix.Name{CommonName: "p384-device"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &template, key)
	if err != nil {
		t.Fatalf("create P-384 CSR: %v", err)
	}

	// Act
	_, _, _, err = ca.IssueDeviceCert(csrDER, "dev-id", time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueDeviceCert() error = nil, want a failure on a non-P-256 CSR key")
	}
}

func TestIssueServerCertChainsToCA(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}

	// Act
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"vps.example.com"}, time.Now())

	// Assert
	if err != nil {
		t.Fatalf("IssueServerCert() unexpected error: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("keyPEM is empty")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCertificate {
		t.Fatalf("certificate PEM block = %v, want %s", block, pemTypeCertificate)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued server certificate: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("Verify() server leaf against CA pool = %v, want nil", err)
	}
	wantNotAfter := time.Now().Add(serverCertValidity)
	if diff := leaf.NotAfter.Sub(wantNotAfter); diff > time.Minute || diff < -time.Minute {
		t.Errorf("NotAfter = %v, want ~%v (398-day server leaf, not the CA validity)",
			leaf.NotAfter, wantNotAfter)
	}
}

func TestCAKeyIsOwnerOnly(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not modelled on Windows")
	}

	// Arrange / Act
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}

	// Assert
	info, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatalf("stat ca.key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != caKeyFileMode {
		t.Errorf("ca.key mode = %#o, want %#o", perm, caKeyFileMode)
	}
}

func TestCACertPEMMatchesFingerprint(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}

	// Act
	certPEM := ca.CertPEM()

	// Assert
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCertificate {
		t.Fatalf("CertPEM() block = %v, want %s", block, pemTypeCertificate)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CertPEM(): %v", err)
	}
	if !parsed.Equal(ca.cert) {
		t.Error("CertPEM() does not round-trip to the loaded CA certificate")
	}
}

func TestLoadOrCreateCARejectsHalfKeypair(t *testing.T) {
	t.Parallel()

	// Arrange — only ca.crt is present, as a partial restore would leave.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caCertFile), []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub CA cert: %v", err)
	}

	// Act
	_, _, err := LoadOrCreateCA(dir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateCA() error = nil, want a failure on a half CA keypair")
	}
	if !strings.Contains(err.Error(), "not both present") {
		t.Errorf("error %q does not explain the half CA keypair", err)
	}
}

func TestLoadOrCreateCARejectsCorruptFiles(t *testing.T) {
	t.Parallel()

	// Arrange — both files present, contents are garbage.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caCertFile), []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}

	// Act
	_, _, err := LoadOrCreateCA(dir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateCA() error = nil, want a failure on a corrupt CA certificate")
	}
	if !strings.Contains(err.Error(), "decode CA certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadOrCreateCARejectsWrongKeyType(t *testing.T) {
	t.Parallel()

	// Arrange — a valid CA certificate paired with an RSA private key, as a
	// corrupted or hand-edited certs directory might contain.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	rsaKeyDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal RSA key: %v", err)
	}
	rsaKeyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: rsaKeyDER})
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), rsaKeyPEM, 0o600); err != nil {
		t.Fatalf("overwrite CA key: %v", err)
	}

	// Act
	_, _, err = LoadOrCreateCA(dir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateCA() error = nil, want a failure on a non-ECDSA CA key")
	}
	if !strings.Contains(err.Error(), "not an ECDSA key") {
		t.Errorf("error %q does not identify the wrong key type", err)
	}
}

func TestGenerateAndWriteCAFailsWhenKeyFileExists(t *testing.T) {
	t.Parallel()

	// Arrange — ca.key already present makes writeExclusive's O_EXCL reject it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub CA key: %v", err)
	}

	// Act
	err := generateAndWriteCA(dir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("generateAndWriteCA() error = nil, want a failure on the pre-existing key")
	}
	if !strings.Contains(err.Error(), "create "+caKeyFile) {
		t.Errorf("error %q does not identify the failing file", err)
	}
}

func TestGenerateAndWriteCACleansUpOrphanedKeyOnCertWriteFailure(t *testing.T) {
	t.Parallel()

	// Arrange — ca.crt already present, so the key write succeeds and the
	// certificate write fails, triggering the rollback.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caCertFile), []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub CA cert: %v", err)
	}

	// Act
	err := generateAndWriteCA(dir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("generateAndWriteCA() error = nil, want a failure on the pre-existing certificate")
	}
	if !strings.Contains(err.Error(), "create "+caCertFile) {
		t.Errorf("error %q does not identify the failing file", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, caKeyFile)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("orphaned ca.key was not cleaned up after the failed certificate write: %v", statErr)
	}
}

func TestLoadOrCreateCAFailsWhenDirIsFile(t *testing.T) {
	t.Parallel()

	// Arrange — a regular file occupying the path LoadOrCreateCA expects to
	// MkdirAll into.
	parent := t.TempDir()
	blocked := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	// Act
	_, _, err := LoadOrCreateCA(blocked, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateCA() error = nil, want a failure when dir is a file")
	}
	if !strings.Contains(err.Error(), "create certs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestGenerateAndWriteCAFailsWhenDirMissing(t *testing.T) {
	t.Parallel()

	// Arrange — a nonexistent dir makes os.OpenRoot fail.
	parent := t.TempDir()
	missingDir := filepath.Join(parent, "does-not-exist")

	// Act
	err := generateAndWriteCA(missingDir, testLogger())

	// Assert
	if err == nil {
		t.Fatal("generateAndWriteCA() error = nil, want a failure when dir is missing")
	}
	if !strings.Contains(err.Error(), "open certs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCAFailsWhenDirMissing(t *testing.T) {
	t.Parallel()

	// Arrange — a nonexistent dir makes os.OpenRoot fail.
	parent := t.TempDir()
	missingDir := filepath.Join(parent, "does-not-exist")

	// Act
	_, err := loadCA(missingDir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure when dir is missing")
	}
	if !strings.Contains(err.Error(), "open certs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCARejectsUnparsableCertDER(t *testing.T) {
	t.Parallel()

	// Arrange — a well-typed PEM certificate block wrapping DER bytes that
	// do not parse as a certificate.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	garbageCertPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: []byte("not real DER")})
	if err := os.WriteFile(filepath.Join(dir, caCertFile), garbageCertPEM, 0o644); err != nil {
		t.Fatalf("overwrite CA cert: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure on unparsable certificate DER")
	}
	if !strings.Contains(err.Error(), "parse CA certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCARejectsUnparsableKeyDER(t *testing.T) {
	t.Parallel()

	// Arrange — a well-typed PEM private key block wrapping DER bytes that
	// do not parse as PKCS#8.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	garbageKeyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: []byte("not real DER")})
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), garbageKeyPEM, 0o600); err != nil {
		t.Fatalf("overwrite CA key: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure on unparsable key DER")
	}
	if !strings.Contains(err.Error(), "parse CA key") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCAFailsWhenKeyFileMissing(t *testing.T) {
	t.Parallel()

	// Arrange — only ca.crt is present, as loadCA would see it if called
	// directly outside the keypairExists gate that normally protects it.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatalf("remove ca.key: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure when ca.key is missing")
	}
	if !strings.Contains(err.Error(), "read CA key") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCAFailsWhenCertFileIsDirectory(t *testing.T) {
	t.Parallel()

	// Arrange — a directory in place of ca.crt opens successfully but fails
	// on read, exercising readFileInRoot's io.ReadAll error branch.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, caCertFile), 0o755); err != nil {
		t.Fatalf("mkdir in place of ca.crt: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure when ca.crt is a directory")
	}
	if !strings.Contains(err.Error(), "read CA certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCAFailsWhenCertFileMissing(t *testing.T) {
	t.Parallel()

	// Arrange — an empty directory, so readFileInRoot's Open fails.
	dir := t.TempDir()

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure when ca.crt is missing")
	}
	if !strings.Contains(err.Error(), "read CA certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadCARejectsCertPEMBlockWrongType(t *testing.T) {
	t.Parallel()

	// Arrange — a well-formed PEM block, but not a certificate.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	wrongTypePEM := pem.EncodeToMemory(&pem.Block{Type: "NOT A CERTIFICATE", Bytes: []byte("x")})
	if err := os.WriteFile(filepath.Join(dir, caCertFile), wrongTypePEM, 0o644); err != nil {
		t.Fatalf("overwrite CA cert: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure on a wrong-typed PEM block")
	}
	if !strings.Contains(err.Error(), "not a PEM certificate") {
		t.Errorf("error %q does not identify the wrong PEM type", err)
	}
}

func TestLoadCARejectsKeyPEMBlockWrongType(t *testing.T) {
	t.Parallel()

	// Arrange — a well-formed PEM block, but not a private key.
	dir := t.TempDir()
	if _, _, err := LoadOrCreateCA(dir, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	wrongTypePEM := pem.EncodeToMemory(&pem.Block{Type: "NOT A KEY", Bytes: []byte("x")})
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), wrongTypePEM, 0o600); err != nil {
		t.Fatalf("overwrite CA key: %v", err)
	}

	// Act
	_, err := loadCA(dir)

	// Assert
	if err == nil {
		t.Fatal("loadCA() error = nil, want a failure on a wrong-typed PEM block")
	}
	if !strings.Contains(err.Error(), "not a PEM private key") {
		t.Errorf("error %q does not identify the wrong PEM type", err)
	}
}

func TestIssueDeviceCertRejectsGarbageCSR(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}

	// Act
	_, _, _, err = ca.IssueDeviceCert([]byte("not a CSR"), "dev-id", time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueDeviceCert() error = nil, want a failure on an unparsable CSR")
	}
	if !strings.Contains(err.Error(), "parse device CSR") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestIssueDeviceCertFailsWhenSignerKeyDoesNotMatchCertificate(t *testing.T) {
	t.Parallel()

	// Arrange — a CA whose stored key does not match its stored certificate,
	// as a corrupted certs directory might produce. x509.CreateCertificate
	// refuses to sign with a mismatched key.
	dir := t.TempDir()
	real, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate unrelated key: %v", err)
	}
	mismatched := &CA{cert: real.cert, key: otherKey}
	csrDER := newDeviceCSR(t, "dev")

	// Act
	_, _, _, err = mismatched.IssueDeviceCert(csrDER, "dev-id", time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueDeviceCert() error = nil, want a failure on a mismatched signer key")
	}
	if !strings.Contains(err.Error(), "create device certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestIssueServerCertFailsWhenSignerKeyDoesNotMatchCertificate(t *testing.T) {
	t.Parallel()

	// Arrange — same mismatch as the device-cert case, exercised against
	// IssueServerCert's own x509.CreateCertificate call.
	dir := t.TempDir()
	real, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate unrelated key: %v", err)
	}
	mismatched := &CA{cert: real.cert, key: otherKey}

	// Act
	_, _, err = mismatched.IssueServerCert([]string{"vps.example.com"}, time.Now())

	// Assert
	if err == nil {
		t.Fatal("IssueServerCert() error = nil, want a failure on a mismatched signer key")
	}
	if !strings.Contains(err.Error(), "create server certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestIssueServerCertRejectsEmptySANs(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA() unexpected error: %v", err)
	}

	// Act
	_, _, err = ca.IssueServerCert(nil, time.Now())

	// Assert — a certificate with no SAN matches nothing and would fail on
	// the phone rather than at issuance.
	if err == nil {
		t.Fatal("IssueServerCert(nil) error = nil, want a failure")
	}
}
