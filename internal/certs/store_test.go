// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serverVerifyOptions returns the x509.VerifyOptions a client uses to check
// a server leaf against the pinned CA.
func serverVerifyOptions(ca *CA) x509.VerifyOptions {
	return x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// testCA returns a freshly created CA rooted at dir, for tests that only
// care about the server-certificate half of the package.
func testCA(t *testing.T, dir string) *CA {
	t.Helper()
	ca, _, err := LoadOrCreateCA(dir, testLogger())
	if err != nil {
		t.Fatalf("LoadOrCreateCA(%s) unexpected error: %v", dir, err)
	}
	return ca
}

func TestLoadOrCreateServerCertChainsToCA(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca := testCA(t, dir)

	// Act
	pair, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err != nil {
		t.Fatalf("LoadOrCreateServerCert() unexpected error: %v", err)
	}
	if _, err := pair.Leaf.Verify(serverVerifyOptions(ca)); err != nil {
		t.Errorf("server leaf does not chain to the CA: %v", err)
	}
}

func TestLoadOrCreateServerCertIsIdempotent(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca := testCA(t, dir)
	sans := []string{"vps.example.com"}

	// Act
	first, err := LoadOrCreateServerCert(dir, sans, ca, testLogger())
	if err != nil {
		t.Fatalf("first LoadOrCreateServerCert() unexpected error: %v", err)
	}
	second, err := LoadOrCreateServerCert(dir, sans, ca, testLogger())
	if err != nil {
		t.Fatalf("second LoadOrCreateServerCert() unexpected error: %v", err)
	}

	// Assert — a changed serial across restarts would mean a new identity,
	// which unpairs every device.
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Errorf("serial changed across reload: %v then %v",
			first.Leaf.SerialNumber, second.Leaf.SerialNumber)
	}
}

func TestLoadOrCreateServerCertPropagatesReissueFailure(t *testing.T) {
	t.Parallel()

	// Arrange — create a real keypair, then make reissueServerCert's
	// os.Remove calls fail while the SAN drift check still finds a
	// perfectly loadable keypair, so LoadOrCreateServerCert reaches and
	// propagates the reissue failure. The removal-failure mechanism is
	// platform specific: os.Remove on an open file succeeds on Linux (no
	// delete-sharing semantics) but fails on Windows, so Windows holds the
	// key file open while every other platform makes the containing
	// directory non-writable instead — which blocks unlink(2) on both the
	// certificate and the key, so the exact failing file differs by
	// platform but the propagation path under test does not.
	dir := t.TempDir()
	ca := testCA(t, dir)
	if _, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger()); err != nil {
		t.Fatalf("first LoadOrCreateServerCert() unexpected error: %v", err)
	}

	if runtime.GOOS == "windows" {
		locked, err := os.Open(filepath.Join(dir, serverKeyFile))
		if err != nil {
			t.Fatalf("open server.key to lock it: %v", err)
		}
		t.Cleanup(func() { _ = locked.Close() })
	} else {
		// A non-writable directory blocks unlink(2) with EACCES on Linux,
		// where the CI coverage floor is enforced. Restore the mode before
		// t.TempDir()'s own cleanup runs, or it cannot remove the directory
		// either.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("make dir non-writable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatalf("restore dir mode: %v", err)
			}
		})
	}

	// Act — a new address triggers SAN drift, which cannot remove the
	// stale certificate or key.
	_, err := LoadOrCreateServerCert(dir, []string{"vps.example.com", "new.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateServerCert() error = nil, want a failure when the stale keypair cannot be removed")
	}
	if !strings.Contains(err.Error(), "remove stale server") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestLoadOrCreateServerCertReissuesOnSANDrift(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	ca := testCA(t, dir)
	first, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger())
	if err != nil {
		t.Fatalf("first LoadOrCreateServerCert() unexpected error: %v", err)
	}

	// Act — the operator adds an address the stored certificate does not cover.
	newSANs := []string{"vps.example.com", "new.example.com"}
	second, err := LoadOrCreateServerCert(dir, newSANs, ca, testLogger())

	// Assert
	if err != nil {
		t.Fatalf("second LoadOrCreateServerCert() unexpected error: %v", err)
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) == 0 {
		t.Error("serial unchanged after SAN drift, want re-issuance")
	}
	if err := second.Leaf.VerifyHostname("new.example.com"); err != nil {
		t.Errorf("re-issued leaf does not cover new.example.com: %v", err)
	}
	if err := second.Leaf.VerifyHostname("vps.example.com"); err != nil {
		t.Errorf("re-issued leaf does not cover vps.example.com: %v", err)
	}
	if _, err := second.Leaf.Verify(serverVerifyOptions(ca)); err != nil {
		t.Errorf("re-issued leaf does not chain to the CA: %v", err)
	}
}

func TestServerKeyIsOwnerOnly(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not modelled on Windows")
	}

	// Arrange / Act
	dir := t.TempDir()
	ca := testCA(t, dir)
	if _, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger()); err != nil {
		t.Fatalf("LoadOrCreateServerCert() unexpected error: %v", err)
	}

	// Assert
	info, err := os.Stat(filepath.Join(dir, serverKeyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != serverKeyFileMode {
		t.Errorf("server.key mode = %#o, want %#o", perm, serverKeyFileMode)
	}
}

func TestLoadOrCreateServerCertRejectsHalfKeypair(t *testing.T) {
	t.Parallel()

	// Arrange — a certificate with no key, as a partial restore would leave.
	dir := t.TempDir()
	ca := testCA(t, dir)
	if err := os.WriteFile(filepath.Join(dir, serverCertFile), []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub cert: %v", err)
	}

	// Act
	_, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger())

	// Assert — regenerating would produce a mismatched pair, so this must be loud.
	if err == nil {
		t.Fatal("LoadOrCreateServerCert() error = nil, want a failure on a half keypair")
	}
	if !strings.Contains(err.Error(), "not both present") {
		t.Errorf("error %q does not explain the half keypair", err)
	}
}

func TestLoadOrCreateServerCertRejectsCorruptKey(t *testing.T) {
	t.Parallel()

	// Arrange — both files present, key is garbage.
	dir := t.TempDir()
	ca := testCA(t, dir)
	if err := os.WriteFile(filepath.Join(dir, serverCertFile), []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverKeyFile), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// Act
	_, err := LoadOrCreateServerCert(dir, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateServerCert() error = nil, want a load failure")
	}
	if !strings.Contains(err.Error(), "load server keypair") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestWriteExclusiveRejectsExistingFile(t *testing.T) {
	t.Parallel()

	// Arrange
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "taken"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	// Act
	err = writeExclusive(root, "taken", []byte("data"), 0o600)

	// Assert — O_EXCL against a file that already exists must fail loudly
	// rather than overwrite whatever is there.
	if err == nil {
		t.Fatal("writeExclusive() error = nil, want a failure on an existing file")
	}
	if !strings.Contains(err.Error(), "create taken") {
		t.Errorf("error %q does not identify the failing file", err)
	}
}

func TestLoadOrCreateServerCertFailsWhenDirIsFile(t *testing.T) {
	t.Parallel()

	// Arrange — a regular file occupying the path LoadOrCreateServerCert
	// expects to MkdirAll into.
	parent := t.TempDir()
	blocked := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	ca := testCA(t, parent)

	// Act
	_, err := LoadOrCreateServerCert(blocked, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateServerCert() error = nil, want a failure when dir is a file")
	}
	if !strings.Contains(err.Error(), "create certs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestReissueServerCertFailsWhenCertPathIsNonEmptyDir(t *testing.T) {
	t.Parallel()

	// Arrange — a non-empty directory in place of server.crt makes
	// os.Remove fail with something other than fs.ErrNotExist.
	dir := t.TempDir()
	ca := testCA(t, dir)
	certPath := filepath.Join(dir, "server.crt")
	if err := os.Mkdir(certPath, 0o755); err != nil {
		t.Fatalf("mkdir certPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certPath, "inner"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write inside certPath: %v", err)
	}
	keyPath := filepath.Join(dir, "server.key")

	// Act
	err := reissueServerCert(dir, certPath, keyPath, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("reissueServerCert() error = nil, want a failure removing a non-empty directory")
	}
	if !strings.Contains(err.Error(), "remove stale server certificate") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestReissueServerCertFailsWhenKeyPathIsNonEmptyDir(t *testing.T) {
	t.Parallel()

	// Arrange — server.crt is a plain, removable file; server.key is a
	// non-empty directory, so its removal fails with a real error.
	dir := t.TempDir()
	ca := testCA(t, dir)
	certPath := filepath.Join(dir, "server.crt")
	if err := os.WriteFile(certPath, []byte("stub"), 0o644); err != nil {
		t.Fatalf("write certPath: %v", err)
	}
	keyPath := filepath.Join(dir, "server.key")
	if err := os.Mkdir(keyPath, 0o755); err != nil {
		t.Fatalf("mkdir keyPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keyPath, "inner"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write inside keyPath: %v", err)
	}

	// Act
	err := reissueServerCert(dir, certPath, keyPath, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("reissueServerCert() error = nil, want a failure removing a non-empty directory")
	}
	if !strings.Contains(err.Error(), "remove stale server key") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestWriteServerCertFailsWhenIssuanceFails(t *testing.T) {
	t.Parallel()

	// Arrange — no SANs makes ca.IssueServerCert fail before any file is
	// touched.
	dir := t.TempDir()
	ca := testCA(t, dir)

	// Act
	_, err := LoadOrCreateServerCert(dir, nil, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("LoadOrCreateServerCert(nil sans) error = nil, want an issuance failure")
	}
	if !strings.Contains(err.Error(), "no subject alternative names given") {
		t.Errorf("error %q does not surface the issuance failure", err)
	}
}

func TestWriteServerCertCleansUpOrphanedKeyOnCertWriteFailure(t *testing.T) {
	t.Parallel()

	// Arrange — server.crt is already present so writeExclusive's O_EXCL
	// rejects it after the key has already been written.
	dir := t.TempDir()
	ca := testCA(t, dir)
	certPath := filepath.Join(dir, serverCertFile)
	if err := os.WriteFile(certPath, []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub cert: %v", err)
	}

	// Act
	err := writeServerCert(dir, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("writeServerCert() error = nil, want a failure on the pre-existing certificate")
	}
	if !strings.Contains(err.Error(), "create "+serverCertFile) {
		t.Errorf("error %q does not identify the failing file", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, serverKeyFile)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("orphaned server.key was not cleaned up after the failed certificate write: %v", statErr)
	}
}

func TestKeypairExistsReturnsErrorOnRealStatFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		certPath func(dir string) string
		keyPath  func(dir string) string
	}{
		{
			name:     "certificate path fails to stat",
			certPath: func(dir string) string { return filepath.Join(dir, "server.crt\x00bad") },
			keyPath:  func(dir string) string { return filepath.Join(dir, "server.key") },
		},
		{
			name:     "key path fails to stat",
			certPath: func(dir string) string { return filepath.Join(dir, "server.crt") },
			keyPath:  func(dir string) string { return filepath.Join(dir, "server.key\x00bad") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange — a NUL byte makes os.Stat fail with something other
			// than fs.ErrNotExist on every platform, unlike a merely
			// missing path.
			dir := t.TempDir()

			// Act
			exists, err := keypairExists(tt.certPath(dir), tt.keyPath(dir))

			// Assert
			if err == nil {
				t.Fatal("keypairExists() error = nil, want a real stat failure")
			}
			if exists {
				t.Error("exists = true, want false alongside the error")
			}
			if !strings.Contains(err.Error(), "stat") {
				t.Errorf("error %q does not identify the failing step", err)
			}
		})
	}
}

func TestWriteServerCertFailsWhenDirMissing(t *testing.T) {
	t.Parallel()

	// Arrange — a nonexistent dir makes os.OpenRoot fail.
	parent := t.TempDir()
	ca := testCA(t, parent)
	missingDir := filepath.Join(parent, "does-not-exist")

	// Act
	err := writeServerCert(missingDir, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("writeServerCert() error = nil, want a failure when dir is missing")
	}
	if !strings.Contains(err.Error(), "open certs dir") {
		t.Errorf("error %q does not identify the failing step", err)
	}
}

func TestWriteServerCertFailsWhenKeyFileExists(t *testing.T) {
	t.Parallel()

	// Arrange — server.key already present makes the first writeExclusive
	// call fail before the certificate is ever written.
	dir := t.TempDir()
	ca := testCA(t, dir)
	if err := os.WriteFile(filepath.Join(dir, serverKeyFile), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub key: %v", err)
	}

	// Act
	err := writeServerCert(dir, []string{"vps.example.com"}, ca, testLogger())

	// Assert
	if err == nil {
		t.Fatal("writeServerCert() error = nil, want a failure on the pre-existing key")
	}
	if !strings.Contains(err.Error(), "create "+serverKeyFile) {
		t.Errorf("error %q does not identify the failing file", err)
	}
}

func TestIpStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []net.IP
		want []string
	}{
		{name: "empty", in: nil, want: []string{}},
		{name: "single", in: []net.IP{net.ParseIP("203.0.113.7")}, want: []string{"203.0.113.7"}},
		{
			name: "multiple",
			in:   []net.IP{net.ParseIP("203.0.113.7"), net.ParseIP("2001:db8::1")},
			want: []string{"203.0.113.7", "2001:db8::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := ipStrings(tt.in)

			// Assert
			if len(got) != len(tt.want) {
				t.Fatalf("ipStrings() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ipStrings()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCheckIdentityConsistencyReturnsErrorOnRealStatFailure(t *testing.T) {
	t.Parallel()

	// Arrange — a NUL byte in the certs directory path makes the underlying
	// keypairExists stat fail with something other than fs.ErrNotExist.
	certsDir := filepath.Join(t.TempDir(), "certs\x00bad")

	// Act
	err := CheckIdentityConsistency(certsDir, true)

	// Assert
	if err == nil {
		t.Fatal("CheckIdentityConsistency() error = nil, want a real stat failure")
	}
	if errors.Is(err, ErrIdentityIncomplete) {
		t.Error("error wraps ErrIdentityIncomplete, want a plain stat failure to propagate instead")
	}
}

func TestCheckIdentityConsistency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		dbExisted     bool
		createCA      bool
		wantErr       error
		wantErrSubstr string
	}{
		{
			name:      "no database and no CA is a genuine first run",
			dbExisted: false,
			createCA:  false,
			wantErr:   nil,
		},
		{
			name:      "database and CA both present is the normal running state",
			dbExisted: true,
			createCA:  true,
			wantErr:   nil,
		},
		{
			name:          "database present without a CA is a partial restore",
			dbExisted:     true,
			createCA:      false,
			wantErr:       ErrIdentityIncomplete,
			wantErrSubstr: "certificate authority",
		},
		{
			name:          "CA present without a database is a partial restore",
			dbExisted:     false,
			createCA:      true,
			wantErr:       ErrIdentityIncomplete,
			wantErrSubstr: "state database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			dir := t.TempDir()
			if tt.createCA {
				testCA(t, dir)
			}

			// Act
			err := CheckIdentityConsistency(dir, tt.dbExisted)

			// Assert
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckIdentityConsistency(dbExisted=%v, caPresent=%v) error = %v, want errors.Is match for %v",
					tt.dbExisted, tt.createCA, err, tt.wantErr)
			}
			if tt.wantErrSubstr != "" && !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("error %q does not name which half is missing (want substring %q)", err, tt.wantErrSubstr)
			}
		})
	}
}
