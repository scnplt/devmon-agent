// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/x509"
	"errors"
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
