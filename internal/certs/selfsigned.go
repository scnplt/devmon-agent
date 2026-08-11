// SPDX-License-Identifier: AGPL-3.0-only

// Package certs owns the agent's TLS material on disk: generating a server
// certificate on first start, reusing it afterwards, and reporting when the
// configured addresses have drifted away from what it covers.
//
// Phase 1 is self-signed. Phase 2 replaces generation with CA issuance; the
// shape returned here is identical, so tlsconf and httpapi need no change.
//
// Nothing in this package ever logs key material, PEM bytes, or a file's
// contents — only SANs, serials, and expiry.
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
	"net"
	"time"
)

const (
	// serverCertValidity is the CA/Browser Forum maximum for a leaf certificate.
	// It is not a round number by accident: modern mobile TLS stacks reject
	// longer-lived leaves outright, so a "convenient" 10-year self-signed cert
	// would fail on the Android client with an opaque error.
	serverCertValidity = 398 * 24 * time.Hour

	// certCommonName is cosmetic. Verification uses SANs; CN has been ignored by
	// every relevant TLS stack for years.
	certCommonName = "devmon-agent"

	certOrganization = "DevMon"

	// serialBits is the entropy in the certificate serial number.
	serialBits = 128
)

const (
	pemTypeCertificate = "CERTIFICATE"
	// PKCS#8 rather than SEC1 ("EC PRIVATE KEY"): both work with Go, but PKCS#8
	// is the portable form for the Android client.
	pemTypePrivateKey = "PRIVATE KEY"
)

// GenerateServerCert produces a self-signed EC P-256 leaf covering sans.
//
// sans are split into DNS names and IP addresses automatically. notBefore is a
// parameter so tests can pin the validity window.
func GenerateServerCert(sans []string, notBefore time.Time) (certPEM, keyPEM []byte, err error) {
	if len(sans) == 0 {
		return nil, nil, fmt.Errorf("generate server certificate: no subject alternative names given")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, nil, fmt.Errorf("generate certificate serial: %w", err)
	}

	dnsNames, ips := splitSANs(sans)
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   certCommonName,
			Organization: []string{certOrganization},
		},
		NotBefore:             notBefore.Add(-time.Minute), // tolerate minor host clock skew
		NotAfter:              notBefore.Add(serverCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
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

// splitSANs sorts entries into IP addresses and DNS names. Config has already
// rejected anything that is neither.
func splitSANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, s)
	}
	return dnsNames, ips
}
