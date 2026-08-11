// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// File names and modes for the certificate authority within
// $DEVMON_STATE_DIR/certs. The CA is retained across the agent's whole
// lifetime: it is what lets a device pin the agent's identity once and never
// have to re-pair, even when the server leaf is re-issued (see store.go).
const (
	// caValidity is 10 years. Unlike a leaf, a CA is never presented to a TLS
	// stack directly, so the mobile-TLS 398-day ceiling that bounds
	// serverCertValidity does not apply here.
	caValidity = 10 * 365 * 24 * time.Hour
	// deviceCertValidity is 90 days. Short enough that a lost or stolen
	// device's credential expires on its own well within a plausible
	// detection window; renewal (Task 8) keeps a healthy device from ever
	// noticing.
	deviceCertValidity = 90 * 24 * time.Hour

	caCertFile = "ca.crt"
	caKeyFile  = "ca.key"

	caKeyFileMode = 0o600
	caCrtFileMode = 0o644

	// caCommonName is cosmetic, like certCommonName in selfsigned.go — nothing
	// verifies against it.
	caCommonName = "devmon-agent CA"
)

// CA is the agent's certificate authority: a self-signed root that issues
// the server leaf and every paired device's client certificate. Its private
// key never leaves this process; nothing in this package logs it, its PEM
// encoding, or any other key material.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// LoadOrCreateCA returns the persisted certificate authority, generating and
// writing one if it is absent. created reports whether this call generated a
// new CA — the caller (main.go) logs the fingerprint at WARN exactly once,
// on that transition, so the operator can record it.
func LoadOrCreateCA(dir string, log *slog.Logger) (*CA, bool, error) {
	if err := os.MkdirAll(dir, certsDirMode); err != nil {
		return nil, false, fmt.Errorf("create certs dir %s: %w", dir, err)
	}

	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	exists, err := keypairExists(certPath, keyPath)
	if err != nil {
		return nil, false, err
	}

	created := false
	if !exists {
		if err := generateAndWriteCA(dir, log); err != nil {
			return nil, false, err
		}
		created = true
	}

	ca, err := loadCA(dir)
	if err != nil {
		return nil, false, err
	}
	return ca, created, nil
}

// generateAndWriteCA creates a new self-signed CA keypair and writes it to
// dir, using the same os.Root write discipline as ensureKeypair in store.go.
func generateAndWriteCA(dir string, log *slog.Logger) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return fmt.Errorf("generate CA certificate serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   caCommonName,
			Organization: []string{certOrganization},
		},
		NotBefore:             now.Add(-time.Minute), // tolerate minor host clock skew
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// x509 treats MaxPathLen=0 as "unset" unless MaxPathLenZero is true.
		// Without it the CA would encode no path-length constraint at all,
		// permitting sub-CAs it was never meant to allow.
		MaxPathLenZero: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: keyDER})

	// Writes go through an os.Root scoped to the certs directory, mirroring
	// ensureKeypair in store.go: the key is written first, exclusively, at
	// 0600, so it is never briefly world-readable.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open certs dir %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	if err := writeExclusive(root, caKeyFile, keyPEM, caKeyFileMode); err != nil {
		return err
	}
	if err := writeExclusive(root, caCertFile, certPEM, caCrtFileMode); err != nil {
		if rmErr := root.Remove(caKeyFile); rmErr != nil {
			log.Warn("could not remove the orphaned CA key after a failed certificate write",
				slog.Any("err", rmErr))
		}
		return err
	}
	return nil
}

// loadCA reads and parses a persisted CA keypair from dir. Reads go through
// an os.Root scoped to the certs directory, mirroring loadServerKeypair in
// store.go: no variable path is ever handed to a bare os call, so a path can
// never escape the certs directory — including via a symlink planted in the
// bind mount.
func loadCA(dir string) (*CA, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open certs dir %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	certPEMBytes, err := readFileInRoot(root, caCertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", certPath, err)
	}
	block, _ := pem.Decode(certPEMBytes)
	if block == nil || block.Type != pemTypeCertificate {
		return nil, fmt.Errorf("decode CA certificate %s: not a PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate %s: %w", certPath, err)
	}

	keyPEMBytes, err := readFileInRoot(root, caKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read CA key %s: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEMBytes)
	if keyBlock == nil || keyBlock.Type != pemTypePrivateKey {
		return nil, fmt.Errorf("decode CA key %s: not a PEM private key", keyPath)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key %s: %w", keyPath, err)
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key %s is not an ECDSA key", keyPath)
	}

	return &CA{cert: cert, key: key}, nil
}

// readFileInRoot reads name from an os.Root scoped to the certs directory,
// the read counterpart to writeExclusive in store.go.
func readFileInRoot(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Fingerprint is the hex SHA-256 digest over the CA certificate's DER bytes.
// It is the pinning anchor a client records at pairing time and is
// published on GET /v1/status — publishing it is the point, and nothing else
// about the CA may join it.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// CertPEM returns the CA certificate, PEM-encoded, for the client to pin.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: c.cert.Raw})
}

// Pool returns an x509.CertPool containing only this CA, suitable for
// verifying either a device's client certificate or the agent's own server
// leaf.
func (c *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	return pool
}
