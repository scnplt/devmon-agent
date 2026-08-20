// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// IssueDeviceCert signs a client leaf from a device-supplied PKCS#10 CSR.
//
// The CSR's Subject is attacker-controlled and is ignored entirely — the
// leaf's CommonName always comes from commonName, the agent's own record of
// the device (its ID). A CSR asking to be CN=admin does not get it.
//
// now is a parameter so tests can pin the validity window, matching
// IssueServerCert's convention.
func (c *CA) IssueDeviceCert(csrDER []byte, commonName string, now time.Time) (certPEM []byte, serial string, notAfter time.Time, err error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("parse device CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("verify device CSR signature: %w", err)
	}

	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", time.Time{}, fmt.Errorf("device CSR public key is not ECDSA")
	}
	if pub.Curve != elliptic.P256() {
		return nil, "", time.Time{}, fmt.Errorf("device CSR public key is not P-256")
	}

	serialNum, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("generate device certificate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore: now.Add(-time.Minute), // tolerate minor host clock skew
		NotAfter:  now.Add(deviceCertValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		// A leaf missing ClientAuth fails the TLS handshake with an opaque
		// "bad certificate" alert, before any handler or middleware runs.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, c.cert, pub, c.key)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("create device certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	return certPEM, serialNum.Text(16), template.NotAfter, nil
}

// IssueServerCert produces a CA-signed EC P-256 server leaf covering sans,
// replacing the self-signed leaf Phase 1 generated. sans are split into DNS
// names and IP addresses automatically.
//
// serverCertValidity — not caValidity — bounds this leaf: modern mobile TLS
// stacks reject certificates longer-lived than the CA/Browser Forum maximum,
// and clients pin the CA, not this leaf, so its short life is invisible to
// them.
func (c *CA) IssueServerCert(sans []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(sans) == 0 {
		return nil, nil, fmt.Errorf("issue server certificate: no subject alternative names given")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}

	serialNum, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, nil, fmt.Errorf("generate server certificate serial: %w", err)
	}

	dnsNames, ips := splitSANs(sans)
	template := x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			CommonName:   certCommonName,
			Organization: []string{certOrganization},
		},
		NotBefore:             now.Add(-time.Minute), // tolerate minor host clock skew
		NotAfter:              now.Add(serverCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create server certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal server key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: keyDER})
	return certPEM, keyPEM, nil
}
